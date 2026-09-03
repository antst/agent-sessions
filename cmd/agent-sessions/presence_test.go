package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestLivePresenceConnectionDefinesSessionLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan liveSessionReport, 1)
	left := make(chan liveSessionReport, 1)
	root := t.TempDir()
	server, err := startLivePresenceServer(ctx, root, func(report liveSessionReport) {
		joined <- report
	}, func(report liveSessionReport) {
		left <- report
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := liveSessionReport{UUID: "session-1", Name: "builder", Groups: []string{"team", "team"}, Product: "codex"}
	clientCtx, stopClient := context.WithCancel(context.Background())
	go maintainLivePresence(clientCtx, server.listener.Addr().String(), report)
	select {
	case got := <-joined:
		if got.UUID != report.UUID || got.Name != report.Name || !reflect.DeepEqual(got.Groups, []string{"team"}) {
			t.Fatalf("join report = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence report was not joined")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-left:
		if got.UUID != report.UUID {
			t.Fatalf("leave report = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence disconnect was not observed")
	}
	rejoined := make(chan liveSessionReport, 1)
	successor, err := startLivePresenceServer(ctx, root, func(report liveSessionReport) {
		rejoined <- report
	}, func(liveSessionReport) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	select {
	case got := <-rejoined:
		if got.UUID != report.UUID {
			t.Fatalf("reconnect report = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live session did not reconnect to the successor daemon")
	}
	stopClient()
}

func TestLivePresenceClientProjectsNameOnTheSameConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan liveSessionReport, 2)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report liveSessionReport) {
		joined <- report
	}, func(liveSessionReport) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	report := liveSessionReport{UUID: "session", Name: "session", Groups: []string{"group"}, Product: "qwen"}
	client := startLiveSessionClient(ctx, server.listener.Addr().String(), report, nil)
	select {
	case initial := <-joined:
		if initial.Name != "session" {
			t.Fatalf("initial name = %q", initial.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial live report was not received")
	}
	report.Name = "product title"
	updateCtx, stopUpdate := context.WithTimeout(ctx, 2*time.Second)
	defer stopUpdate()
	if err := client.UpdateReport(updateCtx, report); err != nil {
		t.Fatal(err)
	}
	select {
	case updated := <-joined:
		if updated.Name != "product title" {
			t.Fatalf("updated name = %q", updated.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("updated live report was not received")
	}
}

func TestLivePresenceConnectionCarriesCallsInBothDirections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan liveSessionReport, 1)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report liveSessionReport) {
		joined <- report
	}, func(liveSessionReport) {}, func(_ context.Context, report liveSessionReport, method string, params json.RawMessage) (json.RawMessage, error) {
		if report.UUID != "session-rpc" || method != "tools/call" {
			t.Fatalf("server call = %+v %q", report, method)
		}
		return append(json.RawMessage(nil), params...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client := startLiveSessionClient(ctx, server.listener.Addr().String(), liveSessionReport{
		UUID: "session-rpc", Name: "rpc", Product: "codex",
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "message.deliver" {
			t.Fatalf("client method = %q", method)
		}
		return append(json.RawMessage(nil), params...), nil
	})
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("live RPC session did not join")
	}
	var fromSession json.RawMessage
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		fromSession, err = client.Call(ctx, "tool", "tools/call", map[string]string{"name": "list_peers"})
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(fromSession), "list_peers") {
		t.Fatalf("session-to-daemon call = %s, %v", fromSession, err)
	}
	fromDaemon, err := server.Call(ctx, "session-rpc", "delivery", "message.deliver", map[string]string{"body": "hello"})
	if err != nil || !strings.Contains(string(fromDaemon), "hello") {
		t.Fatalf("daemon-to-session call = %s, %v", fromDaemon, err)
	}
}

func TestCoordinatorRebuildsLivePeerAndLaneFromReports(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parent := liveSessionReport{UUID: "parent", Name: "reviewer", Groups: []string{"team"}, Product: "codex"}
	coordinator.joinLiveSession(runtime, parent)
	attachment, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active || attachment.Product != "codex" || !reflect.DeepEqual(attachment.Groups, []string{"team"}) {
		t.Fatalf("reported parent = %+v, active=%v, err=%v", attachment, active, err)
	}
	lane := liveSessionReport{
		UUID: "native-lane", Name: "worker", Product: "codex",
		Groups: []string{"session:" + runtime.HostID() + "/parent", "session:" + runtime.HostID() + "/native-lane"},
	}
	coordinator.joinLiveSession(runtime, lane)
	coordinator.mu.Lock()
	actor := coordinator.lanes["native-lane"]
	cached := coordinator.laneNames["parent"]["native-lane"]
	coordinator.mu.Unlock()
	if actor == nil || actor.parentID != "parent" || actor.nativeID != "native-lane" || cached.Name != "worker" {
		t.Fatalf("reported lane actor/cache = %+v / %+v", actor, cached)
	}
	if laneAttachment, active, _ := runtime.Attachments().ActiveAttachment("native-lane"); !active || laneAttachment.State != "lane" {
		t.Fatalf("reported lane connector = %+v, active=%v", laneAttachment, active)
	}
	peers, err := runtime.Attachments().ListActive()
	if err != nil || len(peers) != 1 || peers[0].ID != "parent" {
		t.Fatalf("parent-only roster = %+v, err=%v", peers, err)
	}
	coordinator.leaveLiveSession(runtime, parent)
	coordinator.mu.Lock()
	_, cacheSurvived := coordinator.laneNames["parent"]
	coordinator.mu.Unlock()
	if cacheSurvived {
		t.Fatal("lane name cache survived its active parent")
	}
	coordinator.leaveLiveSession(runtime, lane)
	coordinator.mu.Lock()
	_, laneSurvived := coordinator.lanes["native-lane"]
	coordinator.mu.Unlock()
	if laneSurvived {
		t.Fatal("lane survived its live report connection")
	}
}

func TestLaneOnlyProductReportCannotBecomePeerButItsLaneRemainsLive(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	coordinator.joinLiveSession(runtime, liveSessionReport{UUID: "dsh-root", Name: "root", Product: "dsh"})
	if attachment, active, err := runtime.Attachments().ActiveAttachment("dsh-root"); err != nil || active {
		t.Fatalf("lane-only root projected as peer: %+v, active=%v, err=%v", attachment, active, err)
	}

	coordinator.joinLiveSession(runtime, liveSessionReport{UUID: "parent", Name: "parent", Product: "codex"})
	coordinator.joinLiveSession(runtime, liveSessionReport{
		UUID: "dsh-lane", Name: "worker", Product: "dsh",
		Groups: []string{"session:" + runtime.HostID() + "/parent"},
	})
	lane, active, err := runtime.Attachments().ActiveAttachment("dsh-lane")
	if err != nil || !active || lane.State != "lane" || lane.Product != "dsh" {
		t.Fatalf("DSH lane report = %+v, active=%v, err=%v", lane, active, err)
	}
	peers, err := runtime.Attachments().ListActive()
	if err != nil || len(peers) != 1 || peers[0].ID != "parent" {
		t.Fatalf("peer roster with lane-only reports = %+v, err=%v", peers, err)
	}
}

func TestLaneReportBindsExistingProductSessionWithoutDuplicatingActor(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parentGroups := []string{"team"}
	coordinator.joinLiveSession(runtime, liveSessionReport{UUID: "parent", Name: "parent", Product: "codex", Groups: parentGroups})
	stableGroups := []string{
		"session:" + runtime.HostID() + "/parent",
		"session:" + runtime.HostID() + "/parent/native-lane",
		"team/child",
	}
	coordinator.mu.Lock()
	coordinator.lanes["public-lane"] = &laneActor{
		id: "public-lane", product: "codex", name: "worker", parentID: "parent", state: "running",
		groups: stableGroups, done: make(chan struct{}),
	}
	coordinator.mu.Unlock()
	coordinator.joinLiveSession(runtime, liveSessionReport{
		UUID: "native-lane", Name: "worker", Product: "codex",
		Groups: []string{"team/child", "session:" + runtime.HostID() + "/parent"},
	})
	coordinator.mu.Lock()
	actor := coordinator.lanes["public-lane"]
	coordinator.mu.Unlock()
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: "native-lane", NativeSessionID: "native-lane", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	bound := coordinator.lanes["native-lane"]
	_, provisional := coordinator.lanes["public-lane"]
	coordinator.mu.Unlock()
	if provisional || bound != actor || actor.id != "native-lane" || actor.nativeID != "native-lane" || actor.state != "running" {
		t.Fatalf("bound actor = %+v, provisional=%v", actor, provisional)
	}
	if !reflect.DeepEqual(actor.groups, stableGroups) {
		t.Fatalf("bound actor groups = %v, want %v", actor.groups, stableGroups)
	}
	attachment, active, err := runtime.Attachments().ActiveAttachment("native-lane")
	if err != nil || !active || !reflect.DeepEqual(attachment.Groups, stableGroups) {
		t.Fatalf("bound lane attachment = %+v, active=%v, err=%v", attachment, active, err)
	}
	parent, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active || !reflect.DeepEqual(parent.Groups, parentGroups) {
		t.Fatalf("ordinary peer attachment = %+v, active=%v, err=%v", parent, active, err)
	}
}

func TestRestartLeavesCandidateNonLiveUntilProductConfirmedResume(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	coordinator.joinLiveSession(runtime, liveSessionReport{UUID: "parent", Name: "parent", Product: "codex"})
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "codex", Parent: "parent",
		PrimaryGroup: "session:" + runtime.HostID() + "/parent", SecondaryGroups: []string{"team"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	if _, live, err := runtime.Attachments().ActiveAttachment(candidate.NativeSessionID); err != nil || live {
		t.Fatalf("candidate before product confirmation live=%v err=%v", live, err)
	}
	calls := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		calls++
		if got.NativeSessionID != candidate.NativeSessionID {
			return laneNameEntry{}, false
		}
		return laneNameEntry{Name: "archived-worker"}, true
	}
	parent, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active {
		t.Fatalf("parent active=%v err=%v", active, err)
	}
	actor, err := coordinator.resolveLaneActor(runtime, parent, "codex", "archived-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if actor.nativeID != "native-lane" || actor.state != "archived" || !reflect.DeepEqual(actor.explicitGroups, []string{"team"}) {
		t.Fatalf("materialized candidate = %+v", actor)
	}
	wantGroups := []string{
		"session:" + runtime.HostID() + "/parent",
		"session:" + runtime.HostID() + "/parent/native-lane",
		"team",
	}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("materialized groups = %v, want %v", actor.groups, wantGroups)
	}
	if _, live, err := runtime.Attachments().ActiveAttachment(candidate.NativeSessionID); err != nil || live {
		t.Fatalf("product-confirmed candidate was published as live: live=%v err=%v", live, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parent, "codex", "native-lane", true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("product confirmation calls = %d, want one per explicit lookup", calls)
	}
}

func TestLaneEOFRematerializesArchivedGroupsOnlyFromDurableCandidate(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parentReport := liveSessionReport{UUID: "parent", Name: "parent", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentReport)
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "claude", Parent: "parent",
		PrimaryGroup:    "session:" + runtime.HostID() + "/parent",
		SecondaryGroups: []string{"shared/child", "shared"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	confirmations := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		confirmations++
		if !reflect.DeepEqual(got, candidate) {
			t.Fatalf("candidate = %+v", got)
		}
		return laneNameEntry{Name: "worker"}, true
	}
	parent, active, err := runtime.Attachments().ActiveAttachment(parentReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent active=%v err=%v", active, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parent, candidate.Product, "worker", true); err != nil {
		t.Fatal(err)
	}
	laneReport := liveSessionReport{
		UUID: candidate.NativeSessionID, Name: "worker", Product: candidate.Product,
		Groups: candidateLaneGroups(candidate),
	}
	coordinator.joinLiveSession(runtime, laneReport)
	parentBReport := liveSessionReport{UUID: "parent-b", Name: "parent-b", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentBReport)
	parentB, active, err := runtime.Attachments().ActiveAttachment(parentBReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent B active=%v err=%v", active, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "worker", true); err != nil {
		t.Fatal(err)
	}
	coordinator.leaveLiveSession(runtime, laneReport)
	coordinator.mu.Lock()
	_, actorSurvived := coordinator.lanes[candidate.NativeSessionID]
	coordinator.mu.Unlock()
	if actorSurvived {
		t.Fatal("EOF left the live lane actor")
	}

	actor, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if confirmations != 3 || actor.parentID != parentB.ID || actor.cwd != "" || !reflect.DeepEqual(actor.explicitGroups, candidate.SecondaryGroups) || !reflect.DeepEqual(actor.groups, candidateLaneGroups(candidate)) {
		t.Fatalf("rematerialized actor=%+v confirmations=%d", actor, confirmations)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rematerialization changed durable candidate: before=%+v after=%+v", before, after)
	}
}

func TestParentPresenceEOFArchivesNonPersistentAndReleasesPersistentLane(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	driver := &parentExitLaneDriver{}
	var err error
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"claude": driver})
	if err != nil {
		t.Fatal(err)
	}
	parentA := liveSessionReport{UUID: "parent-a", Name: "parent-a", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentA)
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	for _, nativeID := range []string{"native-idle", "native-persistent"} {
		if err := engine.Remember(daemonpkg.LaneCandidate{
			NativeSessionID: nativeID, Product: "claude", Parent: parentA.UUID,
			PrimaryGroup: "session:" + runtime.HostID() + "/" + parentA.UUID, SecondaryGroups: []string{"shared"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	idle := &laneActor{
		id: "native-idle", nativeID: "native-idle", nativeGeneration: 7, product: "claude", name: "worker-idle",
		parentID: parentA.UUID, groups: []string{"shared"}, state: "idle", done: closedLaneDone(),
	}
	persistent := &laneActor{
		id: "native-persistent", nativeID: "native-persistent", nativeGeneration: 7, product: "claude", name: "worker-persistent",
		parentID: parentA.UUID, groups: []string{"shared"}, permission: "default", persistent: true, state: "idle", done: closedLaneDone(),
	}
	coordinator.lanes[idle.id], coordinator.lanes[persistent.id] = idle, persistent
	laneReport := liveSessionReport{
		UUID: persistent.nativeID, Name: persistent.name, Product: persistent.product,
		Groups: []string{"shared", "session:" + runtime.HostID() + "/" + parentA.UUID},
	}
	coordinator.joinLiveSession(runtime, laneReport)
	if coordinator.reportedLanes[persistent.nativeID] != persistent.id {
		t.Fatalf("persistent lane report was not bound to its launched actor: %+v", coordinator.reportedLanes)
	}

	coordinator.leaveLiveSession(runtime, parentA)
	coordinator.leaveLiveSession(runtime, parentA)
	if idle.state != "archived" || len(driver.archives) != 1 || driver.archives[0].NativeSessionID != idle.nativeID {
		t.Fatalf("nonpersistent parent exit actor=%+v archives=%+v", idle, driver.archives)
	}
	if persistent.state != "idle" || persistent.parentID != "" {
		t.Fatalf("persistent parent exit actor=%+v", persistent)
	}

	parentBReport := liveSessionReport{UUID: "parent-b", Name: "parent-b", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentBReport)
	if coordinator.lanes[persistent.id] != persistent || !persistent.persistent || coordinator.reportedLanes[persistent.nativeID] != persistent.id {
		t.Fatalf("unowned persistent lane was reclassified: actor=%+v lanes=%+v reported=%+v", persistent, coordinator.lanes, coordinator.reportedLanes)
	}
	parentB, active, err := runtime.Attachments().ActiveAttachment(parentBReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent B active=%v err=%v", active, err)
	}
	parentB.Cwd = t.TempDir()
	result, err := coordinator.resumeLane(context.Background(), runtime, parentB, "claude", parsedLaneCommand{target: persistent.name}, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if result["result"] != "resumed" || persistent.parentID != parentB.ID || len(driver.opens) != 1 ||
		driver.opens[0].LaneID != persistent.id || driver.opens[0].ResumeNativeID != persistent.nativeID {
		t.Fatalf("persistent reattach result=%+v actor=%+v opens=%+v", result, persistent, driver.opens)
	}
	if !persistent.autoArchive || persistent.autoArchiveDelay != defaultUnifiedLaneAutoArchiveDelay {
		t.Fatalf("resume auto-archive default = enabled:%t delay:%s", persistent.autoArchive, persistent.autoArchiveDelay)
	}
	openEnvironment := "\n" + strings.Join(driver.opens[0].Environment, "\n") + "\n"
	for _, expected := range []string{
		"\nAGENT_SESSIONS_SESSION_ID=" + persistent.id + "\n",
		"\nAGENT_SESSIONS_PRODUCT=claude\n",
		"\nAGENT_SESSIONS_SESSION_NAME=" + persistent.name + "\n",
	} {
		if !strings.Contains(openEnvironment, expected) {
			t.Fatalf("driver open environment %q lacks %q", openEnvironment, expected)
		}
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("parent exit or handover changed durable rows: before=%+v after=%+v", before, after)
	}
}

func TestOfflineLaneCandidateVisibilityAndOwnershipFollowLiveParentGroups(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "claude", Parent: "parent-a",
		PrimaryGroup:    "session:" + runtime.HostID() + "/parent-a",
		SecondaryGroups: []string{"parent-a/child", "shared"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	confirmations := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		confirmations++
		if !reflect.DeepEqual(got, candidate) {
			t.Fatalf("candidate = %+v", got)
		}
		return laneNameEntry{Name: "shared-worker"}, true
	}
	outsider := daemonpkg.ManagedAttachment{ID: "parent-outside", Product: "claude", Cwd: t.TempDir(), Groups: []string{"outside"}}
	coordinator.liveReports[outsider.ID] = liveSessionReport{UUID: outsider.ID, Product: outsider.Product, Groups: outsider.Groups}
	if err := coordinator.ensureActiveLaneNames(context.Background(), runtime, outsider, candidate.Product); err != nil {
		t.Fatal(err)
	}
	if confirmations != 0 || len(coordinator.laneNames[outsider.ID]) != 0 {
		t.Fatalf("non-sharing parent confirmed candidate: calls=%d names=%+v", confirmations, coordinator.laneNames[outsider.ID])
	}

	parentB := daemonpkg.ManagedAttachment{ID: "parent-b", Product: "claude", Cwd: t.TempDir(), Groups: []string{"shared"}}
	coordinator.liveReports[parentB.ID] = liveSessionReport{UUID: parentB.ID, Product: parentB.Product, Groups: parentB.Groups}
	actor, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "shared-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if confirmations != 1 || actor.parentID != parentB.ID || !reflect.DeepEqual(actor.explicitGroups, candidate.SecondaryGroups) {
		t.Fatalf("shared candidate actor=%+v confirmations=%d", actor, confirmations)
	}
	actor.groups, err = coordinator.effectiveLaneGroups(runtime, actor, parentB)
	if err != nil {
		t.Fatal(err)
	}
	parentBAnchor := "session:" + runtime.HostID() + "/" + parentB.ID
	wantGroups := []string{"parent-a/child", parentBAnchor, parentBAnchor + "/" + candidate.NativeSessionID, "shared"}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("handed-over groups = %v, want %v", actor.groups, wantGroups)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: candidate.NativeSessionID, NativeSessionID: candidate.NativeSessionID, Generation: 1}); err != nil {
		t.Fatalf("resume rewrote immutable candidate: %v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("resume changed durable candidate: before=%+v after=%+v", before, after)
	}
}

func TestResolveLaneActorUsesExistingNativeSessionWithoutCacheDuplicate(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parent := daemonpkg.ManagedAttachment{ID: "parent", Product: "opencode", Cwd: "/workspace", Groups: []string{"team"}}
	coordinator.liveReports[parent.ID] = liveSessionReport{UUID: parent.ID, Product: parent.Product, Groups: parent.Groups}
	actor := &laneActor{
		id: "ses_product", nativeID: "ses_product", product: "opencode", name: "worker",
		cwd: "/workspace", parentID: parent.ID, groups: []string{"team"}, explicitGroups: []string{"team/worker"},
		state: "archived", done: closedLaneDone(),
	}
	coordinator.lanes[actor.id] = actor
	coordinator.laneNames[parent.ID] = map[string]laneNameEntry{
		actor.nativeID: {UUID: actor.nativeID, Name: actor.name, Product: actor.product},
	}

	resolved, err := coordinator.resolveLaneActor(runtime, parent, actor.product, actor.nativeID, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != actor || resolved.id != "ses_product" || resolved.cwd != "/workspace" || !reflect.DeepEqual(resolved.explicitGroups, []string{"team/worker"}) {
		t.Fatalf("resolved actor = %+v, want original %+v", resolved, actor)
	}
	if len(coordinator.lanes) != 1 {
		t.Fatalf("lane count = %d, want one existing actor", len(coordinator.lanes))
	}
}

func TestConnectorLiveReportUsesOnlyReportedFields(t *testing.T) {
	values := map[string]string{
		"AGENT_SESSIONS_SESSION_ID":   "session-1",
		"AGENT_SESSIONS_SESSION_NAME": "reviewer",
		"AGENT_SESSIONS_GROUPS":       `["team","team"]`,
	}
	report, ok := connectorLiveReport("claude", func(name string) string { return values[name] })
	if !ok || !reflect.DeepEqual(report, liveSessionReport{
		UUID: "session-1", Name: "reviewer", Groups: []string{"team"}, Product: "claude",
	}) {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestConnectorLiveReportPrefersProductNativeUUID(t *testing.T) {
	values := map[string]string{
		"AGENT_SESSIONS_SESSION_ID": "public-lane",
		"CODEX_THREAD_ID":           "native-lane",
	}
	report, ok := connectorLiveReport("codex", func(name string) string { return values[name] })
	if !ok || report.UUID != "native-lane" {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestConnectorLiveReportNeverFabricatesAProductName(t *testing.T) {
	values := map[string]string{
		"CLAUDE_CODE_SESSION_ID": "native-session",
		"AGENT_SESSIONS_GROUPS":  `["team"]`,
	}
	report, ok := connectorLiveReport("claude", func(name string) string { return values[name] })
	if !ok || report.UUID != "native-session" || report.Name != "" || !reflect.DeepEqual(report.Groups, []string{"team"}) {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestClaudeActiveSessionNameComesFromExactNativeInteractiveRow(t *testing.T) {
	payload := []byte(`[
		{"sessionId":"target","name":"background","kind":"background"},
		{"sessionId":"other","name":"other title","kind":"interactive"},
		{"sessionId":"target","name":"product title","kind":"interactive"}
	]`)
	if got := claudeActiveSessionNameFromJSON(payload, "target"); got != "product title" {
		t.Fatalf("Claude active session name = %q", got)
	}
	for _, invalid := range [][]byte{
		[]byte(`not-json`),
		[]byte(`[{"sessionId":"other","name":"product title","kind":"interactive"}]`),
		[]byte(`[{"sessionId":"target","name":"one","kind":"interactive"},{"sessionId":"target","name":"two","kind":"interactive"}]`),
	} {
		if got := claudeActiveSessionNameFromJSON(invalid, "target"); got != "" {
			t.Fatalf("invalid native rows produced name %q from %s", got, invalid)
		}
	}
}

func newPresenceTestRuntime(t *testing.T) *daemonpkg.Runtime {
	t.Helper()
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}
