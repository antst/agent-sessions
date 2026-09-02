package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
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
	coordinator.joinLiveSession(runtime, liveSessionReport{UUID: "parent", Name: "parent", Product: "codex"})
	coordinator.mu.Lock()
	coordinator.lanes["public-lane"] = &laneActor{
		id: "public-lane", product: "codex", name: "worker", parentID: "parent", state: "running", done: make(chan struct{}),
	}
	coordinator.mu.Unlock()
	coordinator.joinLiveSession(runtime, liveSessionReport{
		UUID: "native-lane", Name: "worker", Product: "codex",
		Groups: []string{"session:" + runtime.HostID() + "/parent", "session:" + runtime.HostID() + "/public-lane"},
	})
	coordinator.mu.Lock()
	actor := coordinator.lanes["public-lane"]
	_, duplicate := coordinator.lanes["native-lane"]
	coordinator.mu.Unlock()
	if duplicate || actor == nil || actor.nativeID != "native-lane" || actor.state != "running" {
		t.Fatalf("bound actor = %+v, duplicate=%v", actor, duplicate)
	}
}

func TestActiveLaneNameCacheAsksProductOnceAndMaterializesConfirmedCandidate(t *testing.T) {
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
	calls := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		calls++
		if got.NativeSessionID != candidate.NativeSessionID {
			return laneNameEntry{}, false
		}
		return laneNameEntry{Name: "archived-worker", Cwd: filepath.Clean(t.TempDir())}, true
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
	if _, err := coordinator.resolveLaneActor(runtime, parent, "codex", "native-lane", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("product confirmation calls = %d, want 1", calls)
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

func newPresenceTestRuntime(t *testing.T) *daemonpkg.Runtime {
	t.Helper()
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}
