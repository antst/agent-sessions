package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

const (
	codexDaemonLaneThreadID = "0198f27a-0ae5-7d10-9716-d97f0f035e25"
	codexDaemonLaneTurnID   = "0198f27b-62dd-73ee-8f68-b58cbb6eb252"
)

var (
	_ daemonpkg.LaneAdapter          = (*codexDaemonAdapter)(nil)
	_ daemonpkg.LaneStartAdapter     = (*codexDaemonAdapter)(nil)
	_ daemonpkg.LaneReconnectAdapter = (*codexDaemonAdapter)(nil)
	_ daemonpkg.LaneInterruptAdapter = (*codexDaemonAdapter)(nil)
	_ daemonpkg.LaneCollectAdapter   = (*codexDaemonAdapter)(nil)
	_ daemonpkg.LaneCleanupAdapter   = (*codexDaemonAdapter)(nil)
)

func TestCodexDaemonLaneAppServerStartInterruptCollectAndArchive(t *testing.T) {
	serverState := newCodexDaemonLaneAppServerState()
	_, socket := startFakeNativeAppServer(t, serverState.handle)
	profile := codexDaemonLaneTestProfile(t)
	adapter := newConnectedCodexDaemonLaneAdapter(t, profile, socket)
	lane, turn := codexDaemonLaneRecords(profile, t.TempDir())
	turn.InputReference["options"] = map[string]any{"native": map[string]any{
		"model": "gpt-native", "effort": "high", "sandbox": "danger-full-access", "approval_policy": "never",
		"config": []any{"features.test=true"}, "web": true,
		"output_schema": map[string]any{"type": "object", "required": []any{"ok"}},
	}}

	dispatched, err := adapter.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start committed Codex lane turn: %v", err)
	}
	if dispatched.LaneSessionID != lane.LaneSessionID || dispatched.DispatchState != "running" ||
		stringValue(dispatched.NativeActor["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(dispatched.NativeActor["profile"]) != profile ||
		stringValue(dispatched.NativeTurnIdentity["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(dispatched.NativeTurnIdentity["turn_id"]) != codexDaemonLaneTurnID {
		t.Fatalf("Codex lane dispatch result = %#v", dispatched)
	}
	serverState.requireCall(t, "thread/start", func(params map[string]any) bool {
		config := mapValue(params["config"])
		features := mapValue(config["features"])
		tools := mapValue(config["tools"])
		ephemeral, ephemeralOK := params["ephemeral"].(bool)
		return stringValue(params["cwd"]) == lane.Cwd && ephemeralOK && !ephemeral &&
			stringValue(params["serviceName"]) == "agent-sessions" &&
			stringValue(params["model"]) == "gpt-native" && boolValue(features["test"]) && boolValue(tools["web_search"]) &&
			stringValue(params["approvalPolicy"]) == "never" &&
			stringValue(params["sandbox"]) == "danger-full-access"
	})
	serverState.requireCall(t, "thread/name/set", func(params map[string]any) bool {
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID && stringValue(params["name"]) == lane.Name
	})
	serverState.requireCall(t, "turn/start", func(params map[string]any) bool {
		input, _ := params["input"].([]any)
		first, _ := firstCodexDaemonLaneValue(input).(map[string]any)
		schema := mapValue(params["outputSchema"])
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID &&
			stringValue(params["model"]) == "gpt-native" && stringValue(params["effort"]) == "high" &&
			stringValue(schema["type"]) == "object" &&
			stringValue(first["type"]) == "text" && stringValue(first["text"]) == "inspect the daemon boundary"
	})

	lane.LaneSessionID = dispatched.LaneSessionID
	lane.NativeActor = dispatched.NativeActor
	turn.NativeTurnIdentity = dispatched.NativeTurnIdentity
	turn.DispatchState = dispatched.DispatchState
	if err := adapter.InterruptTurn(context.Background(), lane, turn); err != nil {
		t.Fatalf("interrupt exact Codex lane turn: %v", err)
	}
	serverState.requireCall(t, "turn/interrupt", func(params map[string]any) bool {
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID &&
			stringValue(params["turnId"]) == codexDaemonLaneTurnID
	})

	terminal, err := adapter.CollectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("collect interrupted Codex lane turn: %v", err)
	}
	if terminal.TerminalOutcome != "interrupted" ||
		stringValue(terminal.NativeTurnIdentity["turn_id"]) != codexDaemonLaneTurnID ||
		stringValue(terminal.ResultReference["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(terminal.ResultReference["turn_id"]) != codexDaemonLaneTurnID ||
		stringValue(terminal.ResultReference["status"]) != "interrupted" {
		t.Fatalf("interrupted Codex terminal result = %#v", terminal)
	}
	if lane.CollectionCursor != "" || turn.CollectionRevision != 0 || turn.CollectedAt != 0 {
		t.Fatalf("Codex adapter advanced daemon collection state: lane=%#v turn=%#v", lane, turn)
	}

	if err := adapter.Archive(context.Background(), lane); err != nil {
		t.Fatalf("archive Codex lane thread: %v", err)
	}
	serverState.requireCall(t, "thread/archive", func(params map[string]any) bool {
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID
	})
	serverState.requireCall(t, "thread/list", func(params map[string]any) bool {
		return boolValue(params["archived"])
	})
	if err := adapter.Cleanup(context.Background(), lane); err != nil {
		t.Fatalf("clean Codex lane adapter artifacts: %v", err)
	}
	if deletes := serverState.methodCount("thread/delete"); deletes != 0 {
		t.Fatalf("Codex cleanup deleted %d vendor-owned thread(s)", deletes)
	}
}

func TestCodexDaemonLaneReconnectsActiveTurnWithoutRedispatch(t *testing.T) {
	serverState := newCodexDaemonLaneAppServerState()
	_, socket := startFakeNativeAppServer(t, serverState.handle)
	profile := codexDaemonLaneTestProfile(t)
	lane, turn := codexDaemonLaneRecords(profile, t.TempDir())

	first := newConnectedCodexDaemonLaneAdapter(t, profile, socket)
	dispatched, err := first.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start committed Codex lane turn: %v", err)
	}
	first.Close()

	// A successor daemon gets a new App Server connection, while the App Server
	// and its durable thread/turn identities remain vendor-owned and unchanged.
	lane.LaneSessionID = dispatched.LaneSessionID
	lane.NativeActor = dispatched.NativeActor
	turn.NativeTurnIdentity = dispatched.NativeTurnIdentity
	turn.DispatchState = dispatched.DispatchState
	successor := newConnectedCodexDaemonLaneAdapter(t, profile, socket)
	reconnected, err := successor.ReconnectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("reconnect accepted Codex lane turn: %v", err)
	}
	if reconnected.DispatchState != "running" || reconnected.TerminalOutcome != "" ||
		stringValue(reconnected.NativeActor["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(reconnected.NativeTurnIdentity["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(reconnected.NativeTurnIdentity["turn_id"]) != codexDaemonLaneTurnID {
		t.Fatalf("Codex lane reconnect result = %#v", reconnected)
	}
	if starts, turns := serverState.methodCount("thread/start"), serverState.methodCount("turn/start"); starts != 1 || turns != 1 {
		t.Fatalf("restart redispatched accepted Codex work: thread/start=%d turn/start=%d", starts, turns)
	}
	if connections := serverState.methodCount("initialize"); connections != 2 {
		t.Fatalf("successor daemon App Server connections = %d, want 2 generations", connections)
	}
	serverState.requireCall(t, "thread/resume", func(params map[string]any) bool {
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID && boolValue(params["excludeTurns"])
	})
	serverState.requireCall(t, "thread/turns/list", func(params map[string]any) bool {
		return stringValue(params["threadId"]) == codexDaemonLaneThreadID
	})

	serverState.complete("DAEMON_RESTART_RESULT_CANARY")
	terminal, err := successor.CollectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("collect reconnected Codex lane turn: %v", err)
	}
	encoded, err := json.Marshal(terminal.ResultReference)
	if err != nil {
		t.Fatalf("encode Codex terminal result reference: %v", err)
	}
	if terminal.TerminalOutcome != "completed" ||
		stringValue(terminal.NativeTurnIdentity["turn_id"]) != codexDaemonLaneTurnID ||
		stringValue(terminal.ResultReference["thread_id"]) != codexDaemonLaneThreadID ||
		stringValue(terminal.ResultReference["turn_id"]) != codexDaemonLaneTurnID ||
		stringValue(terminal.ResultReference["status"]) != "completed" ||
		!strings.Contains(string(encoded), "DAEMON_RESTART_RESULT_CANARY") {
		t.Fatalf("reconnected Codex terminal result = %#v", terminal)
	}
	repeated, err := successor.CollectTurn(context.Background(), lane, turn)
	if err != nil || !reflect.DeepEqual(repeated, terminal) {
		t.Fatalf("repeated Codex collection = %#v, %v; want stable %#v", repeated, err, terminal)
	}
	if lane.CollectionCursor != "" || turn.CollectionRevision != 0 || turn.CollectedAt != 0 {
		t.Fatalf("Codex adapter advanced daemon collection state: lane=%#v turn=%#v", lane, turn)
	}
	if starts, turns := serverState.methodCount("thread/start"), serverState.methodCount("turn/start"); starts != 1 || turns != 1 {
		t.Fatalf("collection redispatched accepted Codex work: thread/start=%d turn/start=%d", starts, turns)
	}
}

func codexDaemonLaneRecords(profile, cwd string) (daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) {
	lane := daemonpkg.LaneRecord{
		LaneSessionID: "accepted-codex-lane", Name: "codex-researcher", Product: "codex", Cwd: cwd,
		PermissionMode: "bypassPermissions", NativeActor: map[string]any{"profile": profile},
	}
	turn := daemonpkg.LaneTurnRecord{
		TurnID: "accepted-daemon-turn", LaneSessionID: lane.LaneSessionID,
		InputReference: map[string]any{"kind": "inline", "content": "inspect the daemon boundary"}, DispatchState: "accepted",
	}
	return lane, turn
}

func codexDaemonLaneTestProfile(t *testing.T) string {
	t.Helper()
	profile, err := canonicalCodexProfile(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize Codex daemon lane profile: %v", err)
	}
	return profile
}

func newConnectedCodexDaemonLaneAdapter(t *testing.T, profile, socket string) *codexDaemonAdapter {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatalf("dial fake Codex App Server: %v", err)
	}
	coordinator := newCodexAppServerCoordinator()
	coordinator.clients[profile] = client
	adapter := newCodexDaemonAdapter(coordinator)
	t.Cleanup(adapter.Close)
	return adapter
}

type codexDaemonLaneAppServerState struct {
	mu       sync.Mutex
	calls    []codexDaemonLaneAppServerCall
	status   string
	items    []map[string]any
	threadID string
	turnID   string
	cwd      string
	archived bool
}

type codexDaemonLaneAppServerCall struct {
	method string
	params map[string]any
}

func newCodexDaemonLaneAppServerState() *codexDaemonLaneAppServerState {
	return &codexDaemonLaneAppServerState{
		status: "inProgress", threadID: codexDaemonLaneThreadID, turnID: codexDaemonLaneTurnID,
	}
}

func (state *codexDaemonLaneAppServerState) handle(request map[string]any) (any, error) {
	method := stringValue(request["method"])
	params, _ := request["params"].(map[string]any)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.calls = append(state.calls, codexDaemonLaneAppServerCall{method: method, params: cloneCodexDaemonLaneMap(params)})
	if method == "thread/start" && stringValue(params["cwd"]) != "" {
		state.cwd = stringValue(params["cwd"])
	}
	thread := map[string]any{
		"id": state.threadID, "sessionId": state.threadID, "cwd": state.cwd,
		"name": "codex-researcher", "source": "appServer", "parentThreadId": "",
	}
	if thread["cwd"] == "" {
		thread["cwd"] = "/work"
	}
	turn := map[string]any{"id": state.turnID, "status": state.status, "items": state.items}
	switch method {
	case "initialize":
		return map[string]any{}, nil
	case "thread/start", "thread/resume", "thread/read":
		if method == "thread/read" {
			thread["turns"] = []any{turn}
		}
		return map[string]any{"thread": thread}, nil
	case "thread/name/set":
		return map[string]any{}, nil
	case "thread/archive":
		state.archived = true
		return map[string]any{}, nil
	case "thread/list":
		data := []any{}
		if params["archived"] == state.archived {
			data = append(data, thread)
		}
		return map[string]any{"data": data, "nextCursor": ""}, nil
	case "turn/start":
		return map[string]any{"turn": turn}, nil
	case "turn/interrupt":
		state.status = "interrupted"
		return map[string]any{}, nil
	case "thread/turns/list":
		turn["status"] = state.status
		turn["items"] = state.items
		return map[string]any{"data": []any{turn}, "nextCursor": ""}, nil
	default:
		return nil, fmt.Errorf("unexpected Codex App Server method %q", method)
	}
}

func (state *codexDaemonLaneAppServerState) complete(result string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.status = "completed"
	state.items = []map[string]any{{
		"id": "answer-after-restart", "type": "agentMessage", "phase": "final_answer", "text": result,
	}}
}

func (state *codexDaemonLaneAppServerState) methodCount(method string) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	count := 0
	for _, call := range state.calls {
		if call.method == method {
			count++
		}
	}
	return count
}

func (state *codexDaemonLaneAppServerState) requireCall(t *testing.T, method string, matches func(map[string]any) bool) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, call := range state.calls {
		if call.method == method && matches(call.params) {
			return
		}
	}
	t.Fatalf("Codex App Server calls did not contain matching %s: %#v", method, state.calls)
}

func cloneCodexDaemonLaneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	body, _ := json.Marshal(input)
	var cloned map[string]any
	_ = json.Unmarshal(body, &cloned)
	return cloned
}

func firstCodexDaemonLaneValue(values []any) any {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}
