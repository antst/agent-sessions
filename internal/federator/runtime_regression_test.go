package federator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestReplacementShadowWaitsForOldSocketOwner(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "shadow.sock")
	oldCtx, cancelOld := context.WithCancel(context.Background())
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- RunShadow(oldCtx, ShadowOptions{
			Listen: socket, Control: filepath.Join(root, "missing-control.sock"),
			TargetID: "remote/peer", Logger: discardLogger(),
		})
	}()
	if !waitFor(func() bool { return probeUnix(socket, 50*time.Millisecond) }, time.Second) {
		cancelOld()
		t.Fatal("first shadow did not become ready")
	}
	oldInfo, err := os.Lstat(socket)
	if err != nil {
		cancelOld()
		t.Fatal(err)
	}

	newCtx, cancelNew := context.WithCancel(context.Background())
	newDone := make(chan error, 1)
	go func() {
		newDone <- RunShadow(newCtx, ShadowOptions{
			Listen: socket, Control: filepath.Join(root, "missing-control.sock"),
			TargetID: "remote/peer", Logger: discardLogger(),
		})
	}()
	time.Sleep(75 * time.Millisecond)
	currentInfo, err := os.Lstat(socket)
	if err != nil || !os.SameFile(oldInfo, currentInfo) {
		cancelOld()
		cancelNew()
		t.Fatalf("replacement rebound the socket before the old owner exited: %v", err)
	}

	cancelOld()
	if err := <-oldDone; err != nil {
		cancelNew()
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var lastStatErr, lastDialErr error
	for {
		_, lastStatErr = os.Lstat(socket)
		conn, dialErr := net.DialTimeout("unix", socket, 50*time.Millisecond)
		lastDialErr = dialErr
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		select {
		case err := <-newDone:
			cancelNew()
			t.Fatalf("replacement shadow exited before retaining its socket: %v", err)
		case <-deadline.C:
			cancelNew()
			t.Fatalf("replacement shadow stayed live but did not retain a reachable socket: lstat=%v dial=%v", lastStatErr, lastDialErr)
		case <-ticker.C:
		}
	}
	select {
	case err := <-newDone:
		t.Fatalf("replacement shadow exited early: %v", err)
	default:
	}
	cancelNew()
	if err := <-newDone; err != nil {
		t.Fatal(err)
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

func TestLocalDeliveryFailureDoesNotDisconnectAgent(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(registry, 0700); err != nil {
		t.Fatal(err)
	}
	agent := &agent{
		logger: discardLogger(), registryDir: registry,
		local: map[string]localPeer{}, remote: map[string]Peer{}, shadows: map[string]*shadowHandle{},
	}
	frame, err := json.Marshal(map[string]any{"type": "user"})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.handleHubMessage(Message{
		Type: "deliver", SourceID: "remote/source", TargetID: "gone/target", Frame: frame,
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
			ClaudeConfigDir: configDir, RuntimeDir: runtimeDir, ScanInterval: 20 * time.Millisecond,
			Logger: discardLogger(),
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
	recordPath := filepath.Join(registryDir, strconv.Itoa(os.Getpid())+".json")
	if err := writeJSONAtomic(recordPath, registryRecord{
		PID: os.Getpid(), SessionID: "offline-session", Name: "offline-peer",
		ProcStart: processStart(os.Getpid()), MessagingSocketPath: peerSocket,
	}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool {
		status, statusErr := ReadAgentStatus(runtimeDir)
		return statusErr == nil && !status.Connected && status.LocalPeers == 1
	}, 2*time.Second) {
		t.Fatal("local peer count did not refresh while the hub was disconnected")
	}
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool {
		status, statusErr := ReadAgentStatus(runtimeDir)
		return statusErr == nil && !status.Connected && status.LocalPeers == 0
	}, 2*time.Second) {
		t.Fatal("removed local peer remained in disconnected status")
	}
}
