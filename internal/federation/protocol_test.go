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

func TestProtocolDuplicateDeliveryAndReconnectGenerationRules(t *testing.T) {
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
	if calls.Load() != 1 {
		t.Fatalf("concurrent duplicate was presented %d times", calls.Load())
	}

	h := &hub{
		logger: discardTestLogger(), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
	}
	currentServer, currentPeer := net.Pipe()
	defer func() { _ = currentPeer.Close() }()
	current := &hubClient{hostID: "host-a", generation: 9, wire: newWireConn(currentServer), peers: map[string]Peer{}}
	if err := h.register(current); err != nil {
		t.Fatal(err)
	}
	staleServer, stalePeer := net.Pipe()
	defer func() { _ = staleServer.Close(); _ = stalePeer.Close() }()
	stale := &hubClient{hostID: "host-a", generation: 8, wire: newWireConn(staleServer), peers: map[string]Peer{}}
	if err := h.register(stale); err == nil || !strings.Contains(err.Error(), "older") {
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
	err := scanMessages(strings.NewReader(`{"type":"hello","version":3,"host_id":"host-a","host_name":"host-a","future":{"nested":true}}`+"\n"), func(message Message) error {
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
