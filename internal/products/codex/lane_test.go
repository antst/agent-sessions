package codex

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type nativeFixture struct {
	start    bridge.CodexStartRequest
	resume   []any
	turn     bridge.CodexLaneTurnRequest
	archived string
}

func (native *nativeFixture) StartThread(_ context.Context, request bridge.CodexStartRequest) (bridge.CodexNativeThread, error) {
	native.start = request
	return bridge.CodexNativeThread{ID: "thread-start", Cwd: request.Cwd}, nil
}
func (native *nativeFixture) PrepareLaneThread(_ context.Context, id, cwd, approval, sandbox string) (bridge.CodexNativeThread, error) {
	native.resume = []any{id, cwd, approval, sandbox}
	return bridge.CodexNativeThread{ID: id, Cwd: cwd}, nil
}
func (native *nativeFixture) StartLaneTurn(_ context.Context, request bridge.CodexLaneTurnRequest) (string, error) {
	native.turn = request
	return "turn", nil
}
func (*nativeFixture) WaitLaneTurn(_ context.Context, thread, turn string) (bridge.CodexLaneTurnResult, error) {
	return bridge.CodexLaneTurnResult{ThreadID: thread, TurnID: turn, Outcome: "completed", Result: "done"}, nil
}
func (*nativeFixture) InterruptLaneTurn(context.Context, string, string) error { return nil }
func (native *nativeFixture) ArchiveThread(_ context.Context, id string) error {
	native.archived = id
	return nil
}

func TestLaneDriverTranslatesStartResumeTurnAndLifecycle(t *testing.T) {
	native := &nativeFixture{}
	driver, err := NewLaneDriver(func() (LaneNative, error) { return native, nil })
	if err != nil {
		t.Fatal(err)
	}
	started, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Name: "named", Cwd: "/work",
		PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil || started.NativeSessionID != "thread-start" || native.start.ApprovalPolicy != "never" || native.start.Sandbox != "danger-full-access" {
		t.Fatalf("start = %#v request=%#v err=%v", started, native.start, err)
	}
	resumed, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", ResumeNativeID: "thread-resume", Cwd: "/work",
		PermissionMode: permissionmode.Default, ApprovalPolicy: "on-request", Sandbox: "workspace-write",
	})
	if err != nil || !reflect.DeepEqual(native.resume, []any{"thread-resume", "/work", "on-request", "workspace-write"}) {
		t.Fatalf("resume = %#v request=%#v err=%v", resumed, native.resume, err)
	}
	turn, err := driver.StartTurn(context.Background(), resumed, productruntime.TurnStartRequest{
		Prompt: "work", PermissionMode: permissionmode.Default, ApprovalPolicy: "on-request",
		Sandbox: "workspace-write", Effort: "high", SchemaPath: "/schema", Arguments: []string{"--model", "fixture"},
	})
	if err != nil || turn.NativeTurnID != "turn" || native.turn.ThreadID != "thread-resume" || native.turn.Prompt != "work" || native.turn.Effort != "high" {
		t.Fatalf("turn = %#v request=%#v err=%v", turn, native.turn, err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "done" {
		t.Fatalf("terminal = %#v err=%v", terminal, err)
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), resumed); err != nil || native.archived != "thread-resume" {
		t.Fatalf("archive=%q err=%v", native.archived, err)
	}
}

func TestLaneDriverRejectsNativeIdentitySubstitution(t *testing.T) {
	native := &nativeFixture{}
	driver, _ := NewLaneDriver(func() (LaneNative, error) { return native, nil })
	native.resume = nil
	bad := &substitutingNative{nativeFixture: native}
	driver.native = func() (LaneNative, error) { return bad, nil }
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", ResumeNativeID: "wanted", Cwd: "/work", PermissionMode: permissionmode.Default,
	}); err == nil {
		t.Fatal("resume identity substitution unexpectedly accepted")
	}
	driver.native = func() (LaneNative, error) { return nil, errors.New("native unavailable") }
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Name: "named", Cwd: "/work", PermissionMode: permissionmode.Default,
	}); err == nil {
		t.Fatal("native provider failure unexpectedly hidden")
	}
}

type substitutingNative struct{ *nativeFixture }

func (native *substitutingNative) PrepareLaneThread(_ context.Context, _, cwd, _, _ string) (bridge.CodexNativeThread, error) {
	return bridge.CodexNativeThread{ID: "other", Cwd: cwd}, nil
}
