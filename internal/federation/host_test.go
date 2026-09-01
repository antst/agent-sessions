package federation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostReconnectsWhenHubStopsAnsweringHeartbeats(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var accepts atomic.Int32
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepts.Add(1)
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				_ = newWireConn(conn).Send(Message{Type: "hello_ok", Version: ProtocolVersion})
				for scanner.Scan() {
					// Consume pings without answering them to force heartbeat recovery.
				}
			}(conn)
		}
	}()
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		Hub: listener.Addr().String(), HostID: "heartbeat-host", HostName: "heartbeat-host",
		HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 75 * time.Millisecond,
		Snapshot: func(context.Context) ([]Peer, error) { return nil, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
		Logger:   discardTestLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runTestHost(host, ctx)
	waitTest(t, func() bool { return accepts.Load() >= 2 }, "heartbeat reconnect")
	cancel()
	waitTestHost(t, done)
	_ = listener.Close()
	<-serverDone
}

func TestDeliveryAcknowledgementDataMayBePresentOrEmpty(t *testing.T) {
	host := &EmbeddedHost{pendingDeliveries: map[string]chan deliveryOutcome{
		"new": make(chan deliveryOutcome, 1), "old": make(chan deliveryOutcome, 1),
	}}
	host.completePendingDelivery(Message{Type: "delivery_ack", RequestID: "new", Data: []byte(`{"receipt_id":"exact","receipt_sequence":2}`)})
	newOutcome := <-host.pendingDeliveries["new"]
	if newOutcome.err != nil || string(newOutcome.data) != `{"receipt_id":"exact","receipt_sequence":2}` {
		t.Fatalf("acknowledgement with data = data %q err %v", newOutcome.data, newOutcome.err)
	}
	host.completePendingDelivery(Message{Type: "delivery_ack", RequestID: "old"})
	oldOutcome := <-host.pendingDeliveries["old"]
	if oldOutcome.err != nil || len(oldOutcome.data) != 0 {
		t.Fatalf("acknowledgement without data = data %q err %v", oldOutcome.data, oldOutcome.err)
	}
}

func TestHostRefreshesDaemonSnapshotWhileHubIsDisconnected(t *testing.T) {
	address := unusedTestAddress(t)
	peer := mustTestPeer(t, "offline-host", "late", "codex", "project")
	var include atomic.Bool
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		Hub: address, HostID: "offline-host", HostName: "offline-host",
		ScanInterval: 10 * time.Millisecond,
		Snapshot: func(context.Context) ([]Peer, error) {
			if include.Load() {
				return []Peer{peer}, nil
			}
			return nil, nil
		},
		Deliver: func(context.Context, Peer, Peer, AgentFrame) error { return nil },
		Logger:  discardTestLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runTestHost(host, ctx)
	include.Store(true)
	waitTest(t, func() bool { return len(host.localSnapshot()) == 1 }, "offline daemon snapshot refresh")
	cancel()
	waitTestHost(t, done)
}

func TestHostDeduplicatesAcknowledgedDeliveryAcrossReconnectBoundary(t *testing.T) {
	source := mustTestPeer(t, "host-a", "source", "codex", "project")
	target := mustTestPeer(t, "host-b", "target", "qwen", "project")
	var calls atomic.Int32
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		HostID: "host-b", HostName: "host-b",
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{target}, nil },
		Deliver: func(context.Context, Peer, Peer, AgentFrame) error {
			calls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.refreshLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.remote[source.ID] = source
	frame := DeliveryFrame(AgentFrame{Version: AgentFrameVersion, Type: "send", MessageID: "message-1", Content: "hello"}, source)
	body, _ := json.Marshal(frame)
	message := Message{
		Type: "group_deliver", RequestID: "request-stable", SourceID: source.ID, TargetID: target.ID, Frame: body,
	}
	if err := host.deliverInbound(message); err != nil {
		t.Fatal(err)
	}
	// Simulate the same request arriving again after the transport reconnects.
	if err := host.deliverInbound(message); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("delivery callback count = %d, want exactly one", got)
	}
	conflict := message
	conflict.TargetID = "host-b/other"
	if err := host.deliverInbound(conflict); err == nil {
		t.Fatal("conflicting reuse of a delivery id was accepted")
	}
}

func TestHostReconnectsAndPreservesRemoteDeliveryAndLaneTransport(t *testing.T) {
	address := unusedTestAddress(t)
	hubCancel, hubDone := runTestHub(t, address)
	source := mustTestPeer(t, "host-a", "source", "codex", "project")
	target := mustTestPeer(t, "host-b", "target", "qwen", "project")
	deliveries := make(chan string, 4)
	laneKeys := make(chan string, 2)
	hostA, err := NewEmbeddedHost(EmbeddedHostOptions{
		Hub: address, HostID: "host-a", HostName: "host-a", Generation: 11, Build: "host-build-a",
		ScanInterval: 20 * time.Millisecond, HeartbeatInterval: 30 * time.Millisecond, HeartbeatTimeout: time.Second,
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{source}, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := NewEmbeddedHost(EmbeddedHostOptions{
		Hub: address, HostID: "host-b", HostName: "host-b", Generation: 22, Build: "host-build-b",
		Capabilities: []string{"future-lane"}, ScanInterval: 20 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond, HeartbeatTimeout: time.Second,
		Snapshot: func(context.Context) ([]Peer, error) { return []Peer{target}, nil },
		Deliver: func(_ context.Context, gotSource, gotTarget Peer, frame AgentFrame) error {
			if gotSource.ID != source.ID || gotTarget.ID != target.ID {
				return errors.New("unexpected routed identity")
			}
			deliveries <- frame.Content
			return nil
		},
		RunLane: func(_ context.Context, request RemoteLaneRequest) (RemoteLaneResult, error) {
			if request.Parent.SessionID != source.SessionID || request.Product != "future" || request.Capability != "future-lane" {
				return RemoteLaneResult{}, errors.New("remote parent attestation changed")
			}
			laneKeys <- request.IdempotencyKey
			return RemoteLaneResult{Stdout: []byte("lane-ok\n"), ExitCode: 7}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := runTestHost(hostA, ctx), runTestHost(hostB, ctx)
	defer func() {
		cancel()
		waitTestHost(t, doneA)
		waitTestHost(t, doneB)
		hubCancel()
		<-hubDone
	}()
	waitTest(t, func() bool { return len(hostA.RemotePeers()) == 1 && len(hostB.RemotePeers()) == 1 }, "initial host roster")
	if !hostA.Connected() || !hostB.Connected() {
		t.Fatalf("connected state hostA=%t hostB=%t", hostA.Connected(), hostB.Connected())
	}
	remoteHosts := hostA.RemoteHosts()
	if len(remoteHosts) != 1 || !reflect.DeepEqual(remoteHosts[0].Capabilities, []string{"future-lane"}) {
		t.Fatalf("host advertised capabilities = %#v", remoteHosts)
	}
	if err := hostA.Send(context.Background(), source, target, "message-before", "before restart", ""); err != nil {
		t.Fatal(err)
	}
	if got := <-deliveries; got != "before restart" {
		t.Fatalf("delivery before reconnect = %q", got)
	}
	parent := ParentContext{
		HostID: source.HostID, SessionID: source.SessionID, Product: source.Product,
		InstanceID: source.InstanceID, Groups: append([]string(nil), source.Groups...),
	}
	laneRequest := RemoteLaneRequest{
		Source: source, Parent: parent, TargetHostID: "host-b", Product: "future", Capability: "future-lane",
		Arguments: []string{"list", "--all"}, IdempotencyKey: "stable-remote-lane",
	}
	result, err := hostA.RunRemoteLane(context.Background(), laneRequest)
	if err != nil || string(result.Stdout) != "lane-ok\n" || result.ExitCode != 7 {
		t.Fatalf("remote lane result = %#v, %v", result, err)
	}
	replayed, err := hostA.RunRemoteLane(context.Background(), laneRequest)
	if err != nil || string(replayed.Stdout) != "lane-ok\n" || replayed.ExitCode != 7 {
		t.Fatalf("remote lane idempotent re-query = %#v, %v", replayed, err)
	}
	firstKey, secondKey := <-laneKeys, <-laneKeys
	if firstKey == "" || firstKey != secondKey {
		t.Fatalf("destination remote lane idempotency keys = %q / %q", firstKey, secondKey)
	}

	hubCancel()
	if err := <-hubDone; err != nil {
		t.Fatal(err)
	}
	waitTest(t, func() bool {
		return !hostA.Connected() && !hostB.Connected() &&
			len(hostA.RemoteHosts()) == 0 && len(hostB.RemoteHosts()) == 0
	}, "hosts to observe the stopped hub")
	hubCancel, hubDone = runTestHub(t, address)
	waitTest(t, func() bool {
		hosts := hostA.RemoteHosts()
		return len(hosts) == 1 && hosts[0].Generation == 22 && hosts[0].Build == "host-build-b"
	}, "reconnected host generation")
	if err := hostA.Send(context.Background(), source, target, "message-after", "after restart", ""); err != nil {
		t.Fatal(err)
	}
	if got := <-deliveries; got != "after restart" {
		t.Fatalf("delivery after reconnect = %q", got)
	}
}

func TestHostPreservesUnknownCapabilitiesInOptionsAndRoster(t *testing.T) {
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		HostID: "host-a", HostName: "host-a", Capabilities: []string{"future-lane", CapabilityCodexLane, "future-lane"},
		Snapshot: func(context.Context) ([]Peer, error) { return nil, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
		RunLane:  func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error) { return RemoteLaneResult{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host.options.Capabilities, []string{CapabilityCodexLane, "future-lane"}) {
		t.Fatalf("host capabilities = %q", host.options.Capabilities)
	}
	host.network = &wireConn{}
	if err := host.handleHubMessage(Message{
		Type: "roster", Version: ProtocolVersion,
		Hosts: []Host{{ID: "host-b", Name: "host-b", Capabilities: []string{"future-lane", "future-lane"}}},
	}); err != nil {
		t.Fatal(err)
	}
	remote := host.RemoteHosts()
	if len(remote) != 1 || !reflect.DeepEqual(remote[0].Capabilities, []string{"future-lane"}) {
		t.Fatalf("remote opaque roster = %#v", remote)
	}
}

func TestHostPassesExactCapabilityToDestinationAndRejectsMismatchBeforeCallback(t *testing.T) {
	source := mustTestPeer(t, "host-a", "source", "codex", "project")
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()
	var calls atomic.Int32
	requests := make(chan RemoteLaneRequest, 1)
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		HostID: "host-b", HostName: "host-b", Capabilities: []string{"future-lane"},
		Snapshot: func(context.Context) ([]Peer, error) { return nil, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
		RunLane: func(_ context.Context, request RemoteLaneRequest) (RemoteLaneResult, error) {
			calls.Add(1)
			requests <- request
			return RemoteLaneResult{ExitCode: 0}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host.remote[source.ID] = source
	host.network = newWireConn(server)
	responses := make(chan Message, 4)
	go func() {
		_ = scanMessages(client, func(message Message) error {
			responses <- message
			return nil
		})
	}()
	base := Message{
		Type: "lane_exec", RequestID: "mismatch", SourceID: source.ID, TargetHostID: "host-b",
		Product: "future", Capabilities: []string{"other-lane"}, Args: []string{"run"},
		ParentContext: testParentContext(source),
	}
	host.runInboundLane(base, &laneRun{})
	if calls.Load() != 0 {
		t.Fatal("mismatched capability reached destination callback")
	}
	if response := <-responses; response.Type != "lane_error" {
		t.Fatalf("mismatch response = %#v", response)
	}

	base.RequestID = "accepted"
	base.Capabilities = []string{"future-lane"}
	host.runInboundLane(base, &laneRun{})
	request := <-requests
	if request.Product != "future" || request.Capability != "future-lane" || calls.Load() != 1 {
		t.Fatalf("destination callback request = %#v, calls=%d", request, calls.Load())
	}
}

func TestHostBoundsInboundRemoteLaneConcurrencyAndRequiresLiveHub(t *testing.T) {
	host, err := NewEmbeddedHost(EmbeddedHostOptions{
		HostID: "host-a", HostName: "host-a",
		Snapshot: func(context.Context) ([]Peer, error) { return nil, nil },
		Deliver:  func(context.Context, Peer, Peer, AgentFrame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxRemoteLaneRuns; index++ {
		host.laneRuns[string(rune('a'+index))] = &laneRun{}
	}
	host.startLaneRun(Message{RequestID: "rejected"})
	if _, exists := host.laneRuns["rejected"]; exists {
		t.Fatal("remote lane concurrency limit was exceeded")
	}
	host.remoteHosts["host-b"] = Host{ID: "host-b", Name: "host-b", Capabilities: []string{CapabilityCodexLane}}
	if _, err := host.resolveRemoteHost("host-b", CapabilityCodexLane); err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("remote host resolved without a live hub: %v", err)
	}
}

func mustTestPeer(t *testing.T, host, session, product string, groups ...string) Peer {
	t.Helper()
	peer, err := BuildPeer(host, host, session, session, "idle", "/work", product, "default", product+":"+session, "", groups)
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

func unusedTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func runTestHub(t *testing.T, address string) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunHub(ctx, HubOptions{Listen: address, Logger: discardTestLogger()}) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return cancel, done
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("hub did not start")
	return nil, nil
}

func runTestHost(host *EmbeddedHost, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	return done
}

func waitTestHost(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("host stopped: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("host did not stop")
	}
}

func waitTest(t *testing.T, predicate func() bool, label string) {
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

func discardTestLogger() *log.Logger { return log.New(io.Discard, "", 0) }
