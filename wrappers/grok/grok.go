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
	handoff     host.Handoff
	run         *sessionkit.Run
	active      *nativePrompt
	answers     map[string]*strings.Builder
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
	cwd, err := filepath.Abs(first(request.Open.Cwd, "."))
	if err != nil {
		return sessionkit.OpenResult{}, fmt.Errorf("resolve Grok cwd: %w", err)
	}
	request.Open.Cwd = cwd
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
	child, input, output, err := host.StartChild(primaryCommand, lock, endpoint)
	if err != nil {
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
		return sessionkit.OpenResult{}, err
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
		Meta      struct {
			SessionID string `json:"sessionId"`
			Detail    struct {
				SessionID string `json:"sessionId"`
			} `json:"x.ai/sessionDetail"`
		} `json:"_meta"`
	}
	if err := primary.request(ctx, method, params, &opened); err != nil {
		return fmt.Errorf("open Grok session: %w", err)
	}
	identity := opened.SessionID
	if request.ResumeSessionID != "" {
		for _, returned := range []string{identity, opened.Meta.SessionID, opened.Meta.Detail.SessionID} {
			if returned != "" && returned != request.ResumeSessionID {
				return fmt.Errorf("Grok returned session identity %q instead of %q", returned, request.ResumeSessionID)
			}
		}
		identity = request.ResumeSessionID
	}
	if identity == "" {
		return errors.New("Grok returned no session identity")
	}
	if request.ResumeSessionID == "" {
		if err := lock.Rename(identity); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.sessionID = identity
	p.mu.Unlock()
	return nil
}

func (p *Wrapper) startLeader(cwd, permission string) (*nativeProcess, error) {
	return startLeader(p.socket, p.key, cwd, permission, nativeEnvironment())
}

func startLeader(socket, key, cwd, permission string, environment []string) (*nativeProcess, error) {
	path := leaderSocket(socket, key)
	_ = os.Remove(path)
	arguments := []string{"--permission-mode", first(permission, "default")}
	if permission == "" || permission == "default" {
		arguments = append(arguments, "--allow", "MCPTool(agent_sessions__*)")
	}
	arguments = append(arguments, "agent", "leader", "--leader-socket", path, "--relay-on-demand", "--no-auto-update")
	cmd := command("grok", arguments...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, environment, os.Stderr, os.Stderr
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
			_ = process.cmd.Process.Signal(syscall.SIGKILL)
			<-process.done
			return nil, errors.New("Grok leader socket was not ready")
		case <-tick.C:
			if grokSocketReady(path) {
				return process, nil
			}
		}
	}
}

func (p *Wrapper) startObserverClient(ctx context.Context, cwd string) (*acpClient, *nativeProcess, error) {
	args := []string{"--no-auto-update", "--permission-mode", "default", "--leader-socket", leaderSocket(p.socket, p.key), "agent", "--leader", "stdio"}
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
	p.run, p.answers = run, map[string]*strings.Builder{}
	primary, id := p.primary, p.sessionID
	go p.retireRun(run)
	p.mu.Unlock()
	result, err := p.handoff.Run(ctx, run, input, func(ctx context.Context, prompt string) (host.Turn, error) {
		return p.startPrompt(ctx, primary, id, prompt)
	})
	p.mu.Lock()
	p.answers = nil
	p.mu.Unlock()
	return result, err
}

type nativePrompt struct {
	owner     *Wrapper
	client    *acpClient
	sessionID string
	done      chan error
	result    struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			PromptID string `json:"promptId"`
		} `json:"_meta"`
	}
}

func (p *Wrapper) startPrompt(ctx context.Context, primary *acpClient, id, prompt string) (host.Turn, error) {
	t := &nativePrompt{owner: p, client: primary, sessionID: id, done: make(chan error, 1)}
	started := make(chan error, 1)
	go func() {
		t.done <- primary.requestStarted(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": prompt}}}, &t.result, started)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.active = t
	p.mu.Unlock()
	return t, nil
}

func (t *nativePrompt) Wait(ctx context.Context) (sessionkit.TurnResult, error) {
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-t.done:
	}
	answer := ""
	t.owner.mu.Lock()
	if builder := t.owner.answers[t.result.Meta.PromptID]; builder != nil {
		answer = builder.String()
	}
	if t.owner.active == t {
		t.owner.active = nil
	}
	t.owner.mu.Unlock()
	if err != nil {
		return sessionkit.TurnResult{}, err
	}
	if t.result.Meta.PromptID == "" {
		return sessionkit.TurnResult{}, errors.New("Grok prompt response omitted its identity")
	}
	outcome := "completed"
	if t.result.StopReason == "cancelled" {
		outcome = "interrupted"
	} else if t.result.StopReason != "end_turn" {
		outcome = "failed"
	}
	return sessionkit.TurnResult{Outcome: outcome, Result: answer, NativeStopReason: t.result.StopReason}, nil
}

func (t *nativePrompt) Interrupt(context.Context) error { return t.client.cancel(t.sessionID) }

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
		Meta struct {
			PromptID string `json:"promptId"`
		} `json:"_meta"`
	}
	if json.Unmarshal(frame.Params, &update) != nil || update.Update.Kind != "agent_message_chunk" || update.Meta.PromptID == "" {
		return
	}
	p.mu.Lock()
	if p.answers != nil && update.SessionID == p.sessionID {
		if p.answers[update.Meta.PromptID] == nil {
			p.answers[update.Meta.PromptID] = &strings.Builder{}
		}
		p.answers[update.Meta.PromptID].WriteString(update.Update.Content.Text)
	}
	p.mu.Unlock()
}

func (p *Wrapper) Interrupt(ctx context.Context, run *sessionkit.Run) error {
	return p.handoff.Interrupt(ctx, run)
}

func (p *Wrapper) Deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	return p.handoff.Deliver(ctx, request, func(ctx context.Context, message string) (bool, error) {
		return p.inject(ctx, request.MessageID, message)
	})
}

func (p *Wrapper) inject(ctx context.Context, messageID, message string) (bool, error) {
	p.mu.Lock()
	observer, id, native := p.observer, p.sessionID, p.active
	p.mu.Unlock()
	if native == nil {
		return false, nil
	}
	if observer == nil {
		return false, errors.New("Grok observer is unavailable")
	}
	if err := observer.interject(ctx, id, messageID, message); err != nil {
		return false, fmt.Errorf("Grok interject: %w", err)
	}
	p.mu.Lock()
	active := p.active == native
	p.mu.Unlock()
	return active, nil
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
	failures = append(failures, closeNative("observer", watcher), closeNative("leader", leader))
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

func (p *Wrapper) stopAux(process *nativeProcess) {
	if process == nil {
		return
	}
	_ = process.cmd.Process.Signal(syscall.SIGKILL)
	<-process.done
}

func stopNative(process *nativeProcess) error {
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-process.done
	return nil
}

func closeNative(role string, process *nativeProcess) error {
	if process == nil {
		return nil
	}
	if err := stopNative(process); err != nil {
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

func grokSocketReady(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func launchArguments(request sessionkit.OpenRequest, leader string) ([]string, error) {
	extra, err := extraArguments(request.Open.Arguments)
	if err != nil {
		return nil, err
	}
	arguments := []string{"--no-auto-update"}
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
