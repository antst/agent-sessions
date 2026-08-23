package federator

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestGroupedDiscoverySendMulticastAndBroadcast(t *testing.T) {
	root := t.TempDir()
	aSocket, aFrames := startAgentFrameSink(t, filepath.Join(root, "a.sock"))
	bSocket, bFrames := startAgentFrameSink(t, filepath.Join(root, "b.sock"))
	otherSocket, otherFrames := startAgentFrameSink(t, filepath.Join(root, "other.sock"))
	agent := &agent{
		options:     AgentOptions{HostID: "host-a", HostName: "host-a"},
		controlPath: filepath.Join(root, "agent.sock"), routeRefresh: func() error { return nil },
		local: map[string]localPeer{}, remote: map[string]Peer{},
	}
	source := localPeer{Peer: Peer{
		ID: "host-a/source", HostID: "host-a", SessionID: "source", Name: "source", Entrypoint: "codex",
		Groups: []string{"project", "session:host-a/source"},
	}}
	aPeer := localPeer{Peer: Peer{
		ID: "host-a/a", HostID: "host-a", SessionID: "a", Name: "a", Entrypoint: "claude",
		Groups: []string{"project", "session:host-a/a"},
	}, Socket: aSocket}
	bPeer := localPeer{Peer: Peer{
		ID: "host-a/b", HostID: "host-a", SessionID: "b", Name: "b", Entrypoint: "grok",
		Groups: []string{"project", "session:host-a/b"},
	}, Socket: bSocket}
	other := localPeer{Peer: Peer{
		ID: "host-a/other", HostID: "host-a", SessionID: "other", Name: "other", Entrypoint: "codex",
		Groups: []string{"other", "session:host-a/other"},
	}, Socket: otherSocket}
	for _, peer := range []localPeer{source, aPeer, bPeer, other} {
		agent.local[peer.ID] = peer
	}

	result, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "discover", MessageID: "discover-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(result.Peers))
	for _, peer := range result.Peers {
		ids = append(ids, peer.ID)
	}
	if !reflect.DeepEqual(ids, []string{"host-a/a", "host-a/b"}) {
		t.Fatalf("discover peers = %v", ids)
	}

	result, err = agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "send-1", Targets: []string{"a", "b"}, Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deliveries) != 2 || result.Deliveries[0].Status != "accepted" || result.Deliveries[1].Status != "accepted" {
		t.Fatalf("multicast result = %+v", result.Deliveries)
	}
	for name, frames := range map[string]<-chan AgentFrame{"a": aFrames, "b": bFrames} {
		frame := waitAgentFrame(t, frames)
		if frame.Type != "delivery" || frame.Content != "hello" || frame.Source == nil || frame.Source.SessionID != "source" {
			t.Fatalf("%s delivery = %+v", name, frame)
		}
	}

	if _, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "send-denied", Targets: []string{"a", "other"}, Content: "no",
	}); err == nil {
		t.Fatal("mixed accessible/inaccessible multicast unexpectedly admitted")
	}
	assertNoAgentFrame(t, aFrames)
	assertNoAgentFrame(t, otherFrames)

	result, err = agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "broadcast", MessageID: "broadcast-1", Group: "project", Content: "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deliveries) != 2 {
		t.Fatalf("broadcast result = %+v", result.Deliveries)
	}
	if frame := waitAgentFrame(t, aFrames); frame.Group != "project" || frame.Content != "all" {
		t.Fatalf("a broadcast = %+v", frame)
	}
	if frame := waitAgentFrame(t, bFrames); frame.Group != "project" || frame.Content != "all" {
		t.Fatalf("b broadcast = %+v", frame)
	}
	assertNoAgentFrame(t, otherFrames)
}

func TestCapabilityNormalizationUsesCompleteProductDescriptor(t *testing.T) {
	input := []string{CapabilityQwenLane, CapabilityCodexLane, "unknown", CapabilityQwenLane, CapabilityGrokLane, CapabilityClaudeLane}
	got := normalizeCapabilities(input)
	want := []string{CapabilityClaudeLane, CapabilityCodexLane, CapabilityGrokLane, CapabilityQwenLane}
	if !equalStrings(got, want) {
		t.Fatalf("normalized capabilities = %v, want %v", got, want)
	}
}

func TestAgentFrameRejectsDuplicateTargetsAndGlobalBroadcast(t *testing.T) {
	agent := &agent{
		options: AgentOptions{HostID: "host-a"}, routeRefresh: func() error { return nil },
		local: map[string]localPeer{
			"host-a/source": {Peer: Peer{ID: "host-a/source", HostID: "host-a", SessionID: "source", Groups: []string{"one"}}},
			"host-a/a":      {Peer: Peer{ID: "host-a/a", HostID: "host-a", SessionID: "a", Name: "a", Groups: []string{"one"}}},
		},
		remote: map[string]Peer{},
	}
	if _, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "duplicate", Targets: []string{"a", "a"}, Content: "x",
	}); err == nil {
		t.Fatal("duplicate multicast unexpectedly admitted")
	}
	if _, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "alias-duplicate",
		Targets: []string{"a", "host-a/a"}, Content: "x",
	}); err == nil {
		t.Fatal("aliases resolving to one peer unexpectedly admitted")
	}
	if _, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "broadcast", MessageID: "global", Group: "", Content: "x",
	}); err == nil {
		t.Fatal("global broadcast unexpectedly admitted")
	}
}

func TestRemoteMulticastWaitsConcurrentlyForOrderedAcknowledgements(t *testing.T) {
	agentSide, hubSide := net.Pipe()
	defer func() { _ = agentSide.Close(); _ = hubSide.Close() }()
	agent := &agent{
		options: AgentOptions{HostID: "local"}, routeRefresh: func() error { return nil },
		local: map[string]localPeer{
			"local/source": {Peer: Peer{
				ID: "local/source", HostID: "local", SessionID: "source", Name: "source",
				Groups: []string{"project", "session:local/source"},
			}},
		},
		remote: map[string]Peer{
			"remote/a": {ID: "remote/a", HostID: "remote", SessionID: "a", Name: "a", Groups: []string{"project"}},
			"remote/b": {ID: "remote/b", HostID: "remote", SessionID: "b", Name: "b", Groups: []string{"project"}},
		},
		network: newWireConn(agentSide), pendingDeliveries: map[string]chan error{},
	}
	go func() {
		_ = scanMessages(hubSide, func(message Message) error {
			go func(message Message) {
				time.Sleep(200 * time.Millisecond)
				agent.completePendingDelivery(Message{
					Type: "delivery_ack", RequestID: message.RequestID,
					SourceID: message.SourceID, TargetID: message.TargetID,
				})
			}(message)
			return nil
		})
	}()
	started := time.Now()
	result, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "remote-multi",
		Targets: []string{"b", "a"}, Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 350*time.Millisecond {
		t.Fatalf("remote recipients were delivered serially in %s", elapsed)
	}
	if len(result.Deliveries) != 2 || result.Deliveries[0].SessionID != "b" || result.Deliveries[1].SessionID != "a" ||
		result.Deliveries[0].Status != "accepted" || result.Deliveries[1].Status != "accepted" {
		t.Fatalf("ordered multicast result = %+v", result.Deliveries)
	}
}

func TestGroupedTargetResolutionIgnoresHiddenNameCollision(t *testing.T) {
	root := t.TempDir()
	visibleSocket, visibleFrames := startAgentFrameSink(t, filepath.Join(root, "visible.sock"))
	agent := &agent{
		options: AgentOptions{HostID: "host-a"}, routeRefresh: func() error { return nil },
		local: map[string]localPeer{
			"host-a/source":  {Peer: Peer{ID: "host-a/source", HostID: "host-a", SessionID: "source", Groups: []string{"shared"}}},
			"host-a/visible": {Peer: Peer{ID: "host-a/visible", HostID: "host-a", SessionID: "visible", Name: "worker", Groups: []string{"shared"}}, Socket: visibleSocket},
			"host-a/hidden":  {Peer: Peer{ID: "host-a/hidden", HostID: "host-a", SessionID: "hidden", Name: "worker", Groups: []string{"private"}}},
		}, remote: map[string]Peer{},
	}
	result, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "visible-name", Targets: []string{"worker"}, Content: "ok",
	})
	if err != nil || len(result.Deliveries) != 1 || result.Deliveries[0].SessionID != "visible" {
		t.Fatalf("visible resolution = %+v, %v", result, err)
	}
	if frame := waitAgentFrame(t, visibleFrames); frame.Content != "ok" {
		t.Fatalf("visible frame = %+v", frame)
	}
	if _, err := agent.handleAgentFrame("source", AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "hidden-id", Targets: []string{"host-a/hidden"}, Content: "no",
	}); err == nil || strings.Contains(err.Error(), "host-a/visible") {
		t.Fatalf("hidden exact target leak = %v", err)
	}
}

func TestClaudeNativeDiscoverGetsProtocolResultThroughService(t *testing.T) {
	root := t.TempDir()
	sourceSocket, resultBodies := startNativeResultSink(t, filepath.Join(root, "source.sock"))
	agent := &agent{
		options:     AgentOptions{HostID: "host-a", HostName: "host-a"},
		controlPath: filepath.Join(root, "agent.sock"), routeRefresh: func() error { return nil },
		local: map[string]localPeer{
			"host-a/source": {Peer: Peer{
				ID: "host-a/source", HostID: "host-a", SessionID: "source", Name: "source",
				Entrypoint: "claude", Groups: []string{"project", "session:host-a/source"},
			}, Socket: sourceSocket},
			"host-a/worker": {Peer: Peer{
				ID: "host-a/worker", HostID: "host-a", SessionID: "worker", Name: "worker",
				Entrypoint: "codex", Groups: []string{"project", "session:host-a/worker"},
			}},
		}, remote: map[string]Peer{},
	}
	request := AgentFrame{Version: AgentFrameVersion, Type: "discover", MessageID: "discover-native"}
	inner, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{
		"type": "user", "from": encodeUDS(sourceSocket),
		"message": map[string]any{"content": claudeAgentFramePrefix + string(inner)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := agent.handleNativeCarrierFrame(outer); err != nil || result.Type != "discover.result" {
		t.Fatalf("native discover = %+v, %v", result, err)
	}
	select {
	case body := <-resultBodies:
		var result AgentFrameResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if result.Type != "discover.result" || len(result.Peers) != 1 || result.Peers[0].SessionID != "worker" {
			t.Fatalf("native result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("native discover result was not pushed back through the service")
	}
}

func TestDecodeAgentFrameBodyAcceptsPrefixedFrameInsideNativeEnvelope(t *testing.T) {
	want := AgentFrame{Version: AgentFrameVersion, Type: "discover", MessageID: "wrapped-prefix"}
	inner, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	body := `<cross-session-message from="uds:/tmp/source.sock">` + "\n" +
		claudeAgentFramePrefix + string(inner) + "\n</cross-session-message>"
	got, err := DecodeAgentFrameBody(body)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapped prefixed frame = %+v, %v; want %+v", got, err, want)
	}
}

func TestClaudeNativeAuthenticatedStreamClosesWithoutInlineReply(t *testing.T) {
	root := t.TempDir()
	sourceSocket, resultBodies := startNativeResultSink(t, filepath.Join(root, "source.sock"))
	controlPath := filepath.Join(root, "agent.sock")
	const token = "0123456789abcdef0123456789abcdef"
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "host-a"}, controlPath: controlPath,
		routeRefresh: func() error { return nil }, serviceToken: token, remote: map[string]Peer{},
		local: map[string]localPeer{
			"host-a/source": {Peer: Peer{
				ID: "host-a/source", HostID: "host-a", SessionID: "source", Name: "source",
				Entrypoint: "claude", Groups: []string{"project"},
			}, Socket: sourceSocket},
		},
	}
	listener, err := net.Listen("unix", controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			agent.handleControl(conn)
		}
	}()
	request := AgentFrame{Version: AgentFrameVersion, Type: "discover", MessageID: "native-stream"}
	inner, _ := json.Marshal(request)
	outer, _ := json.Marshal(map[string]any{
		"type": "user", "from": encodeUDS(sourceSocket),
		"message": map[string]any{"content": claudeAgentFramePrefix + string(inner)},
	})
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: controlPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(append([]byte(`{"type":"auth","token":"`+token+`"}`+"\n"), append(outer, '\n')...)); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	inline, readErr := io.ReadAll(client)
	_ = client.Close()
	if readErr != nil || len(inline) != 0 {
		t.Fatalf("Claude native stream inline response = %q, %v; want clean EOF", inline, readErr)
	}
	<-serverDone
	select {
	case body := <-resultBodies:
		var result AgentFrameResult
		if json.Unmarshal(body, &result) != nil || result.Type != "discover.result" || result.MessageID != "native-stream" {
			t.Fatalf("pushed native stream result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("authenticated native stream did not push its result")
	}
}

func TestClaudeNativeSendGetsPerTargetResultThroughService(t *testing.T) {
	root := t.TempDir()
	sourceSocket, resultBodies := startNativeResultSink(t, filepath.Join(root, "source.sock"))
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "host-a"}, controlPath: filepath.Join(root, "agent.sock"),
		routeRefresh: func() error { return nil }, remote: map[string]Peer{},
		local: map[string]localPeer{
			"host-a/source": {Peer: Peer{ID: "host-a/source", HostID: "host-a", SessionID: "source", Name: "source", Entrypoint: "claude", Groups: []string{"project"}}, Socket: sourceSocket},
			"host-a/gone":   {Peer: Peer{ID: "host-a/gone", HostID: "host-a", SessionID: "gone", Name: "gone", Entrypoint: "grok", Groups: []string{"project"}}, Socket: filepath.Join(root, "gone.sock")},
		},
	}
	request := AgentFrame{Version: AgentFrameVersion, Type: "send", MessageID: "send-native", Targets: []string{"gone"}, Content: "hello"}
	inner, _ := json.Marshal(request)
	outer, _ := json.Marshal(map[string]any{
		"type": "user", "from": encodeUDS(sourceSocket), "message": map[string]any{"content": claudeAgentFramePrefix + string(inner)},
	})
	result, err := agent.handleNativeCarrierFrame(outer)
	if err != nil || result.Type != "send.result" || len(result.Deliveries) != 1 || result.Deliveries[0].Status != "failed" {
		t.Fatalf("native send = %+v, %v", result, err)
	}
	select {
	case body := <-resultBodies:
		var pushed AgentFrameResult
		if json.Unmarshal(body, &pushed) != nil || pushed.Type != "send.result" || len(pushed.Deliveries) != 1 || pushed.Deliveries[0].Status != "failed" {
			t.Fatalf("pushed native send result = %+v", pushed)
		}
	case <-time.After(time.Second):
		t.Fatal("native send result was not pushed back through the service")
	}
}

func TestClaudeNativeCarrierRejectsUnmarkedJSON(t *testing.T) {
	root := t.TempDir()
	sourceSocket, _ := startNativeResultSink(t, filepath.Join(root, "source.sock"))
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "host-a"}, routeRefresh: func() error { return nil },
		local: map[string]localPeer{
			"host-a/source": {Peer: Peer{
				ID: "host-a/source", HostID: "host-a", SessionID: "source", Name: "source",
				Entrypoint: "claude", Groups: []string{"project"},
			}, Socket: sourceSocket},
		}, remote: map[string]Peer{},
	}
	inner, _ := json.Marshal(AgentFrame{Version: AgentFrameVersion, Type: "discover", MessageID: "unmarked"})
	outer, _ := json.Marshal(map[string]any{
		"type": "user", "from": encodeUDS(sourceSocket), "message": map[string]any{"content": string(inner)},
	})
	if _, err := agent.handleNativeCarrierFrame(outer); err == nil || !strings.Contains(err.Error(), "frame marker") {
		t.Fatalf("unmarked native carrier error = %v", err)
	}
}

func TestFederatedDeliveryRequiresCurrentNamedBroadcastGroup(t *testing.T) {
	root := t.TempDir()
	targetSocket, targetFrames := startAgentFrameSink(t, filepath.Join(root, "target.sock"))
	source := Peer{ID: "remote/source", HostID: "remote", SessionID: "source", Groups: []string{"remaining"}}
	target := localPeer{Peer: Peer{ID: "local/target", HostID: "local", SessionID: "target", Groups: []string{"remaining"}}, Socket: targetSocket}
	agent := &agent{
		options: AgentOptions{HostID: "local"}, routeRefresh: func() error { return nil },
		local: map[string]localPeer{target.ID: target}, remote: map[string]Peer{source.ID: source},
	}
	frame, _ := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "stale-broadcast", SourceSessionID: source.SessionID,
		Source: &source, Group: "removed", Content: "must not cross",
	})
	if err := agent.deliverGroupedLocal(Message{Type: "group_deliver", SourceID: source.ID, TargetID: target.ID, Frame: frame}); err == nil {
		t.Fatal("stale broadcast crossed through an unrelated shared group")
	}
	assertNoAgentFrame(t, targetFrames)
}

func TestOrphanTerminalNoticeUsesDurableParentWithoutLiveSource(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "catalog.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "parent", Product: "codex", Kind: SessionKindInteractive, ExplicitGroups: []string{"project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "grok", Kind: SessionKindLane, ParentSession: "parent", ParentSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	targetSocket, targetFrames := startAgentFrameSink(t, filepath.Join(root, "parent.sock"))
	parentGroups := []string{"project", "session:host-a/parent"}
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "host-a"}, catalog: catalog, routeRefresh: func() error { return nil },
		local: map[string]localPeer{"host-a/parent": {Peer: Peer{
			ID: "host-a/parent", HostID: "host-a", SessionID: "parent", Name: "parent", Groups: parentGroups,
		}, Socket: targetSocket}}, remote: map[string]Peer{},
	}
	body, _ := json.Marshal(AgentFrame{Version: AgentFrameVersion, Type: "send", MessageID: "orphan-terminal", Content: "collect child"})
	result, err := agent.routeTerminalNotice("child", "session:parent", body)
	if err != nil || len(result.Deliveries) != 1 || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("orphan terminal result = %+v, %v", result, err)
	}
	delivered := waitAgentFrame(t, targetFrames)
	if delivered.Source == nil || delivered.Source.SessionID != "child" || delivered.Content != "collect child" {
		t.Fatalf("orphan terminal delivery = %+v", delivered)
	}
	if _, err := agent.routeTerminalNotice("child", "not-parent", body); err == nil {
		t.Fatal("orphan terminal notice was routed to a non-parent")
	}
}

func TestFederatedOrphanTerminalNoticeAcknowledgesOnlyAfterDestinationWrite(t *testing.T) {
	root := t.TempDir()
	parent := groupedHubTestPeer("host-b", "parent", "project")
	catalog, err := openSessionCatalog(filepath.Join(root, "catalog.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "grok", Kind: SessionKindLane,
		ParentSession: parent.SessionID, ParentHostID: parent.HostID,
		ParentGroups: parent.Groups, ParentSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	targetSocket, targetFrames := startAgentFrameSink(t, filepath.Join(root, "parent.sock"))
	process := procinfo.Read(os.Getpid())
	target := localPeer{
		Peer: parent, PID: os.Getpid(), ProcStart: process.Start, AdapterStrongStart: process.StrongStart,
		Socket: targetSocket, LifecyclePID: os.Getpid(), LifecycleProcStart: process.Start,
		LifecycleStrongStart: process.StrongStart,
	}
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	sourceAgent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "host-a"}, catalog: catalog, logger: discardLogger(),
		local: map[string]localPeer{}, remote: map[string]Peer{}, pendingDeliveries: map[string]chan error{},
		localChanged: make(chan struct{}, 1),
	}
	destinationAgent := &agent{
		options: AgentOptions{HostID: "host-b", HostName: "host-b"}, logger: discardLogger(),
		local: map[string]localPeer{parent.ID: target}, remote: map[string]Peer{},
		pendingDeliveries: map[string]chan error{}, localChanged: make(chan struct{}, 1),
	}
	connectAgentToTestHub(t, hub, sourceAgent, nil)
	connectAgentToTestHub(t, hub, destinationAgent, []Peer{parent})
	if !waitFor(func() bool {
		sourceAgent.mu.RLock()
		defer sourceAgent.mu.RUnlock()
		_, ok := sourceAgent.remote[parent.ID]
		return ok
	}, time.Second) {
		t.Fatal("source agent did not receive remote parent roster")
	}
	body, _ := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "remote-orphan", Content: "collect child",
	})
	result, err := sourceAgent.routeTerminalNotice("child", parent.ID, body)
	if err != nil || len(result.Deliveries) != 1 || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("remote orphan delivery = %+v, %v", result, err)
	}
	if delivered := waitAgentFrame(t, targetFrames); delivered.Content != "collect child" || delivered.Source == nil || delivered.Source.SessionID != "child" {
		t.Fatalf("remote orphan frame = %+v", delivered)
	}

	destinationAgent.mu.Lock()
	broken := destinationAgent.local[parent.ID]
	broken.Socket = filepath.Join(root, "missing.sock")
	destinationAgent.local[parent.ID] = broken
	destinationAgent.mu.Unlock()
	result, err = sourceAgent.routeTerminalNotice("child", parent.ID, json.RawMessage(strings.ReplaceAll(string(body), "remote-orphan", "remote-orphan-failed")))
	if err != nil || len(result.Deliveries) != 1 || result.Deliveries[0].Status != "failed" {
		t.Fatalf("failed remote orphan acknowledgement = %+v, %v", result, err)
	}
}

func connectAgentToTestHub(t *testing.T, hub *hub, agent *agent, peers []Peer) {
	t.Helper()
	hubSide, agentSide := net.Pipe()
	wire := newWireConn(agentSide)
	agent.setNetwork(wire)
	go hub.handleConnection(hubSide)
	go func() {
		_ = scanMessages(agentSide, agent.handleHubMessage)
	}()
	if err := wire.Send(Message{
		Type: "hello", Version: ProtocolVersion,
		HostID: agent.options.HostID, HostName: agent.options.HostName,
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.Send(Message{Type: "snapshot", Peers: peers}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentSide.Close() })
}

func startNativeResultSink(t *testing.T, path string) (string, <-chan []byte) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	results := make(chan []byte, 4)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			_ = conn.Close()
			var outer map[string]any
			if json.Unmarshal(line, &outer) != nil {
				continue
			}
			message, _ := outer["message"].(map[string]any)
			content, _ := message["content"].(string)
			newline := strings.IndexByte(content, '\n')
			const suffix = "\n</cross-session-message>"
			if newline >= 0 && strings.HasSuffix(content, suffix) {
				results <- []byte(content[newline+1 : len(content)-len(suffix)])
			}
		}
	}()
	return path, results
}

func startAgentFrameSink(t *testing.T, path string) (string, <-chan AgentFrame) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	frames := make(chan AgentFrame, 8)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			_ = conn.Close()
			var outer map[string]any
			if json.Unmarshal(line, &outer) != nil {
				continue
			}
			message, _ := outer["message"].(map[string]any)
			content, _ := message["content"].(string)
			frame, decodeErr := DecodeAgentFrameBody(content)
			if decodeErr == nil {
				frames <- frame
			}
		}
	}()
	return path, frames
}

func waitAgentFrame(t *testing.T, frames <-chan AgentFrame) AgentFrame {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent frame")
		return AgentFrame{}
	}
}

func assertNoAgentFrame(t *testing.T, frames <-chan AgentFrame) {
	t.Helper()
	select {
	case frame := <-frames:
		t.Fatalf("unexpected agent frame: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}
