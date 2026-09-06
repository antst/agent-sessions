package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

const Product = "grok-peer"

var command = exec.Command

type nativeProcess struct {
	cmd  *exec.Cmd
	done chan error
}

type Wrapper struct {
	socket, key string
	backend     mcp.Backend
	mu          sync.Mutex
	primary     *acpClient
	observer    *acpClient
	child       *host.Child
	leader      *nativeProcess
	watcher     *nativeProcess
	sessionID   string
	run         *sessionkit.Run
	answer      strings.Builder
	closing     bool
	shutdown    func()
}

func New(socket, token string) *Wrapper {
	return &Wrapper{socket: socket, key: host.LaunchTokenDigest(token)}
}

func (p *Wrapper) SetShutdown(shutdown func()) { p.shutdown = shutdown }
func (p *Wrapper) SetCall(call func(context.Context, string, any) (json.RawMessage, error)) {
	p.backend = mcp.BackendFunc(call)
}

func (*Wrapper) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{Product: Product,
		SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"},
		ExtraArguments: []sessionkit.ExtraArgument{
			{Name: "--agent", Description: "Agent name or definition", TakesValue: true},
			{Name: "--disable-web-search", Description: "Disable web tools", TakesValue: false},
			{Name: "--no-plan", Description: "Disable plan mode", TakesValue: false},
			{Name: "--no-subagents", Description: "Disable subagents", TakesValue: false},
		},
	}, nil
}

func (p *Wrapper) Open(ctx context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	if p.backend == nil || p.socket == "" || p.key == "" {
		return sessionkit.OpenResult{}, errors.New("Grok lane host is incomplete")
	}
	name, err := namePart(request.Name)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	lock, err := host.AcquireSessionLock(p.socket, "grok", first(request.ResumeSessionID, p.key))
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	endpoint, err := host.ListenPrivate(p.socket, p.key)
	if err != nil {
		_ = lock.Close()
		return sessionkit.OpenResult{}, err
	}
	go func() { _ = mcp.ServeLane(ctx, endpoint, p.backend) }()
	leader, err := p.startLeader(request.Open.Cwd, request.Open.PermissionMode)
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, err)
	}
	hold, holdProcess, err := p.startObserverClient(ctx, request.Open.Cwd)
	if err != nil {
		p.stopAux(leader)
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, err)
	}
	releaseHold := func() {
		hold.close()
		p.stopAux(holdProcess)
	}
	fail := func(cause error) error {
		releaseHold()
		p.stopAux(leader)
		return closeLaunch(lock, endpoint, cause)
	}
	primaryArgs, err := launchArguments(request, leaderSocket(p.socket, p.key))
	if err != nil {
		return sessionkit.OpenResult{}, fail(err)
	}
	primaryCommand := command("grok", primaryArgs...)
	primaryCommand.Dir, primaryCommand.Stderr = request.Open.Cwd, os.Stderr
	input, _ := primaryCommand.StdinPipe()
	output, _ := primaryCommand.StdoutPipe()
	child, err := host.StartChild(primaryCommand, lock, endpoint)
	if err != nil {
		_ = input.Close()
		return sessionkit.OpenResult{}, fail(fmt.Errorf("start Grok primary: %w", err))
	}
	primary := newACPClient(input, output, p.receive)
	p.mu.Lock()
	p.primary, p.child, p.leader = primary, child, leader
	p.mu.Unlock()
	if err = initializeACP(ctx, primary); err == nil {
		err = p.openSession(ctx, primary, request, endpoint.Path, lock)
	}
	if err == nil {
		err = p.startObserver(ctx, request.Open.Cwd, name)
	}
	releaseHold()
	if err != nil {
		return sessionkit.OpenResult{}, errors.Join(err, p.abortLaunch(ctx))
	}
	go p.watch(child)
	return sessionkit.OpenResult{SessionID: p.sessionID}, nil
}

func (p *Wrapper) openSession(ctx context.Context, primary *acpClient, request sessionkit.OpenRequest, laneSocket string, lock *host.SessionLock) error {
	params := map[string]any{"cwd": request.Open.Cwd, "mcpServers": []any{mcpServer(laneSocket)}, "_meta": map[string]bool{"yoloMode": request.Open.PermissionMode == "bypassPermissions", "autoMode": false}}
	method := "session/new"
	if request.ResumeSessionID != "" {
		method, params["sessionId"] = "session/load", request.ResumeSessionID
	}
	var opened struct {
		SessionID string `json:"sessionId"`
	}
	if err := primary.request(ctx, method, params, &opened); err != nil {
		return fmt.Errorf("open Grok session: %w", err)
	}
	if opened.SessionID == "" || request.ResumeSessionID != "" && opened.SessionID != request.ResumeSessionID {
		return errors.New("Grok returned a different session identity")
	}
	if request.ResumeSessionID == "" {
		if err := lock.Rename(opened.SessionID); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.sessionID = opened.SessionID
	p.mu.Unlock()
	return nil
}

func (p *Wrapper) startLeader(cwd, permission string) (*nativeProcess, error) {
	path := leaderSocket(p.socket, p.key)
	_ = os.Remove(path)
	arguments := []string{"--permission-mode", first(permission, "default")}
	if permission == "" || permission == "default" {
		arguments = append(arguments, "--allow", "MCPTool(agent_sessions__*)")
	}
	arguments = append(arguments, "agent", "leader", "--leader-socket", path, "--relay-on-demand", "--no-auto-update")
	cmd := command("grok", arguments...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, nativeEnvironment(), os.Stderr, os.Stderr
	process, err := startNative(cmd)
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-process.done:
			return nil, fmt.Errorf("Grok leader exited: %v", err)
		case <-deadline.C:
			p.stopAux(process)
			return nil, errors.New("Grok leader socket was not ready")
		case <-tick.C:
			if _, err := os.Stat(path); err == nil {
				return process, nil
			}
		}
	}
}

func (p *Wrapper) startObserverClient(ctx context.Context, cwd string) (*acpClient, *nativeProcess, error) {
	args := []string{"--permission-mode", "default", "--leader-socket", leaderSocket(p.socket, p.key), "agent", "--leader", "stdio"}
	cmd := command("grok", args...)
	cmd.Dir, cmd.Env, cmd.Stderr = cwd, nativeEnvironment(), os.Stderr
	input, _ := cmd.StdinPipe()
	output, _ := cmd.StdoutPipe()
	process, err := startNative(cmd)
	if err != nil {
		return nil, nil, err
	}
	client := newACPClient(input, output, nil)
	if err = initializeACP(ctx, client); err != nil {
		client.close()
		p.stopAux(process)
		return nil, nil, err
	}
	return client, process, nil
}

func (p *Wrapper) startObserver(ctx context.Context, cwd, title string) error {
	observer, process, err := p.startObserverClient(ctx, cwd)
	if err != nil {
		return fmt.Errorf("start Grok observer: %w", err)
	}
	var renamed struct {
		Success bool `json:"success"`
	}
	err = observer.request(ctx, "_x.ai/session/rename", map[string]string{"sessionId": p.sessionID, "title": title}, &renamed)
	if err == nil && !renamed.Success {
		err = errors.New("Grok did not apply the session title")
	}
	if err != nil {
		observer.close()
		p.stopAux(process)
		return err
	}
	p.mu.Lock()
	p.observer, p.watcher = observer, process
	p.mu.Unlock()
	return nil
}

func initializeACP(ctx context.Context, client *acpClient) error {
	var initialized struct {
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	if err := client.request(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &initialized); err != nil {
		return err
	}
	if !slices.ContainsFunc(initialized.AuthMethods, func(method struct {
		ID string `json:"id"`
	}) bool {
		return method.ID == "cached_token"
	}) {
		return errors.New("Grok cached_token authentication is unavailable")
	}
	return client.request(ctx, "authenticate", map[string]any{"methodId": "cached_token", "_meta": map[string]bool{"headless": true}}, &map[string]any{})
}

func (p *Wrapper) Run(ctx context.Context, run *sessionkit.Run, input string) (sessionkit.TurnResult, error) {
	p.mu.Lock()
	if p.primary == nil || p.run != nil || p.closing {
		p.mu.Unlock()
		return sessionkit.TurnResult{}, errors.New("Grok lane is not idle")
	}
	p.run, p.answer = run, strings.Builder{}
	primary, id := p.primary, p.sessionID
	go p.retireRun(run)
	if run.Interrupted() {
		p.mu.Unlock()
		return sessionkit.TurnResult{Outcome: "interrupted"}, nil
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	started, finished := make(chan error, 1), make(chan error, 1)
	go func() {
		finished <- primary.requestStarted(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": input}}}, &result, started)
	}()
	if err := <-started; err != nil {
		p.mu.Unlock()
		return sessionkit.TurnResult{}, err
	}
	native := &nativePrompt{client: primary, sessionID: id}
	run.Native = native
	interrupted := run.Interrupted()
	if interrupted {
		run.Native = nil
	}
	p.mu.Unlock()
	if interrupted {
		_ = native.client.cancel(native.sessionID)
	}
	err := <-finished
	p.mu.Lock()
	answer := p.answer.String()
	if run.Native == native {
		run.Native = nil
	}
	p.mu.Unlock()
	if err != nil {
		return sessionkit.TurnResult{}, err
	}
	outcome := "completed"
	if result.StopReason == "cancelled" {
		outcome = "interrupted"
	} else if result.StopReason != "end_turn" {
		outcome = "failed"
	}
	return sessionkit.TurnResult{Outcome: outcome, Result: answer, NativeStopReason: result.StopReason}, nil
}

type nativePrompt struct {
	client    *acpClient
	sessionID string
}

func (p *Wrapper) retireRun(run *sessionkit.Run) {
	<-run.Done()
	p.mu.Lock()
	if p.run == run {
		p.run = nil
	}
	p.mu.Unlock()
}

func (p *Wrapper) receive(frame acpFrame) {
	if frame.Method != "session/update" {
		return
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(frame.Params, &update) != nil || update.Update.Kind != "agent_message_chunk" {
		return
	}
	p.mu.Lock()
	if p.run != nil && p.run.Native != nil && update.SessionID == p.sessionID {
		p.answer.WriteString(update.Update.Content.Text)
	}
	p.mu.Unlock()
}

func (p *Wrapper) Interrupt(_ context.Context, run *sessionkit.Run) error {
	p.mu.Lock()
	native, _ := run.Native.(*nativePrompt)
	if native != nil {
		run.Native = nil
	}
	p.mu.Unlock()
	if native == nil {
		return nil
	}
	return native.client.cancel(native.sessionID)
}

func (p *Wrapper) Deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	observer, id := p.observer, p.sessionID
	active := p.run != nil && p.run.Native != nil
	if observer == nil {
		return sessionkit.DeliveryReceipt{}, errors.New("Grok observer is unavailable")
	}
	if err := observer.interject(ctx, id, request.MessageID, message); err != nil {
		return sessionkit.DeliveryReceipt{}, fmt.Errorf("Grok interject: %w", err)
	}
	if active {
		return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
	}
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (p *Wrapper) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	return p.closeProcesses(ctx)
}

func (p *Wrapper) closeProcesses(ctx context.Context) error {
	p.mu.Lock()
	primary, observer, child, watcher, leader, id := p.primary, p.observer, p.child, p.watcher, p.leader, p.sessionID
	p.mu.Unlock()
	var failures []error
	if primary != nil && id != "" {
		failures = append(failures, primary.request(ctx, "session/close", map[string]string{"sessionId": id}, &map[string]any{}))
		primary.close()
	}
	if observer != nil {
		observer.close()
	}
	failures = append(failures, closeNative(ctx, "observer", watcher), closeNative(ctx, "leader", leader))
	if child != nil {
		if err := child.Close(ctx, func(context.Context) error { return nil }); err != nil {
			failures = append(failures, err)
		}
	}
	path := leaderSocket(p.socket, p.key)
	failures = appendRemoveError(failures, path)
	failures = appendRemoveError(failures, strings.TrimSuffix(path, ".sock")+".lock")
	return errors.Join(failures...)
}

func (p *Wrapper) watch(child *host.Child) {
	err := child.Wait()
	p.mu.Lock()
	closing, run, shutdown := p.closing, p.run, p.shutdown
	p.mu.Unlock()
	if err != nil && !closing {
		fmt.Fprintf(os.Stderr, "agentbus: Grok primary exited: %v\n", err)
	}
	if !closing && shutdown != nil {
		if run != nil {
			<-run.Done()
		}
		shutdown()
	}
}

func (p *Wrapper) abortLaunch(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	return p.closeProcesses(ctx)
}

func (p *Wrapper) stopAux(process *nativeProcess) {
	if process == nil {
		return
	}
	_ = process.cmd.Process.Signal(syscall.SIGKILL)
	<-process.done
}

func stopNative(ctx context.Context, process *nativeProcess) error {
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeNative(ctx context.Context, role string, process *nativeProcess) error {
	if process == nil {
		return nil
	}
	if err := stopNative(ctx, process); err != nil {
		return fmt.Errorf("close Grok %s: %w", role, err)
	}
	return nil
}

func appendRemoveError(failures []error, path string) []error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return append(failures, err)
	}
	return failures
}

func startNative(cmd *exec.Cmd) (*nativeProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &nativeProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { process.done <- cmd.Wait(); close(process.done) }()
	return process, nil
}

func mcpServer(socket string) map[string]any {
	return map[string]any{"name": "agent_sessions", "command": Product, "args": []string{"mcp"}, "env": []map[string]string{{"name": mcp.LaneSocketEnv, "value": socket}}}
}

func leaderSocket(socket, key string) string {
	return filepath.Join(filepath.Dir(socket), "grok-"+key+".sock")
}

func launchArguments(request sessionkit.OpenRequest, leader string) ([]string, error) {
	extra, err := extraArguments(request.Open.Arguments)
	if err != nil {
		return nil, err
	}
	arguments := []string{}
	if request.ResumeSessionID != "" {
		arguments = append(arguments, "--resume", request.ResumeSessionID)
	}
	mode := first(request.Open.PermissionMode, "default")
	arguments = append(arguments, "--permission-mode", mode)
	if request.Open.ReasoningEffort != "" {
		arguments = append(arguments, "--reasoning-effort", request.Open.ReasoningEffort)
	}
	if request.Open.Model != "" {
		arguments = append(arguments, "-m", request.Open.Model)
	}
	arguments = append(arguments, extra...)
	return append(arguments, "--leader-socket", leader, "agent", "--leader", "stdio"), nil
}

var argumentRules = []host.ArgumentRule{
	{Name: "--agent", TakesValue: true}, {Name: "--disable-web-search"},
	{Name: "--no-plan"}, {Name: "--no-subagents"},
	{Name: "--permission-mode", TakesValue: true, ConflictField: "permission_mode"},
	{Name: "--always-approve", ConflictField: "permission_mode"},
	{Name: "--reasoning-effort", TakesValue: true, ConflictField: "reasoning_effort"},
	{Name: "--effort", TakesValue: true, ConflictField: "reasoning_effort"},
	{Name: "-m", TakesValue: true, ConflictField: "model"},
	{Name: "--model", TakesValue: true, ConflictField: "model"},
	{Name: "--cwd", TakesValue: true, ConflictField: "cwd"},
	{Name: "--session-id", TakesValue: true, ConflictField: "session_id"},
	{Name: "--resume", TakesValue: true, ConflictField: "session_id"},
	{Name: "-r", TakesValue: true, ConflictField: "session_id"},
	{Name: "--leader", ConflictField: "leader"}, {Name: "--no-leader", ConflictField: "leader"},
	{Name: "--leader-socket", TakesValue: true, ConflictField: "leader"},
}

func extraArguments(arguments []string) ([]string, error) {
	return host.BuildArguments(arguments, argumentRules)
}

func nativeEnvironment() []string {
	return slices.DeleteFunc(os.Environ(), func(value string) bool {
		name, _, _ := strings.Cut(value, "=")
		return slices.Contains([]string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv, mcp.LaneSocketEnv}, name)
	})
}

func namePart(name string) (string, error) {
	index := strings.LastIndexByte(name, '@')
	if index < 1 {
		return "", errors.New("Grok lane name is invalid")
	}
	return name[:index], nil
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

var _ sessionkit.WorkerCallbacks = (*Wrapper)(nil)
