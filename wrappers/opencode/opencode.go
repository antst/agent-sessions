package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/wrappers/host"
	"github.com/antst/sessionbus/wrappers/mcp"
)

const (
	Product        = "opencode-peer"
	ToolName       = "sessionbus"
	LaneSocketEnv  = "SESSIONBUS_LANE_SOCKET"
	bootstrapProbe = 5 * time.Second
)

var (
	laneCommand   = exec.Command
	listenTCP     = net.Listen
	bootstrapWait = bootstrapProbe
	retryReady    = func(ctx context.Context) error {
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

type Wrapper struct {
	socket, provisional string
	backend             mcp.Backend
	mu                  sync.Mutex
	child               *host.Child
	command             *exec.Cmd
	client              *nativeClient
	id                  string
	eventCancel         context.CancelFunc
	runDone             <-chan struct{}
	opened, closing     bool
	shutdown            func()
}

func New(socket, provisional string) *Wrapper {
	return &Wrapper{socket: socket, provisional: provisional}
}
func (p *Wrapper) SetShutdown(shutdown func()) { p.shutdown = shutdown }
func (p *Wrapper) SetCall(call func(context.Context, string, any) (json.RawMessage, error)) {
	p.backend = mcp.BackendFunc(call)
}

func (*Wrapper) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{
		Product: Product, Version: "1.18.29", SupportedOpenFields: []string{"cwd", "permission_mode", "model", "arguments"},
		ExtraArguments: []sessionkit.ExtraArgument{
			{Name: "--agent", Description: "OpenCode agent", TakesValue: true},
			{Name: "--print-logs", Description: "Print OpenCode logs"},
			{Name: "--log-level", Description: "OpenCode log level", TakesValue: true},
			{Name: "--mdns", Description: "Enable mDNS discovery"},
			{Name: "--mdns-domain", Description: "mDNS domain", TakesValue: true},
			{Name: "--cors", Description: "Allowed CORS origin", TakesValue: true},
		},
	}, nil
}

func (p *Wrapper) Open(ctx context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	if p.provisional == "" {
		return sessionkit.OpenResult{}, errors.New("OpenCode provisional identity is unavailable")
	}
	cwd, err := filepath.Abs(first(request.Open.Cwd, "."))
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	name, err := namePart(request.Name)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	arguments, model, agent, err := launchArguments(request.Open)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	permission := "ask"
	if request.Open.PermissionMode == "bypassPermissions" {
		permission = "allow"
	}
	lock, err := host.AcquireSessionLock(p.socket, "opencode", p.provisional)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	endpoint, err := host.ListenPrivate(p.socket, p.provisional)
	if err != nil {
		_ = lock.Close()
		return sessionkit.OpenResult{}, err
	}
	if p.backend == nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, errors.New("sessionbus lane backend is unavailable"))
	}
	go func() { _ = mcp.ServeLane(ctx, endpoint, p.backend) }()
	listener, err := listenTCP("tcp", "127.0.0.1:0")
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, err)
	}
	address := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	username, password, err := credentials()
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, err)
	}
	argv := append([]string{"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(address.Port)}, arguments...)
	command := laneCommand("opencode", argv...)
	command.Dir, command.Stderr = cwd, os.Stderr
	command.Env = scrub(os.Environ(), LaneSocketEnv, "OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD")
	command.Env = append(command.Env, LaneSocketEnv+"="+endpoint.Path, "OPENCODE_SERVER_USERNAME="+username, "OPENCODE_SERVER_PASSWORD="+password)
	child, input, output, err := host.StartChild(command, lock, endpoint)
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, fmt.Errorf("start OpenCode server: %w", err))
	}
	_ = input.Close()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, output); _ = output.Close(); close(drained) }()
	client := &nativeClient{endpoint: "http://127.0.0.1:" + strconv.Itoa(address.Port), username: username, password: password, directory: cwd, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	p.mu.Lock()
	p.child, p.command, p.client = child, command, client
	p.mu.Unlock()
	go p.watch(child, drained)
	createdID := ""
	cleanup := func(cause error) (sessionkit.OpenResult, error) {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		if createdID != "" {
			cause = errors.Join(cause, client.remove(ctx, createdID))
		}
		_ = child.Close(ctx, func(context.Context) error { return command.Process.Signal(syscall.SIGTERM) })
		return sessionkit.OpenResult{}, cause
	}
	if err = waitReady(ctx, child.Done(), client.bootstrap, client.ready); err != nil {
		return cleanup(err)
	}
	eventCtx, eventCancel := context.WithCancel(ctx)
	events, err := client.subscribe(eventCtx, func(eventCtx context.Context, event nativeEvent) error {
		if event.Type != "permission.asked" {
			return nil
		}
		p.mu.Lock()
		id := p.id
		p.mu.Unlock()
		if id == "" || event.Properties.SessionID != id {
			return nil
		}
		return client.rejectPermission(eventCtx, id, event.Properties.ID)
	})
	if err != nil {
		eventCancel()
		return cleanup(err)
	}
	p.mu.Lock()
	p.eventCancel = eventCancel
	p.mu.Unlock()
	var session nativeSession
	if request.ResumeSessionID == "" {
		session, err = client.create(ctx, name, permission)
		if err == nil {
			createdID = session.ID
		}
	} else if !validNativeID(request.ResumeSessionID) {
		err = errors.New("OpenCode resume session id is invalid")
	} else {
		session, err = client.resume(ctx, request.ResumeSessionID, name, permission)
	}
	if err != nil {
		return cleanup(err)
	}
	if err = client.configure(ctx, session.ID, model, agent); err != nil {
		return cleanup(err)
	}
	if err = lock.Rename(session.ID); err != nil {
		return cleanup(err)
	}
	p.mu.Lock()
	p.id, p.eventCancel, p.opened = session.ID, eventCancel, true
	p.mu.Unlock()
	select {
	case <-child.Done():
		return cleanup(errors.New("OpenCode exited before open committed"))
	case err = <-events:
		return cleanup(err)
	default:
	}
	go p.watchEvents(events)
	return sessionkit.OpenResult{SessionID: session.ID}, nil
}

func (p *Wrapper) Run(ctx context.Context, run *sessionkit.Run, input string) (sessionkit.TurnResult, error) {
	return p.run(ctx, run, input)
}

type runToken interface {
	Admitted()
	AdmittedDone() <-chan struct{}
	Done() <-chan struct{}
	Interrupted() bool
}

func (p *Wrapper) run(ctx context.Context, run runToken, input string) (sessionkit.TurnResult, error) {
	if strings.TrimSpace(input) == "" {
		return sessionkit.TurnResult{}, errors.New("OpenCode prompt is empty")
	}
	messageID, err := randomMessageID()
	if err != nil {
		return sessionkit.TurnResult{}, err
	}
	client, id, err := p.beginRun(run.Done())
	if err != nil {
		return sessionkit.TurnResult{}, err
	}
	if _, err = client.prompt(ctx, id, messageID, input, "steer", true); err != nil {
		return sessionkit.TurnResult{}, err
	}
	run.Admitted()
	if err = client.wait(ctx, id); err != nil {
		return sessionkit.TurnResult{}, err
	}
	result, stop, err := client.result(ctx, id, messageID)
	if err != nil {
		return sessionkit.TurnResult{}, err
	}
	outcome := "completed"
	if run.Interrupted() {
		outcome = "interrupted"
		if stop == "" {
			stop = "interrupt"
		}
	}
	return sessionkit.TurnResult{Outcome: outcome, Result: result, NativeStopReason: stop}, nil
}

func (p *Wrapper) beginRun(done <-chan struct{}) (*nativeClient, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opened || p.closing || p.client == nil || p.id == "" {
		return nil, "", errors.New("OpenCode session is not available")
	}
	p.runDone = done
	return p.client, p.id, nil
}

func (p *Wrapper) Interrupt(ctx context.Context, run *sessionkit.Run) error {
	return p.interrupt(ctx, run)
}

func (p *Wrapper) interrupt(ctx context.Context, run runToken) error {
	if run != nil {
		select {
		case <-run.Done():
			return nil
		default:
		}
		select {
		case <-run.Done():
			return nil
		case <-run.AdmittedDone():
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-run.Done():
			return nil
		default:
		}
	}
	client, id, err := p.live()
	if err != nil {
		return err
	}
	return client.interrupt(ctx, id)
}

func (p *Wrapper) Deliver(ctx context.Context, request sessionkit.DeliveryRequest, run *sessionkit.Run) (sessionkit.DeliveryReceipt, error) {
	var token runToken
	if run != nil {
		token = run
	}
	return p.deliver(ctx, request, token)
}

func (p *Wrapper) deliver(ctx context.Context, request sessionkit.DeliveryRequest, run runToken) (sessionkit.DeliveryReceipt, error) {
	client, id, err := p.live()
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	body, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	delivery, resume, disposition := "queue", false, "queued_for_next_turn"
	active, err := activeDelivery(ctx, run)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if active {
		delivery, resume, disposition = "steer", true, "injected"
	}
	_, err = client.prompt(ctx, id, deliveryMessageID(request.MessageID), body, delivery, resume)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	return sessionkit.DeliveryReceipt{Disposition: disposition}, nil
}

func activeDelivery(ctx context.Context, run runToken) (bool, error) {
	if run == nil {
		return false, nil
	}
	select {
	case <-run.Done():
		return false, nil
	default:
	}
	select {
	case <-run.Done():
		return false, nil
	case <-run.AdmittedDone():
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-run.Done():
		return false, nil
	default:
		return true, nil
	}
}

func (p *Wrapper) Close(ctx context.Context, request sessionkit.SessionCloseRequest) error {
	p.mu.Lock()
	p.closing = true
	client, id, child, command, cancel := p.client, p.id, p.child, p.command, p.eventCancel
	p.mu.Unlock()
	var forget error
	if request.Forget && client != nil && id != "" {
		forget = client.remove(ctx, id)
	}
	if cancel != nil {
		cancel()
	}
	if child == nil {
		return forget
	}
	return errors.Join(forget, child.Close(ctx, func(context.Context) error { return command.Process.Signal(syscall.SIGTERM) }))
}

func (p *Wrapper) live() (*nativeClient, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opened || p.closing || p.client == nil || p.id == "" {
		return nil, "", errors.New("OpenCode session is not available")
	}
	return p.client, p.id, nil
}

func (p *Wrapper) watch(child *host.Child, drained <-chan struct{}) {
	_ = child.Wait()
	<-drained
	p.failAfterRun()
}

func (p *Wrapper) watchEvents(events <-chan error) {
	err := <-events
	if err != nil {
		p.failAfterRun()
	}
}

func (p *Wrapper) failAfterRun() {
	p.mu.Lock()
	if !p.opened || p.closing || p.shutdown == nil {
		p.mu.Unlock()
		return
	}
	p.closing = true
	done, shutdown := p.runDone, p.shutdown
	p.mu.Unlock()
	if done != nil {
		<-done
	}
	shutdown()
}

func credentials() (string, string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", "", err
	}
	return "sessionbus", hex.EncodeToString(value), nil
}

func namePart(name string) (string, error) {
	index := strings.LastIndexByte(name, '@')
	if index < 1 {
		return "", errors.New("OpenCode lane name is invalid")
	}
	return name[:index], nil
}

func closeLaunch(lock *host.SessionLock, endpoint *host.PrivateEndpoint, err error) error {
	return errors.Join(err, endpoint.Close(), lock.Close())
}

func scrub(environment []string, names ...string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(names, name) {
			result = append(result, entry)
		}
	}
	return result
}

func connectionFailure(err error) bool {
	var operation *net.OpError
	return errors.As(err, &operation)
}

func waitReady(ctx context.Context, exited <-chan struct{}, bootstrap func(context.Context) error, ready func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	watched := make(chan struct{})
	defer func() { cancel(nil); <-watched }()
	go func() {
		defer close(watched)
		select {
		case <-exited:
			cancel(errors.New("OpenCode exited before plugin readiness"))
		case <-ctx.Done():
		}
	}()
	for {
		attempt, stop := context.WithTimeout(ctx, bootstrapWait)
		err := bootstrap(attempt)
		timedOut := errors.Is(attempt.Err(), context.DeadlineExceeded)
		stop()
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if err == nil || timedOut {
			break
		}
		if !connectionFailure(err) {
			return err
		}
		if err = retryReady(ctx); err != nil {
			return err
		}
	}
	for {
		ok, err := ready(ctx)
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if err == nil && ok {
			return nil
		}
		if err != nil && !connectionFailure(err) {
			return err
		}
		if err = retryReady(ctx); err != nil {
			return err
		}
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ sessionkit.WorkerCallbacks = (*Wrapper)(nil)
