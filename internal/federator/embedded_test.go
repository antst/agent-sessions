package federator

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestEmbeddedHostsReuseProtocolForRosterDeliveryReconnectAndRemoteLane(t *testing.T) {
	address := reserveTCPAddress(t)
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan error, 1)
	go func() { hubDone <- RunHub(hubCtx, HubOptions{Listen: address, ClientTimeout: 2 * time.Second}) }()
	t.Cleanup(stopHub)

	peerA := mustEmbeddedPeer(t, "host-a", "session-a", "alpha", []string{"shared"})
	peerB := mustEmbeddedPeer(t, "host-b", "session-b", "beta", []string{"shared"})
	isolated := mustEmbeddedPeer(t, "host-b", "session-c", "isolated", []string{"other"})
	delivered := make(chan AgentFrame, 1)
	laneRequests := make(chan RemoteLaneRequest, 1)
	hostA := mustEmbeddedHost(t, EmbeddedHostOptions{
		Hub: address, HostID: "host-a", HostName: "host-a", Capabilities: []string{CapabilityCodexLane},
		ScanInterval: 20 * time.Millisecond, HeartbeatInterval: 40 * time.Millisecond, HeartbeatTimeout: 300 * time.Millisecond,
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{peerA}, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
		RunLane: func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error) {
			return RemoteLaneResult{Stdout: []byte(`{"type":"unused"}`)}, nil
		},
	})
	hostB := mustEmbeddedHost(t, EmbeddedHostOptions{
		Hub: address, HostID: "host-b", HostName: "host-b", Capabilities: []string{CapabilityQwenLane},
		ScanInterval: 20 * time.Millisecond, HeartbeatInterval: 40 * time.Millisecond, HeartbeatTimeout: 300 * time.Millisecond,
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{peerB, isolated}, nil },
		Deliver: func(_ context.Context, source, target Peer, frame AgentFrame) error {
			if source.ID != peerA.ID || target.ID != peerB.ID {
				t.Fatalf("delivery identities = %s -> %s", source.ID, target.ID)
			}
			delivered <- frame
			return nil
		},
		RunLane: func(_ context.Context, request RemoteLaneRequest) (RemoteLaneResult, error) {
			laneRequests <- request
			return RemoteLaneResult{Stdout: []byte(`{"type":"lane.ready"}`), ExitCode: 0}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runEmbeddedForTest(t, ctx, hostA)
	runEmbeddedForTest(t, ctx, hostB)
	waitEmbedded(t, func() bool { return len(hostA.RemotePeers()) == 2 && len(hostB.RemotePeers()) == 1 }, "initial roster")

	if err := hostA.Send(context.Background(), peerA, peerB, "message-1", "hello", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-delivered:
		if frame.Content != "hello" || frame.Source == nil || frame.Source.ID != peerA.ID {
			t.Fatalf("delivered frame = %#v", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("grouped delivery was not acknowledged")
	}
	if err := hostA.Send(context.Background(), peerA, isolated, "message-2", "forbidden", ""); err == nil {
		t.Fatal("group-isolated remote target accepted a delivery")
	}

	parent := ParentContext{
		HostID: peerA.HostID, SessionID: peerA.SessionID, Product: peerA.Entrypoint,
		InstanceID: peerA.InstanceID, Groups: append([]string(nil), peerA.Groups...), PermissionMode: "default",
	}
	result, err := hostA.RunRemoteLane(context.Background(), RemoteLaneRequest{
		Source: peerA, Parent: parent, TargetHostID: "host-b", Product: "qwen",
		Arguments: []string{"start", "--name", "worker"}, Input: []byte("inspect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != `{"type":"lane.ready"}` || result.ExitCode != 0 {
		t.Fatalf("remote lane result = %#v", result)
	}
	select {
	case request := <-laneRequests:
		if request.Source.ID != peerA.ID || !reflect.DeepEqual(request.Parent.Groups, peerA.Groups) || request.Product != "qwen" {
			t.Fatalf("remote lane request = %#v", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("destination lane callback was not invoked")
	}

	stopHub()
	select {
	case err := <-hubDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hub did not stop for reconnect test")
	}
	waitEmbedded(t, func() bool { return len(hostA.RemotePeers()) == 0 && len(hostB.RemotePeers()) == 0 }, "disconnected roster cleanup")
	reconnectCtx, stopReconnect := context.WithCancel(context.Background())
	t.Cleanup(stopReconnect)
	go func() { _ = RunHub(reconnectCtx, HubOptions{Listen: address, ClientTimeout: 2 * time.Second}) }()
	waitEmbedded(t, func() bool { return len(hostA.RemotePeers()) == 2 && len(hostB.RemotePeers()) == 1 }, "reconnected roster")
}

func TestBuildPeerRejectsReservedIdentityAndAddsPrivateAnchor(t *testing.T) {
	peer := mustEmbeddedPeer(t, "host", "session", "worker", []string{"project"})
	if !containsString(peer.Groups, "session:host/session") || !containsString(peer.Groups, "project") {
		t.Fatalf("peer groups = %q", peer.Groups)
	}
	if _, err := BuildPeer("bad/host", "bad", "session", "worker", "idle", "/work", "codex", "default", "instance", "", []string{"project"}); err == nil {
		t.Fatal("invalid host identity was accepted")
	}
}

func mustEmbeddedPeer(t *testing.T, host, session, name string, groups []string) Peer {
	t.Helper()
	peer, err := BuildPeer(host, host, session, name, "idle", "/work", "codex", "default", "instance-"+session, "", groups)
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

func mustEmbeddedHost(t *testing.T, options EmbeddedHostOptions) *EmbeddedHost {
	t.Helper()
	host, err := NewEmbeddedHost(options)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func runEmbeddedForTest(t *testing.T, ctx context.Context, host *EmbeddedHost) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- host.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("embedded host stopped: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("embedded host did not stop")
		}
	})
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitEmbedded(t *testing.T, predicate func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
