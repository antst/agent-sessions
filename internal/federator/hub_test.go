package federator

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

type testHubClient struct {
	conn     net.Conn
	wire     *wireConn
	messages chan Message
	errors   chan error
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

func TestHubRoutesRemoteLaneOnlyThroughConnectedCapableHosts(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "host-a")
	defer func() { _ = source.conn.Close() }()
	sourcePeer := Peer{ID: "host-a/source", HostID: "host-a", SessionID: "source"}
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
		TargetHostID: "host-b", Product: "codex", Args: []string{"list"},
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
		TargetHostID: "host-b", Product: "claude", Args: []string{"list"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := source.waitType(t, "lane_error"); got.RequestID != "request-2" || got.Error == "" {
		t.Fatalf("capability rejection = %#v", got)
	}
}

func TestHubCancelsRemoteLaneWhenSourceDisconnects(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{}}
	source := newTestHubClient(t, hub, "host-a")
	sourcePeer := Peer{ID: "host-a/source", HostID: "host-a", SessionID: "source"}
	if err := source.wire.Send(Message{Type: "snapshot", Peers: []Peer{sourcePeer}}); err != nil {
		t.Fatal(err)
	}
	source.waitType(t, "roster")
	destination := newTestHubClient(t, hub, "host-b", CapabilityCodexLane)
	defer func() { _ = destination.conn.Close() }()
	source.waitType(t, "roster")
	if err := source.wire.Send(Message{
		Type: "lane_exec", RequestID: "request-loss", SourceID: sourcePeer.ID,
		TargetHostID: "host-b", Product: "codex", Args: []string{"wait", "lane"},
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
	peerA := Peer{ID: "host-a/a", HostID: "host-a", HostName: "a", SessionID: "a"}
	if err := a.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerA}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")

	b := newTestHubClient(t, hub, "host-b")
	defer func() { _ = b.conn.Close() }()
	peerB := Peer{ID: "host-b/b", HostID: "host-b", HostName: "b", SessionID: "b"}
	if err := b.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerB}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")
	b.waitType(t, "roster")

	frame := json.RawMessage(`{"type":"user","from":"uds:/tmp/a.sock"}`)
	if err := a.wire.Send(Message{Type: "deliver", SourceID: peerA.ID, TargetID: peerB.ID, Frame: frame}); err != nil {
		t.Fatal(err)
	}
	got := b.waitType(t, "deliver")
	if got.SourceID != peerA.ID || got.TargetID != peerB.ID || string(got.Frame) != string(frame) {
		t.Fatalf("delivery = %#v", got)
	}
}

func TestHubDeliveryFailureDoesNotDisconnectSourceHost(t *testing.T) {
	hub := &hub{logger: discardLogger(), clients: map[string]*hubClient{}}
	a := newTestHubClient(t, hub, "host-a")
	defer func() { _ = a.conn.Close() }()
	peerA := Peer{ID: "host-a/a", HostID: "host-a", HostName: "a", SessionID: "a"}
	if err := a.wire.Send(Message{Type: "snapshot", Peers: []Peer{peerA}}); err != nil {
		t.Fatal(err)
	}
	a.waitType(t, "roster")

	frame := json.RawMessage(`{"type":"user","from":"uds:/tmp/a.sock"}`)
	if err := a.wire.Send(Message{Type: "deliver", SourceID: peerA.ID, TargetID: "gone/peer", Frame: frame}); err != nil {
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
