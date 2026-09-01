package federation

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHubRejectsStaleGenerationAndReplacesEqualOrNewerReconnect(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	server1, peer1 := net.Pipe()
	defer func() { _ = peer1.Close() }()
	current := &hubClient{hostID: "host-a", hostName: "host-a", generation: 9, wire: newWireConn(server1), peers: map[string]Peer{}}
	if err := h.register(current); err != nil {
		t.Fatal(err)
	}
	server2, peer2 := net.Pipe()
	defer func() { _ = server2.Close() }()
	defer func() { _ = peer2.Close() }()
	stale := &hubClient{hostID: "host-a", hostName: "host-a", generation: 8, wire: newWireConn(server2), peers: map[string]Peer{}}
	if err := h.register(stale); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale generation result = %v", err)
	}
	if h.clients["host-a"] != current {
		t.Fatal("stale registration displaced the current host")
	}
	server3, peer3 := net.Pipe()
	defer func() { _ = peer3.Close() }()
	reconnected := &hubClient{hostID: "host-a", hostName: "host-a", generation: 9, wire: newWireConn(server3), peers: map[string]Peer{}}
	if err := h.register(reconnected); err != nil {
		t.Fatal(err)
	}
	if h.clients["host-a"] != reconnected {
		t.Fatal("same-generation reconnect did not atomically replace the old connection")
	}
}

func TestHubRejectsMalformedSnapshotWithoutReplacingLastGoodRoster(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	server, peerConn := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = peerConn.Close() }()
	valid := mustTestPeer(t, "host-a", "valid", "codex", "project")
	client := &hubClient{hostID: "host-a", hostName: "host-a", wire: newWireConn(server), peers: map[string]Peer{valid.ID: valid}}
	h.clients[client.hostID] = client
	invalid := valid
	invalid.Groups = []string{""}
	if err := h.handleClientMessage(client, Message{Type: "snapshot", Peers: []Peer{invalid}}); err == nil {
		t.Fatal("malformed snapshot was accepted")
	}
	if got := client.peers[valid.ID]; got.InstanceID != valid.InstanceID || len(client.peers) != 1 {
		t.Fatalf("last good roster changed: %#v", client.peers)
	}
	if client.ready {
		t.Fatal("malformed initial snapshot made the host ready")
	}
}

func TestHubPublishesHostOnlyAfterInitialSnapshot(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	server, peerConn := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = peerConn.Close() }()
	client := &hubClient{
		hostID: "host-a", hostName: "host-a", generation: 7,
		wire: newWireConn(server), peers: map[string]Peer{},
	}
	h.clients[client.hostID] = client
	if client.ready {
		t.Fatal("registered host was ready before its initial snapshot")
	}

	received := make(chan Message, 1)
	go func() {
		_ = scanMessages(peerConn, func(message Message) error {
			received <- message
			return nil
		})
	}()
	if err := h.handleClientMessage(client, Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	if !client.ready {
		t.Fatal("valid initial snapshot did not make the host ready")
	}
	select {
	case roster := <-received:
		if roster.Type != "roster" || len(roster.Hosts) != 1 || roster.Hosts[0].ID != client.hostID {
			t.Fatalf("initial roster = %#v", roster)
		}
	case <-time.After(time.Second):
		t.Fatal("initial snapshot did not publish the host roster")
	}
}

func TestHubRejectsDuplicateDeliveryWhileOriginalIsPending(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	sourceServer, sourcePeer := net.Pipe()
	defer func() { _ = sourceServer.Close() }()
	defer func() { _ = sourcePeer.Close() }()
	destinationServer, destinationPeer := net.Pipe()
	defer func() { _ = destinationServer.Close() }()
	defer func() { _ = destinationPeer.Close() }()
	source := &hubClient{hostID: "source", wire: newWireConn(sourceServer)}
	destination := &hubClient{hostID: "destination", wire: newWireConn(destinationServer)}
	received := make(chan Message, 1)
	go func() {
		_ = scanMessages(destinationPeer, func(message Message) error {
			received <- message
			return nil
		})
	}()
	message := Message{Type: "group_deliver", RequestID: "same-request", SourceID: "source/a", TargetID: "destination/b"}
	if err := h.forwardDelivery(source, destination, message); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("original delivery was not forwarded")
	}
	if err := h.forwardDelivery(source, destination, message); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate pending delivery result = %v", err)
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
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
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
	h.dropDeliveryRoutes(destination)
	select {
	case message := <-received:
		if message.Type != "delivery_error" || message.RequestID != "pending" ||
			!strings.Contains(message.Error, "disconnected") {
			t.Fatalf("disconnect outcome = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not receive destination disconnect outcome")
	}
	if len(h.deliveryRoutes) != 0 {
		t.Fatalf("delivery route survived destination disconnect: %#v", h.deliveryRoutes)
	}
}

func TestHubRejectsStaleBroadcastThroughDifferentSharedGroup(t *testing.T) {
	sourcePeer := mustTestPeer(t, "host-a", "source", "codex", "remaining")
	targetPeer := mustTestPeer(t, "host-b", "target", "qwen", "remaining")
	source := &hubClient{hostID: "host-a", peers: map[string]Peer{sourcePeer.ID: sourcePeer}}
	target := &hubClient{hostID: "host-b", peers: map[string]Peer{targetPeer.ID: targetPeer}}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{"host-a": source, "host-b": target},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	frame, err := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "stale-broadcast",
		SourceSessionID: sourcePeer.SessionID, Source: &sourcePeer, Group: "removed", Content: "must not cross",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.routeGrouped(source, Message{
		Type: "group_deliver", RequestID: "stale-request", SourceID: sourcePeer.ID,
		TargetID: targetPeer.ID, Frame: frame,
	}); err == nil {
		t.Fatal("hub forwarded a stale broadcast through an unrelated shared group")
	}
}

func TestHubRoutesUnknownOpaqueCapabilityOnlyToExactAdvertisement(t *testing.T) {
	sourcePeer := mustTestPeer(t, "host-a", "source", "codex", "project")
	sourceHub, sourceConn := net.Pipe()
	destinationHub, destinationConn := net.Pipe()
	defer func() {
		_ = sourceHub.Close()
		_ = sourceConn.Close()
		_ = destinationHub.Close()
		_ = destinationConn.Close()
	}()
	source := &hubClient{
		hostID: "host-a", ready: true, wire: newWireConn(sourceHub),
		peers: map[string]Peer{sourcePeer.ID: sourcePeer},
	}
	destination := &hubClient{
		hostID: "host-b", ready: true, wire: newWireConn(destinationHub), peers: map[string]Peer{},
		capabilities: []string{"future-lane"},
	}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{"host-a": source, "host-b": destination},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	forwarded := make(chan Message, 1)
	sourceResponses := make(chan Message, 1)
	go func() {
		_ = scanMessages(destinationConn, func(message Message) error {
			forwarded <- message
			return nil
		})
	}()
	go func() {
		_ = scanMessages(sourceConn, func(message Message) error {
			sourceResponses <- message
			return nil
		})
	}()
	request := Message{
		Type: "lane_exec", RequestID: "opaque-route", SourceID: sourcePeer.ID, TargetHostID: "host-b",
		Product: "future", Capabilities: []string{"future-lane"}, Args: []string{"run"},
		ParentContext: testParentContext(sourcePeer),
	}
	if err := h.routeLaneExec(source, request); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-forwarded:
		if got.Product != "future" || !reflect.DeepEqual(got.Capabilities, []string{"future-lane"}) {
			t.Fatalf("forwarded opaque request = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("opaque lane request was not forwarded")
	}

	mismatch := request
	mismatch.RequestID = "wrong-route"
	mismatch.Capabilities = []string{"other-lane"}
	if err := h.routeLaneExec(source, mismatch); err != nil {
		t.Fatal(err)
	}
	response := <-sourceResponses
	if response.Type != "lane_error" || !strings.Contains(response.Error, "lacks") {
		t.Fatalf("wrong capability response = %#v", response)
	}

	destination.capabilities = []string{CapabilityCodexLane}
	legacy := request
	legacy.RequestID = "legacy-route"
	legacy.Product = "codex"
	legacy.Capabilities = nil
	if err := h.routeLaneExec(source, legacy); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-forwarded:
		if got.RequestID != "legacy-route" || len(got.Capabilities) != 0 {
			t.Fatalf("forwarded legacy request = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy original-four lane request was not forwarded")
	}
}

func TestHubMixedV3CapabilityAdmissionUsesFrozenLegacyMapOnly(t *testing.T) {
	for _, test := range []struct {
		name         string
		product      string
		capabilities []string
		want         string
		legacy       bool
		wantError    bool
	}{
		{name: "legacy codex", product: "codex", want: CapabilityCodexLane, legacy: true},
		{name: "legacy claude", product: "claude", want: CapabilityClaudeLane, legacy: true},
		{name: "legacy grok", product: "grok", want: CapabilityGrokLane, legacy: true},
		{name: "legacy qwen", product: "qwen", want: CapabilityQwenLane, legacy: true},
		{name: "new empty", product: "future", wantError: true},
		{name: "new exact", product: "future", capabilities: []string{"future-lane"}, want: "future-lane"},
		{name: "multi", product: "future", capabilities: []string{"future-lane", "other-lane"}, wantError: true},
		{name: "duplicate", product: "future", capabilities: []string{"future-lane", "future-lane"}, wantError: true},
		{name: "invalid", product: "future", capabilities: []string{"Future-lane"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, legacy, err := laneCapabilityForMessage(Message{Product: test.product, Capabilities: test.capabilities})
			if test.wantError {
				if err == nil {
					t.Fatalf("capability admission = %q, legacy=%t", got, legacy)
				}
				return
			}
			if err != nil || got != test.want || legacy != test.legacy {
				t.Fatalf("capability admission = %q, legacy=%t, err=%v", got, legacy, err)
			}
		})
	}
}

func TestHubRejectsInvalidHelloCapabilitiesWithoutSilentFiltering(t *testing.T) {
	for _, capabilities := range [][]string{{"valid-lane", "Invalid"}, append(make([]string, maxWireCapabilities), "valid-lane")} {
		server, client := net.Pipe()
		h := &hub{
			logger: discardTestLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
			deliveryRoutes: map[string]*deliveryRoute{}, clientTimeout: time.Second,
		}
		go h.handleConnection(server)
		if err := newWireConn(client).Send(Message{
			Type: "hello", Version: ProtocolVersion, HostID: "host-a", HostName: "host-a", Capabilities: capabilities,
		}); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		var response Message
		if err := json.NewDecoder(client).Decode(&response); err == nil {
			t.Fatalf("invalid capability hello received response %#v", response)
		}
		if len(h.clients) != 0 {
			t.Fatalf("invalid capability hello registered: %#v", h.clients)
		}
		_ = client.Close()
	}
}

func TestHubFencesMessagesFromReplacedGeneration(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
		deliveryRoutes: map[string]*deliveryRoute{},
	}
	oldServer, oldPeer := net.Pipe()
	newServer, newPeer := net.Pipe()
	defer func() {
		_ = oldServer.Close()
		_ = oldPeer.Close()
		_ = newServer.Close()
		_ = newPeer.Close()
	}()
	old := &hubClient{hostID: "host-a", generation: 7, wire: newWireConn(oldServer), peers: map[string]Peer{}}
	current := &hubClient{hostID: "host-a", generation: 8, wire: newWireConn(newServer), peers: map[string]Peer{}}
	h.clients[old.hostID] = current
	if err := h.handleClientMessage(old, Message{Type: "snapshot"}); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("superseded client message result = %v", err)
	}
	if current.ready {
		t.Fatal("superseded generation mutated the current registration")
	}
}

func testParentContext(peer Peer) *ParentContext {
	return &ParentContext{
		HostID: peer.HostID, SessionID: peer.SessionID, Product: peer.Product,
		InstanceID: peer.InstanceID, Groups: append([]string(nil), peer.Groups...),
	}
}
