package bridge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const shimHelperEnvironment = "CODEX_MESSAGING_SHIM_HELPER"

func TestBridgeShimHelperProcess(_ *testing.T) {
	if os.Getenv(shimHelperEnvironment) != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(2)
	}
	runShimMain(os.Args[separator+1:])
}

func TestBridgeOwnerHelperProcess(_ *testing.T) {
	if os.Getenv("CODEX_MESSAGING_OWNER_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestKilledShimArtifactsAreGarbageCollected(t *testing.T) {
	paths := isolatedNativePaths(t, t.TempDir())
	command, state := startShimHelper(t, paths, "killed-shim-session", 0, "")
	marker := filepath.Join(filepath.Dir(stringValue(state["inboxDir"])), "inbox", "pending", "keep-message.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILLed shim unexpectedly exited successfully")
	}

	for _, path := range bridgeArtifactPaths(state) {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("artifact disappeared before garbage collection: %s: %v", path, err)
		}
	}
	stats := cleanupStaleBridgeArtifacts(paths)
	if stats.StateFiles != 1 || stats.RegistryFiles != 1 || stats.SocketFiles != 2 {
		t.Fatalf("unexpected cleanup stats: %#v", stats)
	}
	for _, path := range bridgeArtifactPaths(state) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale artifact survived garbage collection: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("queued message was removed with transport artifacts: %v", err)
	}
}

func TestDevelopEraTokenlessShimRowsAreGarbageCollectedAfterPIDExit(t *testing.T) {
	paths := isolatedNativePaths(t, t.TempDir())
	runtimeRoot := filepath.Dir(paths.supervisorSock)
	registryRoot := filepath.Join(paths.claudeRoot, "sessions")
	if err := os.MkdirAll(registryRoot, 0700); err != nil {
		t.Fatal(err)
	}
	deadPID := 99999990
	sessionID := "00000000-0000-0000-0000-000000000090"
	key := sessionKey(sessionID)
	statePath := filepath.Join(paths.dataRoot, "sessions", key, "state.json")
	registryPath := filepath.Join(registryRoot, strconv.Itoa(deadPID)+".json")
	stable := filepath.Join(runtimeRoot, "session-"+key+".sock")
	backend := filepath.Join(runtimeRoot, strconv.Itoa(deadPID)+".sock")
	if err := writeJSONAtomic(registryPath, map[string]any{
		"pid": deadPID, "procStart": "", "sessionId": sessionID, "entrypoint": "codex",
		"version": "codex-claude-peer/0.1.0", "messagingSocketPath": stable,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(statePath, map[string]any{
		"pid": deadPID, "procStart": "", "sessionId": sessionID,
		"backendSocketPath": backend, "socketPath": stable, "registryFile": registryPath,
	}); err != nil {
		t.Fatal(err)
	}
	stats := cleanupStaleBridgeArtifacts(paths)
	if stats.StateFiles != 1 || stats.RegistryFiles != 1 {
		t.Fatalf("tokenless develop-era rows were not reaped: %#v", stats)
	}
	for _, path := range []string{statePath, registryPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("tokenless stale row survived cleanup: %s: %v", path, err)
		}
	}
}

func TestShimCleansUpWhenItsOwnerIsKilled(t *testing.T) {
	paths := isolatedNativePaths(t, t.TempDir())
	owner := exec.Command(os.Args[0], "-test.run=^TestBridgeOwnerHelperProcess$")
	owner.Env = append(os.Environ(), "CODEX_MESSAGING_OWNER_HELPER=1")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if owner.Process != nil {
			_ = owner.Process.Kill()
		}
	})
	ownerStart := readProcStart(owner.Process.Pid)
	command, state := startShimHelper(t, paths, "owner-death-session", owner.Process.Pid, ownerStart)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shim failed while handling owner death: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("shim did not exit after its owner died")
	}
	for _, path := range bridgeArtifactPaths(state) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("artifact survived graceful owner-death cleanup: %s: %v", path, err)
		}
	}
}

func TestCleanupPreservesNativeClaudeAndUnownedFiles(t *testing.T) {
	paths := isolatedNativePaths(t, t.TempDir())
	runtimeRoot := filepath.Dir(paths.supervisorSock)
	registryRoot := filepath.Join(paths.claudeRoot, "sessions")
	if err := os.MkdirAll(registryRoot, 0700); err != nil {
		t.Fatal(err)
	}
	deadPID := 99999991
	nativeRegistry := filepath.Join(registryRoot, strconv.Itoa(deadPID)+".json")
	if err := writeJSONAtomic(nativeRegistry, map[string]any{
		"pid": deadPID, "entrypoint": "claude", "version": "2.0.0",
		"messagingSocketPath": filepath.Join(runtimeRoot, "native-claude.sock"),
	}); err != nil {
		t.Fatal(err)
	}
	foreignRegistry := filepath.Join(registryRoot, strconv.Itoa(deadPID+1)+".json")
	if err := writeJSONAtomic(foreignRegistry, map[string]any{
		"pid": deadPID + 1, "entrypoint": "codex", "version": "some-other-bridge/1",
		"messagingSocketPath": filepath.Join(runtimeRoot, "foreign.sock"),
	}); err != nil {
		t.Fatal(err)
	}

	sessionID := "unowned-state-session"
	key := sessionKey(sessionID)
	statePath := filepath.Join(paths.dataRoot, "sessions", key, "state.json")
	victim := filepath.Join(paths.dataRoot, "must-survive.json")
	if err := writeJSONAtomic(victim, map[string]any{"important": true}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(statePath, map[string]any{
		"pid": deadPID, "sessionId": sessionID,
		"backendSocketPath": filepath.Join(runtimeRoot, strconv.Itoa(deadPID)+".sock"),
		"socketPath":        filepath.Join(runtimeRoot, "session-"+key+".sock"),
		"registryFile":      victim,
	}); err != nil {
		t.Fatal(err)
	}

	cleanupStaleBridgeArtifacts(paths)
	for _, path := range []string{nativeRegistry, foreignRegistry, statePath, victim} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup touched an unowned file %s: %v", path, err)
		}
	}
}

func TestCleanupRemovesOrphanedBridgeSocketAliases(t *testing.T) {
	paths := isolatedNativePaths(t, t.TempDir())
	runtimeRoot := filepath.Dir(paths.supervisorSock)
	if err := os.MkdirAll(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	deadPID := 99999992
	backend := filepath.Join(runtimeRoot, fmt.Sprintf("%d.sock", deadPID))
	stable := filepath.Join(runtimeRoot, "session-orphan.sock")
	if err := os.WriteFile(backend, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backend, stable); err != nil {
		t.Fatal(err)
	}
	stats := cleanupStaleBridgeArtifacts(paths)
	if stats.SocketFiles != 2 {
		t.Fatalf("unexpected orphan cleanup stats: %#v", stats)
	}
	for _, path := range []string{backend, stable} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan socket survived cleanup: %s: %v", path, err)
		}
	}
}

func TestProcessIdentityTreatsLinuxZombieAsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc process states are required")
	}
	command := exec.Command("sh", "-c", "exit 0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait() //nolint:errcheck
	pid := command.Process.Pid
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if probe := probeProcessIdentity(pid); probe.status == processIdentityProbeKnown && probe.state == "Z" {
			if observeProcessIdentity(pid, "").Status != processIdentityStale {
				t.Fatalf("zombie process %d was classified as live", pid)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child did not enter a zombie state before Wait")
}

func isolatedNativePaths(t *testing.T, root string) nativePaths {
	t.Helper()
	runtimeDir := filepath.Join(os.TempDir(), "cm-"+first8(sessionKey(root)))
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, os.Getuid())
	return nativePaths{
		dataRoot:        filepath.Join(root, "state"),
		claudeRoot:      filepath.Join(root, "claude"),
		codexHome:       filepath.Join(root, "codex"),
		runtimeDir:      runtimeDir,
		supervisorSock:  filepath.Join(runtimeRoot, "supervisor.sock"),
		supervisorState: filepath.Join(root, "state", "supervisor.json"),
		appServerSock:   filepath.Join(root, "codex", "app-server.sock"),
	}
}

func startShimHelper(t *testing.T, paths nativePaths, sessionID string, ownerPID int, ownerStart string) (*exec.Cmd, map[string]any) {
	t.Helper()
	arguments := []string{
		"-test.run=^TestBridgeShimHelperProcess$", "--",
		"--session-id", sessionID,
		"--cwd", filepath.Dir(paths.dataRoot),
		"--data-dir", paths.dataRoot,
		"--claude-config-dir", paths.claudeRoot,
		"--codex-home", paths.codexHome,
		"--runtime-dir", paths.runtimeDir,
		"--heartbeat-ms", "50",
	}
	if ownerPID > 0 {
		arguments = append(arguments, "--owner-pid", strconv.Itoa(ownerPID), "--owner-proc-start", ownerStart)
	}
	command := exec.Command(os.Args[0], arguments...)
	command.Env = append(os.Environ(), shimHelperEnvironment+"=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(sessionID), "state.json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := readJSONMap(statePath)
		if intValue(state["pid"]) == command.Process.Pid &&
			probeUnixSocket(stringValue(state["socketPath"]), 100*time.Millisecond) {
			if _, err := os.Stat(stringValue(state["registryFile"])); err == nil {
				return command, state
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	t.Fatalf("shim helper %d did not become ready", command.Process.Pid)
	return nil, nil
}

func bridgeArtifactPaths(state map[string]any) []string {
	return []string{
		stringValue(state["socketPath"]),
		stringValue(state["backendSocketPath"]),
		stringValue(state["registryFile"]),
		filepath.Join(filepath.Dir(stringValue(state["inboxDir"])), "state.json"),
	}
}
