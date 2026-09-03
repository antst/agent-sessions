package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federator "github.com/antst/agent-sessions/internal/federation"
)

func TestDaemonSettingUsesEnvironmentThenCrossPlatformServiceFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AGENT_SESSIONS_HUB", "")
	root := filepath.Join(home, "config", "agent-sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.env"), []byte("AGENT_SESSIONS_HUB=hub.example:7419\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := daemonSetting("AGENT_SESSIONS_HUB"); got != "hub.example:7419" {
		t.Fatalf("service-file hub = %q", got)
	}
	t.Setenv("AGENT_SESSIONS_HUB", "override:7419")
	if got := daemonSetting("AGENT_SESSIONS_HUB"); got != "override:7419" {
		t.Fatalf("environment hub = %q", got)
	}
}

func TestMessagingToolsDiscoverAndDeliverThroughEmbeddedFederation(t *testing.T) {
	address := testFederationAddress(t)
	t.Setenv("AGENT_SESSIONS_HUB", address)
	t.Setenv("AGENT_SESSIONS_HOST_NAME", "workstation-a")
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan error, 1)
	go func() { hubDone <- federator.RunHub(hubCtx, federator.HubOptions{Listen: address}) }()
	t.Cleanup(func() {
		stopHub()
		select {
		case <-hubDone:
		case <-time.After(3 * time.Second):
			t.Error("test hub did not stop")
		}
	})
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	localHost := runtime.HostID()
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "parent", Product: "codex", NativeSessionID: "native-parent",
		Cwd: "/work", Groups: []string{"project"}, PermissionMode: "default",
	})
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	coordinator.lanesLoaded = true
	target, err := federator.BuildPeer(
		"host-b", "host-b", "target", "target", "idle", "/work", "qwen", "default",
		"qwen:target", "", []string{"project"},
	)
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan federator.AgentFrame, 1)
	hostA, err := daemonpkg.NewFederation(federator.EmbeddedHostOptions{
		Hub: address, HostID: localHost, HostName: localHost,
		ScanInterval: 20 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond,
		Snapshot: func(context.Context) ([]federator.Peer, error) {
			return coordinator.federationSnapshot(runtime, localHost, localHost)
		},
		Deliver: func(context.Context, federator.Peer, federator.Peer, federator.AgentFrame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := daemonpkg.NewFederation(federator.EmbeddedHostOptions{
		Hub: address, HostID: "host-b", HostName: "host-b",
		ScanInterval: 20 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond,
		Snapshot: func(context.Context) ([]federator.Peer, error) { return []federator.Peer{target}, nil },
		Deliver: func(_ context.Context, source, got federator.Peer, frame federator.AgentFrame) error {
			if source.SessionID != "parent" || got.ID != target.ID {
				t.Errorf("federated identities = %s -> %s", source.ID, got.ID)
			}
			delivered <- frame
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.federation = hostA
	testCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runFederationComponentForTest(t, testCtx, hostA)
	runFederationComponentForTest(t, testCtx, hostB)
	waitFederationTest(t, func() bool { return len(hostA.RemotePeers()) == 1 }, "remote peer roster")

	rawRoster, err := coordinator.operatorRoster(runtime)
	if err != nil {
		t.Fatal(err)
	}
	var roster operatorRosterReport
	if err := json.Unmarshal(rawRoster, &roster); err != nil {
		t.Fatal(err)
	}
	if roster.Host.ID != localHost || roster.Host.Name != "workstation-a" ||
		!roster.Host.HubConfigured || !roster.Host.FederationConnected {
		t.Fatalf("operator host roster = %+v", roster.Host)
	}
	if len(roster.Local) != 1 || roster.Local[0].LocalID != "parent" ||
		len(roster.FederatedHosts) != 1 || roster.FederatedHosts[0].ID != "host-b" ||
		len(roster.Remote) != 1 || roster.Remote[0].LocalID != "target" {
		t.Fatalf("operator roster local=%+v hosts=%+v remote=%+v", roster.Local, roster.FederatedHosts, roster.Remote)
	}

	listed, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "list_peers", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	peers := listed.Data.(map[string]any)["peers"].([]map[string]any)
	if len(peers) != 1 || peers[0]["name"] != "target--host-b" {
		t.Fatalf("listed remote peers = %#v", peers)
	}
	if _, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
		"target": "target--host-b", "message": "hello remote",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-delivered:
		if frame.Content != "hello remote" {
			t.Fatalf("delivered content = %q", frame.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("messaging tool did not reach remote daemon")
	}
	if _, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
		"group": "project", "message": "hello remote group",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-delivered:
		if frame.Content != "hello remote group" || frame.Group != "project" {
			t.Fatalf("group-delivered frame = %#v", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("group messaging did not reach remote daemon")
	}
}

func TestFederationSnapshotProjectsCurrentDaemonAttachmentsAndLanes(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "parent", Product: "codex", NativeSessionID: "native-parent",
		Cwd: "/work", Groups: []string{"project"}, PermissionMode: "bypassPermissions",
	})
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	coordinator.lanesLoaded = true
	coordinator.lanes["lane"] = &laneActor{
		id: "lane", product: "qwen", name: "worker", cwd: "/work", parentID: "parent",
		groups:     []string{"project", "session:host-a/parent", "session:host-a/lane"},
		permission: "default", state: "running",
	}
	peers, err := coordinator.federationSnapshot(runtime, "host-a", "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("federation snapshot = %#v", peers)
	}
	byID := map[string]federator.Peer{}
	for _, peer := range peers {
		byID[peer.SessionID] = peer
	}
	if byID["parent"].PermissionMode != "bypassPermissions" || byID["lane"].Status != "busy" ||
		!containsString(byID["parent"].Groups, "session:host-a/parent") ||
		!containsString(byID["lane"].Groups, "session:host-a/lane") {
		t.Fatalf("projected peers = %#v", byID)
	}
}

func testFederationAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func runFederationComponentForTest(t *testing.T, parent context.Context, host *daemonpkg.Federation) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("federation component stopped: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("federation component did not stop")
		}
	})
}

func waitFederationTest(t *testing.T, predicate func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func TestFederatedLaneUsesAttestedRemoteParentAndForcesPersistence(t *testing.T) {
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	coordinator.lanesLoaded = true
	source, err := federator.BuildPeer(
		"host-a", "host-a", "parent", "parent", "idle", "/work", "codex",
		"bypassPermissions", "instance", "", []string{"project"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.runFederatedLane(context.Background(), runtime, federator.RemoteLaneRequest{
		Source: source,
		Parent: federator.ParentContext{
			HostID: source.HostID, SessionID: source.SessionID, Product: source.Entrypoint,
			InstanceID: source.InstanceID, Groups: source.Groups, PermissionMode: source.PermissionMode,
		},
		TargetHostID: "host-b", Product: "codex", Capability: "codex-lane", Arguments: []string{"list", "--all"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(result.Stdout, &body) != nil || body["type"] != "lane.list" {
		t.Fatalf("remote lane output = %q", result.Stdout)
	}
	parent := daemonpkg.ManagedAttachment{ID: source.ID, Groups: source.Groups}
	groups, err := coordinator.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groups, uniqueStrings(source.Groups)) {
		t.Fatalf("remote parent groups = %q", groups)
	}
}

func TestFederatedLaneDoctorDoesNotResolveSourceHostCwd(t *testing.T) {
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	source, err := federator.BuildPeer(
		"linux-source", "linux-source", "parent", "parent", "idle",
		"/home/source/path-that-does-not-exist-on-the-destination", "codex",
		"bypassPermissions", "instance", "", []string{"project"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.runFederatedLane(context.Background(), runtime, federator.RemoteLaneRequest{
		Source: source,
		Parent: federator.ParentContext{
			HostID: source.HostID, SessionID: source.SessionID, Product: source.Entrypoint,
			InstanceID: source.InstanceID, Groups: source.Groups, PermissionMode: source.PermissionMode,
		},
		TargetHostID: "mac-destination", Product: "codex", Capability: "codex-lane", Arguments: []string{"doctor", "--json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Stdout, &body); err != nil {
		t.Fatal(err)
	}
	ready, _ := body["ready"].(bool)
	if body["type"] != "lane.doctor" || !ready {
		t.Fatalf("remote doctor = %#v", body)
	}
}

func TestFederatedLaneRejectsProductCapabilityMismatchBeforeDispatch(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	source, err := federator.BuildPeer(
		"host-a", "host-a", "parent", "parent", "idle", "/work", "codex",
		"default", "instance", "", []string{"project"},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.runFederatedLane(context.Background(), runtime, federator.RemoteLaneRequest{
		Source: source, Parent: federator.ParentContext{
			HostID: source.HostID, SessionID: source.SessionID, Product: source.Product,
			InstanceID: source.InstanceID, Groups: source.Groups,
		},
		TargetHostID: "host-b", Product: "qwen", Capability: "codex-lane", Arguments: []string{"list", "--all"},
	})
	if err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("product/capability mismatch result = %v", err)
	}
	if len(coordinator.lanes) != 0 {
		t.Fatalf("mismatched request dispatched a lane: %#v", coordinator.lanes)
	}
}

func TestRemoteLaneTerminalNoticeIsAcknowledgedOnceAndPointsBackToDestination(t *testing.T) {
	address := testFederationAddress(t)
	hubCtx, stopHub := context.WithCancel(context.Background())
	t.Cleanup(stopHub)
	go func() { _ = federator.RunHub(hubCtx, federator.HubOptions{Listen: address}) }()

	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	localHost := runtime.HostID()
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	coordinator.lanesLoaded = true
	actor := &laneActor{
		id: "lane", product: "qwen", name: "worker", cwd: "/work", parentID: "host-a/parent",
		groups:     []string{"project", "session:host-a/parent", "session:" + localHost + "/lane"},
		permission: "default", persistent: true, state: "terminal", outcome: "completed",
		turnID: "turn", completedAt: time.Now().UnixMilli(), done: closedTestLaneDone(),
	}
	coordinator.lanes[actor.id] = actor

	parent, err := federator.BuildPeer(
		"host-a", "host-a", "parent", "parent", "idle", "/work", "codex", "default",
		"codex:parent", "", []string{"project"},
	)
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan federator.AgentFrame, 2)
	hostA, err := daemonpkg.NewFederation(federator.EmbeddedHostOptions{
		Hub: address, HostID: "host-a", HostName: "host-a", ScanInterval: 20 * time.Millisecond,
		Snapshot: func(context.Context) ([]federator.Peer, error) { return []federator.Peer{parent}, nil },
		Deliver: func(_ context.Context, _, _ federator.Peer, frame federator.AgentFrame) error {
			delivered <- frame
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := daemonpkg.NewFederation(federator.EmbeddedHostOptions{
		Hub: address, HostID: localHost, HostName: localHost, ScanInterval: 20 * time.Millisecond,
		Snapshot: func(context.Context) ([]federator.Peer, error) {
			return coordinator.federationSnapshot(runtime, localHost, localHost)
		},
		Deliver: func(context.Context, federator.Peer, federator.Peer, federator.AgentFrame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.federation = hostB
	testCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runFederationComponentForTest(t, testCtx, hostA)
	runFederationComponentForTest(t, testCtx, hostB)
	waitFederationTest(t, func() bool { return len(hostB.RemotePeers()) == 1 }, "remote terminal parent")

	if err := coordinator.publishLaneTerminalNotice(runtime, actor); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-delivered:
		if frame.MessageID != laneTerminalNoticeID("lane", "turn") ||
			!strings.Contains(frame.Content, "QWEN_LANE_TERMINAL") ||
			!strings.Contains(frame.Content, "collection=required") ||
			!strings.Contains(frame.Content, `mcp_hint={"tool":"agent_sessions.lane","arguments":{"host":"`+localHost+`","product":"qwen","command":"wait","arguments":["lane"]}}`) ||
			strings.Contains(frame.Content, "Collection hint:") {
			t.Fatalf("terminal frame = %#v", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("remote terminal notice was not delivered")
	}
	terminal := cloneLaneActor(actor)
	coordinator.mu.Lock()
	actor.state = "idle"
	coordinator.mu.Unlock()
	if err := coordinator.publishLaneTerminalNotice(runtime, &terminal); err != nil {
		t.Fatal(err)
	}
	select {
	case repeated := <-delivered:
		if repeated.MessageID != laneTerminalNoticeID("lane", "turn") ||
			!strings.Contains(repeated.Content, "collection=none") || strings.Contains(repeated.Content, "mcp_hint=") {
			t.Fatalf("repeated live terminal frame = %#v", repeated)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("explicit second live terminal delivery was not presented")
	}
}

func closedTestLaneDone() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
