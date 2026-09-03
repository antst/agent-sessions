package claude

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
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type testStreamProcess struct {
	reads chan []byte
	done  chan struct{}
	once  sync.Once

	mu     sync.Mutex
	writes []map[string]any
}

func newTestStreamProcess() *testStreamProcess {
	return &testStreamProcess{reads: make(chan []byte, 32), done: make(chan struct{})}
}

func (process *testStreamProcess) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-process.done:
		return nil, io.EOF
	case frame := <-process.reads:
		return frame, nil
	}
}

func (process *testStreamProcess) WriteFrame(_ context.Context, body []byte) error {
	var frame map[string]any
	if err := json.Unmarshal(body, &frame); err != nil {
		return err
	}
	process.mu.Lock()
	process.writes = append(process.writes, frame)
	process.mu.Unlock()
	return nil
}

func (process *testStreamProcess) Cleanup(context.Context) error {
	process.once.Do(func() { close(process.done) })
	return nil
}

func (process *testStreamProcess) emit(frame map[string]any) {
	body, err := json.Marshal(frame)
	if err != nil {
		panic(err)
	}
	process.reads <- body
}

func (process *testStreamProcess) written(index int) map[string]any {
	process.mu.Lock()
	defer process.mu.Unlock()
	if len(process.writes) <= index {
		return nil
	}
	return process.writes[index]
}

func (process *testStreamProcess) waitWritten(t *testing.T, index int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if frame := process.written(index); frame != nil {
			return frame
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Claude stream write %d did not arrive", index)
	return nil
}

type testProcessFactory struct {
	process *testStreamProcess
	command productruntime.NativeCommand
}

func (factory *testProcessFactory) StartStream(_ context.Context, command productruntime.NativeCommand) (streamProcess, error) {
	factory.command = command
	return factory.process, nil
}

func newTestDriver(t *testing.T, process *testStreamProcess) (*LaneDriver, *testProcessFactory) {
	t.Helper()
	descriptor, ok := productcatalog.ByID(ProductID)
	if !ok {
		t.Fatal("Claude descriptor missing")
	}
	descriptor.NativeExecutable = "/bin/claude"
	factory := &testProcessFactory{process: process}
	driver, err := NewLaneDriver(LaneConfig{
		Descriptor: descriptor, HostExecutable: "/bin/agent-sessions", Generation: 7,
		Processes: factory, Now: func() time.Time { return time.Unix(123, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver, factory
}

func TestLaneUsesNativeStreamNameGroupsPolicyAndSynchronousTurns(t *testing.T) {
	process := newTestStreamProcess()
	driver, factory := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "11111111-1111-4111-8111-111111111111", Name: "native name",
		Groups: []string{"parent/private", "project/child"}, Cwd: "/work", PermissionMode: permissionmode.Default,
		Arguments: []string{"--model", "haiku"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.NativeSessionID != "11111111-1111-4111-8111-111111111111" || ref.Generation != 7 {
		t.Fatalf("session ref = %+v", ref)
	}
	wantArgs := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages",
		"--session-id", ref.NativeSessionID, "--name", "native name", "--permission-mode", "dontAsk",
		"--allowedTools", "mcp__plugin_agent-sessions_agent_sessions__*", "--model", "haiku",
	}
	if !reflect.DeepEqual(factory.command.Args, wantArgs) {
		t.Fatalf("Claude args = %q, want %q", factory.command.Args, wantArgs)
	}
	wantEnv := []productruntime.EnvVar{
		{Name: "AGENT_SESSIONS_HOST_BINARY", Value: "/bin/agent-sessions"},
		{Name: "AGENT_SESSIONS_PRODUCT", Value: ProductID},
		{Name: "AGENT_SESSIONS_SESSION_ID", Value: ref.LaneID},
		{Name: "AGENT_SESSIONS_SESSION_NAME", Value: "native name"},
		{Name: "AGENT_SESSIONS_GROUPS", Value: `["parent/private","project/child"]`},
	}
	if !reflect.DeepEqual(factory.command.Env, wantEnv) {
		t.Fatalf("Claude env = %#v, want %#v", factory.command.Env, wantEnv)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "preserve this prompt", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 0), "preserve this prompt")
	process.emit(map[string]any{"type": "system", "subtype": "init", "session_id": ref.NativeSessionID})
	emitReplay(process, ref.NativeSessionID, "preserve this prompt")
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": "native answer", "terminal_reason": "completed", "session_id": ref.NativeSessionID,
	})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native answer" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
	reused, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: ref.LaneID, ResumeNativeID: ref.NativeSessionID,
		Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil || reused != ref {
		t.Fatalf("reuse = %+v, %v", reused, err)
	}
	if err := driver.Archive(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSemanticSteerAndControlInterruptShareOneStream(t *testing.T) {
	process := newTestStreamProcess()
	driver, _ := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "22222222-2222-4222-8222-222222222222", Name: "worker",
		Cwd: "/work", PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "original", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := driver.Steer(ctx, turn, productruntime.TurnStartRequest{Prompt: "steered", PermissionMode: permissionmode.BypassPermissions})
	if err != nil || accepted.NativeSessionID != ref.NativeSessionID || accepted.NativeMessageID != turn.NativeTurnID || !accepted.AcceptedAt.Equal(time.Unix(123, 0)) {
		t.Fatalf("steer = %+v, %v", accepted, err)
	}
	assertUserText(t, process.waitWritten(t, 0), "original")
	assertUserText(t, process.waitWritten(t, 1), "steered")
	emitReplay(process, ref.NativeSessionID, "original")
	emitReplay(process, ref.NativeSessionID, "steered")
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "STEERED_SENTINEL",
		"terminal_reason": "completed", "session_id": ref.NativeSessionID,
	})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Result != "STEERED_SENTINEL" {
		t.Fatalf("steered terminal = %+v, %v", terminal, err)
	}

	interruptedTurn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "sleep", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	emitReplay(process, ref.NativeSessionID, "sleep")
	interruptDone := make(chan error, 1)
	go func() { interruptDone <- driver.Interrupt(ctx, interruptedTurn) }()
	control := process.waitWritten(t, 3)
	if control["type"] != "control_request" || control["request"].(map[string]any)["subtype"] != "interrupt" {
		t.Fatalf("interrupt frame = %#v", control)
	}
	requestID := control["request_id"].(string)
	process.emit(map[string]any{
		"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": requestID, "response": map[string]any{"still_queued": []any{}},
		},
	})
	if err := <-interruptDone; err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{
		"type": "result", "subtype": "error_during_execution", "is_error": true,
		"terminal_reason": "aborted_streaming", "session_id": ref.NativeSessionID,
	})
	terminal, err = driver.WaitTurn(ctx, interruptedTurn)
	if err != nil || terminal.Outcome != productruntime.TurnInterrupted || terminal.ExitLike != 130 {
		t.Fatalf("interrupted terminal = %+v, %v", terminal, err)
	}

	finalTurn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "after", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	emitReplay(process, ref.NativeSessionID, "after")
	process.emit(map[string]any{"type": "result", "subtype": "success", "result": "AFTER_INTERRUPT", "session_id": ref.NativeSessionID})
	terminal, err = driver.WaitTurn(ctx, finalTurn)
	if err != nil || terminal.Result != "AFTER_INTERRUPT" {
		t.Fatalf("post-interrupt terminal = %+v, %v", terminal, err)
	}
}

func TestLaneMessageCorrelatesProductReplayAcrossInterjectionAndEnqueue(t *testing.T) {
	process := newTestStreamProcess()
	driver, _ := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "33333333-3333-4333-8333-333333333333", Name: "worker",
		Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "original", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	message := `<cross-session-message from="parent">change course</cross-session-message>`
	if err := driver.SendMessage(ctx, ref, message); err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 0), "original")
	assertUserText(t, process.waitWritten(t, 1), "The following Agent Sessions peer message is the current user turn. Act on its enclosed content and preserve its sender metadata.\n\n"+message)
	emitReplay(process, ref.NativeSessionID, "original")
	emitReplay(process, ref.NativeSessionID, message)
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "BUSY_MESSAGE_SENTINEL",
		"session_id": ref.NativeSessionID,
	})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Result != "BUSY_MESSAGE_SENTINEL" {
		t.Fatalf("busy-message terminal = %+v, %v", terminal, err)
	}

	queuedMessage := `<cross-session-message from="parent">queue after final inference</cross-session-message>`
	queuedTurn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "final inference", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 2), "final inference")
	emitReplay(process, ref.NativeSessionID, "final inference")
	if err := driver.SendMessage(ctx, ref, queuedMessage); err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 3), "The following Agent Sessions peer message is the current user turn. Act on its enclosed content and preserve its sender metadata.\n\n"+queuedMessage)
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "FINAL_INFERENCE_RESULT",
		"session_id": ref.NativeSessionID,
	})
	terminal, err = driver.WaitTurn(ctx, queuedTurn)
	if err != nil || terminal.Result != "FINAL_INFERENCE_RESULT" {
		t.Fatalf("final-inference terminal = %+v, %v", terminal, err)
	}
	emitReplay(process, ref.NativeSessionID, queuedMessage)
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "QUEUED_MESSAGE_RESULT",
		"session_id": ref.NativeSessionID,
	})
	waitReplayCount(t, driver, ref, 4)

	idleMessage := `<cross-session-message from="parent">reply then finish</cross-session-message>`
	if err := driver.SendMessage(ctx, ref, idleMessage); err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 4), "The following Agent Sessions peer message is the current user turn. Act on its enclosed content and preserve its sender metadata.\n\n"+idleMessage)
	immediate, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "tracked after message", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 5), "tracked after message")
	process.emit(map[string]any{
		"type": "user", "session_id": ref.NativeSessionID,
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "native-tool"}}},
	})
	emitReplay(process, ref.NativeSessionID, idleMessage)
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "UNTRACKED_MESSAGE_RESULT",
		"session_id": ref.NativeSessionID,
	})
	emitReplay(process, ref.NativeSessionID, "tracked after message")
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "TRACKED_AFTER_MESSAGE",
		"session_id": ref.NativeSessionID,
	})
	terminal, err = driver.WaitTurn(ctx, immediate)
	if err != nil || terminal.Result != "TRACKED_AFTER_MESSAGE" {
		t.Fatalf("immediate post-message terminal = %+v, %v", terminal, err)
	}

	after, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "healthy after queued result", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	emitReplay(process, ref.NativeSessionID, "healthy after queued result")
	process.emit(map[string]any{"type": "result", "subtype": "success", "result": "STILL_HEALTHY", "session_id": ref.NativeSessionID})
	terminal, err = driver.WaitTurn(ctx, after)
	if err != nil || terminal.Result != "STILL_HEALTHY" {
		t.Fatalf("healthy terminal = %+v, %v", terminal, err)
	}
}

func TestLaneIgnoresResultWithoutAConsumedTrackedTurn(t *testing.T) {
	process := newTestStreamProcess()
	driver, _ := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "stray", Name: "worker", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{
		"type": "result", "subtype": "success", "result": "unowned", "session_id": ref.NativeSessionID,
	})
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "after ignored result", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	emitReplay(process, ref.NativeSessionID, "after ignored result")
	process.emit(map[string]any{"type": "result", "subtype": "success", "result": "owned", "session_id": ref.NativeSessionID})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Result != "owned" {
		t.Fatalf("terminal after ignored result = %+v, %v", terminal, err)
	}
}

func TestLaneTurnSurfacesProductError(t *testing.T) {
	process := newTestStreamProcess()
	driver, _ := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "33333333-3333-4333-8333-333333333333", Name: "worker",
		Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := `<cross-session-message from="parent">wake</cross-session-message>`
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: message, PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	assertUserText(t, process.waitWritten(t, 0), "The following Agent Sessions peer message is the current user turn. Act on its enclosed content and preserve its sender metadata.\n\n"+message)
	emitReplay(process, ref.NativeSessionID, message)
	process.emit(map[string]any{
		"type": "result", "subtype": "error_max_turns", "is_error": true,
		"result": "Claude product refused this exact turn", "session_id": ref.NativeSessionID,
	})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err == nil || err.Error() != "Claude product refused this exact turn" || terminal.Outcome != productruntime.TurnFailed || terminal.Result != "Claude product refused this exact turn" {
		t.Fatalf("failed terminal = %+v, %v", terminal, err)
	}
}

func TestLaneResumeUsesExactNativeIDWithoutRenamingAndRejectsLifecycleArgs(t *testing.T) {
	process := newTestStreamProcess()
	driver, factory := newTestDriver(t, process)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Name: "cached name", ResumeNativeID: "44444444-4444-4444-8444-444444444444",
		Cwd: "/work", PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages",
		"--resume", ref.NativeSessionID, "--dangerously-skip-permissions",
		"--allowedTools", "mcp__plugin_agent-sessions_agent_sessions__*",
	}
	if !reflect.DeepEqual(factory.command.Args, want) {
		t.Fatalf("resume args = %q, want %q", factory.command.Args, want)
	}
	second := newTestStreamProcess()
	secondDriver, _ := newTestDriver(t, second)
	_, err = secondDriver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "other", Name: "worker", Cwd: "/work", PermissionMode: permissionmode.Default,
		Arguments: []string{"--session-id", "override"},
	})
	if !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("reserved argument error = %v", err)
	}
}

func emitReplay(process *testStreamProcess, sessionID, text string) {
	process.emit(map[string]any{
		"type": "user", "isReplay": true, "session_id": sessionID,
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
	})
}

func waitReplayCount(t *testing.T, driver *LaneDriver, ref productruntime.NativeSessionRef, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session, err := driver.lookupSession(ref)
		if err != nil {
			t.Fatal(err)
		}
		session.mu.Lock()
		replays := session.replays
		session.mu.Unlock()
		if replays >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Claude replay count did not reach %d", want)
}

func assertUserText(t *testing.T, frame map[string]any, want string) {
	t.Helper()
	message := frame["message"].(map[string]any)
	content := message["content"].([]any)
	first := content[0].(map[string]any)
	if frame["type"] != "user" || message["role"] != "user" || first["type"] != "text" || first["text"] != want {
		t.Fatalf("user frame = %#v, want text %q", frame, want)
	}
}
