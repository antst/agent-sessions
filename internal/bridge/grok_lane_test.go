package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const grokLaneLockHolderEnv = "AGENT_SESSIONS_GROK_LANE_LOCK_HOLDER"
const grokLaneSessionHelperEnv = "AGENT_SESSIONS_GROK_LANE_SESSION_HELPER"
const grokLaneDetachedChildEnv = "AGENT_SESSIONS_GROK_LANE_DETACHED_CHILD"
const grokToolWrapperExecHelperEnv = "AGENT_SESSIONS_GROK_TOOL_EXEC_HELPER"

func TestGrokToolWrapperExecHelper(_ *testing.T) {
	if os.Getenv(grokToolWrapperExecHelperEnv) != "1" {
		return
	}
	os.Exit(runGrokToolWrapper([]string{"-c", "printf TOOL_WRAPPER_EXEC_OK"}))
}

func TestGrokLaneLifecycleLockHolder(_ *testing.T) {
	if os.Getenv(grokLaneLockHolderEnv) != "1" {
		return
	}
	paths := resolveNativePaths()
	sessionID := os.Getenv("GROK_LANE_LOCK_SESSION")
	lock, err := lockLaneLifecycle(paths, "grok-"+sessionID)
	if err != nil {
		os.Exit(41)
	}
	state, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		unlockLaneLifecycle(lock)
		os.Exit(42)
	}
	state.ManagerPID = os.Getpid()
	state.ManagerProcStart = readProcStart(os.Getpid())
	if writeGrokLaneState(paths, state) != nil || os.WriteFile(os.Getenv("GROK_LANE_LOCK_READY"), []byte("ready\n"), 0o600) != nil {
		unlockLaneLifecycle(lock)
		os.Exit(43)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	unlockLaneLifecycle(lock)
}

func TestGrokLaneProcessSessionHelper(_ *testing.T) {
	if os.Getenv(grokLaneSessionHelperEnv) != "1" {
		return
	}
	child := exec.Command("sleep", "30")
	if os.Getenv(grokLaneDetachedChildEnv) == "1" {
		// Model Darwin's restricted executables: XNU exposes their argv but
		// deliberately omits the inherited environment from KERN_PROCARGS2.
		child.Env = []string{}
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := child.Start(); err != nil {
		os.Exit(51)
	}
	if _, err := fmt.Fprintf(os.Stdout, "%d\n", child.Process.Pid); err != nil {
		_ = child.Process.Kill()
		os.Exit(52)
	}
	if err := child.Wait(); err != nil {
		os.Exit(53)
	}
}

func TestParseGrokLaneArgsCommandContract(t *testing.T) {
	t.Parallel()

	if options, err := parseGrokLaneArgs(nil); err != nil || !options.help {
		t.Fatalf("empty argv = %#v, %v; want help", options, err)
	}
	if options, err := parseGrokLaneArgs([]string{"--help"}); err != nil || !options.help {
		t.Fatalf("help argv = %#v, %v; want help", options, err)
	}
	for _, command := range []string{"resume", "wait", "status", "interrupt", "archive"} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			options, err := parseGrokLaneArgs([]string{command, "lane-a"})
			if err != nil {
				t.Fatalf("parse %s: %v", command, err)
			}
			if options.command != command || options.target != "lane-a" {
				t.Fatalf("parse %s = %#v", command, options)
			}
		})
	}
}

func TestGrokLaneUsageAdvertisesGroupOptions(t *testing.T) {
	t.Parallel()

	usage := grokLaneUsage()
	for _, option := range []string{"--group GROUP", "--inherit-groups", "--no-inherit-groups"} {
		if !strings.Contains(usage, option) {
			t.Fatalf("Grok lane usage does not advertise %s", option)
		}
	}
}

func TestGrokMCPReadinessFailurePreservesRepeatedProtocolCause(t *testing.T) {
	t.Parallel()

	failures := grokMCPReadinessFailures{}
	rpcFailure := &grokRPCError{Code: -32603, Message: "Internal error", Data: "server 'agent_sessions' not found"}
	failures.record(grokMCPReadinessDiagnostic(rpcFailure))
	failures.record(grokMCPReadinessDiagnostic(rpcFailure))
	failures.record(fmt.Errorf("grok ACP _x.ai/sessions/list: %w", context.DeadlineExceeded))

	summary := failures.summary(context.DeadlineExceeded)
	if !strings.Contains(summary, "Grok ACP error -32603: Internal error") ||
		!strings.Contains(summary, "server 'agent_sessions' not found") ||
		!strings.Contains(summary, "repeated 2 times") ||
		!strings.Contains(summary, "context deadline exceeded") {
		t.Fatalf("Grok MCP readiness failure summary = %q", summary)
	}
}

func TestGrokLanePrivateACPErrorPreservesBoundedProtocolCause(t *testing.T) {
	t.Parallel()

	failure := grokLanePrivateACPError("open headless Grok lane session", &grokRPCError{
		Code: -32602, Message: "Invalid params",
		Data: "data did not match any variant of untagged enum McpServer",
	})
	message := failure.Error()
	if !strings.Contains(message, "open headless Grok lane session") ||
		!strings.Contains(message, "Grok ACP error -32602: Invalid params") ||
		!strings.Contains(message, "untagged enum McpServer") ||
		strings.Contains(message, "managed process join incomplete") {
		t.Fatalf("private Grok lane ACP failure = %q", message)
	}
}

func TestParseGrokLaneArgsRejectsUnknownOrMissingTarget(t *testing.T) {
	t.Parallel()

	if _, err := parseGrokLaneArgs([]string{"invent"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
	for _, command := range []string{"resume", "wait", "status", "interrupt", "archive"} {
		if _, err := parseGrokLaneArgs([]string{command}); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("missing %s target error = %v", command, err)
		}
	}
}

func TestDoctorGrokLaneReportsSuccessfulVersionProbe(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' 'grok 1.0.4 (test) [stable]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", bin)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "missing-supervisor.sock"))

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, doctorErr := doctorGrokLane()
	_ = write.Close()
	os.Stdout = original
	body, _ := io.ReadAll(read)
	_ = read.Close()
	if doctorErr != nil || code != 0 {
		t.Fatalf("doctor success = code %d err %v", code, doctorErr)
	}
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("decode doctor event %q: %v", body, err)
	}
	if event["grok_version"] != "grok 1.0.4 (test) [stable]" || event["grok_error"] != nil {
		t.Fatalf("doctor event = %#v", event)
	}
}

func TestAcknowledgeGrokLaneTurnDoesNotWriteBehindLiveManager(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))

	paths := resolveNativePaths()
	sessionID := randomID()
	turn := newGrokLaneTurn("terminal result")
	turn.Status, turn.Outcome, turn.Exit = "completed", "completed", 0
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "live-ack-test", SessionID: sessionID,
		Cwd: root, Status: "idle", ManagerPID: os.Getpid(), ManagerProcStart: readProcStart(os.Getpid()),
		ControlSocket: filepath.Join(root, "missing-control.sock"), PermissionMode: "bypassPermissions",
		Turns: []grokLaneTurn{turn}, TurnID: turn.ID, LatestTurnID: turn.ID,
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeGrokLaneTurn(paths, sessionID, turn.ID); err == nil || !strings.Contains(err.Error(), "acknowledge live") {
		t.Fatalf("live manager acknowledgement error = %v", err)
	}
	unchanged, err := readGrokLaneState(paths, sessionID)
	if err != nil || unchanged.Turns[0].Collected || unchanged.TurnID != turn.ID {
		t.Fatalf("acknowledgement wrote behind live manager: %+v, %v", unchanged, err)
	}
}

func TestGrokLaneCwdIsCanonicalAcrossDirectAndSymlinkPaths(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{realRoot, alias} {
		got, err := canonicalGrokLaneDirectory(requested)
		if err != nil || got != want {
			t.Fatalf("canonical Grok lane cwd for %q = %q, %v; want %q", requested, got, err, want)
		}
	}
}

func TestWaitGrokLaneReadyDistinguishesManagerExit(t *testing.T) {
	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	pid, start := process.Process.Pid, readProcStart(process.Process.Pid)
	_ = process.Process.Kill()
	_ = process.Wait()
	_, err := waitGrokLaneReady(resolveNativePaths(), randomID(), pid, start, 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "exited during startup") || !strings.Contains(err.Error(), "private manager log") {
		t.Fatalf("Grok lane manager exit diagnostic = %v", err)
	}
}

func TestGrokLaneInfrastructureFailuresUseFailedTerminalTaxonomy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	for _, reason := range []string{"manager startup failed", "persist active Grok lane turn failed", "Grok ACP worker exited"} {
		turn := newGrokLaneTurn("work")
		manager := &grokLaneManager{
			paths: paths, state: grokLaneState{SessionID: randomID(), Status: "starting", RuntimeDir: paths.runtimeDir, Turns: []grokLaneTurn{turn}},
			done: make(chan struct{}), persistOverride: func(grokLaneState) error { return nil },
		}
		manager.shutdown(reason, false)
		if manager.state.Turns[0].Status != "failed" || manager.state.Turns[0].Outcome != "failed" || manager.state.Turns[0].Exit != 1 {
			t.Fatalf("infrastructure failure %q taxonomy = %+v", reason, manager.state.Turns[0])
		}
	}
}

func TestGrokLaneStartupShutdownKeepsFirstSignalOrArchiveCause(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	for _, test := range []struct {
		name   string
		reason string
	}{
		{name: "signal", reason: "manager signalled: hangup"},
		{name: "archive", reason: "explicit archive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn := newGrokLaneTurn("work")
			startupDone := make(chan struct{})
			close(startupDone)
			manager := &grokLaneManager{
				paths: paths,
				state: grokLaneState{
					SessionID: randomID(), Status: "starting", RuntimeDir: paths.runtimeDir,
					Turns: []grokLaneTurn{turn},
				},
				done: make(chan struct{}), startupDone: startupDone,
				persistOverride: func(grokLaneState) error { return nil },
			}
			manager.beginShutdown(test.reason, true)

			// Reproduce the startup goroutine reaching its generic cleanup before
			// the original signal/archive goroutine gets cleanupOnce.
			manager.shutdown("manager startup failed", false)
			manager.shutdown(test.reason, true)

			got := manager.state.Turns[0]
			if got.Status != "interrupted" || got.Outcome != "interrupted" || got.Exit != 130 || got.Error != test.reason {
				t.Fatalf("first shutdown cause %q taxonomy = %+v", test.reason, got)
			}
			if err := manager.closingError("grok lane manager is closing before publication"); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("private closing diagnostic = %v; want cause %q", err, test.reason)
			}
		})
	}
}

func TestGrokLaneFinalOwnershipPersistFailureRemainsReconcileable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: randomID(), Name: "normalization-retry", Status: "idle",
		ManagerPID: 999999, ManagerProcStart: "stale-manager", RuntimeDir: paths.runtimeDir,
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	calls := 0
	manager := &grokLaneManager{paths: paths, state: state, done: make(chan struct{})}
	manager.persistOverride = func(candidate grokLaneState) error {
		calls++
		if calls == 2 {
			return errors.New("injected final normalization failure")
		}
		return writeGrokLaneState(paths, candidate)
	}
	manager.shutdown("explicit archive", true)
	archived, err := readGrokLaneState(paths, state.SessionID)
	if err != nil || grokLaneStateNormalized(archived) {
		t.Fatalf("failed final persist did not retain reconcileable ownership: %+v, %v", archived, err)
	}
	if err := waitGrokLaneArchived(paths, state.SessionID, 2*time.Second); err != nil {
		t.Fatalf("reconcile final ownership persist: %v", err)
	}
	archived, err = readGrokLaneState(paths, state.SessionID)
	if err != nil || !grokLaneStateNormalized(archived) {
		t.Fatalf("reconciled final ownership = %+v, %v", archived, err)
	}
}

func TestGrokLaneControlRequiresExactSessionAndTerminalAck(t *testing.T) {
	sessionID := randomID()
	turn := newGrokLaneTurn("queued")
	manager := &grokLaneManager{state: grokLaneState{SessionID: sessionID, Turns: []grokLaneTurn{turn}}}
	if _, err := manager.handleControl(map[string]any{"action": "archive"}); err == nil || !strings.Contains(err.Error(), "session mismatch") {
		t.Fatalf("missing control session error = %v", err)
	}
	if _, err := manager.handleControl(map[string]any{
		"action": "ack", "sessionId": sessionID, "turnId": turn.ID,
	}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("nonterminal acknowledgement error = %v", err)
	}
}

func TestGrokLaneWakeAndResumeRollbackFailedPersistence(t *testing.T) {
	sessionID := randomID()
	base := grokLaneState{
		Type: "grok-peer-lane", SessionID: sessionID, Status: "idle", Persistent: true,
		PermissionMode: "bypassPermissions", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	manager := &grokLaneManager{
		state: base, peer: &daemon{}, turnNotify: make(chan struct{}, 1), done: make(chan struct{}),
		persistOverride: func(grokLaneState) error { return errors.New("injected state write failure") },
	}
	item := map[string]any{"id": "persist-fail", "message": "hello"}
	if _, err := manager.queueWake(item); err == nil || !strings.Contains(err.Error(), "persist accepted") {
		t.Fatalf("queue wake persistence error = %v", err)
	}
	if len(manager.state.Turns) != 0 || manager.state.TurnID != "" || manager.state.LatestTurnID != "" {
		t.Fatalf("failed wake mutated manager state: %+v", manager.state)
	}

	turn := newGrokLaneTurn("resume")
	if _, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": sessionID, "turn": turn, "persistent": true,
	}); err == nil || !strings.Contains(err.Error(), "injected state write") {
		t.Fatalf("resume persistence error = %v", err)
	}
	if len(manager.state.Turns) != 0 || manager.state.TurnID != "" || manager.state.LatestTurnID != "" {
		t.Fatalf("failed resume mutated manager state: %+v", manager.state)
	}

	manager.persistOverride = func(grokLaneState) error { return nil }
	manager.closing = true
	result, err := manager.queueWake(map[string]any{"id": "closing-message", "message": "arrived at archive boundary"})
	if err != nil || stringValue(result["delivery"]) != "interrupted" {
		t.Fatalf("closing wake ownership = %#v, %v", result, err)
	}
	if len(manager.state.Turns) != 1 || manager.state.Turns[0].Status != "interrupted" ||
		!strings.Contains(manager.state.Turns[0].Prompt, "Message from an unidentified peer:") ||
		!strings.Contains(manager.state.Turns[0].Prompt, "arrived at archive boundary") ||
		strings.Contains(strings.ToLower(manager.state.Turns[0].Prompt), "trusted") {
		t.Fatalf("closing wake state/instruction = %+v", manager.state.Turns)
	}
}

type grokLaneFailingWriteCloser struct{}

func (grokLaneFailingWriteCloser) Write([]byte) (int, error) {
	return 0, errors.New("closed ACP stdin")
}
func (grokLaneFailingWriteCloser) Close() error { return nil }

func TestGrokLaneInterruptRollsBackLatchWhenCancelWriteFails(t *testing.T) {
	sessionID := randomID()
	manager := &grokLaneManager{
		state: grokLaneState{SessionID: sessionID, Status: "active", GrokSessionID: "native"},
		peer:  &daemon{}, client: &grokACPClient{stdin: grokLaneFailingWriteCloser{}}, activeTurnID: "turn-active",
	}
	if _, err := manager.handleControl(map[string]any{"action": "interrupt", "sessionId": sessionID}); err == nil {
		t.Fatal("interrupt unexpectedly succeeded through broken ACP stdin")
	}
	if manager.interruptedID != "" {
		t.Fatalf("failed interrupt retained latch %q", manager.interruptedID)
	}
}

func TestGrokLaneWakeTerminalDeliveryAndStatusAreTruthful(t *testing.T) {
	for _, test := range []struct {
		status, delivery string
		exit             int
	}{
		{status: "completed", delivery: "delivered"},
		{status: "failed", delivery: "failed", exit: 1},
		{status: "interrupted", delivery: "interrupted", exit: 130},
		{status: "timed_out", delivery: "timed_out", exit: 124},
	} {
		turn := grokLaneTurn{ID: "turn", MessageID: "message", Status: test.status, Outcome: test.status, Exit: test.exit}
		if got := grokLaneTurnDelivery(turn); got != test.delivery {
			t.Fatalf("delivery for %s = %q, want %q", test.status, got, test.delivery)
		}
		state := grokLaneState{SessionID: "session", LatestTurnID: turn.ID, Turns: []grokLaneTurn{turn}}
		event := grokLaneStatusEvent(state)
		if stringValue(event["turn_status"]) != test.status || stringValue(event["outcome"]) != test.status || intValue(event["exit"]) != test.exit {
			t.Fatalf("status for %s = %#v", test.status, event)
		}
	}
}

func TestGrokLaneTerminalNoticeIsDurablePointerAndCollectionCancelsPending(t *testing.T) {
	turn := grokLaneTurn{ID: "turn-1", Status: "completed", Outcome: "completed"}
	state := grokLaneState{Name: "worker", SessionID: "session-1", NotifyTarget: "session:owner"}
	queueGrokLaneTerminalNotice(&state, turn)
	queueGrokLaneTerminalNotice(&state, turn)
	if len(state.Notices) != 1 || !strings.Contains(state.Notices[0].Message, "GROK_LANE_TERMINAL") ||
		!strings.Contains(state.Notices[0].Message, "collection=required") ||
		!strings.Contains(state.Notices[0].Message, "grok-peer-lane wait session-1") {
		t.Fatalf("terminal notice = %+v", state.Notices)
	}
	manager := &grokLaneManager{state: state}
	manager.cancelTerminalNoticeLocked(turn.ID)
	if manager.state.Notices[0].SentAt == 0 {
		t.Fatal("collection did not cancel pending terminal notice")
	}
}

func TestGrokLaneTerminalNoticeFramePreservesGrokProduct(t *testing.T) {
	state := grokLaneState{SessionID: randomID(), Name: "grok-lane", PermissionMode: "bypassPermissions"}
	frame := createGrokLaneNoticeFrame(grokLaneVirtualSender(state), "notice-id", "terminal pointer")
	message, _ := frame["message"].(map[string]any)
	envelope := parsePeerMessage(stringValue(message["content"]))
	if envelope.FromProduct != "grok" || envelope.FromSession != state.SessionID {
		t.Fatalf("Grok lane terminal notice envelope = %+v", envelope)
	}
	if stringValue(frame["msg_id"]) != "notice-id" || envelope.MessageID != "notice-id" {
		t.Fatalf("Grok lane terminal notice did not retain its deduplication identity: %#v, %+v", frame, envelope)
	}
}

func TestGrokLaneMaintenanceRetriesTerminalNotice(t *testing.T) {
	runtimeDir := useBridgeTestAgent(t)
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "worker", SessionID: "grok-notice-retry", Status: "idle", Persistent: true,
		NotifyTarget: "session:target-session",
		Notices: []claudeLaneNotice{{
			ID: "notice-retry", TurnID: "turn-1", Target: "session:target-session", Message: "terminal", CreatedAt: 1,
		}},
	}
	_, stopParent := registerBridgeTestParent(t, runtimeDir)
	prepareBridgeTestLaneParentForProduct(t, runtimeDir, state.SessionID, "target-session", "grok")
	stopParent()
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &grokLaneManager{paths: paths, state: state, done: make(chan struct{})}
	manager.flushTerminalNotices()
	latest, err := readGrokLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Notices[0].SentAt != 0 || latest.Notices[0].Attempts == 0 {
		t.Fatalf("failed first delivery was not retained for retry: %+v", latest.Notices[0])
	}
	received, _ := registerBridgeTestParent(t, runtimeDir)
	go manager.maintenanceLoop()
	defer close(manager.done)
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("live Grok manager did not retry its terminal notice")
	}
	if !waitForCondition(time.Second, func() bool {
		latest, readErr := readGrokLaneState(paths, state.SessionID)
		return readErr == nil && latest.Notices[0].SentAt != 0
	}) {
		t.Fatal("retried Grok terminal notice was not acknowledged durably")
	}
}

func TestCleanupGrokLanePreservesReusedPIDBackendAndRejectsMalformedOwnership(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	if err := ensurePrivateRuntimeDir(bridgeRuntimeRoot(paths.runtimeDir, os.Getuid())); err != nil {
		t.Fatal(err)
	}
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: randomID(), ManagerPID: os.Getpid(), ManagerProcStart: "different-start",
		RuntimeDir: paths.runtimeDir, LaunchTokenHash: strings.Repeat("a", 64),
	}
	backend := filepath.Join(bridgeRuntimeRoot(paths.runtimeDir, os.Getuid()), fmt.Sprintf("%d.sock", os.Getpid()))
	if err := os.WriteFile(backend, []byte("unrelated reused-pid owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupGrokLaneOwnedFiles(paths, state, 0); err != nil {
		t.Fatalf("cleanup around reused PID: %v", err)
	}
	if body, err := os.ReadFile(backend); err != nil || string(body) != "unrelated reused-pid owner" {
		t.Fatalf("reused PID backend was changed: %q, %v", body, err)
	}

	state.SessionID = randomID()
	launchPath := grokLaunchRecordPath(paths, state.SessionID)
	if err := os.MkdirAll(filepath.Dir(launchPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupGrokLaneOwnedFiles(paths, state, 0); err == nil || !strings.Contains(err.Error(), "launch ownership remains") {
		t.Fatalf("malformed launch cleanup error = %v", err)
	}
}

func TestStopGrokProcessSessionRemovesWorkerAndSeparateChildren(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestGrokLaneProcessSessionHelper$")
	command.Env = append(os.Environ(), grokLaneSessionHelperEnv+"=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var childPID int
	if _, err := fmt.Fscan(stdout, &childPID); err != nil {
		t.Fatalf("read separate process-group child PID: %v", err)
	}
	workerPGID, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatal(err)
	}
	workerSID, err := unix.Getsid(pid)
	if err != nil {
		t.Fatal(err)
	}
	childSID, err := unix.Getsid(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if workerPGID == childPGID || workerSID != pid || childSID != pid {
		t.Fatalf("process topology worker pid/pgid/sid=%d/%d/%d child=%d/%d/%d", pid, workerPGID, workerSID, childPID, childPGID, childSID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		members, err := grokProcessSessionMembers(pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Grok process session %d never exposed worker child: %+v", pid, members)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stopGrokProcessSession(pid, "different-start-identity", 0); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("reused Grok process session cleanup error = %v", err)
	}
	if !exactProcessIdentityMatch(pid, readProcStart(pid)) || !exactProcessIdentityMatch(childPID, readProcStart(childPID)) {
		t.Fatal("mismatched Grok process-session identity terminated unrelated live processes")
	}
	if err := stopGrokProcessSession(pid, readProcStart(pid), 0); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	waited = true
	if grokProcessSessionHasMembers(pid, 0) {
		t.Fatalf("Grok process session %d survived cleanup", pid)
	}
}

func TestGrokProcessSessionHasMembersRejectsUnsetAuthority(t *testing.T) {
	for _, sessionID := range []int{-1, 0, 1} {
		if grokProcessSessionHasMembers(sessionID, 0) {
			t.Fatalf("unset process-session authority %d reported live members", sessionID)
		}
	}
}

func TestStopGrokTaggedProcessesRemovesDetachedLaunchChildren(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	launchToken := randomID() + randomID()
	tokenHash := grokTokenHash(launchToken)
	command := exec.Command(os.Args[0], "-test.run=^TestGrokLaneProcessSessionHelper$")
	command.Env = append(os.Environ(),
		grokLaneSessionHelperEnv+"=1",
		grokLaneDetachedChildEnv+"=1",
		grokLaunchTokenEnv+"="+launchToken,
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var childPID int
	if _, err := fmt.Fscan(stdout, &childPID); err != nil {
		t.Fatalf("read detached child PID: %v", err)
	}
	parentSID, parentErr := unix.Getsid(pid)
	childSID, childErr := unix.Getsid(childPID)
	if parentErr != nil || childErr != nil || parentSID != pid || childSID != childPID {
		t.Fatalf("detached topology parent=%d/%d child=%d/%d errors=%v/%v", pid, parentSID, childPID, childSID, parentErr, childErr)
	}
	workerRoot := grokSessionMember{PID: pid, ProcStart: readProcStart(pid)}
	if workerRoot.ProcStart == "" {
		t.Fatal("tagged Grok ownership root has no process-start identity")
	}
	var members []grokSessionMember
	deadline := time.Now().Add(2 * time.Second)
	for {
		members, err = grokTaggedProcessMembers(tokenHash, workerRoot)
		if err != nil {
			t.Fatalf("enumerate tagged Grok members: %v", err)
		}
		parentFound, childFound := false, false
		for _, member := range members {
			parentFound = parentFound || member.PID == pid
			childFound = childFound || member.PID == childPID
		}
		if parentFound && childFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tagged Grok members = %+v; want parent %d and detached child %d", members, pid, childPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "tagged-install-inventory", SessionID: randomID(), Status: "archived",
		LaunchTokenHash: tokenHash, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	paths := resolveNativePaths()
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if live, err := activeGrokLaunchSessions(paths); err != nil || !reflect.DeepEqual(live, []string{state.SessionID}) {
		t.Fatalf("tagged Grok install inventory = %v, %v", live, err)
	}
	if err := stopGrokTaggedProcesses(tokenHash, 0, workerRoot); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	waited = true
	if grokTaggedProcessesRemain(tokenHash, 0, workerRoot) || exactProcessIdentityMatch(childPID, readProcStart(childPID)) {
		t.Fatal("detached tagged Grok child survived cleanup")
	}
}

func TestWaitGrokLaneReconcilesCrashedManagerAndCollectsDebt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	turn := newGrokLaneTurn("manager crash")
	turn.Status, turn.StartedAt = "active", time.Now().UnixMilli()
	now := time.Now().UnixMilli()
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "crashed-manager", SessionID: randomID(), Cwd: root, Status: "active",
		ManagerPID: 999999, ManagerProcStart: "stale-manager", RuntimeDir: paths.runtimeDir,
		PermissionMode: "bypassPermissions", Persistent: true, Turns: []grokLaneTurn{turn},
		TurnID: turn.ID, LatestTurnID: turn.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	code, err := waitGrokLane(grokLaneOptions{laneCommonOptions: laneCommonOptions{target: state.SessionID, timeout: 2 * time.Second}})
	if err != nil || code != 130 {
		t.Fatalf("collect crashed Grok lane = %d, %v", code, err)
	}
	archived, err := readGrokLaneState(paths, state.SessionID)
	if err != nil || archived.Status != "archived" || archived.ManagerPID != 0 ||
		archived.Turns[0].Status != "interrupted" || !archived.Turns[0].Collected {
		t.Fatalf("reconciled crashed Grok lane = %+v, %v", archived, err)
	}
}

func TestStoppedDaemonRefusesGrokLaneInboxResurrection(t *testing.T) {
	pending := filepath.Join(t.TempDir(), "session", "inbox", "pending")
	daemon := &daemon{pendingDir: pending, stopped: true}
	if err := daemon.enqueue(map[string]any{"id": "late-frame", "message": "late"}); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("stopped daemon enqueue error = %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(filepath.Dir(pending))); !os.IsNotExist(err) {
		t.Fatalf("stopped daemon resurrected session directory: %v", err)
	}
}

func TestGrokLaneShutdownDrainsAcceptedPeerFrameIntoTerminalOwnership(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_YOLO", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestGrokFakeProcess -- \"$@\"\n", shellQuoteForTest(executable))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	manager, paths, state, _ := startTestGrokLaneManager(t, root, true, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	manager.mu.Lock()
	peer := manager.peer
	peer.handleBeforeFrame = func() {
		close(entered)
		<-release
	}
	socket := peer.stableSocket
	manager.mu.Unlock()
	frame := map[string]any{
		"type": "user", "session_id": state.SessionID, "msg_id": "archive-boundary-frame",
		"from": "peer-test", "message": map[string]any{"content": "accepted before archive"},
	}
	if err := sendUnixJSON(socket, frame, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("peer handler did not accept boundary frame")
	}
	shutdownDone := make(chan struct{})
	go func() {
		manager.shutdown("explicit archive", true)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("archive completed before accepted peer frame drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(8 * time.Second):
		t.Fatal("archive did not complete after peer frame drained")
	}
	archived, err := readGrokLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, turn := range archived.Turns {
		if turn.MessageID == "archive-boundary-frame" {
			found = turn.Status == "interrupted"
		}
	}
	if !found || archived.ManagerPID != 0 || archived.WorkerPID != 0 {
		t.Fatalf("accepted boundary frame was not terminally owned before cleanup: %+v", archived)
	}
	if _, err := os.Lstat(filepath.Join(paths.dataRoot, "sessions", sessionKey(state.SessionID), "inbox")); !os.IsNotExist(err) {
		t.Fatalf("archive left fallback inbox residue: %v", err)
	}
}

func TestGrokLaneManagerLifecycleAndPeerWake(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_YOLO", "1")
	t.Setenv("GROK_FAKE_ANSWER", "LANE-ANSWER")
	t.Setenv("GROK_FAKE_GENERATED_SESSION_ID", "native-grok-session")
	t.Setenv("GROK_FAKE_REQUIRE_AGENT_SESSIONS_MCP", "1")
	recordPath := filepath.Join(root, "fake.jsonl")
	t.Setenv("GROK_FAKE_RECORD", recordPath)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestGrokFakeProcess -- \"$@\"\n", shellQuoteForTest(executable))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	paths := resolveNativePaths()
	sessionID := randomID()
	launchToken := randomID() + randomID()
	hostPaths := grokRuntimePaths(paths.runtimeDir, os.Getuid(), launchToken)
	now := time.Now().UnixMilli()
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "grok-lane-test", SessionID: sessionID,
		Cwd: root, Status: "starting", ControlSocket: hostPaths.ControlSocket,
		ManagerLog: filepath.Join(root, "manager.log"), LaunchTokenHash: grokTokenHash(launchToken),
		RuntimeDir: paths.runtimeDir, Persistent: true, AutoArchive: false,
		PermissionMode: "bypassPermissions", Turns: []grokLaneTurn{newGrokLaneTurn("first turn")},
		CreatedAt: now, UpdatedAt: now,
	}
	state.TurnID, state.LatestTurnID = state.Turns[0].ID, state.Turns[0].ID
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &grokLaneManager{
		paths: paths, state: state, launchToken: launchToken,
		turnNotify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := manager.start(); err != nil {
		t.Fatalf("start Grok lane manager: %v", err)
	}
	defer manager.shutdown("test cleanup", true)

	waitForGrokLaneTurn(t, paths, sessionID, state.TurnID, "completed")
	current, err := readGrokLaneState(paths, sessionID)
	if err != nil || current.Turns[0].Result != "LANE-ANSWER" || current.MessagingSocket == "" {
		t.Fatalf("completed Grok lane state = %+v, %v", current, err)
	}
	if current.GrokSessionID != "native-grok-session" || current.GrokSessionID == current.SessionID {
		t.Fatalf("native/bridge Grok identities were not separated: %+v", current)
	}
	status, err := requestControl(current.ControlSocket, map[string]any{
		"action": "status", "sessionId": sessionID, "launchToken": launchToken,
	}, time.Second)
	ready, _ := status["ready"].(bool)
	if err != nil || stringValue(status["permissionMode"]) != "bypassPermissions" || !ready {
		t.Fatalf("Grok lane status = %#v, %v", status, err)
	}
	firstTurnID := current.Turns[0].ID
	if err := acknowledgeGrokLaneTurn(paths, sessionID, firstTurnID); err != nil {
		t.Fatalf("acknowledge first Grok lane turn: %v", err)
	}
	wake, err := requestControl(current.ControlSocket, map[string]any{
		"action": "wake", "sessionId": sessionID, "launchToken": launchToken,
		"item": map[string]any{"id": "peer-message-1", "message": "reply to the peer"},
	}, time.Second)
	if err != nil || stringValue(wake["delivery"]) != "accepted" {
		t.Fatalf("queue Grok lane peer wake = %#v, %v", wake, err)
	}
	duplicate, err := requestControl(current.ControlSocket, map[string]any{
		"action": "wake", "sessionId": sessionID, "launchToken": launchToken,
		"item": map[string]any{"id": "peer-message-1", "message": "reply to the peer"},
	}, time.Second)
	if err != nil || stringValue(duplicate["delivery"]) == "conflict" {
		t.Fatalf("duplicate Grok lane peer wake = %#v, %v", duplicate, err)
	}
	conflict, err := requestControl(current.ControlSocket, map[string]any{
		"action": "wake", "sessionId": sessionID, "launchToken": launchToken,
		"item": map[string]any{"id": "peer-message-1", "message": "different body"},
	}, time.Second)
	if err != nil || stringValue(conflict["delivery"]) != "conflict" {
		t.Fatalf("conflicting Grok lane peer wake = %#v, %v", conflict, err)
	}
	waitForGrokLaneMessage(t, paths, sessionID, "peer-message-1", "completed")
	current, err = readGrokLaneState(paths, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Turns[0].Collected || current.Turns[0].ID != firstTurnID {
		t.Fatalf("later peer wake resurrected collected first turn: %+v", current.Turns)
	}
	for _, turn := range current.Turns {
		if turn.Collected {
			continue
		}
		if err := acknowledgeGrokLaneTurn(paths, sessionID, turn.ID); err != nil {
			t.Fatalf("acknowledge Grok lane turn %s: %v", turn.ID, err)
		}
	}

	manager.shutdown("explicit archive", true)
	archived, err := readGrokLaneState(paths, sessionID)
	if err != nil || archived.Status != "archived" || archived.ManagerPID != 0 || archived.WorkerPID != 0 {
		t.Fatalf("archived Grok lane state = %+v, %v", archived, err)
	}
	if _, err := os.Lstat(hostPaths.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("Grok lane control socket survived archive: %v", err)
	}
	if record := readGrokLaunchRecord(grokLaunchRecordPath(paths, sessionID)); record != nil {
		t.Fatalf("Grok lane launch record survived archive: %+v", record)
	}

	resumedToken := randomID() + randomID()
	resumedPaths := grokRuntimePaths(paths.runtimeDir, os.Getuid(), resumedToken)
	resumed := archived
	resumed.Status, resumed.ControlSocket = "starting", resumedPaths.ControlSocket
	resumed.LaunchTokenHash = grokTokenHash(resumedToken)
	resumed.ManagerPID, resumed.ManagerProcStart, resumed.WorkerPID, resumed.WorkerProcStart = 0, "", 0, ""
	resumed.MessagingSocket = ""
	resumedTurn := newGrokLaneTurn("resumed turn")
	resumed.Turns = append(resumed.Turns, resumedTurn)
	resumed.TurnID, resumed.LatestTurnID = resumedTurn.ID, resumedTurn.ID
	if err := writeGrokLaneState(paths, resumed); err != nil {
		t.Fatal(err)
	}
	second := &grokLaneManager{
		paths: paths, state: resumed, launchToken: resumedToken,
		turnNotify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := second.start(); err != nil {
		t.Fatalf("resume Grok lane manager: %v", err)
	}
	defer second.shutdown("test cleanup", true)
	waitForGrokLaneTurn(t, paths, sessionID, resumedTurn.ID, "completed")
	if counts := grokFakeMethodCounts(t, recordPath); counts["session/new"] != 1 || counts["session/load"] != 1 {
		t.Fatalf("Grok lane session lifecycle calls = %#v; want one new and one load", counts)
	}
	second.shutdown("resumed lane archived", true)
	resumedArchived, err := readGrokLaneState(paths, sessionID)
	if err != nil || resumedArchived.Status != "archived" || resumedArchived.SessionID != sessionID {
		t.Fatalf("resumed Grok lane archive = %+v, %v", resumedArchived, err)
	}
	if err := acknowledgeGrokLaneTurn(paths, sessionID, resumedTurn.ID); err != nil {
		t.Fatalf("acknowledge archived Grok lane turn: %v", err)
	}
	collectedArchived, err := readGrokLaneState(paths, sessionID)
	if err != nil || !collectedArchived.Turns[len(collectedArchived.Turns)-1].Collected || collectedArchived.TurnID != "" {
		t.Fatalf("archived Grok lane acknowledgement = %+v, %v", collectedArchived, err)
	}
}

func TestGrokLaneManagerInterruptsAndAutoArchives(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_YOLO", "1")
	t.Setenv("GROK_FAKE_PROMPT_DELAY_MS", "500")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestGrokFakeProcess -- \"$@\"\n", shellQuoteForTest(executable))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	manager, paths, state, launchToken := startTestGrokLaneManager(t, root, true, 50*time.Millisecond)
	defer manager.shutdown("test cleanup", true)
	waitForGrokLaneTurn(t, paths, state.SessionID, state.TurnID, "active")
	response, err := requestControl(state.ControlSocket, map[string]any{
		"action": "interrupt", "sessionId": state.SessionID,
	}, time.Second)
	if err != nil || stringValue(response["turnId"]) != state.TurnID {
		t.Fatalf("interrupt Grok lane = %#v, %v", response, err)
	}
	waitForGrokLaneTurn(t, paths, state.SessionID, state.TurnID, "interrupted")
	if _, err := requestControl(state.ControlSocket, map[string]any{
		"action": "status", "sessionId": state.SessionID, "launchToken": launchToken,
	}, time.Second); err != nil {
		t.Fatalf("status after interrupt: %v", err)
	}
	if err := waitGrokLaneArchived(paths, state.SessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	archived, err := readGrokLaneState(paths, state.SessionID)
	if err != nil || archived.WorkerPID != 0 || archived.ManagerPID != 0 {
		t.Fatalf("auto-archived Grok lane = %+v, %v", archived, err)
	}
}

func TestGrokLaneShutdownCannotBeOverwrittenByActiveTurnCompletion(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_YOLO", "1")
	t.Setenv("GROK_FAKE_PROMPT_DELAY_MS", "500")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestGrokFakeProcess -- \"$@\"\n", shellQuoteForTest(executable))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	manager, paths, state, _ := startTestGrokLaneManager(t, root, false, 0)
	waitForGrokLaneTurn(t, paths, state.SessionID, state.TurnID, "active")
	manager.shutdown("explicit archive", true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		active := manager.activeTurnID
		manager.mu.Unlock()
		if active == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	archived, err := readGrokLaneState(paths, state.SessionID)
	if err != nil || archived.Status != "archived" || archived.Turns[0].Status != "interrupted" ||
		archived.ManagerPID != 0 || archived.WorkerPID != 0 {
		t.Fatalf("active completion overwrote Grok lane shutdown: %+v, %v", archived, err)
	}
}

func TestGrokLaneManagerArchivesWhenOwnerExits(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_YOLO", "1")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestGrokFakeProcess -- \"$@\"\n", shellQuoteForTest(executable))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerStart := readProcStart(owner.Process.Pid)
	if ownerStart == "" {
		_ = owner.Process.Kill()
		_ = owner.Wait()
		t.Fatal("owner process has no start identity")
	}
	manager, paths, state, _ := startTestGrokLaneManagerWithOwner(t, root, false, 0, owner.Process.Pid, ownerStart)
	defer manager.shutdown("test cleanup", true)
	waitForGrokLaneTurn(t, paths, state.SessionID, state.TurnID, "completed")
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if err := waitGrokLaneArchived(paths, state.SessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestForceArchiveGrokLaneDoesNotBlockOnLiveManagerLock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	sessionID := randomID()
	now := time.Now().UnixMilli()
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "force-archive", SessionID: sessionID, Cwd: root,
		Status: "idle", RuntimeDir: paths.runtimeDir, Persistent: true,
		PermissionMode: "bypassPermissions", LaunchTokenHash: strings.Repeat("a", 64),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	helper := exec.Command(os.Args[0], "-test.run=^TestGrokLaneLifecycleLockHolder$")
	helper.Env = append(os.Environ(),
		grokLaneLockHolderEnv+"=1", "GROK_LANE_LOCK_SESSION="+sessionID, "GROK_LANE_LOCK_READY="+ready,
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
	}()
	waitForGrokLaneFile(t, ready)
	if err := forceArchiveGrokLane(paths, sessionID, "test forced archive"); err != nil {
		t.Fatalf("force archive behind manager lifecycle lock: %v", err)
	}
	_ = helper.Wait()
	waited = true
	archived, err := readGrokLaneState(paths, sessionID)
	if err != nil || archived.Status != "archived" || archived.ManagerPID != 0 {
		t.Fatalf("forced archived Grok lane = %+v, %v", archived, err)
	}
}

func startTestGrokLaneManager(t *testing.T, root string, autoArchive bool, delay time.Duration) (*grokLaneManager, nativePaths, grokLaneState, string) {
	t.Helper()
	return startTestGrokLaneManagerWithOwner(t, root, autoArchive, delay, 0, "")
}

func startTestGrokLaneManagerWithOwner(t *testing.T, root string, autoArchive bool, delay time.Duration, ownerPID int, ownerStart string) (*grokLaneManager, nativePaths, grokLaneState, string) {
	t.Helper()
	paths := resolveNativePaths()
	sessionID := randomID()
	launchToken := randomID() + randomID()
	hostPaths := grokRuntimePaths(paths.runtimeDir, os.Getuid(), launchToken)
	turn := newGrokLaneTurn("managed turn")
	now := time.Now().UnixMilli()
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "grok-lane-" + first8(sessionID), SessionID: sessionID,
		Cwd: root, Status: "starting", ControlSocket: hostPaths.ControlSocket,
		ManagerLog: filepath.Join(root, "manager.log"), LaunchTokenHash: grokTokenHash(launchToken),
		RuntimeDir: paths.runtimeDir, Persistent: ownerPID == 0, OwnerPID: ownerPID, OwnerProcStart: ownerStart,
		AutoArchive: autoArchive, AutoArchiveDelayMS: delay.Milliseconds(), PermissionMode: "bypassPermissions",
		Turns: []grokLaneTurn{turn}, TurnID: turn.ID, LatestTurnID: turn.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &grokLaneManager{
		paths: paths, state: state, launchToken: launchToken,
		turnNotify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := manager.start(); err != nil {
		t.Fatalf("start test Grok lane manager: %v", err)
	}
	return manager, paths, state, launchToken
}

func grokFakeMethodCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	counts := map[string]int{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil || stringValue(row["kind"]) != "request" {
			continue
		}
		request, _ := row["request"].(map[string]any)
		counts[stringValue(request["method"])]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func waitForGrokLaneTurn(t *testing.T, paths nativePaths, sessionID, turnID, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := readGrokLaneState(paths, sessionID)
		if err == nil {
			for _, turn := range state.Turns {
				if turn.ID == turnID && turn.Status == status {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Grok lane turn %s did not reach %s", turnID, status)
}

func waitForGrokLaneMessage(t *testing.T, paths nativePaths, sessionID, messageID, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := readGrokLaneState(paths, sessionID)
		if err == nil {
			for _, turn := range state.Turns {
				if turn.MessageID == messageID && turn.Status == status {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Grok lane peer message %s did not reach %s", messageID, status)
}

func waitForGrokLaneFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file did not appear: %s", path)
}

func TestGrokToolRegistrySerializesRegistrationWithArchive(t *testing.T) {
	launchToken := randomID() + randomID()
	sessionID := "11111111-2222-4333-8444-555555555555"
	runtimeDir := t.TempDir()
	managerInfo := procinfo.Read(os.Getpid())
	if managerInfo.Status != procinfo.Known || managerInfo.Start == "" || managerInfo.StrongStart == "" {
		t.Fatal("current process has no strong identity")
	}
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: sessionID, Status: "idle",
		ManagerPID: os.Getpid(), ManagerProcStart: managerInfo.Start, ManagerStrongStart: managerInfo.StrongStart,
		WorkerPID: os.Getpid(), WorkerProcStart: managerInfo.Start, WorkerStrongStart: managerInfo.StrongStart,
		LaunchTokenHash: grokTokenHash(launchToken), RuntimeDir: runtimeDir,
		ToolRegistryVersion: grokToolRegistryVersion, ToolShellName: "bash", ToolRealShell: "/bin/bash",
	}
	host := grokRuntimePaths(runtimeDir, os.Getuid(), launchToken)
	if err := os.MkdirAll(filepath.Join(host.LaunchDir, "tool-roots"), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(host.LaunchDir, "tool-register.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	statePath := grokLaneStatePath(resolveNativePaths(), sessionID)
	if err := writeJSONAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}
	ledgerConfig, err := grokToolRootLedgerConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareToolRootLedger(ledgerConfig); err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(host.LaunchDir, "bash")
	t.Setenv(grokToolWrapperModeEnv, "1")
	t.Setenv(grokToolRealShellEnv, state.ToolRealShell)
	t.Setenv(grokToolWrapperPathEnv, wrapperPath)
	t.Setenv(grokLaunchTokenEnv, launchToken)
	t.Setenv(grokSessionIDEnv, sessionID)
	originalArgv0 := os.Args[0]
	os.Args[0] = wrapperPath
	defer func() { os.Args[0] = originalArgv0 }()
	if err := registerGrokToolRoot(); err != nil {
		t.Fatalf("register tool root: %v", err)
	}
	guard, roots, err := grokLaneCleanupRoots(state, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].PID != os.Getpid() || roots[0].StrongStart != managerInfo.StrongStart {
		t.Fatalf("tool roots = %+v", roots)
	}
	guard.close()

	exclusive, _, err := grokLaneCleanupRoots(state, true)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- registerGrokToolRoot() }()
	select {
	case err := <-result:
		exclusive.close()
		t.Fatalf("registration bypassed cleanup lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	state.Status = "archived"
	if err := writeJSONAtomic(statePath, state); err != nil {
		exclusive.close()
		t.Fatal(err)
	}
	exclusive.close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("post-archive tool registration succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked tool registration did not finish")
	}
}

func TestGrokProcessStrongIdentityRejectsSameDisplayStart(t *testing.T) {
	info := procinfo.Read(os.Getpid())
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		t.Fatal("current process has no strong identity")
	}
	member := grokSessionMember{PID: os.Getpid(), ProcStart: info.Start, StrongStart: info.StrongStart + "-reused"}
	if status := grokProcessIdentityStatus(member); status != processIdentityStale {
		t.Fatalf("strong identity mismatch status = %v; want stale", status)
	}
}

func TestGrokToolRegistryIntentPrecedesArtifacts(t *testing.T) {
	launchToken := randomID() + randomID()
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: "12345678-1234-4234-8234-123456789abc",
		Status: "starting", LaunchTokenHash: grokTokenHash(launchToken), RuntimeDir: t.TempDir(),
	}
	host := grokRuntimePaths(state.RuntimeDir, os.Getuid(), launchToken)
	if err := os.MkdirAll(host.LaunchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop after durable intent")
	manager := &grokLaneManager{state: state, hostPaths: host}
	manager.persistOverride = func(candidate grokLaneState) error {
		if candidate.ToolRegistryVersion != grokToolRegistryVersion || candidate.ToolShellName == "" || candidate.ToolRealShell == "" {
			t.Fatalf("incomplete registry intent: %+v", candidate)
		}
		paths, pathErr := grokToolRegistryPathsForState(candidate)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		for _, path := range []string{paths.lock, paths.roots, paths.wrapper} {
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Fatalf("registry artifact preceded durable intent: %s (%v)", path, statErr)
			}
		}
		return sentinel
	}
	if err := manager.prepareToolRegistry(); !errors.Is(err, sentinel) {
		t.Fatalf("prepare error = %v; want sentinel", err)
	}
}

func TestGrokToolRegistryMissingLockIsRecoverableOnlyBeforeArtifacts(t *testing.T) {
	launchToken := randomID() + randomID()
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: "87654321-4321-4321-8321-cba987654321",
		Status: "starting", LaunchTokenHash: grokTokenHash(launchToken), RuntimeDir: t.TempDir(),
		ToolRegistryVersion: grokToolRegistryVersion, ToolShellName: "bash", ToolRealShell: "/bin/bash",
	}
	paths, err := grokToolRegistryPathsForState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.lock), 0o700); err != nil {
		t.Fatal(err)
	}
	guard, err := lockGrokToolRegistry(state, true)
	if err != nil {
		t.Fatalf("intent-only registry should be recoverable: %v", err)
	}
	guard.close()
	if err := os.Mkdir(paths.roots, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lockGrokToolRegistry(state, true); err == nil {
		t.Fatal("registry artifacts without their admission lock were accepted")
	}
}

func TestGrokToolWrapperExecutesRealShellAndRegistersIdentity(t *testing.T) {
	launchToken := randomID() + randomID()
	sessionID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	runtimeDir := t.TempDir()
	managerInfo := procinfo.Read(os.Getpid())
	realShell, err := selectGrokLaneRealShell(os.Environ())
	if err != nil || managerInfo.Status != procinfo.Known {
		t.Fatalf("prepare wrapper fixture: shell=%q err=%v manager=%+v", realShell, err, managerInfo)
	}
	state := grokLaneState{
		Type: "grok-peer-lane", SessionID: sessionID, Status: "idle",
		ManagerPID: os.Getpid(), ManagerProcStart: managerInfo.Start, ManagerStrongStart: managerInfo.StrongStart,
		WorkerPID: os.Getpid(), WorkerProcStart: managerInfo.Start, WorkerStrongStart: managerInfo.StrongStart,
		LaunchTokenHash: grokTokenHash(launchToken), RuntimeDir: runtimeDir,
		ToolRegistryVersion: grokToolRegistryVersion, ToolShellName: filepath.Base(realShell), ToolRealShell: realShell,
	}
	host := grokRuntimePaths(runtimeDir, os.Getuid(), launchToken)
	if err := os.MkdirAll(filepath.Join(host.LaunchDir, "tool-roots"), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(host.LaunchDir, "tool-register.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(host.LaunchDir, filepath.Base(realShell))
	if err := os.Symlink(executable, wrapperPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	statePath := grokLaneStatePath(resolveNativePaths(), sessionID)
	if err := writeJSONAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}
	ledgerConfig, err := grokToolRootLedgerConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareToolRootLedger(ledgerConfig); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapperPath, "-test.run=^TestGrokToolWrapperExecHelper$")
	command.Args[0] = filepath.Base(wrapperPath)
	command.Env = grokLaneWorkerEnvironment(os.Environ(), launchToken, state, wrapperPath, realShell)
	command.Env = replaceTestEnvironment(command.Env, grokToolWrapperExecHelperEnv, "1")
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "TOOL_WRAPPER_EXEC_OK" {
		t.Fatalf("wrapper subprocess: err=%v output=%q", err, output)
	}
	ledger, err := openToolRootLedger(ledgerConfig)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ledger.snapshot()
	if err != nil || len(snapshot.Roots) != 1 || snapshot.Roots[0].PID != command.ProcessState.Pid() || snapshot.Roots[0].StrongStart == "" {
		t.Fatalf("wrapper ledger: err=%v snapshot=%+v", err, snapshot)
	}
}

func replaceTestEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
