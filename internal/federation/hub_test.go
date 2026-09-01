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
	destination.capabilities = []string{"future-lane", transportFeatureOpaquePeerProducts}
	featureRequest := request
	featureRequest.RequestID = "transport-feature-route"
	featureRequest.Capabilities = []string{transportFeatureOpaquePeerProducts}
	if err := h.routeLaneExec(source, featureRequest); err != nil {
		t.Fatal(err)
	}
	featureResponse := <-sourceResponses
	if featureResponse.Type != "lane_error" || !strings.Contains(featureResponse.Error, "transport feature") {
		t.Fatalf("transport feature lane response = %#v", featureResponse)
	}
	if _, exists := h.laneRoutes[featureRequest.RequestID]; exists {
		t.Fatal("transport feature marker was offered as a lane route")
	}
	select {
	case leaked := <-forwarded:
		t.Fatalf("transport feature request reached destination: %#v", leaked)
	default:
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

func TestMixedV3AsymmetryMarkerIsToleratedButUnknownPeerProductIsNot(t *testing.T) {
	baseline := mustTestPeer(t, "host-old", "baseline", "codex", "project")
	unknown := baseline
	unknown.ID = "host-old/future"
	unknown.SessionID = "future"
	unknown.GlobalID = globalSessionID("host-old", "future")
	unknown.Product, unknown.Entrypoint = "future-product", "future-product"
	unknown.InstanceID = "future:instance"
	unknown.Groups = []string{"project", PrivateGroup("host-old", "future")}
	roster := Message{
		Type: "roster", Version: ProtocolVersion,
		Hosts: []Host{{
			ID: "host-new", Name: "host-new",
			Capabilities: []string{CapabilityCodexLane, transportFeatureOpaquePeerProducts},
		}},
		Peers: []Peer{baseline},
	}
	if got := normalizeOldHostCompatibleCapabilities(roster.Hosts[0].Capabilities); !reflect.DeepEqual(got, []string{CapabilityCodexLane}) {
		t.Fatalf("old host feature normalization = %q", got)
	}
	if err := validateOldHostCompatibleRoster(roster); err != nil {
		t.Fatalf("old host rejected additive feature marker: %v", err)
	}
	roster.Peers = append(roster.Peers, unknown)
	if err := validateOldHostCompatibleRoster(roster); err == nil || !strings.Contains(err.Error(), "product") {
		t.Fatalf("old host accepted unknown peer product: %v", err)
	}
	if _, _, err := laneCapabilityForMessage(Message{
		Product: "future-product", Capabilities: []string{transportFeatureOpaquePeerProducts},
	}); err == nil {
		t.Fatal("transport feature marker was admitted as a lane capability")
	}
}

func TestLiveHubFiltersNewPeerFromUnmarkedV3ClientAndMarkedClientSeesFullRoster(t *testing.T) {
	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() {
		stopHub()
		if err := <-hubDone; err != nil {
			t.Fatal(err)
		}
	}()

	oldConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-old", HostName: "host-old",
		Capabilities: []string{CapabilityCodexLane},
	})
	defer func() { _ = oldConn.Close() }()
	oldDecoder := json.NewDecoder(oldConn)
	expectRawFrameType(t, oldDecoder, "hello_ok")
	baseline := mustTestPeer(t, "host-old", "baseline", "codex", "project")
	if err := newWireConn(oldConn).Send(Message{Type: "snapshot", Peers: []Peer{baseline}}); err != nil {
		t.Fatal(err)
	}
	initial := readOldCompatibleRoster(t, oldDecoder, 1)
	if len(initial.Peers) != 1 || initial.Peers[0].ID != baseline.ID {
		t.Fatalf("old initial roster = %#v", initial)
	}

	markedConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-marked", HostName: "host-marked",
		Capabilities: []string{transportFeatureOpaquePeerProducts},
	})
	defer func() { _ = markedConn.Close() }()
	markedDecoder := json.NewDecoder(markedConn)
	expectRawFrameType(t, markedDecoder, "hello_ok")
	if err := newWireConn(markedConn).Send(Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}

	newConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-new", HostName: "host-new",
		Capabilities: []string{"future-lane", transportFeatureOpaquePeerProducts},
	})
	defer func() { _ = newConn.Close() }()
	newDecoder := json.NewDecoder(newConn)
	expectRawFrameType(t, newDecoder, "hello_ok")
	future := mustTestPeer(t, "host-new", "future", "codex", "project")
	future.Product, future.Entrypoint = "future-product", "future-product"
	future.InstanceID = "future:instance"
	future.Groups = []string{"future-secret", PrivateGroup("host-new", "future")}
	if err := newWireConn(newConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}

	oldRoster := readOldCompatibleRoster(t, oldDecoder, 3)
	if !containsPeerID(oldRoster.Peers, baseline.ID) || containsPeerID(oldRoster.Peers, future.ID) {
		t.Fatalf("unmarked client roster leaked new peer: %#v", oldRoster.Peers)
	}
	oldBody, _ := json.Marshal(oldRoster)
	if strings.Contains(string(oldBody), "future-secret") || strings.Contains(string(oldBody), future.InstanceID) {
		t.Fatalf("unmarked roster partially leaked filtered peer: %s", oldBody)
	}
	markedRoster := readRosterContaining(t, markedDecoder, future.ID)
	if !containsPeerID(markedRoster.Peers, baseline.ID) || !containsPeerID(markedRoster.Peers, future.ID) {
		t.Fatalf("marked client did not receive full roster: %#v", markedRoster.Peers)
	}
	future.Name = "future-updated"
	if err := newWireConn(newConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}
	updatedOldRoster := readOldCompatibleRoster(t, oldDecoder, 3)
	if containsPeerID(updatedOldRoster.Peers, future.ID) {
		t.Fatalf("unmarked client update leaked new peer: %#v", updatedOldRoster.Peers)
	}
	updatedMarkedRoster := readRosterContaining(t, markedDecoder, future.ID)
	if peerName(updatedMarkedRoster.Peers, future.ID) != "future-updated" {
		t.Fatalf("marked client did not receive peer update: %#v", updatedMarkedRoster.Peers)
	}
	baseline.Name = "baseline-updated"
	if err := newWireConn(oldConn).Send(Message{Type: "snapshot", Peers: []Peer{baseline}}); err != nil {
		t.Fatal(err)
	}
	oldTriggeredRoster := readOldCompatibleRoster(t, oldDecoder, 3)
	if containsPeerID(oldTriggeredRoster.Peers, future.ID) || peerName(oldTriggeredRoster.Peers, baseline.ID) != "baseline-updated" {
		t.Fatalf("unmarked-triggered roster leaked or lost update: %#v", oldTriggeredRoster.Peers)
	}
	markedAfterOldUpdate := readRosterContaining(t, markedDecoder, future.ID)
	if peerName(markedAfterOldUpdate.Peers, baseline.ID) != "baseline-updated" {
		t.Fatalf("marked roster lost baseline update: %#v", markedAfterOldUpdate.Peers)
	}
	if err := newWireConn(oldConn).Send(Message{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	expectOldCompatibleFrameType(t, oldDecoder, "pong")
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
		capabilities: []string{transportFeatureOpaquePeerProducts}, wire: newWireConn(incumbentHub),
		peers: peerMap(incumbentPeers),
	}
	attacker := &hubClient{
		hostID: "host-attacker", hostName: "attacker", ready: true,
		capabilities: []string{transportFeatureOpaquePeerProducts}, wire: newWireConn(attackerHub),
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
	newcomerHub, newcomerConn := net.Pipe()
	defer func() { _ = newcomerHub.Close(); _ = newcomerConn.Close() }()
	newcomer := &hubClient{
		hostID: "host-attacker", hostName: "newcomer", generation: 2,
		capabilities: []string{transportFeatureOpaquePeerProducts}, wire: newWireConn(newcomerHub),
		peers: map[string]Peer{},
	}
	h.clients[newcomer.hostID] = newcomer
	if err := h.handleClientMessage(newcomer, Message{Type: "snapshot", Peers: hostilePeers}); err == nil || !strings.Contains(err.Error(), "roster") {
		t.Fatalf("amplifying newcomer admission result = %v", err)
	}
	if newcomer.ready || len(newcomer.peers) != 0 {
		t.Fatalf("amplifying newcomer became ready: ready=%t peers=%d", newcomer.ready, len(newcomer.peers))
	}
	if h.clients[incumbent.hostID] != incumbent {
		t.Fatal("amplifying newcomer displaced unrelated incumbent")
	}
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

func validateOldHostCompatibleRoster(message Message) error {
	if message.Type != "roster" || message.Version != ProtocolVersion {
		return fmt.Errorf("old host received incompatible roster")
	}
	for _, host := range message.Hosts {
		if !validSimpleID(host.ID) || host.Name == "" {
			return fmt.Errorf("old host received invalid host")
		}
		// The pre-feature normalizer silently discarded capabilities outside
		// its original-four catalog, so the additive marker is tolerated here.
		_ = normalizeOldHostCompatibleCapabilities(host.Capabilities)
	}
	for _, peer := range message.Peers {
		if err := validateWirePeer(peer, peer.HostID); err != nil {
			return err
		}
		product := peer.Product
		if product == "" {
			product = peer.Entrypoint
		}
		if _, known := legacyV3LaneCapabilities[product]; !known {
			return fmt.Errorf("old host rejected unknown peer product %q", product)
		}
	}
	return nil
}

func normalizeOldHostCompatibleCapabilities(capabilities []string) []string {
	result := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if productForLegacyCapability(capability) != "" {
			result = append(result, capability)
		}
	}
	sortStrings(result)
	return result
}

func productForLegacyCapability(capability string) string {
	for product, candidate := range legacyV3LaneCapabilities {
		if candidate == capability {
			return product
		}
	}
	return ""
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

func readOldCompatibleRoster(t *testing.T, decoder *json.Decoder, minimumHosts int) Message {
	t.Helper()
	for {
		message := expectRawFrameType(t, decoder, "roster")
		if err := validateOldHostCompatibleRoster(message); err != nil {
			t.Fatal(err)
		}
		if len(message.Hosts) >= minimumHosts {
			return message
		}
	}
}

func expectOldCompatibleFrameType(t *testing.T, decoder *json.Decoder, wanted string) Message {
	t.Helper()
	for {
		var message Message
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("read old-compatible %s frame: %v", wanted, err)
		}
		if message.Type == "roster" {
			if err := validateOldHostCompatibleRoster(message); err != nil {
				t.Fatal(err)
			}
		}
		if message.Type == wanted {
			return message
		}
	}
}

func readRosterContaining(t *testing.T, decoder *json.Decoder, peerID string) Message {
	t.Helper()
	for {
		message := expectRawFrameType(t, decoder, "roster")
		if containsPeerID(message.Peers, peerID) {
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
