package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

func TestLaneTerminalNoticeBodyGatesStructuredCollectionHint(t *testing.T) {
	actor := laneActor{id: "lane", product: "codex", name: "worker", turnID: "turn", outcome: "completed"}
	required := laneTerminalNoticeBody(actor, "remote", true)
	if !strings.Contains(required, "collection=required") ||
		!strings.Contains(required, "Collection hint: agent_sessions.lane host=remote product=codex command=wait arguments=[\"lane\"]") ||
		strings.Contains(required, "codex-peer-lane") {
		t.Fatalf("required notice = %q", required)
	}
	collected := laneTerminalNoticeBody(actor, "remote", false)
	if !strings.Contains(collected, "collection=not_required") || strings.Contains(collected, "Collection hint:") {
		t.Fatalf("collected notice = %q", collected)
	}
}

func TestLaneTerminalNoticeUsesCommittedDurableFailureOutcome(t *testing.T) {
	actor := laneActor{id: "lane", product: "qwen", name: "worker", turnID: "turn", outcome: "completed", result: "stale success"}
	snapshot := daemonpkg.StateSnapshot{Catalog: daemonpkg.Catalog{Turns: map[string]daemonpkg.Turn{
		"turn": {
			ID: "turn", LaneID: "lane", State: "terminal", Outcome: "failed",
			Diagnostic: "native acceptance commit failed: ambiguous", CompletedAt: 42,
		},
	}}}
	projected := durableLaneTerminalNoticeActor(snapshot, actor)
	body := laneTerminalNoticeBody(projected, "", true)
	if projected.result != "" || projected.failure == "" || projected.completedAt != 42 ||
		!strings.Contains(body, "status=failed outcome=failed exit=1") || strings.Contains(body, "status=completed") {
		t.Fatalf("durable terminal notice projection actor=%+v body=%q", projected, body)
	}
}

type delayedCodexLaneNative struct {
	setupDelay           time.Duration
	setupHadDeadline     bool
	turnStartHadDeadline bool
}

func (n *delayedCodexLaneNative) StartThread(ctx context.Context, _ bridge.CodexStartRequest) (bridge.CodexNativeThread, error) {
	_, n.setupHadDeadline = ctx.Deadline()
	timer := time.NewTimer(n.setupDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return bridge.CodexNativeThread{}, ctx.Err()
	case <-timer.C:
		return bridge.CodexNativeThread{ID: "00000000-0000-0000-0000-000000000001"}, nil
	}
}

func (n *delayedCodexLaneNative) PrepareLaneThread(ctx context.Context, id, _, _, _ string, _ bool) (bridge.CodexNativeThread, error) {
	_, n.setupHadDeadline = ctx.Deadline()
	return bridge.CodexNativeThread{ID: id}, nil
}

func (n *delayedCodexLaneNative) StartLaneTurn(ctx context.Context, _ bridge.CodexLaneTurnRequest) (string, error) {
	_, n.turnStartHadDeadline = ctx.Deadline()
	return "native-turn", nil
}

func (*delayedCodexLaneNative) WaitLaneTurn(ctx context.Context, threadID, turnID string) (bridge.CodexLaneTurnResult, error) {
	<-ctx.Done()
	return bridge.CodexLaneTurnResult{ThreadID: threadID, TurnID: turnID}, ctx.Err()
}

func (*delayedCodexLaneNative) InterruptLaneTurn(context.Context, string, string) error { return nil }
func (*delayedCodexLaneNative) ArchiveThread(context.Context, string) error             { return nil }

func TestParseUnifiedLaneCommandRejectsMissingValuesAndPreservesNativeArguments(t *testing.T) {
	for _, args := range [][]string{{"run", "--name"}, {"run", "--cwd"}, {"wait", "lane", "--timeout"}, {"run", "--permission-mode"}, {"run", "--model"}} {
		if _, err := parseUnifiedLaneCommand(args); err == nil || !strings.Contains(err.Error(), "requires a value") {
			t.Fatalf("parse %v error = %v", args, err)
		}
	}
	got, err := parseUnifiedLaneCommand([]string{"run", "--name", "worker", "--inherit-groups", "--yolo", "--model", "native-model"})
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "worker" || !got.inheritGroups || got.permission != "bypassPermissions" ||
		!got.permissionExplicit || !reflect.DeepEqual(got.native, []string{"--model", "native-model"}) {
		t.Fatalf("parsed command = %+v", got)
	}
}

func TestGrokLaneStartRequiresExplicitBypassBeforeRuntimeAccess(t *testing.T) {
	coordinator := &hostCoordinator{}
	parent := daemonpkg.ManagedAttachment{
		ID: "parent", Cwd: t.TempDir(), PermissionMode: "bypassPermissions",
	}
	for _, arguments := range [][]string{
		{"start", "--name", "worker"},
		{"start", "--name", "worker", "--permission-mode", "default"},
		{"start", "--name", "worker", "--no-yolo"},
	} {
		parsed, err := parseUnifiedLaneCommand(arguments)
		if err != nil {
			t.Fatalf("parse %v: %v", arguments, err)
		}
		// A nil runtime is deliberate: rejection must happen before readiness,
		// catalog mutation, adapter dispatch, or native invocation.
		if _, err = coordinator.startLane(context.Background(), nil, parent, "grok", parsed, "prompt", false, "readiness-receipt"); err == nil ||
			!strings.Contains(err.Error(), "explicit bypassPermissions") {
			t.Fatalf("start %v error = %v", arguments, err)
		}
	}

	for _, arguments := range [][]string{
		{"start", "--name", "worker", "--permission-mode", "bypassPermissions"},
		{"start", "--name", "worker", "--always-approve"},
		{"start", "--name", "worker", "--approval-policy", "never"},
	} {
		parsed, err := parseUnifiedLaneCommand(arguments)
		if err != nil {
			t.Fatalf("parse %v: %v", arguments, err)
		}
		if err := validateGrokLanePermission("grok", parsed, true); err != nil {
			t.Fatalf("explicit bypass %v rejected: %v", arguments, err)
		}
	}
}

func TestGrokLaneResumeRejectsExplicitSaferReplacementButKeepsRecordedMode(t *testing.T) {
	omitted, err := parseUnifiedLaneCommand([]string{"resume", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGrokLanePermission("grok", omitted, false); err != nil {
		t.Fatalf("recorded permission reuse rejected: %v", err)
	}
	explicitDefault, err := parseUnifiedLaneCommand([]string{"resume", "worker", "--permission-mode", "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGrokLanePermission("grok", explicitDefault, false); err == nil {
		t.Fatal("explicit unsupported Grok resume permission succeeded")
	}
}

func TestParseUnifiedLaneCommandRejectsLegacyNotifyBeforeRegistration(t *testing.T) {
	for _, args := range [][]string{
		{"start", "--name", "worker", "--persistent", "--notify", "parent", "-"},
		{"start", "--name", "worker", "--no-notify", "-"},
	} {
		_, err := parseUnifiedLaneCommand(args)
		if err == nil || !strings.Contains(err.Error(), "immediate parent automatically") {
			t.Fatalf("parse %v error = %v", args, err)
		}
	}
}

func TestCodexNativeBypassOptionIsCanonicalizedExactlyOnce(t *testing.T) {
	got, err := parseUnifiedLaneCommand([]string{
		"start", "--name", "worker", "--dangerously-bypass-approvals-and-sandbox", "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.permission != "bypassPermissions" || len(got.native) != 0 {
		t.Fatalf("parsed native bypass = %+v", got)
	}
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")
	command, err := laneNativeCommand(&laneActor{
		product: "codex", cwd: t.TempDir(), permission: got.permission, arguments: got.native,
	}, "prompt", false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, argument := range command.Args {
		if argument == "--dangerously-bypass-approvals-and-sandbox" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Codex native argv = %q, bypass flag count = %d", command.Args, count)
	}
}

func TestParseUnifiedLaneExplicitApprovalPolicyWinsPermissionMode(t *testing.T) {
	for _, test := range []struct {
		name       string
		arguments  []string
		permission string
	}{
		{
			name:       "safer policy overrides earlier bypass",
			arguments:  []string{"start", "--permission-mode", "bypassPermissions", "--approval-policy", "on-request"},
			permission: "default",
		},
		{
			name:       "safer policy overrides later bypass",
			arguments:  []string{"start", "--approval-policy", "on-request", "--permission-mode", "bypassPermissions"},
			permission: "default",
		},
		{
			name:       "never policy selects bypass",
			arguments:  []string{"resume", "worker", "--approval-policy", "never"},
			permission: "bypassPermissions",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseUnifiedLaneCommand(test.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if got.permission != test.permission {
				t.Fatalf("permission = %q, want %q (parsed command: %+v)", got.permission, test.permission, got)
			}
		})
	}
}

func TestParseUnifiedLaneLifecycleOptionsMatchLegacyContract(t *testing.T) {
	got, err := parseUnifiedLaneCommand([]string{"start", "--name", "worker", "--auto-archive-after", "2.5", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if got.persistent || got.noAutoArchive || !got.autoArchiveAfterSet || got.autoArchiveAfter != 2500*time.Millisecond {
		t.Fatalf("custom auto archive = %+v", got)
	}
	got, err = parseUnifiedLaneCommand([]string{"start", "--name", "worker", "--no-auto-archive", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if got.persistent || !got.noAutoArchive {
		t.Fatalf("no-auto-archive changed ownership = %+v", got)
	}
	if _, err := parseUnifiedLaneCommand([]string{"start", "--name", "worker", "--no-auto-archive", "--auto-archive-after", "1", "-"}); err == nil {
		t.Fatal("conflicting auto-archive options were accepted")
	}
}

func TestLaneTerminalProjectionCarriesExplicitStatusOutcomeAndExit(t *testing.T) {
	for _, test := range []struct {
		outcome string
		exit    int
	}{{"completed", 0}, {"failed", 1}, {"timed_out", 124}, {"interrupted", 130}} {
		actor := &laneActor{id: "lane", product: "qwen", turnID: "turn", outcome: test.outcome}
		result := laneActorResult(actor)
		if result["status"] != test.outcome || result["outcome"] != test.outcome || result["exit"] != test.exit {
			t.Fatalf("%s terminal projection = %#v", test.outcome, result)
		}
	}
}

func TestLaneExecutionTimeoutBeginsOnlyAfterNativeDispatch(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane", product: "codex", turnID: "turn", state: "preparing",
		turnTimeout: 200 * time.Millisecond,
	}
	coordinator := newHostCoordinator(context.Background(), root)
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		"command-timeout-boundary", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "timeout-attempt", []byte("work"),
	)
	if err != nil {
		t.Fatal(err)
	}
	actor.inputSequence, actor.activeReceiptID = receipt.Sequence, receipt.ReceiptID
	accepted, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if deadline := accepted.Catalog.Turns[actor.turnID].DeadlineAt; deadline != 0 {
		t.Fatalf("accepted turn armed execution deadline before native dispatch: %d", deadline)
	}

	actor.startedAt = time.Now().UnixMilli()
	actor.deadlineAt = actor.startedAt + actor.turnTimeout.Milliseconds()
	if err := coordinator.markLaneRunning(runtime, actor); err != nil {
		t.Fatal(err)
	}
	dispatched, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	turn := dispatched.Catalog.Turns[actor.turnID]
	if turn.StartedAt != actor.startedAt || turn.DeadlineAt != actor.deadlineAt {
		t.Fatalf("dispatched timeout window = started %d deadline %d, want %d/%d",
			turn.StartedAt, turn.DeadlineAt, actor.startedAt, actor.deadlineAt)
	}
}

func TestCodexExecutionTimeoutDoesNotExpireDuringNativeThreadSetup(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane", parentID: "parent", product: "codex", name: "worker", cwd: root,
		permission: "bypassPermissions", turnID: "turn", state: "preparing", capability: "capability",
		turnTimeout: 20 * time.Millisecond, done: make(chan struct{}),
	}
	coordinator := newHostCoordinator(context.Background(), root)
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		"command-codex-timeout", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "codex-timeout-attempt", []byte("work"),
	)
	if err != nil {
		t.Fatal(err)
	}
	actor.inputSequence, actor.activeReceiptID = receipt.Sequence, receipt.ReceiptID
	native := &delayedCodexLaneNative{setupDelay: 2 * actor.turnTimeout}
	started := time.Now()
	if err := coordinator.dispatchCodexLaneTurnWithNative(runtime, actor, "work", false, false, native); err != nil {
		t.Fatalf("dispatch after slow native setup: %v", err)
	}
	if elapsed := time.Since(started); elapsed < native.setupDelay {
		t.Fatalf("native setup returned in %v, want at least %v", elapsed, native.setupDelay)
	}
	if native.setupHadDeadline || native.turnStartHadDeadline {
		t.Fatalf("execution deadline leaked into Codex setup: thread=%t turn/start=%t",
			native.setupHadDeadline, native.turnStartHadDeadline)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := coordinator.waitLaneActor(ctx, runtime, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result["outcome"] != "timed_out" || result["exit"] != 124 {
		t.Fatalf("post-setup execution timeout = %#v", result)
	}
}

func TestTimedOutLanePersistsExit124(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{ID: "lane", Product: "qwen", State: "running"}
	catalog.Turns["turn"] = daemonpkg.Turn{ID: "turn", LaneID: "lane", Sequence: 1, State: "dispatched"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	actor := &laneActor{id: "lane", product: "qwen", turnID: "turn", state: "terminal", outcome: "timed_out"}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.markLaneTerminal(runtime, actor); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if exit := after.Catalog.Turns[actor.turnID].ExitCode; exit != 124 {
		t.Fatalf("durable timed-out exit = %d, want 124", exit)
	}
}

func TestLaneWorkerEnvironmentReplacesAmbientPeerAuthority(t *testing.T) {
	actor := &laneActor{id: "lane-id", product: "qwen", capability: "lane-capability"}
	got := laneWorkerEnvironment([]string{
		"PATH=/bin", "AGENT_SESSIONS_SESSION_ID=parent", "AGENT_SESSIONS_PRODUCT=codex",
		"AGENT_SESSIONS_QWEN_CAPABILITY=old", "AGENT_SESSIONS_LANE_CAPABILITY=old-lane", "AGENT_SESSIONS_HOST_BINARY=/old/runtime",
	}, actor)
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, wanted := range []string{"\nPATH=/bin\n", "\nAGENT_SESSIONS_SESSION_ID=lane-id\n", "\nAGENT_SESSIONS_PRODUCT=qwen\n", "\nAGENT_SESSIONS_LANE_CAPABILITY=lane-capability\n"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("lane environment %q lacks %q", joined, wanted)
		}
	}
	for _, forbidden := range []string{"parent", "AGENT_SESSIONS_QWEN_CAPABILITY=old", "old-lane", "/old/runtime"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("lane environment retained %q: %q", forbidden, joined)
		}
	}
}

func TestClaudeInboundPeerFrameRemainsAQueryInOneShotNativeMode(t *testing.T) {
	frame := `<cross-session-message from="session:source">work</cross-session-message>`
	got := laneInboundPrompt("claude", frame)
	if strings.HasPrefix(got, "<cross-session-message ") || !strings.HasSuffix(got, frame) {
		t.Fatalf("Claude inbound prompt = %q", got)
	}
	for _, product := range []string{"codex", "grok", "qwen"} {
		if got := laneInboundPrompt(product, frame); got != frame {
			t.Fatalf("%s inbound prompt changed to %q", product, got)
		}
	}
	plain := "ordinary explicit follow-up"
	if got := laneInboundPrompt("claude", plain); got != plain {
		t.Fatalf("ordinary Claude prompt changed to %q", got)
	}
}

func TestClaudeInboundPeerFrameDispatchesAsOrdinaryNativeQuery(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	if err := os.WriteFile(claudeBin, []byte(`#!/bin/sh
last=
for argument do
  last="$argument"
done
case "$last" in
  "The following Agent Sessions peer message is the current user turn."*)
    printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"wrapped peer query completed"}'
    ;;
  *)
    printf '%s\n' 'Error: No messages returned from query' >&2
    exit 31
    ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "00000000-0000-0000-0000-000000000002", parentID: "parent", product: "claude", name: "worker",
		cwd: root, nativeID: "00000000-0000-0000-0000-000000000001", permission: "dontAsk", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	frame := `<cross-session-message from="session:source">work</cross-session-message>`
	if _, err := coordinator.deliverLaneMessageWithID(runtime, actor, "peer-frame-delivery", frame); err != nil {
		t.Fatalf("deliver Claude peer frame: %v", err)
	}
	result, err := coordinator.waitLaneActor(context.Background(), runtime, actor)
	if err != nil {
		t.Fatalf("wait Claude peer frame: %v", err)
	}
	if result["outcome"] != "completed" || result["result"] != "wrapped peer query completed" || result["diagnostic"] != "" {
		t.Fatalf("Claude peer-frame result = %#v", result)
	}
}

func TestClaudeBusyLaneMessageDispatchesExactlyOnceAfterWorkerExitWithoutCollector(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	promptLog := filepath.Join(root, "prompts.log")
	firstStarted := filepath.Join(root, "first-started")
	if err := os.WriteFile(claudeBin, []byte(`#!/bin/sh
last=
for argument do
  last="$argument"
done
printf '%s\n' "$last" >> "$CLAUDE_BUSY_TEST_LOG"
case "$last" in
  turn-one)
    : > "$CLAUDE_BUSY_TEST_STARTED"
    sleep 1
    printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"turn one complete"}'
    ;;
  busy-message-nonce)
    printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"turn two complete"}'
    ;;
  *)
    printf '%s\n' "unexpected prompt: $last" >&2
    exit 31
    ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("CLAUDE_BUSY_TEST_LOG", promptLog)
	t.Setenv("CLAUDE_BUSY_TEST_STARTED", firstStarted)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "00000000-0000-0000-0000-000000000002", parentID: "parent", product: "claude", name: "worker",
		cwd: root, nativeID: "00000000-0000-0000-0000-000000000001", permission: "dontAsk",
		persistent: true, state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if _, err := coordinator.deliverLaneMessageWithID(runtime, actor, "turn-one-delivery", "turn-one"); err != nil {
		t.Fatalf("dispatch slow Claude turn: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(firstStarted); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("slow Claude turn did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := coordinator.deliverLaneMessageWithID(runtime, actor, "busy-message-delivery", "busy-message-nonce"); err != nil {
		t.Fatalf("queue message for busy Claude lane: %v", err)
	}
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		turns := make([]daemonpkg.Turn, 0, 2)
		for _, turn := range after.Catalog.Turns {
			if turn.LaneID == actor.id {
				turns = append(turns, turn)
			}
		}
		if len(turns) == 2 && after.Catalog.Lanes[actor.id].State == "terminal" {
			for _, turn := range turns {
				if turn.State != "terminal" || turn.Outcome != "completed" || strings.Contains(turn.Diagnostic, "active stream worker") {
					t.Fatalf("Claude queued turns = %+v", turns)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("busy Claude message did not complete exactly once: lane=%+v turns=%+v", after.Catalog.Lanes[actor.id], turns)
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	prompts := strings.Fields(string(body))
	if !reflect.DeepEqual(prompts, []string{"turn-one", "busy-message-nonce"}) {
		t.Fatalf("Claude native prompts = %q, want exactly one delivery of each turn", prompts)
	}
	if err := coordinator.claudeLanes.Archive(actor.id); err != nil {
		t.Fatalf("Claude worker registry remained active after terminal turn: %v", err)
	}
}

func TestLaneResumeTransfersDurableParentAndUnarchives(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "old-parent", Product: "qwen", Name: "worker",
		NativeSessionID: "native", Cwd: "/workspace", Groups: []string{"old"}, State: "archived",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{id: "lane", product: "qwen", parentID: "new-parent", groups: []string{"new"}, turnID: "turn"}
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputEngine.UpdateLaneAdmitAndMarkDispatching(
		"command-transfer-parent", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "transfer-attempt", []byte("resume"),
	); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	lane := after.Catalog.Lanes["lane"]
	if lane.ParentAttachmentID != "new-parent" || lane.State != "idle" || !reflect.DeepEqual(lane.Groups, []string{"new"}) {
		t.Fatalf("resumed lane = %+v", lane)
	}
}

func TestLaneRunAndResumeUseStableReceiptLedgerAndReplayWithoutNativeDuplicate(t *testing.T) {
	root := shortDaemonTestRoot(t)
	startID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "cli-run-ledger"})
	laneID := laneIDForInitialReceipt(startID)
	claudeBin := filepath.Join(root, "claude-ledger")
	promptLog := filepath.Join(root, "ledger-prompts.log")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  --version) printf '2.1.233\n'; exit 0 ;;
  'auth status --json') printf '%%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'; exit 0 ;;
esac
last=
for argument do last="$argument"; done
printf '%%s\n' "$last" >> "$LANE_LEDGER_PROMPT_LOG"
printf '%%s\n' '{"session_id":"%s","result":"ledger native result"}'
`, laneID)
	if err := os.WriteFile(claudeBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("LANE_LEDGER_PROMPT_LOG", promptLog)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	parent := daemonpkg.ManagedAttachment{ID: "parent", Product: "codex", Cwd: root, PermissionMode: "bypassPermissions"}
	runOptions := parsedLaneCommand{command: "run", name: "ledger-worker", cwd: root, persistent: true, persistentSet: true}
	run, err := coordinator.startLane(context.Background(), runtime, parent, "claude", runOptions, "initial ledger prompt", true, startID)
	if err != nil {
		t.Fatal(err)
	}
	startReceiptID, _ := run["receipt_id"].(string)
	if !strings.HasPrefix(startReceiptID, startID+"-") || run["receipt_sequence"] != uint64(1) || run["outcome"] != "completed" {
		t.Fatalf("ledger run result = %#v", run)
	}
	resumeID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "cli-resume-ledger"})
	resumed, err := coordinator.resumeLane(context.Background(), runtime, parent, "claude", parsedLaneCommand{
		command: "resume", target: laneID,
	}, "resume ledger prompt", resumeID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed["receipt_id"] != resumeID || resumed["receipt_sequence"] != uint64(2) || resumed["outcome"] != "completed" {
		t.Fatalf("ledger resume result = %#v", resumed)
	}
	// Simulate coordinator cache loss: the durable lane/receipt tuple alone must
	// authorize the exact retry without a second native prompt.
	restarted := newHostCoordinator(context.Background(), root)
	if err := restarted.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	coordinator = restarted
	replayed, err := coordinator.startLane(context.Background(), runtime, parent, "claude", runOptions, "initial ledger prompt", true, startID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed["receipt_id"] != startReceiptID || replayed["receipt_sequence"] != uint64(1) {
		t.Fatalf("stable run replay = %#v", replayed)
	}
	prompts, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(prompts)); !reflect.DeepEqual(got, []string{"initial", "ledger", "prompt", "resume", "ledger", "prompt"}) {
		t.Fatalf("native prompt replayed or changed: %q", prompts)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Catalog.LaneInputs) != 2 || after.Catalog.LaneInputs[startReceiptID].Sequence != 1 ||
		after.Catalog.LaneInputs[resumeID].Sequence != 2 {
		t.Fatalf("durable CLI receipt ledger = %+v", after.Catalog.LaneInputs)
	}
}

func TestExactLaneStartReplayBindsCompleteAuthorityAndRequestTuple(t *testing.T) {
	requested := daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "owner", Product: "claude", Name: "worker", Cwd: "/work",
		Groups: []string{"a", "b"}, ExplicitGroups: []string{"a"}, InheritGroups: true,
		PermissionMode: "dontAsk", ApprovalPolicy: "never", Sandbox: "workspace-write", Effort: "high",
		Schema: "/schema", Arguments: []string{"--model", "sonnet"}, Persistent: true,
		AutoArchive: true, AutoArchiveDelayMS: 2500, State: "idle",
	}
	stored := requested
	stored.State, stored.NativeSessionID, stored.InputSequence, stored.ArchiveRevision = "terminal", "native", 7, 2
	if !exactLaneStartReplay(stored, requested) {
		t.Fatal("mutable lifecycle/native projections rejected an exact replay")
	}
	tests := []struct {
		name string
		edit func(*daemonpkg.Lane)
	}{
		{"owner", func(l *daemonpkg.Lane) { l.ParentAttachmentID = "other" }},
		{"product", func(l *daemonpkg.Lane) { l.Product = "qwen" }},
		{"name", func(l *daemonpkg.Lane) { l.Name = "other" }},
		{"cwd", func(l *daemonpkg.Lane) { l.Cwd = "/other" }},
		{"groups", func(l *daemonpkg.Lane) { l.Groups = []string{"a"} }},
		{"approval", func(l *daemonpkg.Lane) { l.ApprovalPolicy = "on-request" }},
		{"sandbox", func(l *daemonpkg.Lane) { l.Sandbox = "read-only" }},
		{"arguments", func(l *daemonpkg.Lane) { l.Arguments = []string{"--model", "opus"} }},
		{"persistence", func(l *daemonpkg.Lane) { l.Persistent = false }},
		{"archive", func(l *daemonpkg.Lane) { l.AutoArchiveDelayMS++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := requested
			changed.Groups = append([]string(nil), requested.Groups...)
			changed.ExplicitGroups = append([]string(nil), requested.ExplicitGroups...)
			changed.Arguments = append([]string(nil), requested.Arguments...)
			test.edit(&changed)
			if exactLaneStartReplay(stored, changed) {
				t.Fatalf("changed %s tuple was accepted", test.name)
			}
		})
	}
}

func stageInitialLaneStartForRetry(
	t *testing.T,
	runtime *daemonpkg.Runtime,
	coordinator *hostCoordinator,
	parent daemonpkg.ManagedAttachment,
	product string,
	options parsedLaneCommand,
	input string,
	wait bool,
	operationID string,
) (string, string) {
	t.Helper()
	laneID := laneIDForInitialReceipt(operationID)
	cwd, err := filepath.Abs(options.cwd)
	if err != nil {
		t.Fatal(err)
	}
	explicitGroups := uniqueStrings(options.groups)
	groups := append([]string(nil), explicitGroups...)
	if options.inheritGroups {
		groups = uniqueStrings(append(groups, parent.Groups...))
	}
	groups, err = coordinator.anchorLaneGroups(runtime, groups, parent.ID, laneID)
	if err != nil {
		t.Fatal(err)
	}
	permission := options.permission
	if permission == "" {
		permission = laneDefaultPermission(product, parent.PermissionMode)
	}
	actor := &laneActor{
		id: laneID, parentID: parent.ID, product: product, name: options.name, cwd: cwd,
		groups: groups, explicitGroups: explicitGroups, inheritGroups: options.inheritGroups,
		permission: permission, approvalPolicy: options.approvalPolicy, sandbox: options.sandbox,
		effort: options.effort, schema: options.schema, arguments: append([]string(nil), options.native...),
		persistent: options.persistent, autoArchive: !options.noAutoArchive,
		autoArchiveDelay: defaultUnifiedLaneAutoArchiveDelay,
	}
	if options.autoArchiveAfterSet {
		actor.autoArchive, actor.autoArchiveDelay = true, options.autoArchiveAfter
	}
	if options.timeout != "" {
		actor.turnTimeout, err = parseLaneSeconds(options.timeout, false, "--timeout")
		if err != nil {
			t.Fatal(err)
		}
	}
	if product == "claude" {
		actor.nativeID = laneID
	}
	receiptID := initialLaneReceiptID(operationID, actor, input, wait)
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	collisionLaneID := "collision-" + laneID
	collisionTurnID := "collision-turn-" + laneID
	catalog.Lanes[collisionLaneID] = daemonpkg.Lane{ID: collisionLaneID, Product: "qwen", State: "terminal"}
	catalog.Turns[collisionTurnID] = daemonpkg.Turn{
		ID: collisionTurnID, LaneID: collisionLaneID, Sequence: 1, State: "terminal", Outcome: "completed",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := engine.CreateLaneAdmitAndMarkDispatching(
		receiptID, durableLane(actor, "idle"), daemonpkg.Turn{ID: collisionTurnID, LaneID: laneID},
		"staged-attempt-"+laneID, []byte(input),
	)
	if err == nil || queued.State != daemonpkg.ReceiptQueued || queued.Revision != 1 {
		t.Fatalf("staged start receipt=%+v err=%v", queued, err)
	}
	return laneID, receiptID
}

func installClaudeStartRetryTestBinary(t *testing.T, root, laneID string) {
	t.Helper()
	path := filepath.Join(root, "claude-start-retry")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  --version) printf '2.1.233\n'; exit 0 ;;
  'auth status --json') printf '%%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'; exit 0 ;;
esac
printf '%%s\n' '{"session_id":"%s","result":"retried result"}'
`, laneID)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", path)
}

func TestStagedStartRetryBindsTimeoutAndRestoresItAfterRestart(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	parent := daemonpkg.ManagedAttachment{ID: "parent", Product: "codex", Cwd: root, PermissionMode: "bypassPermissions"}
	options := parsedLaneCommand{command: "run", name: "timeout-worker", cwd: root, timeout: "3", persistent: true, persistentSet: true}
	operationID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "staged-timeout-retry"})
	creator := newHostCoordinator(context.Background(), root)
	laneID, receiptID := stageInitialLaneStartForRetry(
		t, runtime, creator, parent, "claude", options, "timeout prompt", true, operationID,
	)
	installClaudeStartRetryTestBinary(t, root, laneID)
	restarted := newHostCoordinator(context.Background(), root)
	if err := restarted.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	changed := options
	changed.timeout = "4"
	if _, err := restarted.startLane(
		context.Background(), runtime, parent, "claude", changed, "timeout prompt", true, operationID,
	); !errors.Is(err, daemonpkg.ErrLaneInputConflict) {
		t.Fatalf("changed timeout retry error=%v", err)
	}
	staged, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Catalog.LaneInputs) != 1 || staged.Catalog.LaneInputs[receiptID].Revision != 1 {
		t.Fatalf("changed timeout created a second ghost: %+v", staged.Catalog.LaneInputs)
	}
	result, err := restarted.startLane(
		context.Background(), runtime, parent, "claude", options, "timeout prompt", true, operationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result["receipt_id"] != receiptID || result["outcome"] != "completed" {
		t.Fatalf("exact timeout retry result=%#v", result)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	turn := after.Catalog.Turns[fmt.Sprint(result["turn_id"])]
	if turn.DeadlineAt-turn.StartedAt != int64(3*time.Second/time.Millisecond) {
		t.Fatalf("retried timeout was not applied to CAS2 turn: %+v", turn)
	}
}

func TestStagedStartCAS2RevalidatesNameAgainstAcceptedLane(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	parent := daemonpkg.ManagedAttachment{ID: "parent", Product: "codex", Cwd: root, PermissionMode: "bypassPermissions"}
	options := parsedLaneCommand{command: "run", name: "claimed-name", cwd: root, persistent: true, persistentSet: true}
	operationID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "staged-name-retry"})
	creator := newHostCoordinator(context.Background(), root)
	laneID, receiptID := stageInitialLaneStartForRetry(
		t, runtime, creator, parent, "claude", options, "name prompt", true, operationID,
	)
	installClaudeStartRetryTestBinary(t, root, laneID)
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["accepted-name-owner"] = daemonpkg.Lane{
		ID: "accepted-name-owner", ParentAttachmentID: parent.ID, Product: "claude", Name: options.name,
		Cwd: root, Groups: append([]string(nil), catalog.Lanes[laneID].Groups...), State: "idle",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	restarted := newHostCoordinator(context.Background(), root)
	if err := restarted.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.startLane(
		context.Background(), runtime, parent, "claude", options, "name prompt", true, operationID,
	); !errors.Is(err, daemonpkg.ErrLaneInputConflict) {
		t.Fatalf("CAS2 name collision error=%v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Catalog.LaneInputs) != 1 || after.Catalog.LaneInputs[receiptID].Revision != 1 ||
		after.Catalog.LaneInputs[receiptID].State != daemonpkg.ReceiptQueued {
		t.Fatalf("name collision advanced or duplicated staged receipt: %+v", after.Catalog.LaneInputs)
	}
	for _, turn := range after.Catalog.Turns {
		if turn.LaneID == laneID {
			t.Fatalf("name collision created a staged lane turn: %+v", turn)
		}
	}
}

func TestReceiptWaitCollectsRecoveredFreshTargetNotOlderSyntheticAudit(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "receipt-wait-lane", parentID: "parent", product: "qwen", name: "worker", cwd: root,
		state: "idle", done: closedLaneDone(),
	}
	engine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	actor.turnID = "crash-window-turn"
	receipt, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-receipt-wait", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "crash-attempt", []byte("body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RecoverAcceptedTurnAndRequeue(receipt.ReceiptID, "synthetic crash audit"); err != nil {
		t.Fatal(err)
	}
	actor.turnID, actor.state, actor.done = "fresh-replay-turn", "terminal", closedLaneDone()
	claimed, err := engine.AcceptTurnAndMarkDispatching(
		receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "fresh-attempt",
	)
	if err != nil {
		t.Fatal(err)
	}
	actor.outcome, actor.result, actor.completedAt = "completed", "fresh result", time.Now().UnixMilli()
	if err := coordinator.markLaneTerminal(runtime, actor); err != nil {
		t.Fatal(err)
	}
	result, current, err := coordinator.waitLaneReceipt(context.Background(), runtime, actor, receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if current.TargetTurnID != claimed.TargetTurnID || result["turn_id"] != "fresh-replay-turn" || result["result"] != "fresh result" {
		t.Fatalf("receipt wait result=%#v receipt=%+v", result, current)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.Turns["crash-window-turn"].State != "terminal" ||
		after.Catalog.Turns["fresh-replay-turn"].State != "collected" {
		t.Fatalf("exact collection authority turns=%+v", after.Catalog.Turns)
	}
}

func TestQueuedLaneFollowupAdvancesDurableTerminalLaneBeforePreparing(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "codex", Name: "worker",
		NativeSessionID: "native", Cwd: "/workspace", State: "terminal",
	}
	catalog.Turns["first-turn"] = daemonpkg.Turn{
		ID: "first-turn", LaneID: "lane", Sequence: 1, State: "terminal", Outcome: "completed",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "lane", parentID: "parent", product: "codex", name: "worker", cwd: "/workspace",
		nativeID: "native", turnID: "queued-turn", capability: "queued-capability", state: "idle",
	}
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputEngine.UpdateLaneAdmitAndMarkDispatching(
		"command-queued-followup", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "queued-attempt", []byte("queued"),
	); err != nil {
		t.Fatalf("accept queued follow-up after terminal turn: %v", err)
	}
	if err := coordinator.commitLaneAuthorization(runtime, actor, "preparing"); err != nil {
		t.Fatalf("prepare queued follow-up after terminal turn: %v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Catalog.Lanes[actor.id].State; got != "preparing" {
		t.Fatalf("queued follow-up lane state = %q, want preparing", got)
	}
	turn := after.Catalog.Turns[actor.turnID]
	if turn.State != "accepted" || turn.Sequence != 2 {
		t.Fatalf("queued follow-up turn = %+v, want accepted sequence 2", turn)
	}
}

func TestConcurrentBusyLaneMessagesCommitOrderedReceiptsWithoutAggregation(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{id: "lane-id", parentID: "parent", product: "claude", state: "running", done: make(chan struct{})}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "running")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	const deliveries = 64
	ready := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < deliveries; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-ready
			if _, err := coordinator.deliverLaneMessageWithID(runtime, actor, fmt.Sprintf("delivery-%03d", index), fmt.Sprintf("marker-%03d", index)); err != nil {
				t.Errorf("admit lane message: %v", err)
			}
		}(index)
	}
	close(ready)
	wait.Wait()
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	sequences := make(map[uint64]bool, deliveries)
	for _, receipt := range after.Catalog.LaneInputs {
		if receipt.LaneID == actor.id {
			sequences[receipt.Sequence] = true
		}
	}
	if len(sequences) != deliveries {
		t.Fatalf("ordered receipts = %d, want %d", len(sequences), deliveries)
	}
	for sequence := uint64(1); sequence <= deliveries; sequence++ {
		if !sequences[sequence] {
			t.Fatalf("missing lane receipt sequence %d", sequence)
		}
	}
}

func TestLaneInputTransientPreNativeFailureRetriesSameReceiptExactlyOnce(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	promptLog := filepath.Join(root, "prompts.log")
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("LANE_INPUT_RETRY_LOG", promptLog)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-retry", parentID: "parent", product: "claude", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	receipt, err := coordinator.deliverLaneMessageWithID(runtime, actor, "stable-retry-delivery", "transient-body")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Sequence != 1 {
		t.Fatalf("receipt sequence = %d", receipt.Sequence)
	}
	if err := os.WriteFile(claudeBin, []byte(`#!/bin/sh
last=
for argument do last="$argument"; done
printf '%s\n' "$last" >> "$LANE_INPUT_RETRY_LOG"
printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"done"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after.Catalog.LaneInputs[receipt.ReceiptID].State == daemonpkg.ReceiptRetired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transient receipt did not retire: %+v", after.Catalog.LaneInputs[receipt.ReceiptID])
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "transient-body\n" {
		t.Fatalf("native prompts = %q", body)
	}
}

func TestLaneInputPermanentPreNativeFailureStopsBoundedAndStaysVisible(t *testing.T) {
	root := shortDaemonTestRoot(t)
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", filepath.Join(root, "permanently-missing-claude"))
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-permanent-failure", parentID: "parent", product: "claude", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	receipt, err := coordinator.deliverLaneMessageWithID(runtime, actor, "permanent-failure-delivery", "body")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var stopped daemonpkg.LaneInputReceipt
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		stopped = after.Catalog.LaneInputs[receipt.ReceiptID]
		if stopped.Revision >= 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permanent failure did not reach retry bound: %+v", stopped)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stopped.State != daemonpkg.ReceiptQueued || stopped.Sequence != receipt.Sequence {
		t.Fatalf("bounded permanent failure receipt = %+v", stopped)
	}
	time.Sleep(450 * time.Millisecond)
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if current := after.Catalog.LaneInputs[receipt.ReceiptID]; current.Revision != stopped.Revision || current.State != daemonpkg.ReceiptQueued {
		t.Fatalf("permanent failure busy-looped after bound: before %+v after %+v", stopped, current)
	}
	if after.Catalog.Lanes[actor.id].State != "terminal" || len(after.Catalog.CleanupDebts) != 0 {
		t.Fatalf("bounded receipt created generic cleanup debt: lane=%+v debts=%+v", after.Catalog.Lanes[actor.id], after.Catalog.CleanupDebts)
	}
	promptLog := filepath.Join(root, "fixed-prompts.log")
	t.Setenv("LANE_INPUT_RETRY_LOG", promptLog)
	if err := os.WriteFile(filepath.Join(root, "permanently-missing-claude"), []byte(`#!/bin/sh
last=
for argument do last="$argument"; done
printf '%s\n' "$last" >> "$LANE_INPUT_RETRY_LOG"
printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"done"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	wake, err := coordinator.deliverLaneMessageWithID(runtime, actor, "fixed-cause-wake", "wake")
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after.Catalog.LaneInputs[receipt.ReceiptID].State == daemonpkg.ReceiptRetired &&
			after.Catalog.LaneInputs[wake.ReceiptID].State == daemonpkg.ReceiptRetired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixed cause did not converge receipts: first=%+v wake=%+v",
				after.Catalog.LaneInputs[receipt.ReceiptID], after.Catalog.LaneInputs[wake.ReceiptID])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCodexLaneInputCommitsExactNativeAcceptanceBeforeTerminalWait(t *testing.T) {
	root := shortDaemonTestRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-codex-accept", parentID: "parent", product: "codex", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(ctx, root)
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.AdmitWithID("codex-exact-acceptance", actor.id, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	actor.inputSequence = receipt.Sequence
	prepareLaneTurnLocked(actor)
	actor.inputPump, actor.activeReceiptID = true, receipt.ReceiptID
	if _, err := inputEngine.AcceptTurnAndMarkDispatching(
		receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "codex-exact-attempt",
	); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.dispatchCodexLaneTurnWithNative(runtime, actor, "body", true, false, &delayedCodexLaneNative{}); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	row := after.Catalog.LaneInputs[receipt.ReceiptID]
	turn := after.Catalog.Turns[row.TargetTurnID]
	if row.State != daemonpkg.ReceiptInjected || row.Revision != 3 || row.NativeAcceptance == nil ||
		row.NativeAcceptance.NativeSessionID != actor.nativeID || row.NativeAcceptance.NativeMessageID != "native-turn" ||
		turn.State != "dispatched" || turn.NativeDispatchID != "native-turn" {
		t.Fatalf("exact Codex acceptance before wait: receipt=%+v turn=%+v", row, turn)
	}
}

func TestFailedNativePromptNeverBecomesInjectedOrRetiredFromSessionIDAlone(t *testing.T) {
	for _, product := range []string{"claude", "grok", "qwen"} {
		t.Run(product, func(t *testing.T) {
			root := shortDaemonTestRoot(t)
			runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			actor := &laneActor{id: "lane-" + product + "-reject", product: product, nativeID: "native-session", state: "idle"}
			snapshot, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			catalog := snapshot.Catalog
			catalog.Lanes[actor.id] = durableLane(actor, "idle")
			if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
				t.Fatal(err)
			}
			coordinator := newHostCoordinator(context.Background(), root)
			engine, err := coordinator.laneInputEngine(runtime)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := engine.AdmitWithID("reject-receipt-"+product, actor.id, []byte("rejected"))
			if err != nil {
				t.Fatal(err)
			}
			actor.inputSequence, actor.turnID = receipt.Sequence, "reject-turn-"+product
			if _, err := engine.AcceptTurnAndMarkDispatching(
				receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "reject-attempt-"+product,
			); err != nil {
				t.Fatal(err)
			}
			if diagnostic := coordinator.finalizeLaneInput(runtime, receipt.ReceiptID, actor.nativeID, "", "failed"); diagnostic != "" {
				t.Fatalf("failure finalization diagnostic = %q", diagnostic)
			}
			after, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			if row := after.Catalog.LaneInputs[receipt.ReceiptID]; row.State != daemonpkg.ReceiptAmbiguous || row.NativeAcceptance != nil {
				t.Fatalf("%s rejected prompt receipt = %+v", product, row)
			}
		})
	}
}

func TestClaudeResumeFailureLeavesLaneInputAmbiguous(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-claude-resume-reject", parentID: "parent", product: "claude", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	receipt, err := coordinator.deliverLaneMessageWithID(runtime, actor, "claude-resume-reject", "body")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		row := after.Catalog.LaneInputs[receipt.ReceiptID]
		if row.State == daemonpkg.ReceiptAmbiguous {
			if row.NativeAcceptance != nil {
				t.Fatalf("Claude failure fabricated acceptance: %+v", row)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Claude failure receipt = %+v", row)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNonCodexTerminalNativeAcceptanceCASFailureIsDurableDiagnostic(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-qwen-acceptance-cas", parentID: "missing-parent", product: "qwen", name: "worker", cwd: root,
		nativeID: "native-a", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	engine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.AdmitWithID("noncodex-terminal-cas", actor.id, []byte("accepted by native"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	prepareLaneTurnLocked(actor)
	actor.inputSequence, actor.inputPump, actor.activeReceiptID = receipt.Sequence, true, receipt.ReceiptID
	turnID, done := actor.turnID, actor.done
	coordinator.mu.Unlock()
	if _, err := engine.AcceptTurnAndMarkDispatching(
		receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "noncodex-terminal-attempt",
	); err != nil {
		t.Fatal(err)
	}
	// Force the exact acceptance CAS to reject: the returned native session
	// does not match the durable lane anchor. Terminalization must retain the
	// ambiguity as a durable turn diagnostic instead of reporting clean success.
	coordinator.mu.Lock()
	actor.nativeID = "native-b"
	coordinator.mu.Unlock()
	coordinator.completeLaneTurn(runtime, actor, turnID, done, laneTurnCompletion{outcome: "completed", result: "native result"})
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	row := after.Catalog.LaneInputs[receipt.ReceiptID]
	turn := after.Catalog.Turns[turnID]
	if row.State != daemonpkg.ReceiptAmbiguous ||
		turn.Outcome != "failed" || turn.ExitCode != 1 ||
		!strings.Contains(turn.Diagnostic, "native acceptance commit failed") {
		t.Fatalf("durable acceptance ambiguity receipt=%+v turn=%+v", row, turn)
	}
}

func TestLaneInputUnprovenPostStartFailureBecomesAmbiguousWithoutReplay(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{
		id: "lane-ambiguous", parentID: "parent", product: "claude", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", state: "idle", done: closedLaneDone(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.claudeLanes.Register(actor.id, func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coordinator.claudeLanes.Complete(actor.id) })
	receipt, err := coordinator.deliverLaneMessageWithID(runtime, actor, "stable-ambiguous-delivery", "body")
	if err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	row := after.Catalog.LaneInputs[receipt.ReceiptID]
	if row.State != daemonpkg.ReceiptAmbiguous || row.AmbiguityCause != daemonpkg.AmbiguityNativeAcceptanceUnproven {
		t.Fatalf("unproven native acceptance = %+v", row)
	}
	revision := row.Revision
	time.Sleep(400 * time.Millisecond)
	after, err = runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.LaneInputs[receipt.ReceiptID].Revision != revision {
		t.Fatal("ambiguous receipt was replayed")
	}
}

func TestArchiveLaneExplicitlyRetiresQueuedReceipts(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	actor := &laneActor{id: "lane-archive-input", parentID: "parent", product: "claude", name: "worker", cwd: root, state: "idle", done: closedLaneDone()}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Host.Host = "host-a"
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	engine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.AdmitWithID("archive-receipt", actor.id, []byte("queued"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.archiveLane(runtime, daemonpkg.ManagedAttachment{ID: "parent"}, "claude", parsedLaneCommand{target: actor.id}); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.LaneInputs[receipt.ReceiptID].State != daemonpkg.ReceiptRetired || after.Catalog.Lanes[actor.id].State != "archived" {
		t.Fatalf("archive state = lane %+v receipt %+v", after.Catalog.Lanes[actor.id], after.Catalog.LaneInputs[receipt.ReceiptID])
	}
}

func TestLaneInputRestartPumpsQueuedReceiptPastCollectionDebt(t *testing.T) {
	root := shortDaemonTestRoot(t)
	claudeBin := filepath.Join(root, "claude")
	promptLog := filepath.Join(root, "restart-prompts.log")
	if err := os.WriteFile(claudeBin, []byte(`#!/bin/sh
last=
for argument do last="$argument"; done
printf '%s\n' "$last" >> "$LANE_INPUT_RESTART_LOG"
printf '%s\n' '{"session_id":"00000000-0000-0000-0000-000000000001","result":"recovered"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("LANE_INPUT_RESTART_LOG", promptLog)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	lane := daemonpkg.Lane{
		ID: "lane-restart-input", ParentAttachmentID: "parent", Product: "claude", Name: "worker", Cwd: root,
		NativeSessionID: "00000000-0000-0000-0000-000000000001", State: "terminal",
	}
	oldTurn := daemonpkg.Turn{ID: "old-turn", LaneID: lane.ID, Sequence: 1, State: "terminal", Outcome: "completed", CompletedAt: time.Now().UnixMilli()}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[lane.ID] = lane
	catalog.Turns[oldTurn.ID] = oldTurn
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	inputEngine, err := daemonpkg.NewLaneInputEngine(runtime.State(), filepath.Join(root, "lane-input-spool"), daemonpkg.DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.AdmitWithID("restart-receipt", lane.ID, []byte("restart-body"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after.Catalog.LaneInputs[receipt.ReceiptID].State == daemonpkg.ReceiptRetired {
			terminalTurns := 0
			for _, turn := range after.Catalog.Turns {
				if turn.LaneID == lane.ID && turn.State == "terminal" {
					terminalTurns++
				}
			}
			if terminalTurns != 2 {
				t.Fatalf("terminal turns = %d, want old debt plus receipt turn", terminalTurns)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart receipt did not retire: %+v", after.Catalog.LaneInputs[receipt.ReceiptID])
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "restart-body\n" {
		t.Fatalf("restart native prompts = %q", body)
	}
}

func TestQueuedUnacknowledgedInitialInputWithDeadOwnerArchivesWithoutDispatch(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["collision-lane"] = daemonpkg.Lane{ID: "collision-lane", Product: "qwen", State: "terminal"}
	catalog.Turns["collision-turn"] = daemonpkg.Turn{
		ID: "collision-turn", LaneID: "collision-lane", Sequence: 1, State: "terminal", Outcome: "completed",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "dead-owner-initial"})
	laneID := laneIDForInitialReceipt(receiptID)
	queued, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		receiptID,
		daemonpkg.Lane{ID: laneID, ParentAttachmentID: "dead-parent", Product: "qwen", Name: "never-dispatched", Cwd: t.TempDir()},
		daemonpkg.Turn{ID: "collision-turn", LaneID: laneID},
		"dead-owner-attempt",
		[]byte("must not reach native I/O"),
	)
	if err == nil || queued.State != daemonpkg.ReceiptQueued || queued.Revision != 1 {
		t.Fatalf("phase-one staged receipt=%+v err=%v", queued, err)
	}
	restarted := newHostCoordinator(context.Background(), root)
	if err := restarted.reconcileOrphanedLanes(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.Lanes[laneID].State != "archived" ||
		after.Catalog.LaneInputs[receiptID].State != daemonpkg.ReceiptRetired {
		t.Fatalf("dead-owner staged input lane=%+v receipt=%+v", after.Catalog.Lanes[laneID], after.Catalog.LaneInputs[receiptID])
	}
	for _, turn := range after.Catalog.Turns {
		if turn.LaneID == laneID {
			t.Fatalf("unacknowledged initial input created native-authorizing turn: %+v", turn)
		}
	}
}

func TestAcceptedTurnCrashWithDeadOwnerReconcilesBeforeAnyStartupKick(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "dead-owner-cas2"})
	laneID := laneIDForInitialReceipt(receiptID)
	receipt, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		receiptID,
		daemonpkg.Lane{ID: laneID, ParentAttachmentID: "dead-parent", Product: "qwen", Name: "never-kicked", Cwd: root},
		daemonpkg.Turn{ID: "accepted-before-crash", LaneID: laneID},
		"accepted-before-crash-attempt", []byte("must remain pre-native"),
	)
	if err != nil || receipt.State != daemonpkg.ReceiptDispatching {
		t.Fatalf("CAS2 receipt=%+v err=%v", receipt, err)
	}
	restarted := newHostCoordinator(context.Background(), root)
	if err := restarted.reconcileAttachmentOwners(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.Lanes[laneID].State != "archived" ||
		after.Catalog.LaneInputs[receiptID].State != daemonpkg.ReceiptRetired {
		t.Fatalf("dead-owner CAS2 lane=%+v receipt=%+v", after.Catalog.Lanes[laneID], after.Catalog.LaneInputs[receiptID])
	}
	turns := make([]daemonpkg.Turn, 0)
	for _, turn := range after.Catalog.Turns {
		if turn.LaneID == laneID {
			turns = append(turns, turn)
		}
	}
	if len(turns) != 1 || turns[0].ID != "accepted-before-crash" || turns[0].State != "terminal" ||
		!strings.Contains(turns[0].Diagnostic, "before native lane I/O") {
		t.Fatalf("startup dispatched after dead-owner CAS2: %+v", turns)
	}
}

func TestAbandonedStagedLaneIsInvisibleInertAndTimeoutRetired(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["stage-collision-lane"] = daemonpkg.Lane{ID: "stage-collision-lane", Product: "qwen", State: "terminal"}
	catalog.Turns["must-not-exist"] = daemonpkg.Turn{
		ID: "must-not-exist", LaneID: "stage-collision-lane", Sequence: 1, State: "terminal", Outcome: "completed",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	creator := newHostCoordinator(context.Background(), root)
	inputEngine, err := creator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: "abandoned-stage"})
	laneID := laneIDForInitialReceipt(receiptID)
	queued, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		receiptID,
		daemonpkg.Lane{ID: laneID, ParentAttachmentID: "live-owner", Product: "qwen", Name: "ghost-name", Cwd: root, Persistent: true},
		daemonpkg.Turn{ID: "must-not-exist", LaneID: laneID}, "must-not-dispatch", []byte("ghost"),
	)
	if err == nil || queued.Revision != 1 {
		t.Fatalf("staged ghost receipt=%+v err=%v", queued, err)
	}
	restarted := newHostCoordinator(context.Background(), root)
	restarted.now = func() time.Time { return time.Unix(queued.AcceptedAt, 0) }
	restarted.laneInputStagingTimeout = 100 * time.Millisecond
	if err := restarted.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range buildOperatorRoster(before, runtime.Generation(), "host", false, false, nil, nil).Local {
		if entry.ID == laneID {
			t.Fatal("staged lane leaked into operator roster")
		}
	}
	peers, err := restarted.federationSnapshot(runtime, "host", "host")
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range peers {
		if peer.SessionID == laneID {
			t.Fatalf("staged lane leaked into federation: %+v", peer)
		}
	}
	restarted.mu.Lock()
	reserved := restarted.liveLaneNameLocked(runtime, daemonpkg.ManagedAttachment{ID: "live-owner"}, "ghost-name")
	restarted.mu.Unlock()
	if reserved {
		t.Fatal("staged lane publicly reserved its requested name")
	}
	listed, err := restarted.listLanes(runtime, daemonpkg.ManagedAttachment{ID: "live-owner"}, "qwen", parsedLaneCommand{all: true})
	if err != nil {
		t.Fatal(err)
	}
	if lanes := listed["lanes"].([]map[string]any); len(lanes) != 0 {
		t.Fatalf("staged lane leaked into lane list: %#v", lanes)
	}
	if _, err := restarted.resolveLaneActor(
		runtime, daemonpkg.ManagedAttachment{ID: "live-owner"}, "qwen", laneID, true,
	); err == nil {
		t.Fatal("staged lane resolved through public discovery")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after.Catalog.LaneInputs[receiptID].State == daemonpkg.ReceiptRetired {
			if after.Catalog.Lanes[laneID].State != "archived" {
				t.Fatalf("timeout retirement lane=%+v turns=%+v", after.Catalog.Lanes[laneID], after.Catalog.Turns)
			}
			for _, turn := range after.Catalog.Turns {
				if turn.LaneID == laneID {
					t.Fatalf("staged timeout created a dispatch turn: %+v", turn)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("staged lane was not timeout-retired: %+v", after.Catalog.LaneInputs[receiptID])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLaneInputRestartRejectsStaleCodexNativeAnchorAsAmbiguous(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	lane := daemonpkg.Lane{
		ID: "lane-stale-native", ParentAttachmentID: "parent", Product: "codex", Name: "worker", Cwd: root,
		NativeSessionID: "00000000-0000-0000-0000-000000000099", State: "running",
	}
	turn := daemonpkg.Turn{
		ID: "stale-native-turn", LaneID: lane.ID, Sequence: 1, State: "dispatched",
		NativeDispatchID: "native-turn-that-no-longer-exists", StartedAt: time.Now().UnixMilli(),
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[lane.ID] = lane
	catalog.Turns[turn.ID] = turn
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	inputEngine, err := daemonpkg.NewLaneInputEngine(runtime.State(), filepath.Join(root, "lane-input-spool"), daemonpkg.DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.AdmitWithID("stale-native-receipt", lane.ID, []byte("must-not-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputEngine.MarkDispatching(receipt.ReceiptID, turn.ID, "stale-native-attempt"); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.openCodex = func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		return nil, errors.New("native session gone")
	}
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	recoveredLane := after.Catalog.Lanes[lane.ID]
	recoveredTurn := after.Catalog.Turns[turn.ID]
	recoveredReceipt := after.Catalog.LaneInputs[receipt.ReceiptID]
	if recoveredLane.State != "terminal" || recoveredTurn.Outcome != "interrupted" ||
		!strings.Contains(recoveredTurn.Diagnostic, "native session gone") {
		t.Fatalf("stale native anchor recovery = lane %+v turn %+v", recoveredLane, recoveredTurn)
	}
	if recoveredReceipt.State != daemonpkg.ReceiptAmbiguous || recoveredReceipt.AmbiguityCause != daemonpkg.AmbiguityNativeAcceptanceUnproven {
		t.Fatalf("stale native receipt = %+v", recoveredReceipt)
	}
}

func TestCodexRestartAfterExactStartAckRecoversAndRetiresInjectedReceipt(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	lane := daemonpkg.Lane{
		ID: "lane-crash-after-start", ParentAttachmentID: "parent", Product: "codex", Name: "worker", Cwd: root,
		NativeSessionID: "00000000-0000-0000-0000-000000000088", State: "idle",
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[lane.ID] = lane
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	inputEngine, err := daemonpkg.NewLaneInputEngine(runtime.State(), filepath.Join(root, "lane-input-spool"), daemonpkg.DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.AdmitWithID("crash-after-start-receipt", lane.ID, []byte("already accepted"))
	if err != nil {
		t.Fatal(err)
	}
	turn := daemonpkg.Turn{ID: "crash-after-start-turn", LaneID: lane.ID}
	if _, err := inputEngine.AcceptTurnAndMarkDispatching(receipt.ReceiptID, lane, turn, "crash-after-start-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := inputEngine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, daemonpkg.NativeAcceptanceRef{
		NativeSessionID: lane.NativeSessionID, NativeMessageID: "accepted-native-turn", AcceptedAt: receipt.AcceptedAt,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog = snapshot.Catalog
	recoveredLane := catalog.Lanes[lane.ID]
	recoveredLane.State = "running"
	catalog.Lanes[lane.ID] = recoveredLane
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.openCodex = func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		return nil, errors.New("native session gone after exact acceptance")
	}
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Catalog.LaneInputs[receipt.ReceiptID]; got.State != daemonpkg.ReceiptRetired || got.NativeAcceptance == nil {
		t.Fatalf("exact accepted receipt was detached after restart: %+v", got)
	}
	terminalTurn := after.Catalog.Turns[turn.ID]
	if terminalTurn.Outcome != "interrupted" || !strings.Contains(terminalTurn.Diagnostic, "native session gone") {
		t.Fatalf("fail-closed exact-ack recovery turn = %+v", terminalTurn)
	}
}

func TestCodexLaneRecoveryRejectsReturnedIdentityAndCwd(t *testing.T) {
	root := shortDaemonTestRoot(t)
	threadID := "00000000-0000-0000-0000-000000000001"
	for name, live := range map[string]bridge.CodexNativeThread{
		"identity": {ID: "00000000-0000-0000-0000-000000000002", Cwd: root, Status: "active"},
		"cwd":      {ID: threadID, Cwd: t.TempDir(), Status: "active"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCodexLaneReattachment(threadID, root, live); err == nil || !strings.Contains(err.Error(), "native session gone") {
				t.Fatalf("reattachment mismatch error = %v", err)
			}
		})
	}
}

func TestLaneInputRestartMarksEveryCompetingDispatchIntentAmbiguous(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	lane := daemonpkg.Lane{ID: "lane-competing", Product: "qwen", NativeSessionID: "native", State: "running"}
	turn := daemonpkg.Turn{ID: "active-turn", LaneID: lane.ID, Sequence: 1, State: "dispatched"}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[lane.ID], catalog.Turns[turn.ID] = lane, turn
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := daemonpkg.NewLaneInputEngine(runtime.State(), filepath.Join(root, "lane-input-spool"), daemonpkg.DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"competing-receipt-a", "competing-receipt-b"}
	for _, id := range ids {
		receipt, err := engine.AdmitWithID(id, lane.ID, []byte(id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.MarkDispatching(receipt.ReceiptID, turn.ID, "attempt-"+id); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if row := after.Catalog.LaneInputs[id]; row.State != daemonpkg.ReceiptAmbiguous {
			t.Fatalf("competing receipt %s = %+v", id, row)
		}
	}
}

func TestLaneInputRestartClassifiesDispatchingReceiptWithMissingSpool(t *testing.T) {
	root := shortDaemonTestRoot(t)
	spoolRoot := filepath.Join(root, "lane-input-spool")
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	lane := daemonpkg.Lane{ID: "lane-missing-spool", Product: "qwen", NativeSessionID: "native", State: "running"}
	turn := daemonpkg.Turn{ID: "missing-spool-turn", LaneID: lane.ID, Sequence: 1, State: "dispatched"}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[lane.ID], catalog.Turns[turn.ID] = lane, turn
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := daemonpkg.NewLaneInputEngine(runtime.State(), spoolRoot, daemonpkg.DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.AdmitWithID("missing-spool-receipt", lane.ID, []byte("lost body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MarkDispatching(receipt.ReceiptID, turn.ID, "missing-spool-attempt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(spoolRoot, receipt.SpoolObjectID)); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	row := after.Catalog.LaneInputs[receipt.ReceiptID]
	if row.State != daemonpkg.ReceiptAmbiguous || after.Catalog.CleanupDebts["lane-input-"+receipt.ReceiptID].Operation != "retire-lane-input" {
		t.Fatalf("missing spool classification = receipt %+v debts %+v", row, after.Catalog.CleanupDebts)
	}
}

func TestLaneResumeRefusesUncollectedDurableTurnWithoutMutation(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "grok", Name: "worker",
		NativeSessionID: "native", Cwd: root, Groups: []string{"shared"}, State: "terminal",
	}
	catalog.Turns["owed-turn"] = daemonpkg.Turn{
		ID: "owed-turn", LaneID: "lane", Sequence: 1, State: "terminal", Outcome: "completed",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes["lane"] = &laneActor{
		id: "lane", parentID: "parent", product: "grok", name: "worker", cwd: root,
		nativeID: "native", groups: []string{"shared"}, turnID: "owed-turn", state: "terminal", done: closedLaneDone(),
	}
	parent := daemonpkg.ManagedAttachment{ID: "parent", Cwd: root, Groups: []string{"shared"}, State: "attached"}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.resumeLane(context.Background(), runtime, parent, "grok", parsedLaneCommand{target: "lane"}, "must not dispatch", "resume-debt-receipt")
	if err == nil || !strings.Contains(err.Error(), "collect outstanding grok lane turn owed-turn before resume") {
		t.Fatalf("resume with collection debt error = %v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Catalog, after.Catalog) {
		t.Fatalf("resume with collection debt mutated durable state:\nbefore=%+v\nafter=%+v", before.Catalog, after.Catalog)
	}
}

func TestLaneWaitCollectsOldestTerminalTurnExactlyOnce(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "qwen", NativeSessionID: "native", State: "terminal",
	}
	catalog.Turns["first"] = daemonpkg.Turn{
		ID: "first", LaneID: "lane", Sequence: 1, State: "terminal", Outcome: "completed", Result: "first result",
	}
	catalog.Turns["second"] = daemonpkg.Turn{
		ID: "second", LaneID: "lane", Sequence: 2, State: "terminal", Outcome: "failed", Diagnostic: "second diagnostic",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "lane", product: "qwen", nativeID: "native", turnID: "second", state: "terminal", done: closedLaneDone(),
	}
	first, err := coordinator.waitLaneActor(context.Background(), runtime, actor)
	if err != nil {
		t.Fatal(err)
	}
	if first["turn_id"] != "first" || first["result"] != "first result" {
		t.Fatalf("first collection = %#v", first)
	}
	middle, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if middle.Catalog.Turns["first"].State != "collected" || middle.Catalog.Turns["second"].State != "terminal" ||
		middle.Catalog.Lanes["lane"].State != "terminal" {
		t.Fatalf("middle cursor state = lane=%+v first=%+v second=%+v",
			middle.Catalog.Lanes["lane"], middle.Catalog.Turns["first"], middle.Catalog.Turns["second"])
	}
	second, err := coordinator.waitLaneActor(context.Background(), runtime, actor)
	if err != nil {
		t.Fatal(err)
	}
	if second["turn_id"] != "second" || second["diagnostic"] != "second diagnostic" || second["exit"] != 1 {
		t.Fatalf("second collection = %#v", second)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.Turns["first"].State != "collected" || after.Catalog.Turns["second"].State != "collected" ||
		after.Catalog.Lanes["lane"].State != "idle" || actor.state != "idle" {
		t.Fatalf("final cursor state = actor=%s lane=%+v first=%+v second=%+v",
			actor.state, after.Catalog.Lanes["lane"], after.Catalog.Turns["first"], after.Catalog.Turns["second"])
	}
}

func TestLaneWaitRejectsIdleLaneWithoutCollectionDebtImmediately(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "qwen", NativeSessionID: "native", State: "idle",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{id: "lane", product: "qwen", nativeID: "native", state: "idle", done: closedLaneDone()}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err = coordinator.waitLaneActor(ctx, runtime, actor)
	if err == nil || !strings.Contains(err.Error(), "idle lane has no collectable turn") {
		t.Fatalf("empty idle wait error = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("empty idle wait consumed its caller deadline")
	}
}

func TestNewLaneRemainsPreparingUntilItsNativeWorkerStarts(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{id: "lane", parentID: "parent", product: "qwen", name: "worker", cwd: "/workspace", turnID: "turn", state: "preparing"}
	inputEngine, err := coordinator.laneInputEngine(runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		"command-preparing-lifecycle", durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), "preparing-attempt", []byte("work"),
	)
	if err != nil {
		t.Fatal(err)
	}
	actor.inputSequence, actor.activeReceiptID = receipt.Sequence, receipt.ReceiptID
	if err := coordinator.commitLaneAuthorization(runtime, actor, "preparing"); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Catalog.Lanes[actor.id].State; got != "preparing" {
		t.Fatalf("new lane state = %q, want preparing", got)
	}
}

func TestParentDetachImmediatelyArchivesIdleNonPersistentLanes(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes["idle"] = &laneActor{id: "idle", parentID: "parent", product: "grok", state: "idle", done: closedLaneDone()}
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes["idle"] = daemonpkg.Lane{ID: "idle", ParentAttachmentID: "parent", Product: "grok", State: "idle"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator.archiveIdleLanesForParent(runtime, "parent")
	after, _ := runtime.State().Read()
	if coordinator.lanes["idle"].state != "archived" || after.Catalog.Lanes["idle"].State != "archived" {
		t.Fatalf("idle orphan survived: actor=%s durable=%s", coordinator.lanes["idle"].state, after.Catalog.Lanes["idle"].State)
	}
}

func TestParentDetachRetiresActiveLaneAndPreservesPersistentLane(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	cancelled := make(chan struct{})
	active := &laneActor{
		id: "active", parentID: "parent", product: "qwen", state: "running", done: make(chan struct{}),
		cancel: func() { close(cancelled) },
	}
	persistent := &laneActor{id: "persistent", parentID: "parent", product: "grok", state: "idle", persistent: true, done: closedLaneDone()}
	coordinator.lanesLoaded = true
	coordinator.lanes[active.id], coordinator.lanes[persistent.id] = active, persistent
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[active.id] = daemonpkg.Lane{ID: active.id, ParentAttachmentID: "parent", Product: "qwen", State: "running"}
	catalog.Lanes[persistent.id] = daemonpkg.Lane{ID: persistent.id, ParentAttachmentID: "parent", Product: "grok", State: "idle", Persistent: true}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator.archiveIdleLanesForParent(runtime, "parent")
	select {
	case <-cancelled:
	default:
		t.Fatal("active orphan was not interrupted")
	}
	after, _ := runtime.State().Read()
	if active.state != "retiring" || after.Catalog.Lanes[active.id].State != "retiring" {
		t.Fatalf("active orphan state actor=%s durable=%s", active.state, after.Catalog.Lanes[active.id].State)
	}
	if persistent.state != "idle" || after.Catalog.Lanes[persistent.id].State != "idle" {
		t.Fatalf("persistent lane was changed actor=%s durable=%s", persistent.state, after.Catalog.Lanes[persistent.id].State)
	}
}

func TestActiveLaneCanActAsAttestedParentForChildLaneCommands(t *testing.T) {
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes["parent-lane"] = &laneActor{
		id: "parent-lane", product: "qwen", name: "parent", cwd: root,
		groups: []string{"shared"}, permission: "bypassPermissions",
		// The raw capability is intentionally process-local and is unavailable
		// after daemon restart. Runtime authorization already verified its
		// durable digest before dispatching this parent command.
		state: "running", done: make(chan struct{}),
	}
	payload, err := json.Marshal(laneCommandEnvelope{
		Product: "codex", Command: "doctor", SourceAttachmentID: "parent-lane",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.handleLaneCommand(context.Background(), runtime, daemonpkg.ControlRequest{Payload: payload})
	if err != nil {
		t.Fatalf("active lane parent doctor: %v", err)
	}
	if !strings.Contains(string(result), `"contract_version":2`) {
		t.Fatalf("lane parent doctor result = %s", result)
	}
}

func TestLaneDoctorDoesNotRequirePeerAttachment(t *testing.T) {
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	payload, err := json.Marshal(laneCommandEnvelope{Product: "codex", Command: "doctor", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.handleLaneCommand(context.Background(), nil, daemonpkg.ControlRequest{Payload: payload})
	if err != nil {
		t.Fatalf("standalone lane doctor: %v", err)
	}
	if !strings.Contains(string(result), `"ready":true`) || !strings.Contains(string(result), `"authority":"daemon"`) {
		t.Fatalf("standalone lane doctor result = %s", result)
	}
}

func TestLaneDoctorProjectsEstablishedGrokReadinessFields(t *testing.T) {
	root := shortDaemonTestRoot(t)
	grok := filepath.Join(root, "grok")
	if err := os.WriteFile(grok, []byte("#!/bin/sh\nprintf '%s\\n' 'grok 1.0.13'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := inspectLaneProductReadiness(context.Background(), "grok", grok, root)
	ready, _ := report["ready"].(bool)
	available, _ := report["grok_available"].(bool)
	supervisor, _ := report["supervisor_reachable"].(bool)
	if !ready || !available || report["grok_path"] != grok ||
		report["grok_version"] != "grok 1.0.13" || !supervisor {
		t.Fatalf("Grok readiness projection = %#v", report)
	}
}

func TestQwenLaneDoctorProjectionPreservesCompleteLegacyEvidence(t *testing.T) {
	ready := map[qwenreadiness.ParserContract]qwenreadiness.State{
		qwenreadiness.ParserDualOutput: qwenreadiness.StateReady, qwenreadiness.ParserNativeDefault: qwenreadiness.StateReady,
		qwenreadiness.ParserDefault: qwenreadiness.StateReady, qwenreadiness.ParserYolo: qwenreadiness.StateReady,
		qwenreadiness.ParserPlan: qwenreadiness.StateReady,
	}
	report := qwenLaneDoctorProjection(qwenreadiness.Report{
		Ready: true, ResolvedExecutable: "/opt/qwen", Version: "0.22.3", MinimumVersion: "0.21.15",
		MinimumVersionOK: true, PackageIdentityOK: true, ParserContracts: ready,
		ACPContract: qwenreadiness.StateReady, ArchiveContract: qwenreadiness.StateReady,
		WorkspaceTrust: qwenreadiness.StateReady, IntegrationReady: true,
		CredentialConfigurationState: qwenreadiness.StateReady,
	})
	for key, want := range map[string]any{
		"ready": true, "qwen_available": true, "qwen_path": "/opt/qwen", "qwen_version": "0.22.3",
		"minimum_version_ok": true, "package_identity_ok": true, "interactive_contract": qwenreadiness.StateReady,
		"acp_contract": qwenreadiness.StateReady, "archive_contract": qwenreadiness.StateReady,
		"workspace_trust": qwenreadiness.StateReady, "integration_ready": true,
	} {
		if report[key] != want {
			t.Fatalf("Qwen readiness %s = %#v, want %#v; report=%#v", key, report[key], want, report)
		}
	}
}

func TestIdleLaneRemainsLiveOwnerOfItsChildUntilParentArchive(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	parent := &laneActor{
		id: "parent-lane", parentID: "root", product: "qwen", name: "parent", cwd: root,
		groups: []string{"shared"}, state: "idle", persistent: true, done: closedLaneDone(),
	}
	child := &laneActor{
		id: "child-lane", parentID: parent.id, product: "grok", name: "child", cwd: root,
		groups: []string{"shared"}, state: "idle", done: closedLaneDone(),
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[parent.id], coordinator.lanes[child.id] = parent, child
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[parent.id] = durableLane(parent, "idle")
	catalog.Lanes[child.id] = durableLane(child, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reconcileOrphanedLanes(runtime); err != nil {
		t.Fatal(err)
	}
	if child.state != "idle" {
		t.Fatalf("idle live parent's child state = %q, want idle", child.state)
	}
	controller := daemonpkg.ManagedAttachment{ID: "root", Cwd: root, Groups: []string{"shared"}, State: "attached"}
	if _, err := coordinator.archiveLane(runtime, controller, "qwen", parsedLaneCommand{target: parent.id}); err != nil {
		t.Fatal(err)
	}
	after, _ := runtime.State().Read()
	if parent.state != "archived" || child.state != "archived" || after.Catalog.Lanes[child.id].State != "archived" {
		t.Fatalf("archive cascade parent=%s child=%s durable_child=%s", parent.state, child.state, after.Catalog.Lanes[child.id].State)
	}
}

func TestLaneReadyKeepsAgentSessionsOwnerDistinctFromNativeSession(t *testing.T) {
	actor := &laneActor{
		id:       "agent-sessions-child-id",
		parentID: "agent-sessions-parent-id",
		nativeID: "native-vendor-session-id",
		product:  "qwen",
		state:    "running",
	}
	ready := laneReadyResult(actor)
	if ready["thread_id"] != actor.id || ready["owner_session_id"] != actor.parentID || ready["session_id"] != actor.nativeID {
		t.Fatalf("lane identity projection = %#v", ready)
	}
	if ready["owner_session_id"] == ready["session_id"] {
		t.Fatalf("Agent Sessions parent identity collapsed into native vendor session: %#v", ready)
	}
}

func TestResolveLaneActorRequiresDurableRowAndExactNativeCache(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	actor := &laneActor{
		id: "lane-routing-window", parentID: "parent", product: "qwen", name: "worker",
		cwd: root, state: "idle", done: closedLaneDone(),
	}
	coordinator.lanes[actor.id] = actor
	parent := daemonpkg.ManagedAttachment{ID: "parent", Cwd: root, State: "attached"}

	// startLane installs its process-local reservation before the durable
	// CreateLaneAdmit CAS. That reservation is name authority only; it must not
	// become a routeable lane actor.
	if _, err := coordinator.resolveLaneActor(runtime, parent, actor.product, actor.id, true); err == nil {
		t.Fatal("pre-create actor without a durable lane row was routeable")
	}

	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = daemonpkg.Lane{
		ID: actor.id, ParentAttachmentID: actor.parentID, Product: actor.product, Name: actor.name,
		Cwd: actor.cwd, State: "idle",
	}
	unbound, err := runtime.State().Commit(snapshot.Revision, catalog)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := coordinator.resolveLaneActor(runtime, parent, actor.product, actor.id, true)
	if err != nil || resolved != actor {
		t.Fatalf("exact unbound durable/cache actor did not route by LaneID: actor=%p resolved=%p err=%v", actor, resolved, err)
	}

	boundCatalog := unbound.Catalog
	durableLane := boundCatalog.Lanes[actor.id]
	durableLane.NativeSessionID = "durable-native"
	boundCatalog.Lanes[actor.id] = durableLane
	if _, err := runtime.State().Commit(unbound.Revision, boundCatalog); err != nil {
		t.Fatal(err)
	}

	// A successful durable bind can be visible before the process-local cache
	// assignment. The empty cache must not route as the bound durable session.
	if _, err := coordinator.resolveLaneActor(runtime, parent, actor.product, actor.id, true); err == nil {
		t.Fatal("actor with stale native-session cache was routeable")
	}
	coordinator.mu.Lock()
	actor.nativeID = "durable-native"
	coordinator.mu.Unlock()
	resolved, err = coordinator.resolveLaneActor(runtime, parent, actor.product, actor.id, true)
	if err != nil || resolved != actor {
		t.Fatalf("exact durable/cache actor did not route: actor=%p resolved=%p err=%v", actor, resolved, err)
	}
}

func TestLaneActorHydrationRestoresNativePolicyAndTerminalResult(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "qwen", Name: "worker", NativeSessionID: "native",
		Cwd: "/workspace", Groups: []string{"effective"}, ExplicitGroups: []string{"explicit"}, InheritGroups: true,
		PermissionMode: "bypassPermissions", ApprovalPolicy: "never", Sandbox: "read-only", Effort: "high", Schema: "/schema",
		Arguments: []string{"--model", "model"}, Persistent: true, AutoArchive: true, AutoArchiveDelayMS: 2500, State: "terminal",
	}
	catalog.Turns["turn"] = daemonpkg.Turn{ID: "turn", LaneID: "lane", Sequence: 3, State: "terminal", Outcome: "interrupted", Result: "partial", Diagnostic: "daemon restarted"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	if err := coordinator.ensureLaneActors(runtime); err != nil {
		t.Fatal(err)
	}
	actor := coordinator.lanes["lane"]
	if actor == nil || actor.nativeID != "native" || actor.permission != "bypassPermissions" || actor.approvalPolicy != "never" ||
		actor.sandbox != "read-only" || actor.effort != "high" || actor.schema != "/schema" || !actor.inheritGroups ||
		!reflect.DeepEqual(actor.explicitGroups, []string{"explicit"}) || !reflect.DeepEqual(actor.arguments, []string{"--model", "model"}) ||
		!actor.persistent || !actor.autoArchive || actor.autoArchiveDelay != 2500*time.Millisecond || actor.turnID != "turn" ||
		actor.outcome != "interrupted" || actor.result != "partial" || actor.failure != "daemon restarted" {
		t.Fatalf("hydrated lane actor = %+v", actor)
	}
}

func TestRestartCatalogPreservesOnlyRecoverableNativeCodexTurn(t *testing.T) {
	catalog := daemonpkg.Catalog{
		Lanes: map[string]daemonpkg.Lane{
			"codex": {ID: "codex", Product: "codex", NativeSessionID: "00000000-0000-0000-0000-000000000001", State: "running"},
			"qwen":  {ID: "qwen", Product: "qwen", NativeSessionID: "native-qwen", State: "running"},
		},
		Turns: map[string]daemonpkg.Turn{
			"codex-turn": {ID: "codex-turn", LaneID: "codex", State: "dispatched", NativeDispatchID: "native-turn"},
			"qwen-turn":  {ID: "qwen-turn", LaneID: "qwen", State: "dispatched"},
		},
	}
	daemonpkg.ReconcileRestartedLaneCatalog(&catalog, 1234, func(lane daemonpkg.Lane, _ daemonpkg.Turn) bool {
		return lane.Product == "codex" && lane.NativeSessionID != ""
	}, "Agent Sessions daemon restarted during the accepted turn")
	if catalog.Lanes["codex"].State != "running" || catalog.Turns["codex-turn"].State != "dispatched" ||
		catalog.Turns["codex-turn"].NativeDispatchID != "native-turn" {
		t.Fatalf("recoverable Codex lane was terminalized: lane=%+v turn=%+v", catalog.Lanes["codex"], catalog.Turns["codex-turn"])
	}
	qwenLane, qwenTurn := catalog.Lanes["qwen"], catalog.Turns["qwen-turn"]
	if qwenLane.State != "terminal" || qwenTurn.State != "terminal" || qwenTurn.Outcome != "interrupted" || qwenTurn.CompletedAt != 1234 {
		t.Fatalf("non-recoverable Qwen lane was not interrupted: lane=%+v turn=%+v", qwenLane, qwenTurn)
	}
}

func TestCollectedLaneAutoArchivesAtDurableDeadline(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "lane", parentID: "parent", product: "grok", name: "worker", state: "terminal", outcome: "completed", result: "done",
		turnID: "turn", done: closedLaneDone(), autoArchive: true, autoArchiveDelay: 20 * time.Millisecond,
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "terminal")
	catalog.Turns[actor.turnID] = daemonpkg.Turn{ID: actor.turnID, LaneID: actor.id, Sequence: 1, State: "terminal", Outcome: "completed", Result: "done"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.waitLaneActor(context.Background(), runtime, actor); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		after, readErr := runtime.State().Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after.Catalog.Lanes[actor.id].State == "archived" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("collected lane did not auto-archive")
}

func TestCodexArchiveReaffirmsAlreadyArchivedAndRetriesNativeCleanup(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "lane", parentID: "parent", product: "codex", name: "worker", cwd: root,
		nativeID: "00000000-0000-0000-0000-000000000001", groups: []string{"shared"},
		state: "archived", done: closedLaneDone(),
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	openCalls := 0
	coordinator.openCodex = func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		openCalls++
		return nil, errors.New("native cleanup still unavailable")
	}
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "archived")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	controller := daemonpkg.ManagedAttachment{ID: "parent", Cwd: root, Groups: []string{"shared"}, State: "attached"}
	result, err := coordinator.archiveLane(runtime, controller, "codex", parsedLaneCommand{target: actor.id})
	if err == nil || !strings.Contains(err.Error(), "native cleanup still unavailable") {
		t.Fatalf("idempotent archive cleanup error: %v", err)
	}
	alreadyArchived, _ := result["already_archived"].(bool)
	if !alreadyArchived || openCalls != 1 {
		t.Fatalf("idempotent archive result=%#v native opens=%d", result, openCalls)
	}
}

func TestArchiveRetryConfirmsNativeAbsenceAndResolvesDurableDebt(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{
		id: "lane-qwen-archive-debt", parentID: "parent", product: "qwen", name: "worker", cwd: root,
		state: "idle", done: closedLaneDone(),
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "idle")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, runErr := coordinator.qwenLanes.Run(context.Background(), daemonpkg.QwenACPLaneRequest{
			LaneID: actor.id, Prompt: "active",
		}, func(context.Context, daemonpkg.QwenACPLaneRequest) (daemonpkg.NativeACPLaneResult, error) {
			close(started)
			<-release
			return daemonpkg.NativeACPLaneResult{}, nil
		})
		finished <- runErr
	}()
	<-started
	controller := daemonpkg.ManagedAttachment{ID: "parent", Cwd: root, Groups: []string{"shared"}, State: "attached"}
	if _, err := coordinator.archiveLane(runtime, controller, "qwen", parsedLaneCommand{target: actor.id}); err == nil ||
		!strings.Contains(err.Error(), "refuse to archive an active qwen ACP client") {
		t.Fatalf("first archive error = %v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	debtID := "lane-native-archive-" + actor.id
	if after.Catalog.Lanes[actor.id].State != "cleanup-debt" || after.Catalog.CleanupDebts[debtID].Operation != "archive-native-lane" {
		t.Fatalf("native cleanup debt not durable: lane=%+v debts=%+v", after.Catalog.Lanes[actor.id], after.Catalog.CleanupDebts)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.archiveLane(runtime, controller, "qwen", parsedLaneCommand{target: actor.id}); err != nil {
		t.Fatalf("archive cleanup retry: %v", err)
	}
	after, err = runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.Lanes[actor.id].State != "archived" {
		t.Fatalf("cleanup retry lane = %+v", after.Catalog.Lanes[actor.id])
	}
	if _, exists := after.Catalog.CleanupDebts[debtID]; exists {
		t.Fatalf("resolved native archive debt remains: %+v", after.Catalog.CleanupDebts)
	}
}

func TestWaitBlocksUntilInterruptReachesTerminalOutcome(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	done := make(chan struct{})
	actor := &laneActor{id: "lane", product: "qwen", state: "interrupting", turnID: "turn", done: done}
	coordinator.lanes[actor.id] = actor
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = daemonpkg.Lane{ID: actor.id, Product: actor.product, State: "interrupting"}
	catalog.Turns[actor.turnID] = daemonpkg.Turn{ID: actor.turnID, LaneID: actor.id, Sequence: 1, State: "dispatched"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		coordinator.mu.Lock()
		actor.state, actor.outcome = "terminal", "interrupted"
		if err := coordinator.markLaneTerminal(runtime, actor); err != nil {
			actor.outcome = "failed"
			actor.failure = err.Error()
		}
		close(done)
		coordinator.mu.Unlock()
	}()
	result, err := coordinator.waitLaneActor(context.Background(), runtime, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result["outcome"] != "interrupted" {
		t.Fatalf("interrupt wait result = %#v", result)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Catalog.Lanes[actor.id].State; got != "idle" {
		t.Fatalf("durable lane state after wait = %q, want idle", got)
	}
	if got := after.Catalog.Turns[actor.turnID].State; got != "collected" {
		t.Fatalf("durable turn state after wait = %q, want collected", got)
	}
}

func TestACPWorkerPublishesNativeIdentityBeforeTurnTerminal(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{id: "lane", parentID: "parent", product: "qwen", state: "running", turnID: "turn"}
	coordinator.lanes[actor.id] = actor
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = daemonpkg.Lane{ID: actor.id, ParentAttachmentID: actor.parentID, Product: actor.product, State: "running"}
	catalog.Turns[actor.turnID] = daemonpkg.Turn{ID: actor.turnID, LaneID: actor.id, Sequence: 1, State: "dispatched"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, "native-session"); err != nil {
		t.Fatal(err)
	}
	after, _ := runtime.State().Read()
	if actor.nativeID != "native-session" || after.Catalog.Lanes[actor.id].NativeSessionID != "native-session" || after.Catalog.Lanes[actor.id].State != "running" {
		t.Fatalf("native identity publication actor=%+v durable=%+v", actor, after.Catalog.Lanes[actor.id])
	}
}

func TestRecordLaneNativeIDUpdatesActorOnlyAfterDurableAcceptance(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	actor := &laneActor{id: "lane-native-cache", product: "qwen", state: "running"}
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = daemonpkg.Lane{ID: actor.id, Product: actor.product, NativeSessionID: "native-durable", State: "running"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, "native-rejected"); err == nil {
		t.Fatal("durable authority accepted a replacement native identity")
	}
	if actor.nativeID != "" {
		t.Fatalf("actor cache advanced despite rejected durable write: %q", actor.nativeID)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Catalog.Lanes[actor.id].NativeSessionID; got != "native-durable" {
		t.Fatalf("durable native authority changed to %q", got)
	}
}

func TestLaneCompletionOnlyCorroboratesNativeIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		completion string
		wantFailed bool
	}{
		{name: "exact-match", completion: "native-durable"},
		{name: "mismatch", completion: "native-other", wantFailed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := shortDaemonTestRoot(t)
			runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			coordinator := newHostCoordinator(context.Background(), root)
			done := make(chan struct{})
			actor := &laneActor{
				id: "lane-completion-corrob", product: "qwen", nativeID: "native-durable",
				state: "running", turnID: "turn-completion-corrob", done: done,
			}
			coordinator.lanes[actor.id] = actor
			snapshot, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			catalog := snapshot.Catalog
			catalog.Lanes[actor.id] = daemonpkg.Lane{ID: actor.id, Product: actor.product, NativeSessionID: actor.nativeID, State: "running"}
			catalog.Turns[actor.turnID] = daemonpkg.Turn{ID: actor.turnID, LaneID: actor.id, Sequence: 1, State: "dispatched"}
			if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
				t.Fatal(err)
			}
			coordinator.completeLaneTurn(runtime, actor, actor.turnID, done, laneTurnCompletion{
				outcome: "completed", result: "result", nativeID: test.completion,
			})
			if actor.nativeID != "native-durable" {
				t.Fatalf("completion rewrote actor cache to %q", actor.nativeID)
			}
			after, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			if got := after.Catalog.Lanes[actor.id].NativeSessionID; got != "native-durable" {
				t.Fatalf("completion rewrote durable identity to %q", got)
			}
			turn := after.Catalog.Turns[actor.turnID]
			if test.wantFailed {
				if actor.outcome != "failed" || turn.Outcome != "failed" || !strings.Contains(turn.Diagnostic, "completion identity did not match") {
					t.Fatalf("mismatch was not truthful terminal failure: actor=%+v turn=%+v", actor, turn)
				}
			} else if actor.outcome != "completed" || turn.Outcome != "completed" {
				t.Fatalf("exact completion did not corroborate: actor=%+v turn=%+v", actor, turn)
			}
		})
	}
}

func TestCodexJSONStreamPublishesNativeIdentityBeforeProcessExit(t *testing.T) {
	var observed string
	buffer := cappedLaneBuffer{onLine: func(line []byte) error {
		observed = parseCodexStartedThreadID(line)
		return nil
	}}
	first := []byte(`{"type":"thread.started","thread_id":"01a04e44-c2f3-7761-b572-3bdd457f141c"}` + "\n" + `{"type":"item.started"`)
	if _, err := buffer.Write(first); err != nil {
		t.Fatal(err)
	}
	if observed != "01a04e44-c2f3-7761-b572-3bdd457f141c" {
		t.Fatalf("early Codex identity = %q", observed)
	}
	if _, err := buffer.Write([]byte("}\n")); err != nil {
		t.Fatal(err)
	}
	if string(buffer.Bytes()) != string(first)+"}\n" {
		t.Fatalf("captured stream = %q", buffer.Bytes())
	}
}

func TestCodexStreamIgnoresLaneMetadataUUIDs(t *testing.T) {
	laneID := "24f498a0-5367-498e-8ba5-2b7937ce3599"
	for _, line := range []string{
		`{"type":"item.completed","thread_id":"` + laneID + `"}`,
		`{"type":"tool.call","session_id":"` + laneID + `"}`,
		`{"type":"thread.started","thread_id":"not-a-uuid"}`,
	} {
		if got := parseCodexStartedThreadID([]byte(line)); got != "" {
			t.Fatalf("non-start event selected native identity %q from %s", got, line)
		}
	}
}

func closedLaneDone() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
