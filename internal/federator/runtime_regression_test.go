package federator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentInstanceLockRejectsSecondOwner(t *testing.T) {
	runtimeDir := t.TempDir()
	first, err := acquireAgentInstanceLock(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgentInstanceLock(first)
	second, err := acquireAgentInstanceLock(runtimeDir)
	if err == nil {
		releaseAgentInstanceLock(second)
		t.Fatal("second agent instance unexpectedly acquired the lock")
	}
}

func TestWireSendHasBoundedWriteDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()
	wire := newWireConnWithTimeout(client, 25*time.Millisecond)
	started := time.Now()
	err := wire.Send(Message{Type: "ping"})
	if err == nil {
		t.Fatal("write to a stalled connection unexpectedly succeeded")
	}
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("write error = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded write took %s", elapsed)
	}
}

func TestAgentReconnectsWhenHubStopsAnsweringHeartbeats(t *testing.T) {
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
					// Deliberately consume pings without sending pong.
				}
			}(conn)
		}
	}()

	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunAgent(ctx, AgentOptions{
			Hub: listener.Addr().String(), HostID: "heartbeat-test",
			ClaudeConfigDir: filepath.Join(root, "config"), RuntimeDir: filepath.Join(root, "runtime"),
			HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 75 * time.Millisecond,
			Logger: discardLogger(),
		})
	}()
	if !waitFor(func() bool { return accepts.Load() >= 2 }, 2*time.Second) {
		cancel()
		t.Fatalf("agent accepted %d hub sessions, want a heartbeat reconnect", accepts.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	<-serverDone
}

func TestHubExpiresSilentAgent(t *testing.T) {
	hub := &hub{
		logger: discardLogger(), clients: map[string]*hubClient{},
		clientTimeout: 30 * time.Millisecond,
	}
	client := newTestHubClient(t, hub, "silent-host")
	defer func() { _ = client.conn.Close() }()
	if !waitFor(func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.clients) == 0
	}, time.Second) {
		t.Fatal("silent hub client was not expired")
	}
}

func TestGroupedLocalDeliveryFailureDoesNotDisconnectAgent(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(registry, 0700); err != nil {
		t.Fatal(err)
	}
	agent := &agent{
		logger: discardLogger(), registryDir: registry,
		local: map[string]localPeer{}, remote: map[string]Peer{
			"remote/source": groupedRemoteLaneParent("remote", "source", "claude"),
		},
	}
	frame, err := json.Marshal(map[string]any{"type": "user"})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.handleHubMessage(Message{
		Type: "group_deliver", SourceID: "remote/source", TargetID: "gone/target", Frame: frame,
	}); err != nil {
		t.Fatalf("one stale peer disconnected its host agent: %v", err)
	}
}

func TestAgentRefreshesLocalPeersWhileHubIsDisconnected(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "claude")
	runtimeDir := filepath.Join(root, "runtime")
	registryDir := filepath.Join(configDir, "sessions")
	if err := ensureDir(registryDir); err != nil {
		t.Fatal(err)
	}
	closedHub, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hubAddress := closedHub.Addr().String()
	_ = closedHub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunAgent(ctx, AgentOptions{
			Hub: hubAddress, HostID: "offline-host", HostName: "offline-host",
			ClaudeConfigDir: configDir, RuntimeDir: runtimeDir, StateDir: filepath.Join(root, "state"),
			ScanInterval: 20 * time.Millisecond,
			Logger:       discardLogger(),
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if !waitFor(func() bool {
		status, statusErr := ReadAgentStatus(runtimeDir)
		return statusErr == nil && !status.Connected && status.LocalPeers == 0
	}, 2*time.Second) {
		t.Fatal("disconnected agent control socket did not become ready")
	}
	if _, err := ResolveSessionPreferences(runtimeDir, ResolvePreferencesRequest{
		SessionID: "offline-session", Product: "codex", Groups: []string{"project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}

	peerSocket := filepath.Join(root, "local-peer.sock")
	peerListener, err := net.Listen("unix", peerSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peerListener.Close() })
	go func() {
		for {
			conn, acceptErr := peerListener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "offline-session", Product: "codex", Name: "offline-peer",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()), Socket: peerSocket,
	}
	if _, err := RegisterPeer(runtimeDir, registration); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool {
		status, statusErr := ReadAgentStatus(runtimeDir)
		return statusErr == nil && !status.Connected && status.LocalPeers == 1
	}, 2*time.Second) {
		t.Fatal("local peer count did not refresh while the hub was disconnected")
	}
	if err := UnregisterPeer(runtimeDir, registration); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool {
		status, statusErr := ReadAgentStatus(runtimeDir)
		return statusErr == nil && !status.Connected && status.LocalPeers == 0
	}, 2*time.Second) {
		t.Fatal("removed local peer remained in disconnected status")
	}
}
