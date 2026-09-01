package federation

import (
	"encoding/json"
	"net"
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
