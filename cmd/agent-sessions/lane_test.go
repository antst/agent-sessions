package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
		if _, err = coordinator.startLane(context.Background(), nil, parent, "grok", parsed, "prompt", false); err == nil ||
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
	if err := coordinator.commitNewLane(runtime, actor); err != nil {
		t.Fatal(err)
	}
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
	if err := coordinator.commitNewLane(runtime, actor); err != nil {
		t.Fatal(err)
	}
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
	if err := coordinator.deliverLaneMessage(runtime, actor, frame); err != nil {
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
	if err := coordinator.deliverLaneMessage(runtime, actor, "turn-one"); err != nil {
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
	if err := coordinator.deliverLaneMessage(runtime, actor, "busy-message-nonce"); err != nil {
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
	if err := coordinator.commitResumeLane(runtime, actor, false); err != nil {
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
	if err := coordinator.commitResumeLane(runtime, actor, true); err != nil {
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

func TestConcurrentIdleLaneMessagesClaimExactlyOneNativeWriter(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	actor := &laneActor{state: "idle", done: closedLaneDone()}
	const deliveries = 64
	ready := make(chan struct{})
	var dispatches atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < deliveries; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-ready
			dispatch, err := coordinator.claimLaneMessage(actor, "marker")
			if err != nil {
				t.Errorf("claim lane message: %v", err)
				return
			}
			if dispatch {
				dispatches.Add(1)
			}
		}()
	}
	close(ready)
	wait.Wait()
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if dispatches.Load() != 1 || actor.state != "preparing" || len(actor.pending) != deliveries-1 {
		t.Fatalf("concurrent claims = dispatches %d state %q pending %d", dispatches.Load(), actor.state, len(actor.pending))
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
	_, err = coordinator.resumeLane(context.Background(), runtime, parent, "grok", parsedLaneCommand{target: "lane"}, "must not dispatch")
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
	if err := coordinator.commitNewLane(runtime, actor); err != nil {
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

func TestCodexArchiveReaffirmsAlreadyArchivedWithoutNativeRedispatch(t *testing.T) {
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
		return nil, errors.New("native archive must not be redispatched")
	}
	snapshot, _ := runtime.State().Read()
	catalog := snapshot.Catalog
	catalog.Lanes[actor.id] = durableLane(actor, "archived")
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	controller := daemonpkg.ManagedAttachment{ID: "parent", Cwd: root, Groups: []string{"shared"}, State: "attached"}
	result, err := coordinator.archiveLane(runtime, controller, "codex", parsedLaneCommand{target: actor.id})
	if err != nil {
		t.Fatalf("idempotent archive: %v", err)
	}
	alreadyArchived, _ := result["already_archived"].(bool)
	if !alreadyArchived || openCalls != 0 {
		t.Fatalf("idempotent archive result=%#v native opens=%d", result, openCalls)
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
