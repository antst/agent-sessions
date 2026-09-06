package codex

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

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

const Product = "codex-peer"

var laneCommand = exec.Command

type Wrapper struct {
	socket, provisional string
	handoff             host.Handoff
	backend             mcp.Backend
	mu                  sync.Mutex
	child               *host.Child
	app                 *appClient
	id, model, effort   string
	approval            string
	sandbox             string
	active              *turn
	run                 *sessionkit.Run
	closing             bool
	shutdown            func()
}

type turn struct {
	owner   *Wrapper
	id      string
	started bool
	ready   chan error
	done    chan error
}

type nativeTurn struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Error       *appError    `json:"error"`
	Items       []nativeItem `json:"items"`
	CompletedAt *int64       `json:"completedAt"`
}

type nativeItem struct{ Type, Text, Phase string }
type nativeThread struct {
	ID, Name, Cwd string
	Status        json.RawMessage
}
type threadReply struct {
	Thread         nativeThread `json:"thread"`
	Cwd            string       `json:"cwd"`
	ApprovalPolicy string       `json:"approvalPolicy"`
	Sandbox        struct {
		Type string `json:"type"`
	} `json:"sandbox"`
}
type turnReply struct {
	Turn nativeTurn `json:"turn"`
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
		Product: Product, SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"},
		ExtraArguments: []sessionkit.ExtraArgument{
			{Name: "-c", Description: "Codex configuration override", TakesValue: true},
			{Name: "--config", Description: "Codex configuration override", TakesValue: true},
			{Name: "--enable", Description: "Enable a Codex feature", TakesValue: true},
			{Name: "--disable", Description: "Disable a Codex feature", TakesValue: true},
			{Name: "--search", Description: "Enable web search"},
		},
	}, nil
}

func (p *Wrapper) Open(ctx context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	arguments, err := processArguments(request.Open.Arguments)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	approval, sandbox, err := permission(request.Open.PermissionMode)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	effectiveCwd, err := filepath.Abs(first(request.Open.Cwd, "."))
	if err != nil {
		return sessionkit.OpenResult{}, fmt.Errorf("resolve Codex cwd: %w", err)
	}
	claim := request.ResumeSessionID
	if claim == "" {
		if _, err = namePart(request.Name); err != nil {
			return sessionkit.OpenResult{}, err
		}
		claim = p.provisional
	}
	lock, err := host.AcquireSessionLock(p.socket, "codex", claim)
	if err != nil {
		return sessionkit.OpenResult{}, err
	}
	endpoint, err := host.ListenPrivate(p.socket, claim)
	if err != nil {
		_ = lock.Close()
		return sessionkit.OpenResult{}, err
	}
	if p.backend == nil {
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, errors.New("Agentbus lane backend is unavailable"))
	}
	go func() { _ = mcp.ServeLane(ctx, endpoint, p.backend) }()
	command := laneCommand("codex", append(arguments, "app-server", "--stdio")...)
	command.Dir, command.Stderr = request.Open.Cwd, os.Stderr
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool { return strings.HasPrefix(value, mcp.LaneSocketEnv+"=") })
	command.Env = append(command.Env, mcp.LaneSocketEnv+"="+endpoint.Path)
	input, _ := command.StdinPipe()
	output, _ := command.StdoutPipe()
	child, err := host.StartChild(command, lock, endpoint)
	if err != nil {
		_ = input.Close()
		return sessionkit.OpenResult{}, closeLaunch(lock, endpoint, fmt.Errorf("start Codex App Server: %w", err))
	}
	p.mu.Lock()
	p.child, p.model, p.effort, p.approval, p.sandbox = child, request.Open.Model, request.Open.ReasoningEffort, approval, sandbox
	p.app = newAppClient(input, output, p.receive, p.fail)
	p.mu.Unlock()
	go p.watch(child)
	threadID := request.ResumeSessionID
	fresh := threadID == ""
	cleanup := func(err error) (sessionkit.OpenResult, error) {
		if fresh && threadID != "" {
			_ = p.app.call(ctx, "thread/delete", map[string]string{"threadId": threadID}, &struct{}{})
		}
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		_ = child.Close(ctx, func(context.Context) error { return p.app.close() })
		return sessionkit.OpenResult{}, err
	}
	if err = p.app.initialize(ctx, "Agentbus Codex Wrapper"); err != nil {
		return cleanup(err)
	}
	config := laneConfig(endpoint.Path)
	if fresh {
		var started threadReply
		params := p.threadParams(request.Open.Cwd, config)
		params["ephemeral"], params["serviceName"], params["historyMode"] = false, Product, "legacy"
		if err = p.app.call(ctx, "thread/start", params, &started); err != nil {
			return cleanup(err)
		}
		threadID = strings.TrimSpace(started.Thread.ID)
		if threadID == "" {
			return cleanup(errors.New("Codex App Server returned an empty thread id"))
		}
		if err = p.checkEffective("thread/start", effectiveCwd, started); err != nil {
			return cleanup(err)
		}
		if err = lock.Rename(threadID); err != nil {
			return cleanup(err)
		}
		name, nameErr := namePart(request.Name)
		if nameErr != nil {
			return cleanup(nameErr)
		}
		if err = p.app.call(ctx, "thread/name/set", map[string]string{"threadId": threadID, "name": name}, &struct{}{}); err != nil {
			return cleanup(err)
		}
	}
	params := p.threadParams(request.Open.Cwd, config)
	params["threadId"], params["excludeTurns"] = threadID, true
	var resumed threadReply
	if err = p.app.call(ctx, "thread/resume", params, &resumed); err != nil {
		return cleanup(err)
	}
	if resumed.Thread.ID != threadID {
		return cleanup(fmt.Errorf("Codex App Server changed native thread from %q to %q", threadID, resumed.Thread.ID))
	}
	if err = p.checkEffective("thread/resume", effectiveCwd, resumed); err != nil {
		return cleanup(err)
	}
	p.mu.Lock()
	p.id = threadID
	p.mu.Unlock()
	return sessionkit.OpenResult{SessionID: threadID}, nil
}

func (p *Wrapper) checkEffective(method, cwd string, reply threadReply) error {
	if reply.ApprovalPolicy == "" {
		return errors.New(method + " did not report its effective approval policy")
	}
	if reply.ApprovalPolicy != p.approval {
		return fmt.Errorf("%s applied approval policy %q, expected %q", method, reply.ApprovalPolicy, p.approval)
	}
	if strings.TrimSpace(reply.Cwd) == "" {
		return errors.New(method + " did not report its effective cwd")
	}
	if reply.Cwd != cwd {
		return fmt.Errorf("%s applied cwd %q, expected %q", method, reply.Cwd, cwd)
	}
	if !slices.Contains([]string{"readOnly", "workspaceWrite", "dangerFullAccess"}, reply.Sandbox.Type) {
		return fmt.Errorf("%s reported unsupported sandbox %q", method, reply.Sandbox.Type)
	}
	if p.sandbox != "" && reply.Sandbox.Type != "dangerFullAccess" {
		return fmt.Errorf("%s applied sandbox %q, expected %q", method, reply.Sandbox.Type, "dangerFullAccess")
	}
	return nil
}

func (p *Wrapper) threadParams(cwd string, config map[string]any) map[string]any {
	params := map[string]any{"approvalPolicy": p.approval, "config": config}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if p.model != "" {
		params["model"] = p.model
	}
	if p.sandbox != "" {
		params["sandbox"] = p.sandbox
	}
	return params
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
	return p.handoff.Deliver(ctx, request, p.inject)
}

func (p *Wrapper) start(ctx context.Context, prompt string) (host.Turn, error) {
	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, errors.New("Codex lane is not idle")
	}
	t := &turn{owner: p, ready: make(chan error, 1), done: make(chan error, 1)}
	p.active = t
	params := map[string]any{"threadId": p.id, "input": textInput(prompt), "approvalPolicy": p.approval}
	if p.model != "" {
		params["model"] = p.model
	}
	if p.effort != "" {
		params["effort"] = p.effort
	}
	if p.sandbox != "" {
		params["sandboxPolicy"] = map[string]string{"type": "dangerFullAccess"}
	}
	p.mu.Unlock()
	var started turnReply
	if err := p.app.call(ctx, "turn/start", params, &started); err != nil {
		p.clear(t)
		return nil, err
	}
	if started.Turn.ID == "" {
		p.clear(t)
		return nil, errors.New("Codex App Server returned an empty turn id")
	}
	p.mu.Lock()
	if t.id != "" && t.id != started.Turn.ID {
		observed := t.id
		p.mu.Unlock()
		p.clear(t)
		return nil, fmt.Errorf("Codex turn/start returned %q after starting %q", started.Turn.ID, observed)
	}
	t.id = started.Turn.ID
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		p.clear(t)
		return nil, ctx.Err()
	case err := <-t.ready:
		if err != nil {
			return nil, err
		}
		return t, nil
	}
}

func (p *Wrapper) inject(ctx context.Context, prompt string) (bool, error) {
	p.mu.Lock()
	t := p.active
	if t == nil || !t.started {
		p.mu.Unlock()
		return false, nil
	}
	threadID, turnID := p.id, t.id
	p.mu.Unlock()
	var result struct {
		TurnID string `json:"turnId"`
	}
	err := p.app.call(ctx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "input": textInput(prompt)}, &result)
	if err != nil {
		return false, err
	}
	if result.TurnID != turnID {
		return false, fmt.Errorf("Codex App Server steered turn %q, expected %q", result.TurnID, turnID)
	}
	p.mu.Lock()
	active := p.active == t && t.started
	p.mu.Unlock()
	return active, nil
}

func (t *turn) Wait(ctx context.Context) (sessionkit.TurnResult, error) {
	select {
	case <-ctx.Done():
		return sessionkit.TurnResult{}, ctx.Err()
	case err := <-t.done:
		if err != nil {
			return sessionkit.TurnResult{}, err
		}
		return t.read(ctx)
	}
}

func (t *turn) read(ctx context.Context) (sessionkit.TurnResult, error) {
	var page struct {
		Data []nativeTurn `json:"data"`
	}
	if err := t.owner.app.call(ctx, "thread/turns/list", map[string]any{
		"threadId": t.owner.id, "limit": 100, "sortDirection": "desc", "itemsView": "full",
	}, &page); err != nil {
		return sessionkit.TurnResult{}, err
	}
	for _, candidate := range page.Data {
		if candidate.ID == t.id {
			return terminal(candidate)
		}
	}
	return sessionkit.TurnResult{}, fmt.Errorf("Codex turn %s is absent from thread %s", t.id, t.owner.id)
}
func (t *turn) Interrupt(ctx context.Context) error {
	return t.owner.app.call(ctx, "turn/interrupt", map[string]string{"threadId": t.owner.id, "turnId": t.id}, &struct{}{})
}

func (p *Wrapper) receive(method string, raw json.RawMessage) {
	if method != "turn/started" && method != "turn/completed" {
		return
	}
	var event struct {
		ThreadID string     `json:"threadId"`
		Turn     nativeTurn `json:"turn"`
	}
	if json.Unmarshal(raw, &event) != nil || event.ThreadID == "" || event.Turn.ID == "" {
		p.fail(errors.New("malformed Codex App Server " + method + " notification"))
		return
	}
	p.mu.Lock()
	t := p.active
	if t == nil || event.ThreadID != p.id {
		p.mu.Unlock()
		return
	}
	if t.id != "" && event.Turn.ID != t.id {
		expected := t.id
		p.mu.Unlock()
		p.fail(fmt.Errorf("Codex App Server reported turn %q while waiting for %q", event.Turn.ID, expected))
		return
	}
	if t.id == "" && (method == "turn/started" || method == "turn/completed") {
		t.id = event.Turn.ID
	}
	switch method {
	case "turn/started":
		if t.id == "" {
			t.id = event.Turn.ID
		}
		if event.Turn.ID == t.id && !t.started {
			t.started = true
			t.ready <- nil
		}
	case "turn/completed":
		if event.Turn.ID == t.id {
			if !t.started {
				t.started = true
				t.ready <- nil
			}
			p.active = nil
			t.done <- nil
		}
	}
	p.mu.Unlock()
}

func terminal(t nativeTurn) (sessionkit.TurnResult, error) {
	if t.CompletedAt == nil {
		return sessionkit.TurnResult{}, fmt.Errorf("Codex turn %s is not product-closed", t.ID)
	}
	result := sessionkit.TurnResult{Outcome: t.Status, NativeStopReason: t.Status}
	for _, item := range t.Items {
		if item.Type == "agentMessage" && item.Phase == "final_answer" {
			result.Result = item.Text
		}
	}
	switch t.Status {
	case "completed":
		result.Outcome = "completed"
		if strings.TrimSpace(result.Result) == "" {
			return sessionkit.TurnResult{}, fmt.Errorf("completed Codex turn %s has no final answer", t.ID)
		}
	case "interrupted":
		result.Outcome = "interrupted"
	default:
		result.Outcome = "failed"
		if t.Error != nil {
			result.Result = t.Error.Message
		}
		if result.Result == "" {
			result.Result = first(t.Status, "Codex turn failed")
		}
	}
	return result, nil
}

func (p *Wrapper) clear(t *turn) {
	p.mu.Lock()
	if p.active == t {
		p.active = nil
	}
	p.mu.Unlock()
}
func (p *Wrapper) fail(err error) {
	p.mu.Lock()
	t := p.active
	p.active = nil
	if t != nil {
		if !t.started {
			t.started = true
			t.ready <- err
			t = nil
		}
	}
	p.mu.Unlock()
	if t != nil {
		t.done <- err
	}
}

func (p *Wrapper) watch(child *host.Child) {
	err := child.Wait()
	if err == nil {
		err = errors.New("Codex App Server exited")
	}
	p.fail(err)
	p.mu.Lock()
	opened, closing, run := p.id != "", p.closing, p.run
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

func (p *Wrapper) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	child, app, id := p.child, p.app, p.id
	p.mu.Unlock()
	if child == nil {
		return nil
	}
	if id != "" {
		archiveErr := app.call(ctx, "thread/archive", map[string]string{"threadId": id}, &struct{}{})
		unsubscribeErr := app.call(ctx, "thread/unsubscribe", map[string]string{"threadId": id}, &struct{}{})
		return errors.Join(archiveErr, unsubscribeErr, child.Close(ctx, func(context.Context) error { return app.close() }))
	}
	return child.Close(ctx, func(context.Context) error { return app.close() })
}

func laneConfig(socket string) map[string]any {
	return map[string]any{"features": map[string]any{"code_mode_host": false}, "mcp_servers": map[string]any{"agent_sessions": map[string]any{
		"command": Product, "args": []string{"mcp"}, "env": map[string]string{mcp.LaneSocketEnv: socket},
	}}}
}
func textInput(text string) []map[string]string {
	return []map[string]string{{"type": "text", "text": text}}
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func closeLaunch(lock *host.SessionLock, endpoint *host.PrivateEndpoint, err error) error {
	return errors.Join(err, endpoint.Close(), lock.Close())
}
