package federation

import (
	"context"
	"encoding/json"
	"net"
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
