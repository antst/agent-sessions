package federation

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHubRejectsDuplicateDeliveryWhileOriginalIsPending(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	sourceServer, sourcePeer := net.Pipe()
	defer func() { _ = sourceServer.Close(); _ = sourcePeer.Close() }()
	destinationServer, destinationPeer := net.Pipe()
	defer func() { _ = destinationServer.Close(); _ = destinationPeer.Close() }()
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
		if message.Type != "delivery_error" || message.RequestID != "pending" || !strings.Contains(message.Error, "disconnected") {
			t.Fatalf("disconnect outcome = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not receive destination disconnect outcome")
	}
}

func TestHubRejectsStaleGroupSendThroughDifferentSharedGroup(t *testing.T) {
	sourcePeer := mustTestPeer(t, "host-a", "source", "codex", "remaining")
	targetPeer := mustTestPeer(t, "host-b", "target", "qwen", "remaining")
	source := &hubClient{hostID: "host-a", peers: map[string]Peer{sourcePeer.ID: sourcePeer}}
	target := &hubClient{hostID: "host-b", peers: map[string]Peer{targetPeer.ID: targetPeer}}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{"host-a": source, "host-b": target},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	frame, err := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "stale-group-send",
		SourceSessionID: sourcePeer.SessionID, Source: &sourcePeer, Group: "removed", Content: "must not cross",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.routeGrouped(source, Message{
		Type: "group_deliver", RequestID: "stale-request", SourceID: sourcePeer.ID,
		TargetID: targetPeer.ID, Frame: frame,
	}); err == nil {
		t.Fatal("hub forwarded a stale group send through an unrelated shared group")
	}
}

func TestHubRoutesExplicitOpaqueCapabilityOnlyToExactAdvertisement(t *testing.T) {
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
		hostID: "host-a", wire: newWireConn(sourceHub),
		peers: map[string]Peer{sourcePeer.ID: sourcePeer},
	}
	destination := &hubClient{
		hostID: "host-b", wire: newWireConn(destinationHub), peers: map[string]Peer{},
		capabilities: []string{"future-lane"},
	}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{"host-a": source, "host-b": destination},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	forwarded := make(chan Message, 1)
	sourceResponses := make(chan Message, 1)
	go func() {
		_ = scanMessages(destinationConn, func(message Message) error { forwarded <- message; return nil })
	}()
	go func() {
		_ = scanMessages(sourceConn, func(message Message) error { sourceResponses <- message; return nil })
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
	if response := <-sourceResponses; response.Type != "lane_error" || !strings.Contains(response.Error, "lacks") {
		t.Fatalf("wrong capability response = %#v", response)
	}
}

func TestLiveHubDeliversIdenticalCompleteRosterToEveryClient(t *testing.T) {
	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() {
		stopHub()
		if err := <-hubDone; err != nil {
			t.Fatal(err)
		}
	}()

	firstConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-first", HostName: "host-first",
		Capabilities: []string{CapabilityCodexLane},
	})
	defer func() { _ = firstConn.Close() }()
	firstDecoder := json.NewDecoder(firstConn)
	expectRawFrameType(t, firstDecoder, "hello_ok")
	baseline := mustTestPeer(t, "host-first", "baseline", "codex", "project")
	if err := newWireConn(firstConn).Send(Message{Type: "snapshot", Peers: []Peer{baseline}}); err != nil {
		t.Fatal(err)
	}

	secondConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-second", HostName: "host-second",
		Capabilities: []string{CapabilityQwenLane},
	})
	defer func() { _ = secondConn.Close() }()
	secondDecoder := json.NewDecoder(secondConn)
	expectRawFrameType(t, secondDecoder, "hello_ok")
	if err := newWireConn(secondConn).Send(Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}

	thirdConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-third", HostName: "host-third",
		Capabilities: []string{"future-lane"},
	})
	defer func() { _ = thirdConn.Close() }()
	thirdDecoder := json.NewDecoder(thirdConn)
	expectRawFrameType(t, thirdDecoder, "hello_ok")
	future := mustTestPeer(t, "host-third", "future", "codex", "project")
	future.Product, future.Entrypoint = "future-product", "future-product"
	future.InstanceID = "future:instance"
	future.Groups = []string{"future-group", PrivateGroup("host-third", "future")}
	if err := newWireConn(thirdConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}

	firstRoster := readCompleteRoster(t, firstDecoder, 3, baseline.ID, future.ID)
	secondRoster := readCompleteRoster(t, secondDecoder, 3, baseline.ID, future.ID)
	thirdRoster := readCompleteRoster(t, thirdDecoder, 3, baseline.ID, future.ID)
	if !reflect.DeepEqual(firstRoster, secondRoster) || !reflect.DeepEqual(firstRoster, thirdRoster) {
		t.Fatalf("clients received different rosters:\nfirst=%#v\nsecond=%#v\nthird=%#v", firstRoster, secondRoster, thirdRoster)
	}
}

func TestHubRejectsVersionMismatchBeforeRegistration(t *testing.T) {
	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() { stopHub(); <-hubDone }()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := newWireConn(conn).Send(Message{
		Type: "hello", Version: ProtocolVersion + 1, HostID: "future", HostName: "future",
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err == nil {
		t.Fatalf("mismatched participant received response %#v", response)
	}
}

func TestHubRejectsInvalidHelloCapabilities(t *testing.T) {
	server, client := net.Pipe()
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
		deliveryRoutes: map[string]*deliveryRoute{}, clientTimeout: time.Second,
	}
	go h.handleConnection(server)
	if err := newWireConn(client).Send(Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-a", HostName: "host-a",
		Capabilities: []string{"valid-lane", "Invalid"},
	}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var response Message
	if err := json.NewDecoder(client).Decode(&response); err == nil {
		t.Fatalf("invalid capability hello received response %#v", response)
	}
	_ = client.Close()
}

func testParentContext(peer Peer) *ParentContext {
	return &ParentContext{
		HostID: peer.HostID, SessionID: peer.SessionID, Product: peer.Product,
		InstanceID: peer.InstanceID, Groups: append([]string(nil), peer.Groups...),
	}
}

func connectRawHubClient(t *testing.T, address string, hello Message) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := newWireConn(conn).Send(hello); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn
}

func expectRawFrameType(t *testing.T, decoder *json.Decoder, wanted string) Message {
	t.Helper()
	for {
		var message Message
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("read %s frame: %v", wanted, err)
		}
		if message.Type == wanted {
			return message
		}
	}
}

func readCompleteRoster(t *testing.T, decoder *json.Decoder, minimumHosts int, peerIDs ...string) Message {
	t.Helper()
	for {
		message := expectRawFrameType(t, decoder, "roster")
		if message.Version != ProtocolVersion || len(message.Hosts) < minimumHosts {
			continue
		}
		complete := true
		for _, peerID := range peerIDs {
			complete = complete && containsPeerID(message.Peers, peerID)
		}
		if complete {
			return message
		}
	}
}

func containsPeerID(peers []Peer, peerID string) bool {
	for _, peer := range peers {
		if peer.ID == peerID {
			return true
		}
	}
	return false
}
