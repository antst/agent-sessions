package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtocolLiveDeliveryAndReconnectGenerationRules(t *testing.T) {
	source := mustTestPeer(t, "host-a", "source", "codex", "project")
	target := mustTestPeer(t, "host-b", "target", "qwen", "project")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	destination, err := NewEmbeddedHost(EmbeddedHostOptions{
		HostID: "host-b", HostName: "host-b",
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{target}, nil },
		Deliver: func(context.Context, Peer, Peer, AgentFrame) error {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.refreshLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	destination.remote[source.ID] = source
	frame, err := json.Marshal(DeliveryFrame(AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "stable-message", Content: "body",
	}, source))
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		Type: "group_deliver", RequestID: "stable-request", SourceID: source.ID,
		TargetID: target.ID, Frame: frame,
	}
	results := make(chan error, 2)
	go func() { results <- destination.deliverInbound(message) }()
	<-started
	go func() { results <- destination.deliverInbound(message) }()
	close(release)
	for index := 0; index < 2; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("two live requests were presented %d times", calls.Load())
	}

	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	currentServer, currentPeer := net.Pipe()
	defer func() { _ = currentPeer.Close() }()
	current := &hubClient{hostID: "host-a", generation: 9, ready: true, wire: newWireConn(currentServer), peers: map[string]Peer{}}
	h.clients[current.hostID] = current
	staleServer, stalePeer := net.Pipe()
	defer func() { _ = staleServer.Close(); _ = stalePeer.Close() }()
	stale := &hubClient{hostID: "host-a", generation: 8, wire: newWireConn(staleServer), peers: map[string]Peer{}}
	if err := h.validateRegistrationCandidate(stale); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale reconnect result = %v", err)
	}
	if h.clients["host-a"] != current {
		t.Fatal("stale reconnect displaced the current generation")
	}
}

func TestProtocolRejectsEveryMismatchedVersionBeforeRegistration(t *testing.T) {
	for _, version := range []int{0, ProtocolVersion - 1, ProtocolVersion + 1} {
		if err := validateHello(Message{Type: "hello", Version: version, HostID: "host-a", HostName: "host-a"}); err == nil {
			t.Fatalf("protocol version %d was accepted", version)
		}
	}
	if err := validateHello(Message{Type: "hello", Version: ProtocolVersion, HostID: "host-a", HostName: "host-a"}); err != nil {
		t.Fatalf("current protocol rejected: %v", err)
	}
}

func TestProtocolRejectsMalformedAndOversizedFrames(t *testing.T) {
	if err := scanMessages(strings.NewReader("{not-json}\n"), func(Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "decode federation frame") {
		t.Fatalf("malformed frame error = %v", err)
	}
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()
	err := newWireConn(server).Send(Message{Type: "lane_stdout", Data: make([]byte, maxWireBytes)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestEqualProtocolUnrelatedBuildsHandshake(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{}, clientTimeout: 2 * time.Second,
	}
	go h.handleConnection(server)
	if err := newWireConn(client).Send(Message{
		Type: "hello", Version: ProtocolVersion, Build: "unrelated-host-build", Generation: 7,
		HostID: "host-a", HostName: "host-a",
	}); err != nil {
		t.Fatal(err)
	}
	var response Message
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "hello_ok" || response.Version != ProtocolVersion {
		t.Fatalf("unrelated-build handshake = %#v", response)
	}
}

func TestNormalizeCapabilitiesPreservesUnknownTokensAndRejectsInvalidRawInput(t *testing.T) {
	got, err := normalizeCapabilities([]string{"future-lane", CapabilityCodexLane, "future-lane", "alpha-2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha-2", CapabilityCodexLane, "future-lane"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized capabilities = %q, want %q", got, want)
	}
	for _, values := range [][]string{
		{""}, {"Future-lane"}, {"future--lane"}, {strings.Repeat("a", 65)},
		append(make([]string, maxWireCapabilities), "duplicate-does-not-evade-raw-count"),
	} {
		if _, err := normalizeCapabilities(values); err == nil {
			t.Fatalf("invalid capabilities accepted: %#v", values)
		}
	}
	maximal := make([]string, maxWireCapabilities)
	for index := range maximal {
		maximal[index] = "a" + strings.Repeat("a", 61) + fmt.Sprintf("%02d", index)
	}
	if got, err := normalizeCapabilities(maximal); err != nil || len(got) != maxWireCapabilities {
		t.Fatalf("exact aggregate bound rejected: count=%d, err=%v", len(got), err)
	}
	overAggregate := append([]string(nil), maximal...)
	overAggregate[len(overAggregate)-1] += "x"
	if _, err := normalizeCapabilities(overAggregate); err == nil || !strings.Contains(err.Error(), "aggregate") {
		t.Fatalf("aggregate overflow result = %v", err)
	}
}

func TestProtocolAcceptsAdditiveUnknownJSONFields(t *testing.T) {
	var got Message
	err := scanMessages(strings.NewReader(`{"type":"hello","version":4,"host_id":"host-a","host_name":"host-a","future":{"nested":true}}`+"\n"), func(message Message) error {
		got = message
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("scan additive frame: %v", err)
	}
	if err := validateHello(got); err != nil {
		t.Fatalf("additive hello rejected: %v", err)
	}
}

func TestWirePeerSyntaxDoesNotRequireLocalCatalogAuthority(t *testing.T) {
	peer := mustTestPeer(t, "host-a", "future", "codex", "project")
	peer.Product, peer.Entrypoint = "future-product", "future-product"
	if err := validateWirePeer(peer, "host-a"); err != nil {
		t.Fatalf("valid opaque wire product rejected: %v", err)
	}
	if err := validateLocalPeer(peer, "host-a"); err == nil {
		t.Fatal("unknown local product bypassed catalog authority")
	}
}

func TestHelloBoundsEveryRosterIdentityField(t *testing.T) {
	valid := Message{
		Type: "hello", Version: ProtocolVersion, HostID: "host-a", HostName: "workstation", Build: "build-1",
	}
	if err := validateHello(valid); err != nil {
		t.Fatal(err)
	}
	for label, mutate := range map[string]func(*Message){
		"host id":   func(message *Message) { message.HostID = strings.Repeat("h", maxHostIDBytes+1) },
		"host name": func(message *Message) { message.HostName = strings.Repeat("n", maxHostNameBytes+1) },
		"build":     func(message *Message) { message.Build = strings.Repeat("b", maxBuildBytes+1) },
	} {
		t.Run(label, func(t *testing.T) {
			message := valid
			mutate(&message)
			if err := validateHello(message); err == nil {
				t.Fatalf("unbounded %s was accepted", label)
			}
		})
	}
}

func TestWirePeerBoundsEveryStringFieldAndCollection(t *testing.T) {
	valid := mustTestPeer(t, "host-a", "session", "codex", "project")
	for label, mutate := range map[string]func(*Peer){
		"id":                func(peer *Peer) { peer.ID = strings.Repeat("i", maxPeerIDBytes+1) },
		"session id":        func(peer *Peer) { peer.SessionID = strings.Repeat("s", maxSessionIDBytes+1) },
		"global id":         func(peer *Peer) { peer.GlobalID = strings.Repeat("g", maxPeerGlobalIDBytes+1) },
		"name":              func(peer *Peer) { peer.Name = strings.Repeat("n", maxPeerNameBytes+1) },
		"display name":      func(peer *Peer) { peer.DisplayName = strings.Repeat("d", maxPeerDisplayNameBytes+1) },
		"host id":           func(peer *Peer) { peer.HostID = strings.Repeat("h", maxHostIDBytes+1) },
		"host name":         func(peer *Peer) { peer.HostName = strings.Repeat("h", maxHostNameBytes+1) },
		"product":           func(peer *Peer) { peer.Product = strings.Repeat("p", maxProductTokenBytes+1) },
		"entrypoint":        func(peer *Peer) { peer.Entrypoint = strings.Repeat("e", maxProductTokenBytes+1) },
		"status":            func(peer *Peer) { peer.Status = strings.Repeat("s", maxPeerStatusBytes+1) },
		"cwd":               func(peer *Peer) { peer.Cwd = strings.Repeat("c", maxPeerCwdBytes+1) },
		"permission mode":   func(peer *Peer) { peer.PermissionMode = strings.Repeat("p", maxPeerPermissionBytes+1) },
		"instance id":       func(peer *Peer) { peer.InstanceID = strings.Repeat("i", maxPeerInstanceIDBytes+1) },
		"parent session id": func(peer *Peer) { peer.ParentSessionID = strings.Repeat("p", maxPeerParentIDBytes+1) },
		"groups per peer":   func(peer *Peer) { peer.Groups = make([]string, maxPeerGroups+1) },
	} {
		t.Run(label, func(t *testing.T) {
			peer := clonePeer(valid)
			mutate(&peer)
			if err := validateWirePeer(peer, "host-a"); err == nil {
				t.Fatalf("unbounded peer %s was accepted", label)
			}
		})
	}
}
