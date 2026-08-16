package bridge

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

// useBridgeTestAgent gives lifecycle tests the same mandatory grouped-agent
// boundary as production launchers. Tests that do not exercise routing still
// use the real catalog/control protocol instead of falling back to a flat
// native registry or accidentally connecting to the developer's live agent.
func useBridgeTestAgent(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- federator.RunAgent(ctx, federator.AgentOptions{
			HostID: "bridge-test-host", HostName: "bridge-test-host",
			ClaudeConfigDir: filepath.Join(root, "claude"),
			RuntimeDir:      runtimeDir, StateDir: filepath.Join(root, "state"),
			ScanInterval: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0),
		})
	}()
	if !waitForCondition(2*time.Second, func() bool {
		_, err := federator.ReadAgentStatus(runtimeDir)
		return err == nil
	}) {
		cancel()
		t.Fatal("test host agent did not become ready")
	}
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("test host agent: %v", err)
		}
	})
	return runtimeDir
}

func prepareBridgeTestLaneParent(t *testing.T, runtimeDir, childID, parentID string) {
	t.Helper()
	if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: childID, Product: "claude", Kind: federator.SessionKindLane,
		ParentSessionID: parentID, ParentHostID: "bridge-test-host", ParentSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func registerBridgeTestParent(t *testing.T, runtimeDir, parentID string) (<-chan struct{}, func()) {
	t.Helper()
	if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: parentID, Product: "codex", Kind: federator.SessionKindInteractive,
	}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 4)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = bufio.NewReader(connection).ReadBytes('\n')
			_ = connection.Close()
			received <- struct{}{}
		}
	}()
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: parentID, Product: "codex", Name: parentID,
		PID: os.Getpid(), ProcStart: readProcStart(os.Getpid()), Socket: socket,
	}
	if _, err := federator.RegisterPeer(runtimeDir, registration); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	stop := func() {
		_ = federator.UnregisterPeer(runtimeDir, registration)
		_ = listener.Close()
	}
	t.Cleanup(stop)
	return received, stop
}
