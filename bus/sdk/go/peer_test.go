package sessionkit

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/daemon"
	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

func TestPeerDesiredIdentityAndDeepSnapshots(t *testing.T) {
	fixtures := loadCallerFixtures(t)
	requests, server, client := peerPipe(t)
	connections := make(chan net.Conn, 1)
	connections <- client
	usePeerDialer(t, func(string, string) (net.Conn, error) {
		select {
		case fd := <-connections:
			return fd, nil
		default:
			return nil, errors.New("offline")
		}
	})
	identity := fixtures.Sequences.CrossedRehello.Identity
	peer, err := ConnectPeer(identity, acceptDelivery)
	if err != nil {
		t.Fatal(err)
	}
	identity.Info["nested"].(map[string]any)["value"] = "mutated"
	firstHello := <-requests
	assertPeerHello(t, firstHello, "initial", "initial")
	first := fixtures.Sequences.CrossedRehello.First
	if err = peer.Rehello(first.Name, first.Info); !isCode(err, protocol.NotConnected) {
		t.Fatalf("rehello before ready = %v", err)
	}
	first.Info["nested"].(map[string]any)["value"] = "mutated"
	mustRPC(t, server.Result(firstHello, struct{}{}))
	readyHello := <-requests
	assertPeerHello(t, readyHello, "first", "first")
	mustRPC(t, server.Result(readyHello, struct{}{}))
	await(t, peer.Ready(), "peer ready")

	fixtures = loadCallerFixtures(t)
	first, second := fixtures.Sequences.CrossedRehello.First, fixtures.Sequences.CrossedRehello.Second
	firstDone := make(chan error, 1)
	go func() { firstDone <- peer.Rehello(first.Name, first.Info) }()
	firstRequest := <-requests
	first.Info["nested"].(map[string]any)["value"] = "mutated"
	secondDone := make(chan error, 1)
	go func() { secondDone <- peer.Rehello(second.Name, second.Info) }()
	secondRequest := <-requests
	second.Info["nested"].(map[string]any)["value"] = "mutated"
	mustRPC(t, server.Result(secondRequest, struct{}{}))
	mustRPC(t, <-secondDone)
	mustRPC(t, server.Result(firstRequest, struct{}{}))
	finalRequest := <-requests
	assertPeerHello(t, finalRequest, "second", "second")
	mustRPC(t, server.Result(finalRequest, struct{}{}))
	mustRPC(t, <-firstDone)
	peer.Shutdown()
	await(t, peer.Closed(), "peer closed")
}

func TestPeerReconnectsButSupersededStops(t *testing.T) {
	var attempts atomic.Int32
	connections := make(chan net.Conn, 2)
	usePeerDialer(t, func(string, string) (net.Conn, error) {
		attempts.Add(1)
		select {
		case fd := <-connections:
			return fd, nil
		default:
			return nil, errors.New("offline")
		}
	})
	requests1, server1, client1 := peerPipe(t)
	connections <- client1
	peer, err := ConnectPeer(PeerIdentity{Product: "fixture-client", SessionID: "peer", Name: "peer", Groups: []string{}, Info: map[string]any{}}, acceptDelivery)
	if err != nil {
		t.Fatal(err)
	}
	hello := <-requests1
	mustRPC(t, server1.Result(hello, struct{}{}))
	await(t, peer.Ready(), "peer ready")
	mustRPC(t, server1.Close())
	awaitConnection(t, peer, false)
	if _, err = peer.Call(context.Background(), "session.list", SessionListRequest{}); !isCode(err, protocol.NotConnected) {
		t.Fatalf("disconnected call = %v", err)
	}
	requests2, server2, client2 := peerPipe(t)
	connections <- client2
	reconnected := <-requests2
	assertPeerHello(t, reconnected, "peer", "")
	mustRPC(t, server2.Result(reconnected, struct{}{}))
	awaitConnection(t, peer, true)
	callDone := make(chan error, 1)
	go func() {
		_, callErr := peer.Call(context.Background(), "session.list", SessionListRequest{})
		callDone <- callErr
	}()
	listed := <-requests2
	mustRPC(t, server2.Result(listed, SessionListResult{Sessions: []SessionSummary{}}))
	mustRPC(t, <-callDone)
	attemptsBeforeSupersession := attempts.Load()
	superseded := make(chan error, 1)
	go func() {
		superseded <- server2.Call(context.Background(), "session.superseded", struct{}{}, &struct{}{})
	}()
	mustRPC(t, <-superseded)
	await(t, peer.Closed(), "superseded peer closed")
	if attempts.Load() != attemptsBeforeSupersession {
		t.Fatal("superseded peer retried")
	}
}

func TestCallerReportsRealDaemonEOF(t *testing.T) {
	fixtures := loadCallerFixtures(t)
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
	service := startDaemon(t, socket)
	caller := startPeer(t, socket, "caller")
	fd, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan *rpc.Request, 1)
	target := rpc.New(fd, true, func(_ context.Context, request *rpc.Request) { requests <- request })
	hello := protocol.PeerHello{Protocol: 1, Product: "fixture-client", SessionID: "lane", Name: "lane", Groups: []string{"shared"}, Info: map[string]any{}}
	mustRPC(t, target.Call(context.Background(), "session.hello", hello, &struct{}{}))
	runs := newCaller(func(ctx context.Context, _ string, _ any, _ any) error {
		var result MessageSendResult
		return caller.call(ctx, "message.send", MessageSendRequest{Target: "lane@local", Message: "hold"}, &result)
	})
	started, err := runs.Start(fixtures.Shapes.Start.Request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		if request.Method != "message.deliver" {
			t.Fatalf("method = %s", request.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not reach target")
	}
	mustRPC(t, service.Close())
	status, err := runs.Wait(WaitRequest{TurnID: started.TurnID})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, status, fixtures.Shapes.EOF.Result)
}

type crossedRehelloFixture struct {
	Identity      PeerIdentity
	First, Second rehelloFixture
}

type rehelloFixture struct {
	Name string
	Info map[string]any
}

func TestPeerDaemonRehelloRules(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
	startDaemon(t, socket)
	peer := startPeer(t, socket, "peer")
	if err := peer.Rehello("renamed", map[string]any{"revision": 2}); err != nil {
		t.Fatal(err)
	}
	listed, err := peer.Caller.List(context.Background(), SessionListRequest{})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Name != "renamed@local" {
		t.Fatalf("same-id rehello = %#v, %v", listed.Sessions, err)
	}
	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	identity := PeerIdentity{Protocol: 1, Product: "fixture-client", SessionID: "raw", Name: "raw", Groups: []string{"one", "two"}, Info: map[string]any{}}
	_, err = client.Call(context.Background(), "session.hello", identity)
	identity.Groups = []string{"two", "one"}
	if _, err = client.Call(context.Background(), "session.hello", identity); !isCode(err, protocol.InvalidHello) {
		t.Fatalf("changed groups = %v", err)
	}
}

func usePeerDialer(t *testing.T, dial func(string, string) (net.Conn, error)) {
	t.Helper()
	oldDial, oldInterval := dialPeer, peerReconnectInterval
	dialPeer, peerReconnectInterval = dial, time.Millisecond
	t.Setenv("AGENTBUS_SOCKET", "fixture.sock")
	t.Cleanup(func() { dialPeer, peerReconnectInterval = oldDial, oldInterval })
}

func peerPipe(t *testing.T) (chan *rpc.Request, *rpc.Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	requests := make(chan *rpc.Request, 8)
	wire := rpc.New(server, false, func(_ context.Context, request *rpc.Request) { requests <- request })
	t.Cleanup(func() { _ = wire.Close(); _ = client.Close() })
	return requests, wire, client
}

func assertPeerHello(t *testing.T, request *rpc.Request, name, nested string) {
	t.Helper()
	hello := request.Params.(*protocol.PeerHello)
	if hello.Name != name || nested != "" && hello.Info["nested"].(map[string]any)["value"] != nested {
		t.Fatalf("hello = %#v", hello)
	}
}

func startDaemon(t *testing.T, socket string) *daemon.Daemon {
	t.Helper()
	service, err := daemon.Start(daemon.Config{SocketPath: socket, TablePath: filepath.Join(filepath.Dir(socket), "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func startPeer(t *testing.T, socket, id string) *Peer {
	t.Helper()
	t.Setenv("AGENTBUS_SOCKET", socket)
	peer, err := ConnectPeer(PeerIdentity{Product: "fixture-client", SessionID: id, Name: id, Groups: []string{"shared"}, Info: map[string]any{}}, acceptDelivery)
	if err != nil {
		t.Fatal(err)
	}
	await(t, peer.Ready(), "peer ready")
	t.Cleanup(peer.Shutdown)
	return peer
}

func acceptDelivery(context.Context, DeliveryRequest) (DeliveryReceipt, error) {
	return DeliveryReceipt{Disposition: "injected"}, nil
}

func await(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(name + " timed out")
	}
}

func awaitConnection(t *testing.T, peer *Peer, connected bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for (peer.connection() != nil) != connected {
		if time.Now().After(deadline) {
			t.Fatal("peer connection timed out")
		}
		runtime.Gosched()
	}
}

func mustRPC(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, rpc.ErrClosed) {
		t.Fatal(err)
	}
}
