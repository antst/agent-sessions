package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type scriptedACPProcess struct {
	ref                productruntime.OwnedProcessRef
	reads              chan []byte
	writes             chan map[string]any
	cleanup            bool
	cleanupCalls       int
	cleanupFailures    int
	cleanupDeadline    bool
	command            productruntime.NativeCommand
	writeHook          func(map[string]any)
	writeFailureMethod string
	mu                 sync.Mutex
}

func newScriptedACPProcess() *scriptedACPProcess {
	return &scriptedACPProcess{
		ref:   productruntime.OwnedProcessRef{Process: procinfo.Identity{PID: 200, Start: "start", StrongStart: "strong"}, ProcessGroup: 200},
		reads: make(chan []byte, 32), writes: make(chan map[string]any, 32),
	}
}

func (process *scriptedACPProcess) Ref() productruntime.OwnedProcessRef { return process.ref }
func (process *scriptedACPProcess) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-process.reads:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}
func (process *scriptedACPProcess) WriteFrame(_ context.Context, body []byte) error {
	var frame map[string]any
	if err := json.Unmarshal(body, &frame); err != nil {
		return err
	}
	process.writes <- frame
	if process.writeHook != nil {
		process.writeHook(frame)
	}
	process.mu.Lock()
	failureMethod := process.writeFailureMethod
	process.mu.Unlock()
	if frame["method"] == failureMethod {
		return errors.New("scripted possible partial write")
	}
	return nil
}
func (process *scriptedACPProcess) Cleanup(ctx context.Context) error {
	process.mu.Lock()
	defer process.mu.Unlock()
	process.cleanupCalls++
	_, process.cleanupDeadline = ctx.Deadline()
	if process.cleanupFailures > 0 {
		process.cleanupFailures--
		return errors.New("scripted cleanup failure")
	}
	process.cleanup = true
	return nil
}
func (process *scriptedACPProcess) Wait(context.Context) (productruntime.ProcessExit, error) {
	return productruntime.ProcessExit{}, nil
}
func (process *scriptedACPProcess) respond(id any, result any, rpcError any) {
	frame := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcError != nil {
		frame["error"] = rpcError
	} else {
		frame["result"] = result
	}
	body, _ := json.Marshal(frame)
	process.reads <- body
}
func (process *scriptedACPProcess) notify(method string, params any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	process.reads <- body
}
func (process *scriptedACPProcess) request(id any, method string, params any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	process.reads <- body
}

type oneProcessFactory struct{ process *scriptedACPProcess }

func (factory oneProcessFactory) StartACPProcess(_ context.Context, command productruntime.NativeCommand) (ACPProcess, error) {
	factory.process.mu.Lock()
	factory.process.command = command
	factory.process.mu.Unlock()
	return factory.process, nil
}

type memoryReceiptReader struct{ body []byte }

func (reader memoryReceiptReader) OpenReceipt(string) (io.ReadCloser, int64, [32]byte, error) {
	return io.NopCloser(bytes.NewReader(reader.body)), int64(len(reader.body)), sha256.Sum256(reader.body), nil
}

type recordingLease struct {
	mu              sync.Mutex
	held            map[string]string
	acquires        int
	releases        int
	releaseCalls    int
	releaseFailures int
	releaseDeadline bool
}

func (lease *recordingLease) Acquire(_ context.Context, claim LeaseClaim) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.held == nil {
		lease.held = map[string]string{}
	}
	key := claim.ProfileIdentity + "\x00" + claim.NativeSessionID
	if owner, ok := lease.held[key]; ok && owner != claim.LaneID {
		return ErrLeaseConflict
	}
	lease.held[key] = claim.LaneID
	lease.acquires++
	return nil
}
func (lease *recordingLease) Recover(ctx context.Context, claim LeaseClaim) error {
	return lease.Acquire(ctx, claim)
}
func (lease *recordingLease) Release(ctx context.Context, claim LeaseClaim) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.releaseCalls++
	if _, ok := ctx.Deadline(); ok {
		lease.releaseDeadline = true
	}
	if lease.releaseFailures > 0 {
		lease.releaseFailures--
		return errors.New("scripted release failure")
	}
	key := claim.ProfileIdentity + "\x00" + claim.NativeSessionID
	if lease.held[key] != claim.LaneID {
		return ErrLeaseConflict
	}
	delete(lease.held, key)
	lease.releases++
	return nil
}

func newTestLane(t *testing.T, process *scriptedACPProcess, lease *recordingLease) *LaneDriver {
	t.Helper()
	dshHome := managedTestDSHHome(t)
	driver, err := NewLaneDriver(LaneConfig{
		Executable: "dsh", ACPProfile: "acp", Generation: 7,
		DSHHome: dshHome, ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: process},
		Leases: lease, Receipts: memoryReceiptReader{body: []byte("hello")},
		Environment: []productruntime.EnvVar{{Name: "DSH_HOME", Value: filepath.Join(t.TempDir(), "foreign-home")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestACPLaneNewPromptStopAndCancelNotification(t *testing.T) {
	process := newScriptedACPProcess()
	lease := &recordingLease{}
	process.writeHook = func(frame map[string]any) {
		method, _ := frame["method"].(string)
		id, hasID := frame["id"]
		switch method {
		case "initialize":
			process.respond(id, map[string]any{"protocolVersion": 1, "authMethods": []any{}}, nil)
		case "session/list":
			process.respond(id, map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(id, map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			go func() {
				process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "private"}}})
				process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "message-1", "content": map[string]any{"type": "text", "text": "done"}}})
				process.respond(id, map[string]any{"stopReason": "end_turn"}, nil)
			}()
		case "session/cancel":
			if hasID {
				t.Errorf("session/cancel was sent as a request")
			}
		case "session/close":
			process.respond(id, map[string]any{}, nil)
		}
	}
	driver := newTestLane(t, process, lease)
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp",
	})
	if err != nil || session.NativeSessionID != "native" {
		t.Fatalf("Open() = %+v, %v", session, err)
	}
	if got := envValue(process.command.Env, "DSH_PERMISSION_MODE"); got != string(SandboxWorkspaceWrite) {
		t.Fatalf("DSH_PERMISSION_MODE = %q, want workspace-write", got)
	}
	if got := envValue(process.command.Env, lanePolicyEnv); got != "workspace-write:ask" {
		t.Fatalf("%s = %q, want workspace-write:ask", lanePolicyEnv, got)
	}
	if got := envValue(process.command.Env, "DSH_HOME"); got != driver.config.DSHHome {
		t.Fatalf("lane DSH_HOME = %q, want managed %q", got, driver.config.DSHHome)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "done" || terminal.NativeStopReason != "end_turn" {
		t.Fatalf("WaitTurn() = %+v, %v", terminal, err)
	}
	// Start a second prompt that remains live, then prove interrupt is a notification.
	var pendingPromptID any
	process.writeHook = func(frame map[string]any) {
		method, _ := frame["method"].(string)
		id, hasID := frame["id"]
		if method == "session/prompt" {
			pendingPromptID = id
			process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "usage_update"}})
			process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "message-2", "content": map[string]any{"type": "text", "text": ""}}})
		}
		if method == "session/cancel" {
			if hasID {
				t.Errorf("cancel request carried id %v", id)
			}
			process.respond(pendingPromptID, map[string]any{"stopReason": "cancelled"}, nil)
		}
	}
	turn, err = driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	cancelFrame := <-process.writes
	for cancelFrame["method"] != "session/cancel" {
		cancelFrame = <-process.writes
	}
	if _, hasID := cancelFrame["id"]; hasID {
		t.Fatal("cancel notification unexpectedly has an id")
	}
	terminal, err = driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnInterrupted || terminal.NativeStopReason != "cancelled" {
		t.Fatalf("cancel terminal = %+v, %v", terminal, err)
	}
}

func TestACPBusyPromptMapsToUnsupportedSteerBeforeNativeAcceptance(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		id := frame["id"]
		switch frame["method"] {
		case "initialize":
			process.respond(id, map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(id, map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(id, map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			process.respond(id, nil, map[string]any{"code": -32602, "message": "a prompt is already in flight for this session"})
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("busy StartTurn error = %v, want ErrUnsupportedSteer", err)
	}
}

func TestRepeatedImmediatePromptErrorsLeaveNoTerminalCacheGhosts(t *testing.T) {
	process := newScriptedACPProcess()
	process.writes = make(chan map[string]any, 1024)
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			process.respond(frame["id"], nil, map[string]any{"code": -32602, "message": "busy"})
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	for range 512 {
		if _, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
			t.Fatalf("immediate prompt error = %v, want ErrUnsupportedSteer", err)
		}
	}
	driver.mu.Lock()
	session := driver.sessions[reference.LaneID]
	turns, order, bytes, active := len(session.turns), len(session.turnOrder), session.terminalBytes, session.active
	driver.mu.Unlock()
	if turns != 0 || order != 0 || bytes != 0 || active != "" {
		t.Fatalf("immediate-error cache turns/order/bytes/active = %d/%d/%d/%q", turns, order, bytes, active)
	}
}

func TestACPTurnProjectsLastOrderedAssistantMessage(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			for _, chunk := range []struct{ id, text string }{
				{"message-1", "first "}, {"message-1", "answer"},
				{"message-2", "final "}, {"message-2", "answer"},
			} {
				process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk", "messageId": chunk.id,
					"content": map[string]any{"type": "text", "text": chunk.text},
				}})
			}
			process.respond(frame["id"], map[string]any{"stopReason": "end_turn"}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Result != "final answer" {
		t.Fatalf("multi-message terminal = %+v, %v", terminal, err)
	}
}

func TestACPAssistantMessageIdentityHasIndependentBound(t *testing.T) {
	turn := &laneTurn{}
	session := &laneSession{
		reference: productruntime.NativeSessionRef{LaneID: "lane", NativeSessionID: "native", Generation: 1},
		active:    "turn", turns: map[string]*laneTurn{"turn": turn},
	}
	driver := &LaneDriver{sessions: map[string]*laneSession{"lane": session}}
	driver.handleNotification(acpNotification{Method: "session/update", Params: mustJSON(t, map[string]any{
		"sessionId": "native", "update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "messageId": strings.Repeat("m", 1025),
			"content": map[string]any{"type": "text", "text": "bounded"},
		},
	})})
	if !errors.Is(turn.resultErr, productruntime.ErrProtocol) {
		t.Fatalf("oversized native message identity error = %v, want ErrProtocol", turn.resultErr)
	}
}

func TestLaneBypassPermissionSetsExactDangerPreset(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	_, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp",
		PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(process.command.Env, "DSH_PERMISSION_MODE"); got != string(SandboxDangerFullAccess) {
		t.Fatalf("DSH_PERMISSION_MODE = %q, want danger-full-access", got)
	}
	if got := envValue(process.command.Env, lanePolicyEnv); got != "danger-full-access:never" {
		t.Fatalf("%s = %q, want danger-full-access:never", lanePolicyEnv, got)
	}
}

func TestLaneLeaseRejectsSecondOwnerBeforeResume(t *testing.T) {
	lease := &recordingLease{held: map[string]string{"acp\x00native": "first"}}
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		if frame["method"] == "initialize" {
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		}
		if frame["method"] == "session/list" {
			process.respond(frame["id"], map[string]any{"sessions": []any{map[string]any{"sessionId": "native", "cwd": "/work"}}}, nil)
		}
		if frame["method"] == "session/resume" {
			t.Error("resume reached native ACP before exclusive lease")
		}
	}
	driver := newTestLane(t, process, lease)
	_, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "second", ResumeNativeID: "native", Cwd: "/work", ProfileIdentity: "acp",
	})
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("Open() error = %v, want lease conflict", err)
	}
}

func TestLaneRecoveryResumesOnlyPriorNativeIdentityAfterLeaseRecovery(t *testing.T) {
	lease := &recordingLease{}
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		id := frame["id"]
		switch frame["method"] {
		case "initialize":
			process.respond(id, map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(id, map[string]any{"sessions": []any{map[string]any{"sessionId": "native", "cwd": "/work"}}}, nil)
		case "session/resume":
			params := frame["params"].(map[string]any)
			if params["sessionId"] != "native" {
				t.Errorf("resume sessionId = %v", params["sessionId"])
			}
			process.respond(id, map[string]any{"sessionId": "native"}, nil)
		}
	}
	dshHome := managedTestDSHHome(t)
	driver, err := NewLaneDriver(LaneConfig{
		Executable: "dsh", ACPProfile: "acp", Generation: 8,
		DSHHome: dshHome, ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: process},
		Leases: lease, Receipts: memoryReceiptReader{body: []byte("hello")},
		ResolveRecovery: func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
			return productruntime.LaneOpenRequest{Cwd: "/work", ProfileIdentity: "acp"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: ProductID, LaneID: "lane", PriorNativeSessionID: "native", PriorGeneration: 7,
	})
	if err != nil || reference.NativeSessionID != "native" || reference.Generation != 8 {
		t.Fatalf("Recover() = %+v, %v", reference, err)
	}
	if lease.acquires != 1 {
		t.Fatalf("lease recovery acquisitions = %d, want 1", lease.acquires)
	}
}

func TestLaneArchiveClosesOwnedSessionCleansProcessAndReleasesLease(t *testing.T) {
	lease := &recordingLease{}
	process := newScriptedACPProcess()
	closeCalls := 0
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/close":
			closeCalls++
			process.respond(frame["id"], map[string]any{}, nil)
		}
	}
	driver := newTestLane(t, process, lease)
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatalf("idempotent Archive retry: %v", err)
	}
	process.mu.Lock()
	cleaned := process.cleanup
	cleanupCalls := process.cleanupCalls
	process.mu.Unlock()
	if !cleaned || closeCalls != 1 || cleanupCalls != 1 || lease.releaseCalls != 1 || lease.releases != 1 {
		t.Fatalf("archive close/cleanup/release = %d/%d/%d (%d confirmed)", closeCalls, cleanupCalls, lease.releaseCalls, lease.releases)
	}
	if _, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("archived reference error = %v, want ErrStale", err)
	}
}

func TestRecoveryFailureUsesDeadlineBoundProcessCleanup(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{map[string]any{"sessionId": "native", "cwd": "/work"}}}, nil)
		case "session/resume":
			process.respond(frame["id"], nil, map[string]any{"code": -32000, "message": "initialize failed"})
		}
	}
	dshHome := managedTestDSHHome(t)
	lease := &recordingLease{}
	driver, err := NewLaneDriver(LaneConfig{
		Executable: "dsh", ACPProfile: "acp", Generation: 8,
		DSHHome: dshHome, ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: process},
		Leases: lease, Receipts: memoryReceiptReader{body: []byte("hello")},
		ResolveRecovery: func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
			return productruntime.LaneOpenRequest{Cwd: "/work", ProfileIdentity: "acp"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: ProductID, LaneID: "lane", PriorNativeSessionID: "native", PriorGeneration: 7,
	}); err == nil {
		t.Fatal("Recover unexpectedly succeeded")
	}
	process.mu.Lock()
	deadline, calls := process.cleanupDeadline, process.cleanupCalls
	process.mu.Unlock()
	if !deadline || calls != 1 {
		t.Fatalf("recovery cleanup deadline/calls = %v/%d, want true/1", deadline, calls)
	}
	lease.mu.Lock()
	releaseDeadline, releaseCalls := lease.releaseDeadline, lease.releaseCalls
	lease.mu.Unlock()
	if !releaseDeadline || releaseCalls != 1 {
		t.Fatalf("recovery lease release deadline/calls = %v/%d, want true/1", releaseDeadline, releaseCalls)
	}
}

func TestACPPermissionRequestRejectsWithoutInteractiveApprovalAuthority(t *testing.T) {
	process := newScriptedACPProcess()
	var promptID any
	process.writeHook = func(frame map[string]any) {
		method, _ := frame["method"].(string)
		switch method {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			promptID = frame["id"]
			process.request(91, "session/request_permission", map[string]any{
				"sessionId": "native", "toolCall": map[string]any{"toolCallId": "tool-1"},
				"options": []any{
					map[string]any{"optionId": "deny", "name": "Reject", "kind": "reject_once"},
					map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"},
					map[string]any{"optionId": "always", "name": "Always", "kind": "allow_always"},
				},
			})
		default:
			if frame["id"] != float64(91) {
				return
			}
			result, _ := frame["result"].(map[string]any)
			outcome, _ := result["outcome"].(map[string]any)
			if outcome["outcome"] != "selected" || outcome["optionId"] != "deny" {
				t.Errorf("permission response = %+v", frame)
			}
			process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "messageId": "message", "content": map[string]any{"type": "text", "text": "allowed"},
			}})
			process.respond(promptID, map[string]any{"stopReason": "end_turn"}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Result != "allowed" {
		t.Fatalf("permission turn = %+v, %v", terminal, err)
	}
}

func TestACPPermissionPolicyNeverAutoAllowsPinnedOptions(t *testing.T) {
	request := func(options []map[string]string) acpServerRequest {
		return acpServerRequest{Method: "session/request_permission", Params: mustJSON(t, map[string]any{
			"sessionId": "native", "toolCall": map[string]any{"toolCallId": "tool"}, "options": options,
		})}
	}
	for _, approval := range []ApprovalMode{ApprovalAsk, ApprovalNever} {
		policy := &lanePermissionPolicy{sessionID: "native", policy: NativePolicy{Sandbox: SandboxWorkspaceWrite, Approval: approval}}
		result, failure := policy.handle(request([]map[string]string{
			{"optionId": "allow", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "reject", "name": "Reject once", "kind": "reject_once"},
		}))
		if failure != nil {
			t.Fatalf("approval %s: %v", approval, failure)
		}
		encoded, _ := json.Marshal(result)
		if !bytes.Contains(encoded, []byte(`"optionId":"reject"`)) || bytes.Contains(encoded, []byte(`"optionId":"allow"`)) {
			t.Fatalf("approval %s response = %s", approval, encoded)
		}
	}
	policy := &lanePermissionPolicy{sessionID: "native", policy: NativePolicy{Sandbox: SandboxWorkspaceWrite, Approval: ApprovalAsk}}
	if _, failure := policy.handle(request([]map[string]string{{"optionId": "allow", "name": "Allow once", "kind": "allow_once"}})); failure == nil || failure.Code != -32602 {
		t.Fatalf("allow-only permission failure = %+v, want -32602", failure)
	}
	if _, failure := policy.handle(request([]map[string]string{
		{"optionId": "reject-a", "name": "Reject", "kind": "reject_once"},
		{"optionId": "reject-b", "name": "Reject again", "kind": "reject_once"},
	})); failure == nil || failure.Code != -32602 {
		t.Fatalf("ambiguous reject permission failure = %+v, want -32602", failure)
	}
	if _, failure := policy.handle(request([]map[string]string{
		{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
		{"optionId": "future", "name": "Future widening", "kind": "allow_workspace"},
	})); failure == nil || failure.Code != -32602 {
		t.Fatalf("unknown pinned-option drift failure = %+v, want -32602", failure)
	}
}

func TestACPUnrelatedUpdateDoesNotAdmitAndPostWriteTimeoutPoisonsOwner(t *testing.T) {
	process := newScriptedACPProcess()
	lease := &recordingLease{}
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "usage_update"}})
			process.notify("session/update", map[string]any{"sessionId": "sibling", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "messageId": "foreign", "content": map[string]any{"type": "text", "text": "wrong"},
			}})
		}
	}
	driver := newTestLane(t, process, lease)
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := driver.StartTurn(ctx, session, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("post-write timeout error = %v, want ErrAmbiguousSession", err)
	}
	process.mu.Lock()
	cleaned := process.cleanup
	process.mu.Unlock()
	if !cleaned || lease.releases != 1 {
		t.Fatalf("poison reconciliation cleanup/release = %v/%d", cleaned, lease.releases)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("poisoned reference reuse error = %v, want ErrStale", err)
	}
	close(process.reads)
}

func TestACPPromptWriteFailurePoisonsPossibleNativeWrite(t *testing.T) {
	process := newScriptedACPProcess()
	lease := &recordingLease{}
	process.writeFailureMethod = "session/prompt"
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		}
	}
	driver := newTestLane(t, process, lease)
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("possible-write prompt error = %v, want ErrAmbiguousSession", err)
	}
	process.mu.Lock()
	cleanupCalls := process.cleanupCalls
	process.mu.Unlock()
	if cleanupCalls != 1 || lease.releases != 1 {
		t.Fatalf("possible-write reconciliation cleanup/release = %d/%d", cleanupCalls, lease.releases)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("possible-write owner reuse error = %v, want ErrStale", err)
	}
}

func TestWaitTurnIsConcurrentRetrySafe(t *testing.T) {
	process := newScriptedACPProcess()
	releaseResponse := make(chan struct{})
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			id := frame["id"]
			go func() {
				process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk", "messageId": "message", "content": map[string]any{"type": "text", "text": "done"},
				}})
				<-releaseResponse
				process.respond(id, map[string]any{"stopReason": "end_turn"}, nil)
			}()
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt"})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan productruntime.NativeTerminal, 2)
	failures := make(chan error, 2)
	for range 2 {
		go func() {
			terminal, waitErr := driver.WaitTurn(context.Background(), turn)
			results <- terminal
			failures <- waitErr
		}()
	}
	close(releaseResponse)
	for range 2 {
		if waitErr := <-failures; waitErr != nil {
			t.Fatal(waitErr)
		}
		if terminal := <-results; terminal.Result != "done" || terminal.Outcome != productruntime.TurnCompleted {
			t.Fatalf("concurrent terminal = %+v", terminal)
		}
	}
}

func TestSettledTurnEvidenceIsByteBoundedAndRetryableUntilEvicted(t *testing.T) {
	process := newScriptedACPProcess()
	prompt := 0
	result := strings.Repeat("x", 900<<10)
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/prompt":
			prompt++
			process.notify("session/update", map[string]any{"sessionId": "native", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "messageId": fmt.Sprintf("message-%d", prompt),
				"content": map[string]any{"type": "text", "text": result},
			}})
			process.respond(frame["id"], map[string]any{"stopReason": "end_turn"}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	sessionRef, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	var first, last productruntime.NativeTurnRef
	for index := 0; index < 6; index++ {
		turn, startErr := driver.StartTurn(context.Background(), sessionRef, productruntime.TurnStartRequest{ReceiptID: "receipt"})
		if startErr != nil {
			t.Fatal(startErr)
		}
		terminal, waitErr := driver.WaitTurn(context.Background(), turn)
		if waitErr != nil || terminal.Result != result {
			t.Fatalf("turn %d terminal bytes/error = %d/%v", index, len(terminal.Result), waitErr)
		}
		if index == 0 {
			first = turn
		}
		last = turn
	}
	if _, err := driver.WaitTurn(context.Background(), first); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("evicted terminal retry error = %v, want ErrStale", err)
	}
	if terminal, err := driver.WaitTurn(context.Background(), last); err != nil || terminal.Result != result {
		t.Fatalf("retained terminal retry bytes/error = %d/%v", len(terminal.Result), err)
	}
	driver.mu.Lock()
	session := driver.sessions[sessionRef.LaneID]
	turnCount, terminalBytes := len(session.turns), session.terminalBytes
	for _, turn := range session.turns {
		if turn.result != nil || turn.lastResult != nil || turn.messageIDs != nil || turn.messageID != "" {
			driver.mu.Unlock()
			t.Fatal("settled turn retained transient ACP result/message buffers")
		}
	}
	driver.mu.Unlock()
	if turnCount > maxSettledTurns || terminalBytes > maxSettledTurnBytes {
		t.Fatalf("settled evidence count/bytes = %d/%d", turnCount, terminalBytes)
	}
}

func TestLaneArchiveRetriesOnlyUnconfirmedSteps(t *testing.T) {
	lease := &recordingLease{releaseFailures: 1}
	process := newScriptedACPProcess()
	process.cleanupFailures = 1
	closeCalls := 0
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/close":
			closeCalls++
			process.respond(frame["id"], map[string]any{}, nil)
		}
	}
	driver := newTestLane(t, process, lease)
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err == nil {
		t.Fatal("archive unexpectedly hid cleanup failure")
	}
	if err := driver.Archive(context.Background(), reference); err == nil {
		t.Fatal("archive unexpectedly hid lease release failure")
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	cleanupCalls := process.cleanupCalls
	process.mu.Unlock()
	if closeCalls != 1 || cleanupCalls != 2 || lease.releaseCalls != 2 || lease.releases != 1 {
		t.Fatalf("archive step calls close/cleanup/release/success = %d/%d/%d/%d", closeCalls, cleanupCalls, lease.releaseCalls, lease.releases)
	}
}

func TestLaneArchiveLostCloseResponseNeverResendsClose(t *testing.T) {
	lease := &recordingLease{}
	process := newScriptedACPProcess()
	closeCalls := 0
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "native"}, nil)
		case "session/close":
			closeCalls++
			// The native close may have completed, but its response is lost.
		}
	}
	driver := newTestLane(t, process, lease)
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := driver.Archive(ctx, reference); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("lost close response error = %v, want ErrAmbiguousSession", err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatalf("post-reconciliation Archive retry: %v", err)
	}
	process.mu.Lock()
	cleanupCalls := process.cleanupCalls
	process.mu.Unlock()
	if closeCalls != 1 || cleanupCalls != 1 || lease.releaseCalls != 1 || lease.releases != 1 {
		t.Fatalf("lost-response close/cleanup/release = %d/%d/%d (%d confirmed)", closeCalls, cleanupCalls, lease.releaseCalls, lease.releases)
	}
}

func TestLaneRequiresConfiguredProfileAndLiveResumeCwd(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{"sessions": []any{map[string]any{"sessionId": "native", "cwd": "/other"}}}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "wrong-profile", Cwd: "/work", ProfileIdentity: "other"}); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("profile mismatch error = %v, want ErrIncompatible", err)
	}
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "wrong-cwd", ResumeNativeID: "native", Cwd: "/work", ProfileIdentity: "acp"}); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("live cwd mismatch error = %v, want ErrProtocol", err)
	}
}

func TestLaneSessionListRequiresExplicitSessionsArray(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/list":
			process.respond(frame["id"], map[string]any{}, nil)
		}
	}
	driver := newTestLane(t, process, &recordingLease{})
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp",
	}); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("missing sessions array error = %v, want ErrProtocol", err)
	}
}

func TestLaneRejectsNonCanonicalNativeSessionIDs(t *testing.T) {
	for _, nativeID := range []string{" native ", "native\nline", "native\x00id"} {
		t.Run(fmt.Sprintf("%q", nativeID), func(t *testing.T) {
			process := newScriptedACPProcess()
			process.writeHook = func(frame map[string]any) {
				switch frame["method"] {
				case "initialize":
					process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
				case "session/list":
					process.respond(frame["id"], map[string]any{"sessions": []any{}}, nil)
				case "session/new":
					process.respond(frame["id"], map[string]any{"sessionId": nativeID}, nil)
				}
			}
			driver := newTestLane(t, process, &recordingLease{})
			if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: ProductID, LaneID: "lane", Cwd: "/work", ProfileIdentity: "acp",
			}); !errors.Is(err, productruntime.ErrProtocol) {
				t.Fatalf("native ID drift error = %v, want ErrProtocol", err)
			}
		})
	}
}
