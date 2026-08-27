package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
)

func TestEnsureRuntimeFailsUnavailableBeforeDiscoveryWithoutMutation(t *testing.T) {
	previous := queryLauncherDaemon
	t.Cleanup(func() { queryLauncherDaemon = previous })
	want := &daemon.UnavailableError{
		Endpoint: "/run/user/501/agent-sessions/daemon.sock",
		Cause:    errors.New("connection refused"), NextAction: "systemctl --user status agent-sessions.service",
	}
	queryLauncherDaemon = func(_ context.Context) error { return want }

	root := t.TempDir()
	missingParent := filepath.Join(root, "must-not-create")
	t.Setenv("CODEX_PEER_PLUGIN_ROOT", filepath.Join(missingParent, "plugin"))
	t.Setenv("CODEX_PEER_NATIVE_RUNTIME", filepath.Join(missingParent, "runtime"))
	_, err := EnsureRuntime()
	if !errors.Is(err, want) {
		t.Fatalf("EnsureRuntime error = %v, want exact unavailable cause", err)
	}
	var exit *ExitError
	var unavailable *daemon.UnavailableError
	if !errors.As(err, &exit) || exit.Code != 3 || !errors.As(err, &unavailable) {
		t.Fatalf("unavailable classification = %T %v", err, err)
	}
	if _, statErr := os.Lstat(missingParent); !os.IsNotExist(statErr) {
		t.Fatalf("unavailable launcher mutated filesystem: %v", statErr)
	}
}

func TestEnsureRuntimeDiscoversExecutableWithoutLegacyState(t *testing.T) {
	previous := queryLauncherDaemon
	t.Cleanup(func() { queryLauncherDaemon = previous })
	queryLauncherDaemon = func(_ context.Context) error { return nil }

	root := t.TempDir()
	runtimePath := filepath.Join(root, "agent-session-runtime")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyState := filepath.Join(root, "legacy-state")
	t.Setenv("CODEX_PEER_NATIVE_RUNTIME", runtimePath)
	t.Setenv("CODEX_PEER_PLUGIN_ROOT", filepath.Join(root, "unused-plugin"))
	t.Setenv("CLAUDE_PEER_DATA_DIR", legacyState)
	selected, err := EnsureRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if selected.Path != runtimePath {
		t.Fatalf("runtime path = %q, want %q", selected.Path, runtimePath)
	}
	if _, statErr := os.Lstat(legacyState); !os.IsNotExist(statErr) {
		t.Fatalf("launcher recreated legacy state root: %v", statErr)
	}
}
