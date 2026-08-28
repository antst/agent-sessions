package federation

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostAgentHandshakesBeforePublishingSnapshot(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverResult := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		scanner := newAgentTestScanner(conn)
		hello, scanErr := scanAgentWireFrame(scanner)
		if scanErr != nil {
			serverResult <- scanErr
			return
		}
		if hello.Type != "hello" || hello.Version != ProtocolVersion || hello.HostID != "host-a" ||
			hello.HostName != "workstation" || hello.RuntimeVersion != "host-release" ||
			hello.RuntimeIdentity != "sha256:host" || hello.Generation != 7 ||
			!reflect.DeepEqual(hello.Products, []string{"codex"}) ||
			!reflect.DeepEqual(hello.Capabilities, []string{"codex-lane"}) {
			serverResult <- &agentTestError{text: "unexpected hello"}
			return
		}
		if sendErr := (&agentWireConn{conn: conn, writeTimeout: time.Second}).send(agentWireFrame{
			Type: "hello_ok", Version: ProtocolVersion,
		}); sendErr != nil {
			serverResult <- sendErr
			return
		}
		snapshot, scanErr := scanAgentWireFrame(scanner)
		if scanErr != nil {
			serverResult <- scanErr
			return
		}
		if snapshot.Type != "snapshot" || len(snapshot.Peers) != 1 || snapshot.Peers[0].ID != "host-a/session-a" {
			serverResult <- &agentTestError{text: "unexpected snapshot"}
			return
		}
		serverResult <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	agent, err := NewHostAgent(HostAgentOptions{
		HubAddress: listener.Addr().String(),
		Advertisement: HostAdvertisement{
			HostID: "host-a", HostName: "workstation", ProtocolVersion: ProtocolVersion,
			RuntimeVersion: "host-release", RuntimeIdentity: "sha256:host", Generation: 7,
			Products: []string{"codex"}, Capabilities: []string{"codex-lane"},
		},
		HeartbeatInterval: time.Second, HeartbeatTimeout: 2 * time.Second,
		Callbacks: AgentCallbacks{
			Snapshot: func(context.Context) ([]Peer, error) {
				return []Peer{{ID: "host-a/session-a", HostID: "host-a", SessionID: "session-a"}}, nil
			},
			StateChanged: func(status AgentStatus) {
				if status.State == AgentConnected {
					cancel()
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	status := agent.Status()
	if status.ConnectionGeneration != 1 || !reflect.DeepEqual(status.Capabilities, []string{"codex-lane"}) {
		t.Fatalf("agent status = %+v", status)
	}
}

func TestHostAgentRejectsMismatchedHelloBeforeSnapshot(t *testing.T) {
	server, client := net.Pipe()
	serverResult := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		scanner := newAgentTestScanner(server)
		if _, err := scanAgentWireFrame(scanner); err != nil {
			serverResult <- err
			return
		}
		serverResult <- (&agentWireConn{conn: server, writeTimeout: time.Second}).send(agentWireFrame{
			Type: "hello_ok", Version: ProtocolVersion + 1,
		})
	}()
	ctx, cancel := context.WithCancel(context.Background())
	agent, err := NewHostAgent(HostAgentOptions{
		HubAddress: "hub.invalid:7443", HostID: "host-a", HostName: "host-a",
		DialContext:    func(context.Context, string, string) (net.Conn, error) { return client, nil },
		InitialBackoff: time.Second, MaximumBackoff: time.Second,
		Callbacks: AgentCallbacks{
			Snapshot: func(context.Context) ([]Peer, error) {
				t.Fatal("protocol mismatch published a peer snapshot")
				return nil, nil
			},
			StateChanged: func(status AgentStatus) {
				if status.State == AgentIncompatible {
					cancel()
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	status := agent.Status()
	if status.ConnectionGeneration != 0 || status.LastErrorCode != "protocol_mismatch" {
		t.Fatalf("mismatch status = %+v", status)
	}
}

func TestHostAgentReconnectsWithTheSameAdvertisement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	serverResults := make(chan error, 2)
	agent, err := NewHostAgent(HostAgentOptions{
		HubAddress: "hub.invalid:7443",
		Advertisement: HostAdvertisement{
			HostID: "stable-host", HostName: "stable-host", ProtocolVersion: ProtocolVersion,
			RuntimeVersion: "host-release", RuntimeIdentity: "sha256:stable", Generation: 19,
			Products: []string{"qwen"}, Capabilities: []string{"qwen-lane"},
		},
		InitialBackoff: time.Millisecond, MaximumBackoff: time.Millisecond,
		HeartbeatInterval: time.Second, HeartbeatTimeout: 2 * time.Second,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			server, client := net.Pipe()
			attempt := attempts.Add(1)
			go func() {
				defer func() { _ = server.Close() }()
				scanner := newAgentTestScanner(server)
				hello, scanErr := scanAgentWireFrame(scanner)
				if scanErr != nil {
					serverResults <- scanErr
					return
				}
				if hello.HostID != "stable-host" || hello.Generation != 19 ||
					!reflect.DeepEqual(hello.Capabilities, []string{"qwen-lane"}) {
					serverResults <- &agentTestError{text: "reconnect changed host advertisement"}
					return
				}
				wire := &agentWireConn{conn: server, writeTimeout: time.Second}
				if sendErr := wire.send(agentWireFrame{Type: "hello_ok", Version: ProtocolVersion}); sendErr != nil {
					serverResults <- sendErr
					return
				}
				if _, scanErr = scanAgentWireFrame(scanner); scanErr != nil {
					serverResults <- scanErr
					return
				}
				serverResults <- nil
				if attempt == 2 {
					cancel()
				}
			}()
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || agent.Status().ConnectionGeneration != 2 {
		t.Fatalf("reconnect attempts=%d status=%+v", attempts.Load(), agent.Status())
	}
	for range 2 {
		if err := <-serverResults; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostAgentRemoteLaneAcceptanceAndCancellationUseTypedConnection(t *testing.T) {
	server, client := net.Pipe()
	serverResult := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		scanner := newAgentTestScanner(server)
		if _, err := scanAgentWireFrame(scanner); err != nil {
			serverResult <- err
			return
		}
		wire := &agentWireConn{conn: server, writeTimeout: time.Second}
		if err := wire.send(agentWireFrame{Type: "hello_ok", Version: ProtocolVersion}); err != nil {
			serverResult <- err
			return
		}
		if _, err := scanAgentWireFrame(scanner); err != nil { // snapshot
			serverResult <- err
			return
		}
		exec, err := scanAgentWireFrame(scanner)
		if err != nil {
			serverResult <- err
			return
		}
		if exec.Type != "lane_exec" || exec.RemoteLane == nil || exec.RemoteLane.RequestID != "request-1" ||
			exec.RemoteLane.Parent.Groups[0] != "global-project" {
			serverResult <- &agentTestError{text: "typed remote lane envelope was not preserved"}
			return
		}
		accepted := RemoteLaneAccepted{RequestID: "request-1", LaneSessionID: "lane-1", TurnID: "turn-1", AcceptedRevision: 3}
		if err := wire.send(agentWireFrame{Type: "lane_accepted", RequestID: "request-1", RemoteAccepted: &accepted}); err != nil {
			serverResult <- err
			return
		}
		cancel, err := scanAgentWireFrame(scanner)
		if err != nil || cancel.Type != "lane_cancel" || cancel.RequestID != "request-1" {
			serverResult <- &agentTestError{text: "typed remote lane cancellation was not preserved"}
			return
		}
		if err := wire.send(agentWireFrame{Type: "lane_cancelled", RequestID: "request-1"}); err != nil {
			serverResult <- err
			return
		}
		result, err := scanAgentWireFrame(scanner)
		if err != nil || result.Type != "lane_result" || result.RemoteResult == nil ||
			result.RemoteResult.ResultReference["native_result_id"] != "result-1" {
			serverResult <- &agentTestError{text: "typed remote lane result was not preserved"}
			return
		}
		if err := wire.send(agentWireFrame{Type: "lane_result_ack", RequestID: "request-1"}); err != nil {
			serverResult <- err
			return
		}
		archive, err := scanAgentWireFrame(scanner)
		if err != nil || archive.Type != "lane_archive" || archive.RemoteArchive == nil ||
			archive.RemoteArchive.RemoteRequestID != "request-1" || archive.RemoteArchive.LaneSessionID != "lane-1" {
			serverResult <- &agentTestError{text: "typed remote lane archive was not preserved"}
			return
		}
		archived := RemoteLaneArchived{RequestID: "archive-1", LaneSessionID: "lane-1", ArchiveRevision: 1}
		if err := wire.send(agentWireFrame{Type: "lane_archived", RequestID: "archive-1", RemoteArchived: &archived}); err != nil {
			serverResult <- err
			return
		}
		serverResult <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{}, 1)
	agent, err := NewHostAgent(HostAgentOptions{
		HubAddress: "hub.invalid:7443", HostID: "host-a", HostName: "host-a",
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return client, nil },
		HeartbeatInterval: time.Second, HeartbeatTimeout: 2 * time.Second,
		Callbacks: AgentCallbacks{
			Snapshot: func(context.Context) ([]Peer, error) {
				return []Peer{{ID: "host-a/parent", HostID: "host-a", SessionID: "parent", InstanceID: "parent-instance"}}, nil
			},
			StateChanged: func(status AgentStatus) {
				if status.State == AgentConnected {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- agent.Run(ctx) }()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("host agent did not connect")
	}
	accepted, err := agent.SendRemoteLane(context.Background(), RemoteLaneEnvelope{
		RequestID: "request-1", SourceID: "host-a/parent", TargetHostID: "host-b",
		Parent:  Peer{ID: "host-a/parent", HostID: "host-a", SessionID: "parent", InstanceID: "parent-instance", Groups: []string{"global-project"}},
		Product: "qwen", LaneSessionID: "lane-1", TurnID: "turn-1", Name: "worker", Cwd: "/workspace",
	})
	if err != nil || accepted.AcceptedRevision != 3 {
		t.Fatalf("remote lane acceptance = %#v, %v", accepted, err)
	}
	if err := agent.CancelRemoteLane(context.Background(), "request-1"); err != nil {
		t.Fatal(err)
	}
	if err := agent.PublishRemoteLaneResult(context.Background(), RemoteLaneResult{
		RequestID: "request-1", LaneSessionID: "lane-1", TurnID: "turn-1", Outcome: "completed",
		ResultReference: map[string]any{"native_result_id": "result-1"},
	}); err != nil {
		t.Fatal(err)
	}
	archived, err := agent.ArchiveRemoteLane(context.Background(), RemoteLaneArchive{
		RequestID: "archive-1", RemoteRequestID: "request-1", SourceID: "host-a/parent",
		TargetHostID: "host-b", Product: "qwen", LaneSessionID: "lane-1",
	})
	if err != nil || archived.ArchiveRevision != 1 {
		t.Fatalf("remote lane archive = %#v, %v", archived, err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestHostAgentRemoteLaneCancellationRefusalIsExplicit(t *testing.T) {
	decision := make(chan error, 1)
	agent := &HostAgent{pendingCancel: map[string]chan error{"request-refused": decision}}
	agent.completePendingCancel(agentWireFrame{
		Type: "lane_cancel_refused", RequestID: "request-refused", Error: "native turn is already terminal",
	})
	err := <-decision
	if !IsRemoteLaneCancellationRefused(err) {
		t.Fatalf("cancellation refusal = %v", err)
	}
}

func TestTwoHostRemoteLaneRoutesOneAcceptanceAndOneTerminalNotice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hubDone := make(chan error, 1)
	go func() {
		hubDone <- RunHub(ctx, HubOptions{Listen: address, RuntimeVersion: "hub-test", RuntimeIdentity: "sha256:hub-test"})
	}()

	var target *HostAgent
	var laneReady atomic.Bool
	ready := make(chan struct{})
	lanePublished := make(chan struct{})
	terminal := make(chan RoutedDelivery, 1)
	var readyOnce, laneOnce sync.Once

	source, err := NewHostAgent(HostAgentOptions{
		HubAddress: address,
		Advertisement: HostAdvertisement{HostID: "host-a", HostName: "host-a", ProtocolVersion: ProtocolVersion,
			RuntimeVersion: "source", RuntimeIdentity: "sha256:source", Generation: 1},
		InitialBackoff: time.Millisecond, MaximumBackoff: 5 * time.Millisecond,
		HeartbeatInterval: time.Second, HeartbeatTimeout: 3 * time.Second, DeliveryTimeout: 2 * time.Second,
		Callbacks: AgentCallbacks{
			Snapshot: func(context.Context) ([]Peer, error) {
				return []Peer{{ID: "host-a/parent", HostID: "host-a", HostName: "host-a", SessionID: "parent",
					GlobalID: GlobalSessionID("host-a", "parent"), Name: "parent", DisplayName: QualifiedPeerName("parent", "host-a"),
					InstanceID: "parent-instance", Entrypoint: "codex", Groups: []string{"global-project", "session:host-a/parent"},
					PeerProtocol: GroupProtocolVersion}}, nil
			},
			Roster: func(_ context.Context, roster Roster) error {
				if len(roster.Hosts) == 2 {
					readyOnce.Do(func() { close(ready) })
				}
				return nil
			},
			TerminalNotice: func(_ context.Context, delivery RoutedDelivery) error {
				terminal <- delivery
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err = NewHostAgent(HostAgentOptions{
		HubAddress: address,
		Advertisement: HostAdvertisement{HostID: "host-b", HostName: "host-b", ProtocolVersion: ProtocolVersion,
			RuntimeVersion: "target", RuntimeIdentity: "sha256:target", Generation: 1,
			Products: []string{"qwen"}, Capabilities: []string{"qwen-lane"}},
		InitialBackoff: time.Millisecond, MaximumBackoff: 5 * time.Millisecond,
		HeartbeatInterval: time.Second, HeartbeatTimeout: 3 * time.Second, DeliveryTimeout: 2 * time.Second,
		Callbacks: AgentCallbacks{
			Snapshot: func(context.Context) ([]Peer, error) {
				if !laneReady.Load() {
					return nil, nil
				}
				return []Peer{{ID: "host-b/lane-1", HostID: "host-b", HostName: "host-b", SessionID: "lane-1",
					GlobalID: GlobalSessionID("host-b", "lane-1"), Name: "worker", DisplayName: QualifiedPeerName("worker", "host-b"),
					InstanceID: "lane-1", Entrypoint: "qwen", Groups: []string{"global-project", "session:host-b/lane-1"},
					PeerProtocol: GroupProtocolVersion, ParentSessionID: "parent"}}, nil
			},
			Roster: func(_ context.Context, roster Roster) error {
				for _, peer := range roster.Peers {
					if peer.ID == "host-b/lane-1" {
						laneOnce.Do(func() { close(lanePublished) })
					}
				}
				return nil
			},
			RemoteLane: func(_ context.Context, envelope RemoteLaneEnvelope) (RemoteLaneAccepted, error) {
				if envelope.Parent.ID != "host-a/parent" || !reflect.DeepEqual(envelope.Parent.Groups, []string{"global-project", "session:host-a/parent"}) ||
					envelope.TargetHostID != "host-b" || envelope.Product != "qwen" {
					return RemoteLaneAccepted{}, &agentTestError{text: "remote parent, groups, or target changed"}
				}
				laneReady.Store(true)
				target.NotifySnapshot()
				go func() {
					select {
					case <-lanePublished:
					case <-ctx.Done():
						return
					}
					_ = target.PublishRemoteLaneNotice(context.Background(), RemoteLaneNotice{
						NoticeID: "notice-1", RequestID: envelope.RequestID, TargetHostID: "host-a", TargetSessionID: "parent",
						LaneSessionID: envelope.LaneSessionID, TurnID: envelope.TurnID, Outcome: "completed",
					}, Peer{ID: "host-b/lane-1", HostID: "host-b", HostName: "host-b", SessionID: "lane-1",
						GlobalID: GlobalSessionID("host-b", "lane-1"), Name: "worker", DisplayName: QualifiedPeerName("worker", "host-b"),
						InstanceID: "lane-1", Entrypoint: "qwen", Groups: []string{"global-project", "session:host-b/lane-1"}, PeerProtocol: GroupProtocolVersion})
				}()
				return RemoteLaneAccepted{RequestID: envelope.RequestID, LaneSessionID: envelope.LaneSessionID,
					TurnID: envelope.TurnID, AcceptedRevision: 1}, nil
			},
			LaneArchive: func(_ context.Context, request RemoteLaneArchive) (RemoteLaneArchived, error) {
				if request.RemoteRequestID != "request-1" || request.SourceID != "host-a/parent" ||
					request.TargetHostID != "host-b" || request.Product != "qwen" || request.LaneSessionID != "lane-1" {
					return RemoteLaneArchived{}, &agentTestError{text: "remote archive identity changed"}
				}
				return RemoteLaneArchived{RequestID: request.RequestID, LaneSessionID: request.LaneSessionID, ArchiveRevision: 1}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceDone, targetDone := make(chan error, 1), make(chan error, 1)
	go func() { sourceDone <- source.Run(ctx) }()
	go func() { targetDone <- target.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("two federation hosts did not publish a common roster")
	}
	accepted, err := source.SendRemoteLane(context.Background(), RemoteLaneEnvelope{
		RequestID: "request-1", SourceID: "host-a/parent", TargetHostID: "host-b",
		Parent: Peer{ID: "host-a/parent", HostID: "host-a", HostName: "host-a", SessionID: "parent",
			GlobalID: GlobalSessionID("host-a", "parent"), Name: "parent", DisplayName: QualifiedPeerName("parent", "host-a"),
			InstanceID: "parent-instance", Entrypoint: "codex", Groups: []string{"global-project", "session:host-a/parent"}, PeerProtocol: GroupProtocolVersion},
		Product: "qwen", LaneSessionID: "lane-1", TurnID: "turn-1", Name: "worker", Cwd: "/workspace",
		Groups: []string{"global-project"}, InheritParentGroups: true,
	})
	if err != nil || accepted.AcceptedRevision != 1 {
		t.Fatalf("two-host remote acceptance = %#v, %v", accepted, err)
	}
	select {
	case delivery := <-terminal:
		if delivery.SourceID != "host-b/lane-1" || delivery.TargetID != "host-a/parent" {
			t.Fatalf("terminal notice route = %#v", delivery)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("two-host remote terminal notice was not acknowledged")
	}
	archived, err := source.ArchiveRemoteLane(context.Background(), RemoteLaneArchive{
		RequestID: "archive-1", RemoteRequestID: "request-1", SourceID: "host-a/parent",
		TargetHostID: "host-b", Product: "qwen", LaneSessionID: "lane-1",
	})
	if err != nil || archived.ArchiveRevision != 1 {
		t.Fatalf("two-host remote archive = %#v, %v", archived, err)
	}
	cancel()
	if err := <-sourceDone; err != nil {
		t.Fatal(err)
	}
	if err := <-targetDone; err != nil {
		t.Fatal(err)
	}
	if err := <-hubDone; err != nil {
		t.Fatal(err)
	}
}

func newAgentTestScanner(conn net.Conn) *bufio.Scanner {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	return scanner
}

type agentTestError struct{ text string }

func (failure *agentTestError) Error() string { return failure.text }
