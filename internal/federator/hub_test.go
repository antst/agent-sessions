package federator

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"reflect"
	"testing"
	"time"
)

type testHubClient struct {
	conn     net.Conn
	wire     *wireConn
	messages chan Message
	errors   chan error
}

func groupedHubTestPeer(host, session string, groups ...string) Peer {
	groups = append(groups, privateGroupPrefix+host+"/"+session)
	return Peer{
		ID: host + "/" + session, HostID: host, HostName: host, SessionID: session,
		GlobalID: globalSessionID(host, session), Name: session, Entrypoint: "codex",
		PeerProtocol: GroupProtocolVersion, InstanceID: "instance-" + host + "-" + session,
		Groups: sortedUnique(groups),
	}
}

func groupedHubTestParent(peer Peer) *ParentContext {
	return &ParentContext{
		HostID: peer.HostID, SessionID: peer.SessionID, Product: peer.Entrypoint,
		InstanceID: peer.InstanceID, Groups: append([]string(nil), peer.Groups...),
	}
}

func TestHubRejectsInvalidGroupedSnapshotWithoutReplacingPrevious(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	hubSide, peerSide := net.Pipe()
	t.Cleanup(func() { _ = hubSide.Close(); _ = peerSide.Close() })
	go func() { _, _ = io.Copy(io.Discard, peerSide) }()
	client := &hubClient{hostID: "host-a", peers: map[string]Peer{}, wire: newWireConn(hubSide)}
	hub.clients[client.hostID] = client
	valid := groupedHubTestPeer("host-a", "valid", "project")
	if err := hub.handleClientMessage(client, Message{Type: "snapshot", Peers: []Peer{valid}}); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.SessionID, invalid.ID, invalid.GlobalID = "forged", "host-a/forged", globalSessionID("host-a", "forged")
	invalid.Groups = []string{""}
	if err := hub.handleClientMessage(client, Message{Type: "snapshot", Peers: []Peer{invalid}}); err == nil {
		t.Fatal("invalid snapshot was accepted")
	}
	if len(client.peers) != 1 || client.peers[valid.ID].SessionID != valid.SessionID {
		t.Fatalf("invalid snapshot replaced previous peers: %+v", client.peers)
	}
	duplicate := groupedHubTestPeer("host-a", "valid", "project")
	duplicate.Name = "duplicate"
	if err := hub.handleClientMessage(client, Message{Type: "snapshot", Peers: []Peer{valid, duplicate}}); err == nil {
		t.Fatal("duplicate snapshot identity was accepted")
	}
}

func newTestHubClient(t *testing.T, hub *hub, host string, capabilities ...string) *testHubClient {
	t.Helper()
	server, client := net.Pipe()
	go hub.handleConnection(server)
	result := &testHubClient{
		conn: client, wire: newWireConn(client), messages: make(chan Message, 32), errors: make(chan error, 1),
	}
	go func() {
		result.errors <- scanMessages(client, func(message Message) error {
			result.messages <- message
			return nil
		})
	}()
	if err := result.wire.Send(Message{
		Type: "hello", Version: ProtocolVersion, HostID: host, HostName: host,
		Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	result.waitType(t, "hello_ok")
	return result
}

func TestHubRejectsProtocolMismatchBeforeHostRegistration(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}, clientTimeout: time.Second}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		hub.handleConnection(server)
		close(done)
	}()
	if err := newWireConn(client).Send(Message{
		Type: "hello", Version: ProtocolVersion + 1, HostID: "mismatch", HostName: "mismatch",
	}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.ReadAll(client)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hub did not terminate the protocol-mismatched connection")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.clients) != 0 {
		t.Fatalf("protocol-mismatched host registered before rejection: %+v", hub.clients)
	}
}

func TestHubRoutesRemoteLaneOnlyThroughConnectedCapableHosts(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "host-a")
	defer func() { _ = source.conn.Close() }()
	sourcePeer := groupedHubTestPeer("host-a", "source")
	if err := source.wire.Send(Message{Type: "snapshot", Peers: []Peer{sourcePeer}}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "roster")

	destination := newTestHubClient(t, hub, "host-b", CapabilityCodexLane)
	defer func() { _ = destination.conn.Close() }()
	roster := source.waitType(t, "roster")
	if len(roster.Hosts) != 2 || !hostHasCapability(roster.Hosts[1], CapabilityCodexLane) {
		t.Fatalf("host roster = %#v", roster.Hosts)
	}

	request := Message{
		Type: "lane_exec", RequestID: "request-1", SourceID: sourcePeer.ID,
		TargetHostID: "host-b", Product: "codex", Args: []string{"list"}, ParentContext: groupedHubTestParent(sourcePeer),
	}
	if err := source.wire.Send(request); err != nil {
		t.Fatal(err)
	}
	forwarded := destination.waitType(t, "lane_exec")
	if forwarded.RequestID != request.RequestID || forwarded.SourceID != sourcePeer.ID {
		t.Fatalf("forwarded request = %#v", forwarded)
	}
	if err := destination.wire.Send(Message{Type: "lane_stdout", RequestID: request.RequestID, Data: []byte("one\n")}); err != nil {
		t.Fatal(err)
	}
	if got := source.waitType(t, "lane_stdout"); string(got.Data) != "one\n" {
		t.Fatalf("lane stdout = %#v", got)
	}
	if err := source.wire.Send(Message{Type: "lane_cancel", RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	if got := destination.waitType(t, "lane_cancel"); got.RequestID != request.RequestID {
		t.Fatalf("lane cancel = %#v", got)
	}
	if err := destination.wire.Send(Message{Type: "lane_exit", RequestID: request.RequestID, ExitCode: 130}); err != nil {
		t.Fatal(err)
	}
	if got := source.waitType(t, "lane_exit"); got.ExitCode != 130 {
		t.Fatalf("lane exit = %#v", got)
	}

	if err := source.wire.Send(Message{
		Type: "lane_exec", RequestID: "request-2", SourceID: sourcePeer.ID,
		TargetHostID: "host-b", Product: "claude", Args: []string{"list"}, ParentContext: groupedHubTestParent(sourcePeer),
	}); err != nil {
		t.Fatal(err)
	}
	if got := source.waitType(t, "lane_error"); got.RequestID != "request-2" || got.Error == "" {
		t.Fatalf("capability rejection = %#v", got)
	}
}

func TestHubRoutesQwenLaneWithExactAttestedParentContext(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "qwen-source")
	defer func() { _ = source.conn.Close() }()
	sourcePeer := groupedHubTestPeer("qwen-source", "parent", "project")
	sourcePeer.Entrypoint = "qwen"
	if err := source.wire.Send(Message{Type: "snapshot", Peers: []Peer{sourcePeer}}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "roster")
	destination := newTestHubClient(t, hub, "qwen-destination", CapabilityQwenLane)
	defer func() { _ = destination.conn.Close() }()
	source.waitType(t, "roster")
	parent := groupedHubTestParent(sourcePeer)
	parent.AgentRuntimeDir = "/tmp/qwen source runtime"
	parent.QwenCapabilityDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request := Message{
		Type: "lane_exec", RequestID: "qwen-request", SourceID: sourcePeer.ID,
		TargetHostID: "qwen-destination", Product: "qwen", Args: []string{"start", "--name", "worker", "-"},
		ParentContext: parent,
	}
	if err := source.wire.Send(request); err != nil {
		t.Fatal(err)
	}
	forwarded := destination.waitType(t, "lane_exec")
	if forwarded.ParentContext == nil || !reflect.DeepEqual(*forwarded.ParentContext, *parent) {
		t.Fatalf("forwarded Qwen parent = %#v, want %#v", forwarded.ParentContext, parent)
	}
	if err := destination.wire.Send(Message{Type: "lane_exit", RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "lane_exit")
}

func TestHubCancelsQwenLaneWhenSourceDisconnects(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "qwen-source")
	sourcePeer := groupedHubTestPeer("qwen-source", "parent")
	sourcePeer.Entrypoint = "qwen"
	if err := source.wire.Send(Message{Type: "snapshot", Peers: []Peer{sourcePeer}}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "roster")
	destination := newTestHubClient(t, hub, "qwen-destination", CapabilityQwenLane)
	defer func() { _ = destination.conn.Close() }()
	source.waitType(t, "roster")
	if err := source.wire.Send(Message{
		Type: "lane_exec", RequestID: "qwen-disconnect", SourceID: sourcePeer.ID,
		TargetHostID: "qwen-destination", Product: "qwen", Args: []string{"wait", "lane"},
		ParentContext: groupedHubTestParent(sourcePeer),
	}); err != nil {
		t.Fatal(err)
	}
	destination.waitType(t, "lane_exec")
	_ = source.conn.Close()
	if got := destination.waitType(t, "lane_cancel"); got.RequestID != "qwen-disconnect" {
		t.Fatalf("Qwen disconnect cancel = %#v", got)
	}
}

func TestHubCancelsRemoteLaneWhenSourceDisconnects(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "host-a")
	sourcePeer := groupedHubTestPeer("host-a", "source")
	if err := source.wire.Send(Message{Type: "snapshot", Peers: []Peer{sourcePeer}}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "roster")
	destination := newTestHubClient(t, hub, "host-b", CapabilityCodexLane)
	defer func() { _ = destination.conn.Close() }()
	source.waitType(t, "roster")
	if err := source.wire.Send(Message{
		Type: "lane_exec", RequestID: "request-loss", SourceID: sourcePeer.ID,
		TargetHostID: "host-b", Product: "codex", Args: []string{"wait", "lane"}, ParentContext: groupedHubTestParent(sourcePeer),
	}); err != nil {
		t.Fatal(err)
	}
	destination.waitType(t, "lane_exec")
	_ = source.conn.Close()
	if got := destination.waitType(t, "lane_cancel"); got.RequestID != "request-loss" {
		t.Fatalf("disconnect cancel = %#v", got)
	}
}

func TestHubDisconnectNotificationsDoNotBlockBehindSlowRoute(t *testing.T) {
	blockedHub, blockedPeer := net.Pipe()
	defer func() { _ = blockedHub.Close() }()
	defer func() { _ = blockedPeer.Close() }()
	healthyHub, healthyPeer := net.Pipe()
	defer func() { _ = healthyHub.Close() }()
	defer func() { _ = healthyPeer.Close() }()
	source := &hubClient{hostID: "source"}
	blocked := &hubClient{hostID: "blocked", wire: newWireConnWithTimeout(blockedHub, 500*time.Millisecond)}
	healthy := &hubClient{hostID: "healthy", wire: newWireConn(healthyHub)}
	hub := &hub{
		logger: discardLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{
			"blocked": newLaneRoute(source, blocked, "", ""),
			"healthy": newLaneRoute(source, healthy, "", ""),
		},
	}
	received := make(chan Message, 1)
	go func() {
		_ = scanMessages(healthyPeer, func(message Message) error {
			received <- message
			return nil
		})
	}()
	started := time.Now()
	hub.dropLaneRoutes(source)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("disconnect cleanup blocked on a slow route for %s", elapsed)
	}
	select {
	case message := <-received:
		if message.Type != "lane_cancel" || message.RequestID != "healthy" {
			t.Fatalf("healthy notification = %#v", message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("healthy route cancellation was delayed by blocked route")
	}
}

func TestHubOutputFailureCancelsOnlyItsLaneRoute(t *testing.T) {
	blockedHubSide, blockedPeerSide := net.Pipe()
	defer func() { _ = blockedHubSide.Close() }()
	defer func() { _ = blockedPeerSide.Close() }()
	destinationHubSide, destinationPeerSide := net.Pipe()
	defer func() { _ = destinationHubSide.Close() }()
	defer func() { _ = destinationPeerSide.Close() }()
	source := &hubClient{hostID: "source", wire: newWireConnWithTimeout(blockedHubSide, 500*time.Millisecond)}
	destination := &hubClient{hostID: "destination", wire: newWireConn(destinationHubSide)}
	hub := &hub{
		logger: discardLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{
			"request": newLaneRoute(source, destination, "", ""),
		},
	}
	cancelled := make(chan Message, 1)
	go func() {
		_ = scanMessages(destinationPeerSide, func(message Message) error {
			cancelled <- message
			return nil
		})
	}()
	go hub.forwardLaneRoute("request", hub.laneRoutes["request"])
	started := time.Now()
	if err := hub.routeLaneResponse(destination, Message{
		Type: "lane_stdout", RequestID: "request", Data: []byte("blocked"),
	}); err != nil {
		t.Fatalf("one failed source disconnected the destination: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("destination scanner blocked on source output for %s", elapsed)
	}
	select {
	case message := <-cancelled:
		if message.Type != "lane_cancel" || message.RequestID != "request" {
			t.Fatalf("cancel = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("destination lane was not cancelled")
	}
	if len(hub.laneRoutes) != 0 {
		t.Fatalf("dead output route retained: %#v", hub.laneRoutes)
	}
}

func (c *testHubClient) waitType(t *testing.T, kind string) Message {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case message := <-c.messages:
			if message.Type == kind {
				return message
			}
		case err := <-c.errors:
			t.Fatalf("hub client ended: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func TestHubRoutesOnlyAdvertisedPeers(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}}
	a := newTestHubClient(t, hub, "host-a")
	defer func() { _ = a.conn.Close() }()
	peerA := groupedHubTestPeer("host-a", "a", "team")
	if err := a.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerA}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")

	b := newTestHubClient(t, hub, "host-b")
	defer func() { _ = b.conn.Close() }()
	peerB := groupedHubTestPeer("host-b", "b", "team")
	if err := b.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerB}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")
	b.waitType(t, "roster")

	frame, err := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "m1",
		SourceSessionID: peerA.SessionID, Source: &peerA, Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.wire.Send(Message{
		Type: "group_deliver", RequestID: "delivery-1",
		SourceID: peerA.ID, TargetID: peerB.ID, Frame: frame,
	}); err != nil {
		t.Fatal(err)
	}
	got := b.waitType(t, "group_deliver")
	if got.SourceID != peerA.ID || got.TargetID != peerB.ID || string(got.Frame) != string(frame) {
		t.Fatalf("delivery = %#v", got)
	}
	if err := b.wire.Send(Message{
		Type: "delivery_ack", RequestID: got.RequestID,
		SourceID: got.SourceID, TargetID: got.TargetID,
	}); err != nil {
		t.Fatal(err)
	}
	if ack := a.waitType(t, "delivery_ack"); ack.RequestID != got.RequestID {
		t.Fatalf("delivery acknowledgement = %#v", ack)
	}
}

func TestHubDestinationDisconnectFailsPendingDelivery(t *testing.T) {
	sourceHub, sourcePeer := net.Pipe()
	destinationHub, destinationPeer := net.Pipe()
	defer func() {
		_ = sourceHub.Close()
		_ = sourcePeer.Close()
		_ = destinationHub.Close()
		_ = destinationPeer.Close()
	}()
	source := &hubClient{hostID: "source", wire: newWireConn(sourceHub)}
	destination := &hubClient{hostID: "destination", wire: newWireConn(destinationHub)}
	hub := &hub{
		logger: discardLogger(), clients: map[string]*hubClient{},
		deliveryRoutes: map[string]*deliveryRoute{
			"pending": {source: source, destination: destination, sourceID: "source/a", targetID: "destination/b"},
		},
	}
	received := make(chan Message, 1)
	go func() {
		_ = scanMessages(sourcePeer, func(message Message) error {
			received <- message
			return nil
		})
	}()
	hub.dropDeliveryRoutes(destination)
	select {
	case message := <-received:
		if message.Type != "delivery_error" || message.RequestID != "pending" {
			t.Fatalf("disconnect outcome = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not receive destination disconnect outcome")
	}
	if len(hub.deliveryRoutes) != 0 {
		t.Fatalf("delivery route survived destination disconnect: %#v", hub.deliveryRoutes)
	}
}

func TestHubSourceDisconnectDropsPendingDeliveryAndIgnoresLateOutcome(t *testing.T) {
	source := &hubClient{hostID: "source"}
	destination := &hubClient{hostID: "destination"}
	hub := &hub{deliveryRoutes: map[string]*deliveryRoute{
		"pending": {source: source, destination: destination, sourceID: "source/a", targetID: "destination/b"},
	}}
	hub.dropDeliveryRoutes(source)
	if len(hub.deliveryRoutes) != 0 {
		t.Fatalf("source disconnect retained routes: %#v", hub.deliveryRoutes)
	}
	if err := hub.routeDeliveryOutcome(destination, Message{
		Type: "delivery_ack", RequestID: "pending", SourceID: "source/a", TargetID: "destination/b",
	}); err != nil {
		t.Fatalf("late destination outcome disconnected the surviving host: %v", err)
	}
}

func TestHubRejectsStaleBroadcastThroughDifferentSharedGroup(t *testing.T) {
	sourcePeer := groupedHubTestPeer("host-a", "a", "remaining")
	targetPeer := groupedHubTestPeer("host-b", "b", "remaining")
	source := &hubClient{hostID: "host-a", peers: map[string]Peer{sourcePeer.ID: sourcePeer}}
	target := &hubClient{hostID: "host-b", peers: map[string]Peer{targetPeer.ID: targetPeer}}
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{"host-a": source, "host-b": target}}
	frame, _ := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "stale-broadcast", SourceSessionID: sourcePeer.SessionID,
		Source: &sourcePeer, Group: "removed", Content: "must not cross",
	})
	if err := hub.routeGrouped(source, Message{Type: "group_deliver", SourceID: sourcePeer.ID, TargetID: targetPeer.ID, Frame: frame}); err == nil {
		t.Fatal("hub forwarded a stale broadcast through an unrelated shared group")
	}
}

func TestHubDeliveryFailureDoesNotDisconnectSourceHost(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}}
	a := newTestHubClient(t, hub, "host-a")
	defer func() { _ = a.conn.Close() }()
	peerA := groupedHubTestPeer("host-a", "a", "team")
	if err := a.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerA}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")

	frame := json.RawMessage(`{"version":1,"type":"delivery","message_id":"m1","source_session_id":"a","source":{"id":"host-a/a"},"content":"hello"}`)
	if err := a.wire.Send(Message{Type: "group_deliver", SourceID: peerA.ID, TargetID: "gone/peer", Frame: frame}); err != nil {
		t.Fatal(err)
	}
	failure := a.waitType(t, "delivery_error")
	if failure.SourceID != peerA.ID || failure.TargetID != "gone/peer" || failure.Error == "" {
		t.Fatalf("delivery error = %#v", failure)
	}
	if err := a.wire.Send(Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "pong")
}

func TestHubProbeReportsCompatibleProtocolWithoutRegisteringHost(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}}
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go hub.handleConnection(server)
	wire := newWireConn(client)
	if err := wire.Send(Message{Type: "probe", Version: ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	var response Message
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "probe_ok" || response.Version != ProtocolVersion {
		t.Fatalf("probe response = %#v", response)
	}
	hub.mu.Lock()
	clientCount := len(hub.clients)
	hub.mu.Unlock()
	if clientCount != 0 {
		t.Fatalf("probe registered %d hub clients", clientCount)
	}
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
