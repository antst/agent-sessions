package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
	codexproduct "github.com/antst/agent-sessions/internal/products/codex"
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

func TestValidateLaneGroupNamesAcceptsOnlyParentGroupsAndTheirSubgroups(t *testing.T) {
	parents := []string{"project", "session:host/parent"}
	if err := validateLaneGroupNames([]string{
		"project",
		"project/bugfixing",
		"session:host/parent/review",
	}, parents); err != nil {
		t.Fatalf("valid lane groups: %v", err)
	}

	for _, group := range []string{"bugfixing", "project:bugfixing", "project-bugfixing"} {
		err := validateLaneGroupNames([]string{group}, parents)
		if err == nil {
			t.Fatalf("group %q was accepted", group)
		}
		if !strings.Contains(err.Error(), "project/") || !strings.Contains(err.Error(), "session:host/parent/") {
			t.Fatalf("group %q error = %q", group, err)
		}
	}
}

func TestAnchorLaneGroupsCompoundsTheParentPrivateNamespace(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := &hostCoordinator{}
	host := runtime.HostID()

	root := daemonpkg.ManagedAttachment{ID: "parent", Groups: []string{"project"}}
	rootGroups, err := coordinator.anchorLaneGroups(runtime, []string{"project/bugfixing"}, root, "lane")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootGroups, []string{"project/bugfixing", "session:" + host + "/parent", "session:" + host + "/parent/lane"}) {
		t.Fatalf("root lane groups = %v", rootGroups)
	}

	nested := daemonpkg.ManagedAttachment{
		ID: "lane", Groups: []string{"session:" + host + "/parent", "session:" + host + "/parent/lane"},
	}
	nestedGroups, err := coordinator.anchorLaneGroups(runtime, nil, nested, "child")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nestedGroups, []string{"session:" + host + "/parent/lane", "session:" + host + "/parent/lane/child"}) {
		t.Fatalf("nested lane groups = %v", nestedGroups)
	}
}

func TestNativeLaneBindingReplacesTemporaryPrivateGroupBeforeRemember(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	primary := "session:" + runtime.HostID() + "/parent"
	actor := &laneActor{
		id: "temporary-lane", product: "codex", parentID: "parent", name: "worker",
		groups: []string{primary, primary + "/temporary-lane", "project/child"},
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, "native-lane"); err != nil {
		t.Fatal(err)
	}
	wantGroups := []string{"project/child", primary, primary + "/native-lane"}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("bound groups = %v, want %v", actor.groups, wantGroups)
	}
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := engine.Candidates("parent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !reflect.DeepEqual(candidates[0].SecondaryGroups, []string{"project/child"}) {
		t.Fatalf("stored candidates = %+v", candidates)
	}
	parent := daemonpkg.ManagedAttachment{ID: "parent", Groups: []string{"project"}}
	actor.explicitGroups = []string{"project/child"}
	actor.groups, err = coordinator.effectiveLaneGroups(runtime, actor, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.commitNewLane(runtime, actor); err != nil {
		t.Fatalf("resume rebuilt different candidate ownership: %v", err)
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

func TestCodexNativeBypassOptionIsCanonicalizedBeforeDriverDispatch(t *testing.T) {
	got, err := parseUnifiedLaneCommand([]string{
		"start", "--name", "worker", "--dangerously-bypass-approvals-and-sandbox", "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.permission != "bypassPermissions" || len(got.native) != 0 {
		t.Fatalf("parsed native bypass = %+v", got)
	}
	native := &delayedCodexLaneNative{}
	driver, err := codexproduct.NewLaneDriver(func() (codexproduct.LaneNative, error) { return native, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: codexproduct.ProductID, LaneID: "lane", Name: "worker", Cwd: t.TempDir(),
		PermissionMode: permissionmode.BypassPermissions,
	}); err != nil {
		t.Fatal(err)
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
	native := &delayedCodexLaneNative{setupDelay: 2 * actor.turnTimeout}
	driver, err := codexproduct.NewLaneDriver(func() (codexproduct.LaneNative, error) { return native, nil })
	if err != nil {
		t.Fatal(err)
	}
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{
		codexproduct.ProductID: driver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.commitNewLane(runtime, actor); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := coordinator.dispatchProductLaneTurn(runtime, actor, "work", false, false, driver); err != nil {
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

func TestLaneWorkerEnvironmentReplacesAmbientPeerAuthority(t *testing.T) {
	actor := &laneActor{id: "lane-id", product: "qwen", capability: "lane-capability"}
	got := laneWorkerEnvironment([]string{
		"PATH=/bin", "AGENT_SESSIONS_SESSION_ID=parent", "AGENT_SESSIONS_PRODUCT=codex",
		"AGENT_SESSIONS_LANE_CAPABILITY=old-lane", "AGENT_SESSIONS_HOST_BINARY=/old/runtime",
	}, actor)
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, wanted := range []string{"\nPATH=/bin\n", "\nAGENT_SESSIONS_SESSION_ID=lane-id\n", "\nAGENT_SESSIONS_PRODUCT=qwen\n", "\nAGENT_SESSIONS_LANE_CAPABILITY=lane-capability\n"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("lane environment %q lacks %q", joined, wanted)
		}
	}
	for _, forbidden := range []string{"parent", "old-lane", "/old/runtime"} {
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
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
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
	coordinator.archiveIdleLanesForParent(runtime, "parent")
	if coordinator.lanes["idle"].state != "archived" {
		t.Fatalf("idle orphan survived: actor=%s", coordinator.lanes["idle"].state)
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
	coordinator.archiveIdleLanesForParent(runtime, "parent")
	select {
	case <-cancelled:
	default:
		t.Fatal("active orphan was not interrupted")
	}
	if active.state != "retiring" {
		t.Fatalf("active orphan state actor=%s", active.state)
	}
	if persistent.state != "idle" {
		t.Fatalf("persistent lane was changed actor=%s", persistent.state)
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
	if parent.state != "archived" || child.state != "archived" {
		t.Fatalf("archive cascade parent=%s child=%s", parent.state, child.state)
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

func closedLaneDone() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
