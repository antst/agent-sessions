package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestClaudeEffectivePermissionModePreservesDurableLaunchDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		native        string
		durableBypass bool
		want          string
		wantErr       bool
	}{
		{name: "omitted native bypass", durableBypass: true, want: "bypassPermissions"},
		{name: "omitted native constrained", durableBypass: false, want: "default"},
		{name: "corroborated bypass", native: "bypassPermissions", durableBypass: true, want: "bypassPermissions"},
		{name: "corroborated constrained", native: "default", durableBypass: false, want: "default"},
		{name: "native downgrade", native: "default", durableBypass: true, wantErr: true},
		{name: "native upgrade", native: "bypassPermissions", durableBypass: false, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := claudeEffectivePermissionMode(test.native, test.durableBypass)
			if (err != nil) != test.wantErr {
				t.Fatalf("claudeEffectivePermissionMode(%q, %t) error = %v, wantErr %t", test.native, test.durableBypass, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("claudeEffectivePermissionMode(%q, %t) = %q, want %q", test.native, test.durableBypass, got, test.want)
			}
		})
	}
}

func TestCleanupClaudeAttachmentAfterDaemonRestartUsesDurableEvidence(t *testing.T) {
	identity := departedClaudeIdentity(t)
	stateRoot := testutil.ShortSocketRoot(t, "as-claude-clean-", "run/c-00000000000000000000.sock")
	configRoot := filepath.Join(t.TempDir(), "claude")
	registryRoot := filepath.Join(configRoot, "sessions")
	if err := os.MkdirAll(registryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	attachmentID := "11111111-2222-4333-8444-555555555555"
	sessionID := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	lifecycleRoot := filepath.Join(stateRoot, "native", "claude", attachmentID)
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	managedSocket := claudeManagedSocket(stateRoot, attachmentID)
	if err := os.MkdirAll(filepath.Dir(managedSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", managedSocket)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatal("managed Claude test listener is not Unix")
	}
	unixListener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(registryRoot, strconv.Itoa(identity.PID)+".json")
	record, err := json.Marshal(launcher.ClaudeNativePeerRecord{
		PID: identity.PID, SessionID: sessionID, ProcStart: identity.Start,
		MessagingSocketPath: managedSocket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	attachment := daemonpkg.ManagedAttachment{
		ID: attachmentID, Product: "claude", ProfileIdentity: configRoot, NativeSessionID: sessionID,
		Evidence: daemonpkg.NativeEvidence{
			Process: identity, RegistryPath: registryPath, SocketPath: managedSocket,
			ThreadID: sessionID, ArtifactPath: settingsPath,
		},
	}
	coordinator := &hostCoordinator{stateRoot: stateRoot}
	if err := coordinator.cleanupClaudeAttachment(context.Background(), attachment); err != nil {
		t.Fatalf("cleanup durable Claude attachment: %v", err)
	}
	for _, path := range []string{registryPath, managedSocket, settingsPath, lifecycleRoot} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("durable Claude cleanup left %s: %v", path, err)
		}
	}
}

func TestCleanupDurableClaudeAttachmentRejectsForeignLifecyclePath(t *testing.T) {
	identity := departedClaudeIdentity(t)
	stateRoot := t.TempDir()
	configRoot := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "launch-settings.json")
	if err := os.WriteFile(foreign, []byte("owned elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachmentID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sessionID := "ffffffff-1111-4222-8333-444444444444"
	attachment := daemonpkg.ManagedAttachment{
		ID: attachmentID, Product: "claude", ProfileIdentity: configRoot, NativeSessionID: sessionID,
		Evidence: daemonpkg.NativeEvidence{
			Process:      identity,
			RegistryPath: filepath.Join(configRoot, "sessions", strconv.Itoa(identity.PID)+".json"),
			SocketPath:   claudeManagedSocket(stateRoot, attachmentID), ThreadID: sessionID,
			ArtifactPath: foreign,
		},
	}
	if err := cleanupDurableClaudeAttachment(stateRoot, attachment); err == nil {
		t.Fatal("foreign durable Claude lifecycle path was accepted")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign lifecycle artifact changed: %v", err)
	}
}

func TestCleanupDurableClaudeAttachmentPreservesUnboundManagedSocket(t *testing.T) {
	identity := departedClaudeIdentity(t)
	stateRoot := testutil.ShortSocketRoot(t, "as-claude-unbound-", "run/c-00000000000000000000.sock")
	configRoot := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	attachmentID := "12121212-3434-4567-8899-abababababab"
	sessionID := "cdcdcdcd-efef-4012-8345-565656565656"
	lifecycleRoot := filepath.Join(stateRoot, "native", "claude", attachmentID)
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	managedSocket := claudeManagedSocket(stateRoot, attachmentID)
	if err := os.MkdirAll(filepath.Dir(managedSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", managedSocket)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatal("managed Claude test listener is not Unix")
	}
	unixListener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	attachment := daemonpkg.ManagedAttachment{
		ID: attachmentID, Product: "claude", ProfileIdentity: configRoot, NativeSessionID: sessionID,
		Evidence: daemonpkg.NativeEvidence{
			Process:      identity,
			RegistryPath: filepath.Join(configRoot, "sessions", strconv.Itoa(identity.PID)+".json"),
			SocketPath:   managedSocket, ThreadID: sessionID, ArtifactPath: settingsPath,
		},
	}
	if err := cleanupDurableClaudeAttachment(stateRoot, attachment); err == nil {
		t.Fatal("unbound managed Claude socket was removed without a durable row")
	}
	for _, path := range []string{managedSocket, settingsPath, lifecycleRoot} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unbound managed Claude artifact changed at %s: %v", path, err)
		}
	}
}

func departedClaudeIdentity(t *testing.T) procinfo.Identity {
	t.Helper()
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := procinfo.CaptureIdentity(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed Claude identity helper exited successfully")
	}
	if procinfo.Read(identity.PID).Status != procinfo.Absent {
		t.Fatalf("Claude identity helper PID %d is not absent", identity.PID)
	}
	return identity
}
