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
	turnStartErr         error
	archived             []string
}

type steerRecordingLaneDriver struct {
	capabilities productruntime.LaneCapabilitySet
	acceptance   productruntime.NativeAcceptance
	calls        int
	interrupts   int
	turn         productruntime.NativeTurnRef
	request      productruntime.TurnStartRequest
}

type parentExitLaneDriver struct {
	opens    []productruntime.LaneOpenRequest
	archives []productruntime.NativeSessionRef
}

func (*parentExitLaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{DurableResume: true}
}
func (d *parentExitLaneDriver) Open(_ context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	d.opens = append(d.opens, request)
	return productruntime.NativeSessionRef{LaneID: request.ResumeNativeID, NativeSessionID: request.ResumeNativeID, Generation: 7}, nil
}
func (*parentExitLaneDriver) StartTurn(_ context.Context, session productruntime.NativeSessionRef, _ productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	return productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: "turn"}, nil
}
func (*parentExitLaneDriver) WaitTurn(context.Context, productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	return productruntime.NativeTerminal{Outcome: productruntime.TurnCompleted, Result: "resumed"}, nil
}
func (*parentExitLaneDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}
func (*parentExitLaneDriver) Interrupt(context.Context, productruntime.NativeTurnRef) error {
	return nil
}
func (d *parentExitLaneDriver) Archive(_ context.Context, session productruntime.NativeSessionRef) error {
	d.archives = append(d.archives, session)
	return nil
}

func (d *steerRecordingLaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return d.capabilities
}
func (*steerRecordingLaneDriver) Open(context.Context, productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	return productruntime.NativeSessionRef{}, nil
}
func (*steerRecordingLaneDriver) StartTurn(context.Context, productruntime.NativeSessionRef, productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	return productruntime.NativeTurnRef{}, nil
}
func (*steerRecordingLaneDriver) WaitTurn(context.Context, productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	return productruntime.NativeTerminal{}, nil
}
func (d *steerRecordingLaneDriver) Steer(_ context.Context, turn productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	d.calls++
	d.turn, d.request = turn, request
	return d.acceptance, nil
}
func (d *steerRecordingLaneDriver) Interrupt(context.Context, productruntime.NativeTurnRef) error {
	d.interrupts++
	return nil
}
func (*steerRecordingLaneDriver) Archive(context.Context, productruntime.NativeSessionRef) error {
	return nil
}

func TestSteerLaneDispatchesOneExactActiveNativeTurn(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	actor := &laneActor{
		id: "lane", nativeID: "ses_native", nativeTurnID: "msg_turn", nativeGeneration: 7,
		parentID: "parent", product: "kilo", name: "worker", groups: []string{"shared"},
		permission: "bypassPermissions", arguments: []string{"--model", "deepseek/deepseek-v4-flash"},
		approvalPolicy: "never", sandbox: "workspace-write", effort: "high", schema: "/schema.json",
		turnID: "turn", state: "running", done: make(chan struct{}),
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	driver := &steerRecordingLaneDriver{
		capabilities: productruntime.LaneCapabilitySet{Steer: true},
		acceptance:   productruntime.NativeAcceptance{NativeSessionID: actor.nativeID, NativeMessageID: "msg_steer"},
	}
	var err error
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"kilo": driver})
	if err != nil {
		t.Fatal(err)
	}
	parent := daemonpkg.ManagedAttachment{ID: actor.parentID, Groups: []string{"shared"}}
	result, err := coordinator.steerLane(context.Background(), runtime, parent, actor.product, parsedLaneCommand{target: actor.name}, "change course")
	if err != nil {
		t.Fatal(err)
	}
	wantTurn := productruntime.NativeTurnRef{
		NativeSessionRef: productruntime.NativeSessionRef{LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration},
		NativeTurnID:     actor.nativeTurnID,
	}
	if driver.calls != 1 || driver.turn != wantTurn || driver.request.Prompt != "change course" ||
		driver.request.PermissionMode != permissionmode.BypassPermissions ||
		!reflect.DeepEqual(driver.request.Arguments, actor.arguments) ||
		driver.request.ApprovalPolicy != actor.approvalPolicy || driver.request.Sandbox != actor.sandbox ||
		driver.request.Effort != actor.effort || driver.request.SchemaPath != actor.schema {
		t.Fatalf("steer call count=%d turn=%+v request=%+v", driver.calls, driver.turn, driver.request)
	}
	if result["type"] != "turn.steered" || result["thread_id"] != actor.id || result["session_id"] != actor.nativeID ||
		result["turn_id"] != actor.turnID || result["native_message_id"] != driver.acceptance.NativeMessageID {
		t.Fatalf("steer result = %#v", result)
	}
}

func TestSteerLaneRejectsUnsupportedAndIdleWithoutCallingDriver(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	actor := &laneActor{
		id: "lane", nativeID: "ses_native", nativeTurnID: "msg_turn", nativeGeneration: 7,
		parentID: "parent", product: "codex", name: "worker", groups: []string{"shared"},
		permission: "default", turnID: "turn", state: "running", done: make(chan struct{}),
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	driver := &steerRecordingLaneDriver{}
	var err error
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"codex": driver})
	if err != nil {
		t.Fatal(err)
	}
	parent := daemonpkg.ManagedAttachment{ID: actor.parentID, Groups: []string{"shared"}}
	if _, err := coordinator.steerLane(context.Background(), runtime, parent, actor.product, parsedLaneCommand{target: actor.id}, "change course"); err == nil || !strings.Contains(err.Error(), "do not support steer") {
		t.Fatalf("unsupported steer error = %v", err)
	}
	actor.state = "idle"
	if _, err := coordinator.steerLane(context.Background(), runtime, parent, actor.product, parsedLaneCommand{target: actor.id}, "change course"); err == nil || !strings.Contains(err.Error(), "no active native turn") {
		t.Fatalf("idle steer error = %v", err)
	}
	if driver.calls != 0 {
		t.Fatalf("rejected steer called driver %d times", driver.calls)
	}
}

func TestInterruptLaneLeavesProductWaitAsSoleTerminalAuthority(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	canceled := false
	actor := &laneActor{
		id: "lane", nativeID: "native", nativeTurnID: "native-turn", nativeGeneration: 7,
		parentID: "parent", product: "claude", name: "worker", groups: []string{"shared"},
		permission: "default", turnID: "turn", state: "running", done: make(chan struct{}),
		cancel: func() { canceled = true },
	}
	coordinator.lanesLoaded = true
	coordinator.lanes[actor.id] = actor
	driver := &steerRecordingLaneDriver{}
	var err error
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"claude": driver})
	if err != nil {
		t.Fatal(err)
	}
	parent := daemonpkg.ManagedAttachment{ID: actor.parentID, Groups: []string{"shared"}}
	result, err := coordinator.interruptLane(runtime, parent, actor.product, parsedLaneCommand{target: actor.id})
	if err != nil {
		t.Fatal(err)
	}
	if driver.interrupts != 1 || canceled {
		t.Fatalf("interrupts = %d, engine canceled wait = %v", driver.interrupts, canceled)
	}
	if result["type"] != "turn.interrupting" || actor.state != "interrupting" {
		t.Fatalf("result = %#v, actor = %+v", result, actor)
	}
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

func (n *delayedCodexLaneNative) PrepareLaneThread(ctx context.Context, id, _, _, _ string) (bridge.CodexNativeThread, error) {
	_, n.setupHadDeadline = ctx.Deadline()
	return bridge.CodexNativeThread{ID: id}, nil
}

func (n *delayedCodexLaneNative) StartLaneTurn(ctx context.Context, _ bridge.CodexLaneTurnRequest) (string, error) {
	_, n.turnStartHadDeadline = ctx.Deadline()
	if n.turnStartErr != nil {
		return "", n.turnStartErr
	}
	return "native-turn", nil
}

func (*delayedCodexLaneNative) WaitLaneTurn(ctx context.Context, threadID, turnID string) (bridge.CodexLaneTurnResult, error) {
	<-ctx.Done()
	return bridge.CodexLaneTurnResult{ThreadID: threadID, TurnID: turnID}, ctx.Err()
}

func (*delayedCodexLaneNative) InterruptLaneTurn(context.Context, string, string) error { return nil }
func (*delayedCodexLaneNative) SendMessage(context.Context, string, string) (string, error) {
	return "started", nil
}
func (n *delayedCodexLaneNative) ArchiveThread(_ context.Context, id string) error {
	n.archived = append(n.archived, id)
	return nil
}

func TestProductLaneTurnFailureLeavesNativeSessionForExplicitLifecycle(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	native := &delayedCodexLaneNative{turnStartErr: errors.New("native turn refused")}
	driver, err := codexproduct.NewLaneDriver(func() (codexproduct.LaneNative, error) { return native, nil })
	if err != nil {
		t.Fatal(err)
	}
	actor := &laneActor{
		id: "lane", parentID: "parent", product: "codex", name: "worker", cwd: root,
		permission: "default", turnID: "turn", state: "preparing", done: make(chan struct{}),
	}
	coordinator.lanes[actor.id] = actor
	if err := coordinator.dispatchProductLaneTurn(runtime, actor, "work", driver); !errors.Is(err, native.turnStartErr) {
		t.Fatalf("dispatch error = %v", err)
	}
	if actor.state != "terminal" || actor.nativeID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("terminal actor = %+v", actor)
	}
	if len(native.archived) != 0 {
		t.Fatalf("turn failure archived native sessions %v", native.archived)
	}
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"codex": driver})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.archiveNativeLane(actor); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(native.archived, []string{actor.nativeID}) {
		t.Fatalf("explicit archive calls = %v", native.archived)
	}
}

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

func TestLaneInvocationCwdUsesOnlyTheCurrentInvocation(t *testing.T) {
	parent := t.TempDir()
	explicit := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, parent, requested, want string
	}{
		{name: "explicit overrides empty candidate", requested: explicit, want: explicit},
		{name: "omitted uses current parent", parent: parent, want: parent},
		{name: "relative uses current parent", parent: parent, requested: "child", want: child},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := laneInvocationCwd(test.parent, test.requested)
			if err != nil || got != test.want {
				t.Fatalf("lane cwd = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := laneInvocationCwd("", "missing-relative"); err == nil {
		t.Fatal("relative cwd without a current parent cwd succeeded")
	}
	if _, err := laneInvocationCwd(parent, "missing"); err == nil {
		t.Fatal("missing lane cwd succeeded")
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
	coordinator.lanes[actor.id] = actor
	coordinator.reportedLanes["native-lane"] = actor.id
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: "other", NativeSessionID: "native-lane", Generation: 1}, true); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("split driver identity returned %v", err)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: "native-lane", NativeSessionID: "native-lane", Generation: 1}, true); err != nil {
		t.Fatal(err)
	}
	if actor.id != "native-lane" || coordinator.lanes["native-lane"] != actor || coordinator.lanes["temporary-lane"] != nil ||
		coordinator.reportedLanes["native-lane"] != "native-lane" {
		t.Fatalf("actor was not re-keyed to its sole native identity: id=%q lanes=%+v reports=%+v", actor.id, coordinator.lanes, coordinator.reportedLanes)
	}
	if status := laneActorStatus(actor); status["thread_id"] != "native-lane" || status["session_id"] != "native-lane" {
		t.Fatalf("public lane identity remained split: %+v", status)
	}
	wantGroups := []string{"project/child", primary, primary + "/native-lane"}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("bound groups = %v, want %v", actor.groups, wantGroups)
	}
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := engine.Candidates("codex")
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

func TestLaneResumePermissionUsesInvocationThenLiveValueThenFreshDefault(t *testing.T) {
	if got := laneResumePermission("bypassPermissions", "default", "bypassPermissions"); got != "default" {
		t.Fatalf("explicit resume permission = %q", got)
	}
	if got := laneResumePermission("bypassPermissions", "", "default"); got != "bypassPermissions" {
		t.Fatalf("live resume permission = %q", got)
	}
	if got := laneResumePermission("", "", "default"); got != "default" {
		t.Fatalf("candidate resume permission = %q", got)
	}
}

func TestResumeRefusesLaneLiveUnderAnotherParent(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parent := daemonpkg.ManagedAttachment{ID: "parent-b", Product: "claude", Cwd: t.TempDir(), Groups: []string{"shared"}}
	coordinator.liveReports[parent.ID] = liveSessionReport{UUID: parent.ID, Product: parent.Product, Groups: parent.Groups}
	coordinator.lanes["lane"] = &laneActor{
		id: "lane", nativeID: "native", name: "worker", product: "claude", parentID: "parent-a",
		groups: []string{"shared"}, state: "idle", done: closedLaneDone(),
	}
	_, err := coordinator.resumeLane(context.Background(), runtime, parent, "claude", parsedLaneCommand{target: "worker"}, "continue")
	if err == nil || err.Error() != "lane is live under parent-a" {
		t.Fatalf("cross-parent live resume error = %v", err)
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
	coordinator.lanes[actor.id] = actor
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
	if err := coordinator.dispatchProductLaneTurn(runtime, actor, "work", driver); err != nil {
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
	got, err := laneWorkerEnvironment(nil, []string{
		"PATH=/bin", "AGENT_SESSIONS_SESSION_ID=parent", "AGENT_SESSIONS_PRODUCT=codex",
		"AGENT_SESSIONS_LANE_CAPABILITY=old-lane", "AGENT_SESSIONS_HOST_BINARY=/old/runtime",
	}, actor, productruntime.LaneCapabilitySet{CallerSuppliedSessionID: true})
	if err != nil {
		t.Fatal(err)
	}
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

func TestFreshProductGeneratedLaneEnvironmentOmitsProvisionalIdentity(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	primary := "session:" + runtime.HostID() + "/parent"
	actor := &laneActor{
		id: "provisional", parentID: "parent", product: "opencode", name: "worker", capability: "lane-capability",
		groups: []string{"project/child", primary, primary + "/provisional"},
	}
	got, err := laneWorkerEnvironment(runtime, []string{"PATH=/bin", "AGENT_SESSIONS_SESSION_ID=parent"}, actor, productruntime.LaneCapabilitySet{})
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(got, "\n") + "\n"
	if strings.Contains(joined, "\nAGENT_SESSIONS_SESSION_ID=") || strings.Contains(joined, primary+"/provisional") {
		t.Fatalf("fresh product-generated lane environment retained provisional identity: %q", joined)
	}
	if !strings.Contains(joined, "\nAGENT_SESSIONS_GROUPS=[\"project/child\",\""+primary+"\"]\n") {
		t.Fatalf("fresh product-generated lane environment lost invocation groups: %q", joined)
	}

	actor.id, actor.nativeID = "ses_exact", "ses_exact"
	actor.groups = []string{"project/child", primary, primary + "/ses_exact"}
	got, err = laneWorkerEnvironment(runtime, nil, actor, productruntime.LaneCapabilitySet{})
	if err != nil {
		t.Fatal(err)
	}
	joined = "\n" + strings.Join(got, "\n") + "\n"
	if !strings.Contains(joined, "\nAGENT_SESSIONS_SESSION_ID=ses_exact\n") || !strings.Contains(joined, primary+"/ses_exact") {
		t.Fatalf("resume environment omitted exact native identity: %q", joined)
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
		id: "active", parentID: "parent", product: "dsh", state: "running", done: make(chan struct{}),
		cancel: func() { close(cancelled) },
	}
	persistent := &laneActor{id: "persistent", parentID: "parent", product: "claude", state: "idle", persistent: true, done: closedLaneDone()}
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

func TestDirectLaneCommandUsesItsInvocationCwd(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parentID := "parent"
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: parentID, Product: "claude", NativeSessionID: parentID, Groups: []string{"shared"},
	})
	coordinator.liveReports[parentID] = liveSessionReport{UUID: parentID, Name: "parent", Product: "claude", Groups: []string{"shared"}}
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Remember(daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "claude", Parent: parentID,
		PrimaryGroup: "session:" + runtime.HostID() + "/" + parentID,
	}); err != nil {
		t.Fatal(err)
	}
	wantCwd := t.TempDir()
	gotCwd := ""
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, _ daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		gotCwd = parent.Cwd
		return laneNameEntry{Name: "worker"}, true
	}
	payload, err := json.Marshal(laneCommandEnvelope{
		Product: "claude", Command: "list", Arguments: []string{"list", "--all"},
		Cwd: wantCwd, SourceAttachmentID: parentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.handleLaneCommand(context.Background(), runtime, daemonpkg.ControlRequest{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if gotCwd != wantCwd {
		t.Fatalf("candidate confirmation cwd = %q, want invocation %q", gotCwd, wantCwd)
	}
}

func TestLaneDoctorUsesNativeVersionReadinessForACPProducts(t *testing.T) {
	for _, product := range []struct{ id, version string }{{"grok", "grok 1.0.13"}, {"qwen", "qwen 0.22.3"}} {
		t.Run(product.id, func(t *testing.T) {
			root := shortDaemonTestRoot(t)
			executable := filepath.Join(root, product.id)
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' '"+product.version+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			report := inspectLaneProductReadiness(context.Background(), product.id, executable, root)
			ready, _ := report["ready"].(bool)
			available, _ := report[product.id+"_available"].(bool)
			supervisor, _ := report["supervisor_reachable"].(bool)
			if !ready || !available || report[product.id+"_path"] != executable ||
				report[product.id+"_version"] != product.version || !supervisor {
				t.Fatalf("%s readiness projection = %#v", product.id, report)
			}
		})
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
