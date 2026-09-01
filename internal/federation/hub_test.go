package federation

import (
	"encoding/json"
	"fmt"
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
	current := &hubClient{hostID: "host-a", hostName: "host-a", generation: 9, ready: true, wire: newWireConn(server1), peers: map[string]Peer{}}
	h.clients[current.hostID] = current
	server2, peer2 := net.Pipe()
	defer func() { _ = server2.Close() }()
	defer func() { _ = peer2.Close() }()
	stale := &hubClient{hostID: "host-a", hostName: "host-a", generation: 8, wire: newWireConn(server2), peers: map[string]Peer{}}
	if err := h.validateRegistrationCandidate(stale); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale generation result = %v", err)
	}
	if h.clients["host-a"] != current {
		t.Fatal("stale registration displaced the current host")
	}
	server3, peer3 := net.Pipe()
	defer func() { _ = peer3.Close() }()
	reconnected := &hubClient{hostID: "host-a", hostName: "host-a", generation: 9, wire: newWireConn(server3), peers: map[string]Peer{}}
	if err := h.validateRegistrationCandidate(reconnected); err != nil {
		t.Fatal(err)
	}
	if h.clients["host-a"] != current {
		t.Fatal("hello candidate replaced the current host before its initial snapshot")
	}
	received := make(chan Message, 1)
	go func() {
		_ = scanMessages(peer3, func(message Message) error {
			received <- message
			return nil
		})
	}()
	if err := h.promoteInitialSnapshot(reconnected, Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	if h.clients["host-a"] != reconnected {
		t.Fatal("validated same-generation snapshot did not atomically replace the old connection")
	}
	select {
	case roster := <-received:
		if roster.Type != "roster" || len(roster.Hosts) != 1 || roster.Hosts[0].Generation != 9 {
			t.Fatalf("promoted roster = %#v", roster)
		}
	case <-time.After(time.Second):
		t.Fatal("promoted reconnect did not receive the uniform roster")
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
	empty := request
	empty.RequestID = "empty-capability"
	empty.Product = "codex"
	empty.Capabilities = nil
	if err := h.routeLaneExec(source, empty); err != nil {
		t.Fatal(err)
	}
	emptyResponse := <-sourceResponses
	if emptyResponse.Type != "lane_error" || !strings.Contains(emptyResponse.Error, "exactly one capability") {
		t.Fatalf("empty capability response = %#v", emptyResponse)
	}
	if _, exists := h.laneRoutes[empty.RequestID]; exists {
		t.Fatal("empty-capability request was offered as a lane route")
	}

	multiple := request
	multiple.RequestID = "multiple-capabilities"
	multiple.Capabilities = []string{"future-lane", "other-lane"}
	if err := h.routeLaneExec(source, multiple); err != nil {
		t.Fatal(err)
	}
	multipleResponse := <-sourceResponses
	if multipleResponse.Type != "lane_error" || !strings.Contains(multipleResponse.Error, "exactly one capability") {
		t.Fatalf("multiple capability response = %#v", multipleResponse)
	}
	select {
	case leaked := <-forwarded:
		t.Fatalf("non-explicit capability request reached destination: %#v", leaked)
	default:
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

	future.Name = "future-updated"
	if err := newWireConn(thirdConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}
	updated := []Message{
		readRosterWithPeerName(t, firstDecoder, future.ID, future.Name),
		readRosterWithPeerName(t, secondDecoder, future.ID, future.Name),
		readRosterWithPeerName(t, thirdDecoder, future.ID, future.Name),
	}
	if !reflect.DeepEqual(updated[0], updated[1]) || !reflect.DeepEqual(updated[0], updated[2]) {
		t.Fatalf("clients received different updated rosters: %#v", updated)
	}
}

func TestHubRejectsNPlusOneHandshakeBeforeRegistrationAndKeepsIncumbent(t *testing.T) {
	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() {
		stopHub()
		if err := <-hubDone; err != nil {
			t.Fatal(err)
		}
	}()

	incumbentConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "incumbent", HostName: "incumbent",
	})
	defer func() { _ = incumbentConn.Close() }()
	incumbentDecoder := json.NewDecoder(incumbentConn)
	expectRawFrameType(t, incumbentDecoder, "hello_ok")
	if err := newWireConn(incumbentConn).Send(Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	readCompleteRoster(t, incumbentDecoder, 1)

	mismatch, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := mismatch.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := newWireConn(mismatch).Send(Message{
		Type: "hello", Version: ProtocolVersion + 1, HostID: "n-plus-one", HostName: "n-plus-one",
	}); err != nil {
		t.Fatal(err)
	}
	var response Message
	if err := json.NewDecoder(mismatch).Decode(&response); err == nil {
		t.Fatalf("N+1 participant received partial admission response %#v", response)
	}
	_ = mismatch.Close()

	if err := newWireConn(incumbentConn).Send(Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	roster := readCompleteRoster(t, incumbentDecoder, 1)
	if len(roster.Hosts) != 1 || roster.Hosts[0].ID != "incumbent" {
		t.Fatalf("N+1 participant entered roster or displaced incumbent: %#v", roster.Hosts)
	}
	if err := newWireConn(incumbentConn).Send(Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	expectRawFrameType(t, incumbentDecoder, "pong")
}

func TestHubRejectsProspectiveRosterAmplificationBeforeReplacingLastGoodSnapshot(t *testing.T) {
	incumbentHub, incumbentConn := net.Pipe()
	attackerHub, attackerConn := net.Pipe()
	defer func() {
		_ = incumbentHub.Close()
		_ = incumbentConn.Close()
		_ = attackerHub.Close()
		_ = attackerConn.Close()
	}()
	incumbentPeers := largeValidPeerSet(t, "host-incumbent", 256*1024)
	lastGood := mustTestPeer(t, "host-attacker", "last-good", "codex", "project")
	incumbent := &hubClient{
		hostID: "host-incumbent", hostName: "incumbent", ready: true,
		capabilities: []string{CapabilityCodexLane}, wire: newWireConn(incumbentHub),
		peers: peerMap(incumbentPeers),
	}
	attacker := &hubClient{
		hostID: "host-attacker", hostName: "attacker", ready: true,
		capabilities: []string{"future-lane"}, wire: newWireConn(attackerHub),
		peers: map[string]Peer{lastGood.ID: lastGood},
	}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{
			incumbent.hostID: incumbent, attacker.hostID: attacker,
		},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	hostilePeers := largeValidPeerSet(t, "host-attacker", maxWireBytes-96*1024)
	snapshotBody, err := json.Marshal(Message{Type: "snapshot", Peers: hostilePeers})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshotBody) >= maxWireBytes || len(hostilePeers) > maxSnapshotPeers {
		t.Fatalf("hostile snapshot is not individually admissible: bytes=%d peers=%d", len(snapshotBody), len(hostilePeers))
	}
	prospectivePeers := append(append([]Peer(nil), incumbentPeers...), hostilePeers...)
	prospectiveBody, err := json.Marshal(Message{
		Type: "roster", Version: ProtocolVersion,
		Hosts: []Host{
			{ID: incumbent.hostID, Name: incumbent.hostName, Capabilities: incumbent.capabilities},
			{ID: attacker.hostID, Name: attacker.hostName, Capabilities: attacker.capabilities},
		},
		Peers: prospectivePeers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prospectiveBody) <= maxWireBytes {
		t.Fatalf("test did not amplify prospective roster: %d bytes", len(prospectiveBody))
	}
	if err := h.handleClientMessage(attacker, Message{Type: "snapshot", Peers: hostilePeers}); err == nil || !strings.Contains(err.Error(), "roster") {
		t.Fatalf("amplified roster admission result = %v", err)
	}
	if len(attacker.peers) != 1 || attacker.peers[lastGood.ID].InstanceID != lastGood.InstanceID {
		t.Fatalf("last-good snapshot was replaced: %#v", attacker.peers)
	}
	if h.clients[incumbent.hostID] != incumbent {
		t.Fatal("unrelated incumbent was disconnected")
	}
	responses := make(chan Message, 1)
	go func() {
		_ = scanMessages(incumbentConn, func(message Message) error {
			responses <- message
			return nil
		})
	}()
	if err := h.handleClientMessage(incumbent, Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.Type != "pong" {
			t.Fatalf("incumbent response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated incumbent was no longer responsive")
	}
}

func TestHubRejectsAmplifyingReconnectBeforeReplacingLiveSameHost(t *testing.T) {
	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() {
		stopHub()
		if err := <-hubDone; err != nil {
			t.Fatal(err)
		}
	}()

	incumbentPeers := largeValidPeerSet(t, "host-incumbent", 256*1024)
	incumbentConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-incumbent", HostName: "incumbent", Generation: 3,
	})
	defer func() { _ = incumbentConn.Close() }()
	incumbentDecoder := json.NewDecoder(incumbentConn)
	expectRawFrameType(t, incumbentDecoder, "hello_ok")
	if err := newWireConn(incumbentConn).Send(Message{Type: "snapshot", Peers: incumbentPeers}); err != nil {
		t.Fatal(err)
	}
	readCompleteRoster(t, incumbentDecoder, 1)

	lastGood := mustTestPeer(t, "host-reconnect", "last-good", "codex", "project")
	previousConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-reconnect", HostName: "previous", Generation: 7,
		Capabilities: []string{"future-lane"},
	})
	defer func() { _ = previousConn.Close() }()
	previousDecoder := json.NewDecoder(previousConn)
	expectRawFrameType(t, previousDecoder, "hello_ok")
	if err := newWireConn(previousConn).Send(Message{Type: "snapshot", Peers: []Peer{lastGood}}); err != nil {
		t.Fatal(err)
	}
	initial := readCompleteRoster(t, previousDecoder, 2, lastGood.ID)
	if generation := hostGeneration(initial.Hosts, "host-reconnect"); generation != 7 {
		t.Fatalf("initial reconnect host generation = %d, want 7", generation)
	}

	hostilePeers := largeValidPeerSet(t, "host-reconnect", maxWireBytes-96*1024)
	snapshotBody, err := json.Marshal(Message{Type: "snapshot", Peers: hostilePeers})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshotBody) >= maxWireBytes || len(hostilePeers) > maxSnapshotPeers {
		t.Fatalf("reconnect snapshot is not individually admissible: bytes=%d peers=%d", len(snapshotBody), len(hostilePeers))
	}
	candidateConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-reconnect", HostName: "candidate", Generation: 8,
		Capabilities: []string{"future-lane"},
	})
	candidateDecoder := json.NewDecoder(candidateConn)
	expectRawFrameType(t, candidateDecoder, "hello_ok")
	if err := newWireConn(candidateConn).Send(Message{Type: "snapshot", Peers: hostilePeers}); err != nil {
		t.Fatal(err)
	}
	var candidateResponse Message
	if err := candidateDecoder.Decode(&candidateResponse); err == nil {
		t.Fatalf("amplifying reconnect received admission response %#v", candidateResponse)
	}
	_ = candidateConn.Close()

	if err := newWireConn(previousConn).Send(Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	expectRawFrameType(t, previousDecoder, "pong")
	if err := newWireConn(previousConn).Send(Message{Type: "snapshot", Peers: []Peer{lastGood}}); err != nil {
		t.Fatal(err)
	}
	preserved := readCompleteRoster(t, previousDecoder, 2, lastGood.ID)
	if generation := hostGeneration(preserved.Hosts, "host-reconnect"); generation != 7 {
		t.Fatalf("rejected reconnect displaced generation 7 with %d: %#v", generation, preserved.Hosts)
	}
	if peerName(preserved.Peers, lastGood.ID) != lastGood.Name {
		t.Fatalf("rejected reconnect damaged last-good peer: %#v", preserved.Peers)
	}
	if err := newWireConn(incumbentConn).Send(Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	expectRawFrameType(t, incumbentDecoder, "pong")
}

func TestHubRejectsSnapshotPeerCountBeforeReplacingLastGoodRoster(t *testing.T) {
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
		deliveryRoutes: map[string]*deliveryRoute{},
	}
	server, peerConn := net.Pipe()
	defer func() { _ = server.Close(); _ = peerConn.Close() }()
	lastGood := mustTestPeer(t, "host-a", "last-good", "codex", "project")
	client := &hubClient{
		hostID: "host-a", hostName: "host-a", ready: true, wire: newWireConn(server),
		peers: map[string]Peer{lastGood.ID: lastGood},
	}
	h.clients[client.hostID] = client
	if err := h.handleClientMessage(client, Message{Type: "snapshot", Peers: make([]Peer, maxSnapshotPeers+1)}); err == nil {
		t.Fatal("over-count snapshot was accepted")
	}
	if len(client.peers) != 1 || client.peers[lastGood.ID].InstanceID != lastGood.InstanceID {
		t.Fatalf("over-count snapshot replaced last good state: %#v", client.peers)
	}
}

func TestHubLaneCapabilityAdmissionRequiresOneExplicitOpaqueToken(t *testing.T) {
	for _, test := range []struct {
		name         string
		product      string
		capabilities []string
		want         string
		wantError    bool
	}{
		{name: "codex explicit", product: "codex", capabilities: []string{CapabilityCodexLane}, want: CapabilityCodexLane},
		{name: "codex empty", product: "codex", wantError: true},
		{name: "future empty", product: "future", wantError: true},
		{name: "new exact", product: "future", capabilities: []string{"future-lane"}, want: "future-lane"},
		{name: "multi", product: "future", capabilities: []string{"future-lane", "other-lane"}, wantError: true},
		{name: "duplicate", product: "future", capabilities: []string{"future-lane", "future-lane"}, wantError: true},
		{name: "invalid", product: "future", capabilities: []string{"Future-lane"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := laneCapabilityForMessage(Message{Product: test.product, Capabilities: test.capabilities})
			if test.wantError {
				if err == nil {
					t.Fatalf("capability admission = %q", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("capability admission = %q, err=%v", got, err)
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
	old := &hubClient{hostID: "host-a", generation: 7, ready: true, wire: newWireConn(oldServer), peers: map[string]Peer{}}
	current := &hubClient{hostID: "host-a", generation: 8, wire: newWireConn(newServer), peers: map[string]Peer{}}
	h.clients[old.hostID] = current
	if err := h.handleClientMessage(old, Message{Type: "snapshot"}); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("superseded client message result = %v", err)
	}
	if current.ready {
		t.Fatal("superseded generation mutated the current registration")
	}
}

func TestHubRejectsOversizedHelloBeforeReplacingLiveGeneration(t *testing.T) {
	currentHub, currentConn := net.Pipe()
	defer func() { _ = currentHub.Close(); _ = currentConn.Close() }()
	current := &hubClient{
		hostID: "host-a", hostName: "host-a", generation: 7, ready: true,
		wire: newWireConn(currentHub), peers: map[string]Peer{},
	}
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{current.hostID: current},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{}, clientTimeout: time.Second,
	}
	candidateHub, candidateConn := net.Pipe()
	go h.handleConnection(candidateHub)
	if err := newWireConn(candidateConn).Send(Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-a", Generation: 8,
		HostName: strings.Repeat("n", maxHostNameBytes+1), Build: "candidate",
	}); err != nil {
		t.Fatal(err)
	}
	_ = candidateConn.SetReadDeadline(time.Now().Add(time.Second))
	var response Message
	if err := json.NewDecoder(candidateConn).Decode(&response); err == nil {
		t.Fatalf("oversized hello received response %#v", response)
	}
	_ = candidateConn.Close()
	if h.clients[current.hostID] != current {
		t.Fatal("oversized candidate replaced the last-good live generation")
	}
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

func readRosterWithPeerName(t *testing.T, decoder *json.Decoder, peerID, name string) Message {
	t.Helper()
	for {
		message := expectRawFrameType(t, decoder, "roster")
		if peerName(message.Peers, peerID) == name {
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

func peerName(peers []Peer, peerID string) string {
	for _, peer := range peers {
		if peer.ID == peerID {
			return peer.Name
		}
	}
	return ""
}

func hostGeneration(hosts []Host, hostID string) uint64 {
	for _, host := range hosts {
		if host.ID == hostID {
			return host.Generation
		}
	}
	return 0
}

func largeValidPeerSet(t *testing.T, hostID string, budget int) []Peer {
	t.Helper()
	makePeer := func(index int) Peer {
		sessionID := fmt.Sprintf("peer-%04d", index)
		peer := mustTestPeer(t, hostID, sessionID, "codex", "project")
		peer.Cwd = "/" + strings.Repeat("x", maxPeerCwdBytes-1)
		return peer
	}
	sample, err := json.Marshal(makePeer(0))
	if err != nil {
		t.Fatal(err)
	}
	count := budget / (len(sample) + 1)
	if count > maxSnapshotPeers {
		count = maxSnapshotPeers
	}
	peers := make([]Peer, 0, count)
	for index := 0; index < count; index++ {
		peers = append(peers, makePeer(index))
	}
	encodedSize := func(values []Peer) int {
		body, marshalErr := json.Marshal(Message{Type: "snapshot", Peers: values})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return len(body)
	}
	for len(peers) > 0 && encodedSize(peers) > budget {
		peers = peers[:len(peers)-1]
	}
	for len(peers) < maxSnapshotPeers && encodedSize(peers)+len(sample)+1 <= budget {
		peers = append(peers, makePeer(len(peers)))
	}
	return peers
}

func peerMap(peers []Peer) map[string]Peer {
	result := make(map[string]Peer, len(peers))
	for _, peer := range peers {
		result[peer.ID] = peer
	}
	return result
}
