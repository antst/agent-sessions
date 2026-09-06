package qwen

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

const Product = "qwen-peer"

var laneCommand = exec.Command

type Wrapper struct {
	socket   string
	handoff  host.Handoff
	backend  mcp.Backend
	mu       sync.Mutex
	child    *host.Child
	client   *acpClient
	id       string
	active   *turn
	run      *sessionkit.Run
	opened   bool
	closing  bool
	shutdown func()
}

type turn struct {
	owner       *Wrapper
	reply       <-chan acpReply
	output      strings.Builder
	interrupted bool
}

type initializeReply struct {
	ProtocolVersion   int                   `json:"protocolVersion"`
	AgentInfo         struct{ Name string } `json:"agentInfo"`
	AgentCapabilities struct {
		LoadSession bool `json:"loadSession"`
	} `json:"agentCapabilities"`
}

type configOption struct {
	ID, CurrentValue string
}

type openReply struct {
	SessionID string `json:"sessionId"`
	Modes     struct {
		CurrentModeID string `json:"currentModeId"`
	} `json:"modes"`
	ConfigOptions []configOption `json:"configOptions"`
}

func New(socket string) *Wrapper               { return &Wrapper{socket: socket} }
func (p *Wrapper) SetShutdown(shutdown func()) { p.shutdown = shutdown }
func (p *Wrapper) SetCall(call func(context.Context, string, any) (json.RawMessage, error)) {
	p.backend = mcp.BackendFunc(call)
}

func (*Wrapper) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{Product: Product, SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}, ExtraArguments: []sessionkit.ExtraArgument{}}, nil
}

func (p *Wrapper) Open(ctx context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	id, err := sessionID(request.ResumeSessionID)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	arguments, err := launchArguments(request.Open)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	cwd, err := filepath.Abs(first(request.Open.Cwd, "."))
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	name, err := namePart(request.Name)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	lock, err := host.AcquireSessionLock(p.socket, "qwen", id)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	endpoint, err := host.ListenPrivate(p.socket, id)
	if err != nil {
		_ = lock.Close()
		return sessionkit.OpenResult{}, err
	}
	if p.backend == nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, errors.New("Agentbus lane backend is unavailable"))
	}
	go func() { _ = mcp.ServeLane(ctx, endpoint, p.backend) }()
	command := laneCommand("qwen", arguments...)
	command.Dir, command.Stderr = request.Open.Cwd, os.Stderr
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool { return strings.HasPrefix(value, mcp.LaneSocketEnv+"=") })
	command.Env = append(command.Env, mcp.LaneSocketEnv+"="+endpoint.Path)
	child, input, output, err := host.StartChild(command, lock, endpoint)
	if err != nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, fmt.Errorf("start Qwen ACP: %w", err))
	}
	p.mu.Lock()
	p.child, p.id = child, id
	p.client = newACPClient(input, output, p.receive, p.drain)
	client := p.client
	p.mu.Unlock()
	go p.watch(child, client.done)
	cleanup := func(err error) (sessionkit.OpenResult, error) {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		_ = child.Close(ctx, func(context.Context) error { return client.close() })
		return sessionkit.OpenResult{}, err
	}
	var initialized initializeReply
	if err = client.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &initialized); err != nil {
		return cleanup(err)
	}
	if initialized.ProtocolVersion != 1 || initialized.AgentInfo.Name != "qwen-code" {
		return cleanup(errors.New("Qwen ACP initialize returned the wrong product"))
	}
	params := map[string]any{"cwd": cwd, "mcpServers": []any{mcpServer(endpoint.Path)}}
	method := "session/new"
	if request.ResumeSessionID == "" {
		params["_meta"] = map[string]string{"qwen-code/sessionId": id}
	} else {
		if !initialized.AgentCapabilities.LoadSession {
			return cleanup(errors.New("Qwen ACP does not support session resume"))
		}
		method, params["sessionId"] = "session/resume", id
	}
	var opened openReply
	if err = client.call(ctx, method, params, &opened); err != nil {
		return cleanup(err)
	}
	if opened.SessionID != id {
		return cleanup(fmt.Errorf("Qwen ACP changed native session from %q to %q", id, opened.SessionID))
	}
	if request.Open.PermissionMode == "bypassPermissions" && opened.Modes.CurrentModeID != "yolo" {
		return cleanup(fmt.Errorf("Qwen ACP applied mode %q, expected %q", opened.Modes.CurrentModeID, "yolo"))
	}
	if request.Open.ReasoningEffort != "" {
		var configured struct {
			ConfigOptions []configOption `json:"configOptions"`
		}
		if err = client.call(ctx, "session/set_config_option", map[string]string{"sessionId": id, "configId": "reasoning_effort", "value": request.Open.ReasoningEffort}, &configured); err != nil {
			return cleanup(err)
		}
		if current(configured.ConfigOptions, "reasoning_effort") != request.Open.ReasoningEffort {
			return cleanup(fmt.Errorf("Qwen ACP did not apply reasoning effort %q", request.Open.ReasoningEffort))
		}
	}
	if request.ResumeSessionID == "" {
		var renamed struct{ Success bool }
		if err = client.call(ctx, "renameSession", map[string]string{"sessionId": id, "title": name}, &renamed); err != nil || !renamed.Success {
			if err == nil {
				err = errors.New("Qwen ACP did not accept the session name")
			}
			return cleanup(err)
		}
	}
	p.mu.Lock()
	p.opened = true
	p.mu.Unlock()
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

func (p *Wrapper) Deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handoff.Deliver(ctx, request, func(context.Context, string) (host.Injection, error) {
		if p.active != nil {
			return host.Pending, nil
		}
		return host.NotInjected, nil
	})
}

func (p *Wrapper) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	child, client := p.child, p.client
	p.mu.Unlock()
	if child == nil {
		return nil
	}
	return child.Close(ctx, func(context.Context) error { return client.close() })
}

func (p *Wrapper) start(ctx context.Context, prompt string) (host.Turn, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("Qwen lane prompt is empty")
	}
	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, errors.New("Qwen lane is not idle")
	}
	t, client, id := &turn{owner: p}, p.client, p.id
	p.active = t
	p.mu.Unlock()
	reply, err := client.start(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": prompt}}})
	p.mu.Lock()
	if err == nil {
		t.reply = reply
	} else if p.active == t {
		p.active = nil
	}
	p.mu.Unlock()
	return t, err
}

func (t *turn) Wait(ctx context.Context) (sessionkit.TurnResult, error) {
	select {
	case <-ctx.Done():
		t.clear()
		return sessionkit.TurnResult{}, ctx.Err()
	case reply := <-t.reply:
		output, interrupted := t.clear()
		if reply.err != nil {
			return sessionkit.TurnResult{}, reply.err
		}
		var result struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(reply.result, &result); err != nil {
			return sessionkit.TurnResult{}, fmt.Errorf("decode Qwen ACP prompt result: %w", err)
		}
		outcome := "completed"
		if result.StopReason == "cancelled" || interrupted {
			outcome = "interrupted"
		}
		return sessionkit.TurnResult{Outcome: outcome, Result: output, NativeStopReason: result.StopReason}, nil
	}
}

func (t *turn) clear() (string, bool) {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.owner.active == t {
		t.owner.active = nil
	}
	return t.output.String(), t.interrupted
}

func (t *turn) Interrupt(ctx context.Context) error {
	var ignored struct{ Cancelled bool }
	if err := t.owner.client.call(ctx, "craft/cancelPendingPrompt", map[string]string{"sessionId": t.owner.id}, &ignored); err != nil {
		return err
	}
	t.owner.mu.Lock()
	if t.owner.active == t {
		t.interrupted = true
	}
	t.owner.mu.Unlock()
	return nil
}

func (p *Wrapper) receive(method string, raw json.RawMessage) {
	if method != "session/update" {
		return
	}
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string                `json:"sessionUpdate"`
			Content struct{ Text string } `json:"content"`
			Text    string
		}
	}
	if json.Unmarshal(raw, &notification) != nil || notification.SessionID != p.id || notification.Update.Kind != "agent_message_chunk" {
		return
	}
	p.mu.Lock()
	if p.active != nil {
		p.active.output.WriteString(first(notification.Update.Content.Text, notification.Update.Text))
	}
	p.mu.Unlock()
}

func (p *Wrapper) drain() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		return []string{}
	}
	claimed := p.handoff.Claim()
	messages := make([]string, len(claimed))
	for index := range claimed {
		messages[index] = string(claimed[index])
	}
	return messages
}

func (p *Wrapper) watch(child *host.Child, drained <-chan struct{}) {
	err := child.Wait()
	<-drained
	if err == nil {
		err = errors.New("Qwen ACP exited")
	}
	p.client.fail(err)
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

func launchArguments(open sessionkit.OpenOptions) ([]string, error) {
	if open.PermissionMode != "" && open.PermissionMode != "default" && open.PermissionMode != "bypassPermissions" {
		return nil, fmt.Errorf("unsupported value permission_mode=%s", open.PermissionMode)
	}
	if open.ReasoningEffort != "" && !slices.Contains([]string{"none", "low", "medium", "xhigh"}, open.ReasoningEffort) {
		return nil, fmt.Errorf("unsupported value reasoning_effort=%s", open.ReasoningEffort)
	}
	arguments := []string{"--acp"}
	if open.PermissionMode == "bypassPermissions" {
		arguments = append(arguments, "--yolo")
	}
	if open.Model != "" {
		arguments = append(arguments, "-m", open.Model)
	}
	for _, argument := range open.Arguments {
		name, _, _ := strings.Cut(argument, "=")
		if field := argumentConflicts[name]; field != "" || argument == "--" {
			return nil, errors.New("argument conflicts with typed field " + first(field, "arguments"))
		}
	}
	return append(arguments, open.Arguments...), nil
}

var argumentConflicts = map[string]string{
	"--acp": "arguments", "--approval-mode": "permission_mode", "--yolo": "permission_mode",
	"-m": "model", "--model": "model", "-r": "session_id", "--resume": "session_id",
	"-c": "session_id", "--continue": "session_id", "--session-id": "session_id",
	"-p": "arguments", "--prompt": "arguments", "-i": "arguments", "--prompt-interactive": "arguments",
	"-o": "arguments", "--output-format": "arguments", "-n": "name", "--name": "name",
}

func mcpServer(socket string) map[string]any {
	return map[string]any{"name": "agent_sessions", "command": Product, "args": []string{"mcp"}, "env": []any{map[string]string{"name": mcp.LaneSocketEnv, "value": socket}}}
}

func current(options []configOption, id string) string {
	for _, option := range options {
		if option.ID == id {
			return option.CurrentValue
		}
	}
	return ""
}

func namePart(name string) (string, error) {
	index := strings.LastIndexByte(name, '@')
	if index < 1 {
		return "", errors.New("Qwen lane name is invalid")
	}
	return name[:index], nil
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
