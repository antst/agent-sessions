package claude

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/wrappers/host"
	"github.com/antst/sessionbus/wrappers/mcp"
)

const (
	Product       = "claude-peer"
	LaneSocketEnv = "SESSIONBUS_LANE_SOCKET"
)

type Wrapper struct {
	socket                string
	handoff               host.Handoff
	backend               mcp.Backend
	mu                    sync.Mutex
	child                 *host.Child
	input                 io.WriteCloser
	encoder               *json.Encoder
	expected, controlID   string
	ready                 chan error
	readyOne              sync.Once
	init, opened, closing bool
	failed                error
	active                *turn
	run                   *sessionkit.Run
	control               chan error
	writes, replays, next uint64
	shutdown              func()
}

type turn struct {
	owner    *Wrapper
	done     chan turnDone
	write    uint64
	consumed bool
}

type turnDone struct {
	result sessionkit.TurnResult
	err    error
}

type frame struct {
	Type           string
	Subtype        string
	SessionID      string `json:"session_id"`
	Result         string
	TerminalReason string `json:"terminal_reason"`
	Error          string
	IsError        bool `json:"is_error"`
	IsReplay       bool
	Response       controlResponse
}

type controlResponse struct {
	RequestID string `json:"request_id"`
	Subtype   string
	Error     string
}

func New(socket string) *Wrapper { return &Wrapper{socket: socket} }

func (p *Wrapper) SetShutdown(shutdown func()) { p.shutdown = shutdown }

func (p *Wrapper) SetCall(call func(context.Context, string, any) (json.RawMessage, error)) {
	p.backend = mcp.BackendFunc(call)
}

func (*Wrapper) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{
		Product: Product, SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"},
		ExtraArguments: []sessionkit.ExtraArgument{{Name: "--agent", Description: "Claude agent", TakesValue: true}},
	}, nil
}

func (p *Wrapper) Open(ctx context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	id, err := sessionID(request.ResumeSessionID)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	lock, err := host.AcquireSessionLock(p.socket, "claude", id)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	endpoint, err := host.ListenPrivate(p.socket, id)
	if err != nil {
		_ = lock.Close()
		return sessionkit.OpenResult{}, err
	}
	arguments, err := launchArguments(request, id, endpoint.Path)
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, err)
	}
	if p.backend == nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, errors.New("Sessionbus lane backend is unavailable"))
	}
	go func() { _ = mcp.ServeLane(ctx, endpoint, p.backend) }()
	command := exec.Command("claude", arguments...)
	command.Dir, command.Stderr = request.Open.Cwd, os.Stderr
	environment := slices.DeleteFunc(os.Environ(), func(value string) bool { return strings.HasPrefix(value, LaneSocketEnv+"=") })
	command.Env = append(environment, LaneSocketEnv+"="+endpoint.Path)
	child, input, output, err := host.StartChild(command, lock, endpoint)
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, fmt.Errorf("start Claude stream: %w", err))
	}
	p.mu.Lock()
	p.child, p.input, p.encoder, p.expected, p.ready, p.opened = child, input, json.NewEncoder(input), id, make(chan error, 1), true
	p.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		p.read(output)
	}()
	go p.watch(child, drained)
	return sessionkit.OpenResult{SessionID: id}, nil
}

func (p *Wrapper) Run(ctx context.Context, run *sessionkit.Run, input string) (sessionkit.TurnResult, error) {
	p.mu.Lock()
	p.run = run
	p.mu.Unlock()
	return p.handoff.Run(ctx, run, input, p.start)
}

func (p *Wrapper) Interrupt(ctx context.Context, run *sessionkit.Run) error {
	return p.handoff.Interrupt(ctx, run)
}

func (p *Wrapper) Deliver(ctx context.Context, request sessionkit.DeliveryRequest, _ *sessionkit.Run) (sessionkit.DeliveryReceipt, error) {
	return p.handoff.Deliver(ctx, request, p.inject)
}

func (p *Wrapper) Close(ctx context.Context, _ sessionkit.SessionCloseRequest) error {
	p.mu.Lock()
	p.closing = true
	child, input := p.child, p.input
	p.mu.Unlock()
	if child == nil {
		return nil
	}
	return child.Close(ctx, func(context.Context) error { return input.Close() })
}

func (p *Wrapper) start(ctx context.Context, prompt string) (host.Turn, error) {
	p.mu.Lock()
	if p.failed != nil || p.active != nil {
		err := firstError(p.failed, errors.New("Claude lane is not idle"))
		p.mu.Unlock()
		return nil, err
	}
	t := &turn{owner: p, done: make(chan turnDone, 1)}
	p.active = t
	err := p.write(prompt)
	needsInit := !p.init
	p.mu.Unlock()
	if err == nil && needsInit {
		err = p.waitReady(ctx)
	}
	if err != nil {
		p.fail(err)
		return nil, err
	}
	return t, nil
}

func (p *Wrapper) inject(_ context.Context, prompt string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil || p.failed != nil {
		return false, nil
	}
	return true, p.write(prompt)
}

func (p *Wrapper) write(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("Claude lane prompt is empty")
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "<cross-session-message ") {
		prompt = "The following Sessionbus peer message is the current user turn. Act on its enclosed content and preserve its sender metadata.\n\n" + prompt
	}
	body := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": prompt}}}}
	if err := p.encoder.Encode(body); err != nil {
		return err
	}
	p.writes++
	if p.active != nil {
		p.active.write, p.active.consumed = p.writes, false
	}
	return nil
}

func (t *turn) Wait(ctx context.Context) (sessionkit.TurnResult, error) {
	select {
	case <-ctx.Done():
		return sessionkit.TurnResult{}, ctx.Err()
	case done := <-t.done:
		return done.result, done.err
	}
}

func (t *turn) Interrupt(ctx context.Context) error { return t.owner.interrupt(ctx, t) }

func (p *Wrapper) interrupt(ctx context.Context, t *turn) error {
	p.mu.Lock()
	if p.active != t || p.failed != nil {
		p.mu.Unlock()
		return nil
	}
	p.control = make(chan error, 1)
	p.next++
	p.controlID = fmt.Sprintf("interrupt-%d", p.next)
	err := p.encoder.Encode(map[string]any{"type": "control_request", "request_id": p.controlID, "request": map[string]string{"subtype": "interrupt"}})
	control := p.control
	if err != nil {
		p.control = nil
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-control:
		return err
	}
}

func (p *Wrapper) read(output io.ReadCloser) {
	defer output.Close()
	decoder := json.NewDecoder(output)
	for {
		var item frame
		if err := decoder.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				p.fail(io.EOF)
			} else {
				p.fail(fmt.Errorf("malformed Claude stream frame: %w", err))
			}
			return
		}
		p.receive(item)
	}
}

func (p *Wrapper) receive(item frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch item.Type {
	case "system":
		if item.Subtype == "init" {
			if item.SessionID != p.expected {
				p.failLocked(fmt.Errorf("Claude stream changed native session from %q to %q", p.expected, item.SessionID))
				return
			}
			p.init = true
			p.signalReady(nil)
		}
	case "user":
		if item.IsReplay {
			p.replays++
			if p.active != nil && p.replays >= p.active.write {
				p.active.consumed = true
			}
		}
	case "control_response":
		if p.control != nil && item.Response.RequestID == p.controlID {
			err := errors.New(first(item.Response.Error, "Claude rejected interrupt control request"))
			if item.Response.Subtype == "success" {
				err = nil
			}
			p.control <- err
			p.control = nil
		}
	case "result":
		if item.SessionID != "" && item.SessionID != p.expected {
			p.failLocked(fmt.Errorf("Claude result changed native session from %q to %q", p.expected, item.SessionID))
			return
		}
		if p.active != nil && p.active.consumed {
			t := p.active
			p.active = nil
			t.done <- turnDone{result: terminal(item)}
		}
	}
}

func terminal(item frame) sessionkit.TurnResult {
	result := sessionkit.TurnResult{Result: item.Result, NativeStopReason: item.TerminalReason}
	switch {
	case item.Subtype == "interrupted" || item.TerminalReason == "interrupted" || item.TerminalReason == "aborted_streaming":
		result.Outcome = "interrupted"
	case item.Subtype == "success" && !item.IsError:
		result.Outcome = "completed"
	default:
		result.Outcome, result.Result = "failed", first(item.Error, strings.TrimSpace(item.Result), item.Subtype, "Claude turn failed")
	}
	return result
}

func (p *Wrapper) fail(err error) {
	p.mu.Lock()
	p.failLocked(err)
	p.mu.Unlock()
}

func (p *Wrapper) failLocked(err error) {
	if p.failed != nil {
		return
	}
	p.failed = err
	p.signalReady(err)
	t, control := p.active, p.control
	p.active, p.control = nil, nil
	if t != nil {
		t.done <- turnDone{err: err}
	}
	if control != nil {
		control <- err
	}
}

func (p *Wrapper) watch(child *host.Child, drained <-chan struct{}) {
	err := child.Wait()
	<-drained
	if err == nil {
		err = errors.New("Claude stream exited")
	}
	p.fail(err)
	p.mu.Lock()
	opened, closing, run := p.opened, p.closing, p.run
	p.mu.Unlock()
	if opened && !closing {
		if run != nil {
			<-run.Done()
		}
		if p.shutdown != nil {
			p.shutdown()
		}
	}
}

func (p *Wrapper) waitReady(ctx context.Context) error {
	select {
	case err := <-p.ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Wrapper) signalReady(err error) {
	p.readyOne.Do(func() { p.ready <- err; close(p.ready) })
}

func sessionID(resume string) (string, error) {
	if resume != "" {
		return resume, nil
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	id, _, err := optionValue(arguments, "--session-id")
	if err != nil {
		return host.ExecPlan{}, err
	}
	_, resumed, err := optionValue(arguments, "--resume", "-r")
	if err != nil {
		return host.ExecPlan{}, err
	}
	if id == "" && !resumed {
		id, err = sessionID("")
		if err != nil {
			return host.ExecPlan{}, err
		}
		index := slices.Index(arguments, "--")
		if index < 0 {
			index = len(arguments)
		}
		arguments = slices.Concat(arguments[:index], []string{"--session-id", id}, arguments[index:])
	}
	name, _, err := optionValue(arguments, "--name", "-n")
	if err != nil {
		return host.ExecPlan{}, err
	}
	environment = slices.DeleteFunc(environment, func(value string) bool { return strings.HasPrefix(value, LaneSocketEnv+"=") })
	if !slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		environment = append(environment, host.SocketEnv+"="+sessionkit.Socket())
	}
	return host.InteractivePlan("claude", arguments, environment, host.PeerIdentity{SessionID: id, Name: first(name, id)}, claudeOptionTakesValue)
}

func optionValue(arguments []string, names ...string) (string, bool, error) {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		name, value, attached := strings.Cut(argument, "=")
		if slices.Contains(names, name) {
			if !attached {
				if index+1 == len(arguments) {
					return "", true, errors.New(argument + " requires a value")
				}
				value = arguments[index+1]
			}
			if strings.TrimSpace(value) == "" {
				return "", true, errors.New(argument + " requires a value")
			}
			return value, true, nil
		}
		if !attached && claudeOptionTakesValue(name) {
			index++
		}
	}
	return "", false, nil
}

func claudeOptionTakesValue(name string) bool {
	_, found := map[string]bool{
		"--agent": true, "--agents": true, "--allowedTools": true, "--append-system-prompt": true,
		"--betas": true, "--disallowedTools": true, "--effort": true, "--fallback-model": true,
		"--input-format": true, "--mcp-config": true, "--model": true, "--name": true, "-n": true,
		"--output-format": true, "--permission-mode": true, "--resume": true, "-r": true,
		"--session-id": true, "--system-prompt": true, "--tools": true,
	}[name]
	return found
}

func launchArguments(request sessionkit.OpenRequest, id, socket string) ([]string, error) {
	extra, err := extraArguments(request.Open.Arguments)
	if err != nil {
		return nil, err
	}
	arguments := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages"}
	if request.ResumeSessionID == "" {
		index := strings.LastIndexByte(request.Name, '@')
		if index < 1 {
			return nil, errors.New("Claude lane name is invalid")
		}
		arguments = append(arguments, "--session-id", id, "--name", request.Name[:index])
	} else {
		arguments = append(arguments, "--resume", request.ResumeSessionID)
	}
	if request.Open.PermissionMode == "bypassPermissions" {
		arguments = append(arguments, "--dangerously-skip-permissions")
	} else {
		mode := request.Open.PermissionMode
		if mode == "" || mode == "default" {
			mode = "dontAsk"
		}
		arguments = append(arguments, "--permission-mode", mode)
	}
	if request.Open.Model != "" {
		arguments = append(arguments, "--model", request.Open.Model)
	}
	if request.Open.ReasoningEffort != "" {
		arguments = append(arguments, "--effort", request.Open.ReasoningEffort)
	}
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"sessionbus": map[string]any{"command": Product, "args": []string{"mcp"}, "env": map[string]string{LaneSocketEnv: socket}}}})
	arguments = append(arguments, "--mcp-config", string(mcp), "--allowedTools", "mcp__sessionbus__*")
	return append(arguments, extra...), nil
}

var argumentConflicts = map[string]string{
	"-p": "stream", "--print": "stream", "--verbose": "stream", "--replay-user-messages": "stream", "--input-format": "stream", "--output-format": "stream",
	"--session-id": "session_id", "--resume": "session_id", "-r": "session_id", "--name": "name", "-n": "name",
	"--permission-mode": "permission_mode", "--dangerously-skip-permissions": "permission_mode", "--yolo": "permission_mode",
	"--model": "model", "--effort": "reasoning_effort", "--mcp-config": "mcp", "--strict-mcp-config": "mcp",
	"--allowedTools": "mcp", "--tools": "mcp", "--disallowedTools": "mcp", "--": "arguments",
}

func extraArguments(arguments []string) ([]string, error) {
	for index := 0; index < len(arguments); index++ {
		name, _, attached := strings.Cut(arguments[index], "=")
		if field := argumentConflicts[name]; field != "" {
			return nil, errors.New("argument conflicts with typed field " + field)
		}
		if name != "--agent" {
			break
		}
		if !attached {
			index++
		}
	}
	return host.BuildArguments(arguments, []host.ArgumentRule{{Name: "--agent", TakesValue: true}})
}

func closeLaunch(lock *host.SessionLock, endpoint *host.PrivateEndpoint, err error) error {
	return errors.Join(err, endpoint.Close(), lock.Close())
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

var _ sessionkit.WorkerCallbacks = (*Wrapper)(nil)
