package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type testRPCRequest struct {
	ID     int64
	Method string
	Params map[string]any
}

type testRPCProcess struct {
	reads chan []byte
	done  chan struct{}
	once  sync.Once

	mu       sync.Mutex
	requests []testRPCRequest
	handle   func(testRPCRequest)
}

func newTestRPCProcess() *testRPCProcess {
	return &testRPCProcess{reads: make(chan []byte, 32), done: make(chan struct{})}
}

func (process *testRPCProcess) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-process.done:
		return nil, io.EOF
	case frame := <-process.reads:
		return frame, nil
	}
}

func (process *testRPCProcess) WriteFrame(_ context.Context, frame []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(frame, &raw); err != nil {
		return err
	}
	id, _ := rpcID(raw["id"])
	request := testRPCRequest{ID: id, Method: raw["method"].(string), Params: mapValue(raw["params"])}
	process.mu.Lock()
	process.requests = append(process.requests, request)
	handle := process.handle
	process.mu.Unlock()
	if handle != nil {
		handle(request)
	}
	return nil
}

func (process *testRPCProcess) Cleanup(context.Context) error {
	process.once.Do(func() { close(process.done) })
	return nil
}

func (process *testRPCProcess) emit(message any) {
	body, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	select {
	case <-process.done:
	case process.reads <- body:
	}
}

func (process *testRPCProcess) respond(id int64, result map[string]any) {
	process.emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (process *testRPCProcess) notify(method string, params map[string]any) {
	process.emit(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (process *testRPCProcess) methods() []string {
	process.mu.Lock()
	defer process.mu.Unlock()
	methods := make([]string, 0, len(process.requests))
	for _, request := range process.requests {
		methods = append(methods, request.Method)
	}
	return methods
}

func (process *testRPCProcess) request(method string) testRPCRequest {
	process.mu.Lock()
	defer process.mu.Unlock()
	for _, request := range process.requests {
		if request.Method == method {
			return request
		}
	}
	return testRPCRequest{}
}

type testProcessFactory struct {
	process *testRPCProcess
	command productruntime.NativeCommand
}

func (factory *testProcessFactory) StartRPC(_ context.Context, command productruntime.NativeCommand) (rpcProcess, error) {
	factory.command = command
	return factory.process, nil
}

func newTestDriver(t *testing.T, process *testRPCProcess) (*LaneDriver, *testProcessFactory) {
	t.Helper()
	factory := &testProcessFactory{process: process}
	driver, err := NewLaneDriver(LaneConfig{
		Executable: "/bin/qwen", HostExecutable: "/bin/agent-sessions", Generation: 7, Processes: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver, factory
}

func initializeQwen(process *testRPCProcess, request testRPCRequest) bool {
	if request.Method != "initialize" {
		return false
	}
	process.respond(request.ID, map[string]any{
		"protocolVersion": 1,
		"agentInfo":       map[string]any{"name": "qwen-code"},
		"agentCapabilities": map[string]any{
			"loadSession": true,
		},
	})
	return true
}

func TestLaneUsesLiteralYoloNativeNameAndSynchronousTurn(t *testing.T) {
	process := newTestRPCProcess()
	process.handle = func(request testRPCRequest) {
		if initializeQwen(process, request) {
			return
		}
		switch request.Method {
		case "session/new":
			meta := mapValue(request.Params["_meta"])
			requested, _ := meta["qwen-code/sessionId"].(string)
			process.respond(request.ID, map[string]any{
				"sessionId": requested, "modes": map[string]any{"currentModeId": "yolo"},
			})
		case "renameSession":
			process.respond(request.ID, map[string]any{"success": true})
		case "session/prompt":
			process.notify("session/update", map[string]any{
				"sessionId": "11111111-1111-4111-8111-111111111111",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "native answer"},
				},
			})
			process.respond(request.ID, map[string]any{"stopReason": "end_turn"})
		}
	}
	driver, factory := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "11111111-1111-4111-8111-111111111111", Name: "native name", Cwd: "/work",
		PermissionMode: permissionmode.BypassPermissions, Arguments: []string{"--model", "qwen3.8-max"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.LaneID != ref.NativeSessionID || ref.NativeSessionID != "11111111-1111-4111-8111-111111111111" || ref.Generation != 7 {
		t.Fatalf("session ref = %+v", ref)
	}
	split := ref
	split.LaneID = "provisional"
	if _, err := driver.StartTurn(ctx, split, productruntime.TurnStartRequest{Prompt: "must not run", PermissionMode: permissionmode.BypassPermissions}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("provisional identity start error = %v", err)
	}
	if want := []string{"--acp", "--yolo", "--model", "qwen3.8-max"}; !reflect.DeepEqual(factory.command.Args, want) {
		t.Fatalf("Qwen args = %q, want %q", factory.command.Args, want)
	}
	rename := process.request("renameSession")
	if rename.Params["sessionId"] != ref.NativeSessionID || rename.Params["title"] != "native name" {
		t.Fatalf("rename request = %+v", rename)
	}
	server := mapValue(process.request("session/new").Params["mcpServers"].([]any)[0])
	if server["command"] != "/bin/agent-sessions" {
		t.Fatalf("MCP server = %+v", server)
	}
	if meta := mapValue(process.request("session/new").Params["_meta"]); meta["qwen-code/sessionId"] != ref.NativeSessionID {
		t.Fatalf("native session request metadata = %+v", meta)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{
		Prompt: "  preserve this prompt  ", PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := process.request("session/prompt")
	content := mapValue(prompt.Params["prompt"].([]any)[0])
	if content["text"] != "  preserve this prompt  " {
		t.Fatalf("native prompt = %q", content["text"])
	}
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native answer" || terminal.NativeStopReason != "end_turn" {
		t.Fatalf("terminal = %+v", terminal)
	}
	resumed, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: ref.NativeSessionID, ResumeNativeID: ref.NativeSessionID, Cwd: "/work",
		PermissionMode: permissionmode.BypassPermissions, Arguments: []string{"--model", "qwen3.8-max"},
	})
	if err != nil || resumed != ref {
		t.Fatalf("reuse live session = %+v, %v", resumed, err)
	}
	if got := process.methods(); !reflect.DeepEqual(got, []string{"initialize", "session/new", "renameSession", "session/prompt"}) {
		t.Fatalf("reuse reopened native session: methods = %q", got)
	}
	if err := driver.Archive(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestLaneResumesExactSessionWithoutRenamingAndInterruptsWithAcknowledgement(t *testing.T) {
	process := newTestRPCProcess()
	var promptID int64
	process.handle = func(request testRPCRequest) {
		if initializeQwen(process, request) {
			return
		}
		switch request.Method {
		case "session/resume":
			process.respond(request.ID, map[string]any{"modes": map[string]any{"currentModeId": "default"}})
		case "session/prompt":
			promptID = request.ID
		case "craft/cancelPendingPrompt":
			process.respond(request.ID, map[string]any{"cancelled": true})
			process.respond(promptID, map[string]any{"stopReason": "cancelled"})
		}
	}
	driver, _ := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "exact-native", ResumeNativeID: "exact-native", Cwd: "/elsewhere",
		PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.NativeSessionID != "exact-native" {
		t.Fatalf("resume ref = %+v", ref)
	}
	if got := process.methods(); !reflect.DeepEqual(got, []string{"initialize", "session/resume"}) {
		t.Fatalf("open methods = %q", got)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Interrupt(ctx, turn); err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnInterrupted || terminal.NativeStopReason != "cancelled" {
		t.Fatalf("interrupted terminal = %+v", terminal)
	}
	if _, err := driver.Steer(ctx, turn, productruntime.TurnStartRequest{Prompt: "ignored"}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("steer error = %v", err)
	}
	if err := driver.Archive(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestFreshLaneRejectsQwenIgnoringRequestedNativeIdentity(t *testing.T) {
	process := newTestRPCProcess()
	process.handle = func(request testRPCRequest) {
		if initializeQwen(process, request) {
			return
		}
		if request.Method == "session/new" {
			process.respond(request.ID, map[string]any{
				"sessionId": "22222222-2222-4222-8222-222222222222",
				"modes":     map[string]any{"currentModeId": "default"},
			})
		}
	}
	driver, _ := newTestDriver(t, process)
	_, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "11111111-1111-4111-8111-111111111111", Name: "native name",
		Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("ignored requested session id returned %v", err)
	}
}
