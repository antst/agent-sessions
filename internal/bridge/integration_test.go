package bridge

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNativeShimInspectionReportsLiveIdentityAndPermission(t *testing.T) {
	root := t.TempDir()
	d := newDaemon(map[string]string{
		"session-id": "grok-inspection-session", "cwd": root, "entrypoint": "grok",
		"permission-mode": "default", "data-dir": filepath.Join(root, "state"),
		"claude-config-dir": filepath.Join(root, "claude"), "codex-home": filepath.Join(root, "codex"),
		"runtime-dir": filepath.Join(root, "run"),
	})
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.shutdown)

	// Simulate an in-session policy change that has not yet been inferred from
	// the TUI's immutable process argv.
	d.mu.Lock()
	d.permissionMode = "bypassPermissions"
	d.mu.Unlock()

	conn, err := net.DialTimeout("unix", d.stableSocket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(map[string]any{"type": "control", "action": "inspect"}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if stringValue(response["type"]) != "peer_inspection" || intValue(response["pid"]) != os.Getpid() ||
		stringValue(response["procStart"]) != d.procStart || stringValue(response["sessionId"]) != d.sessionID ||
		stringValue(response["entrypoint"]) != "grok" || stringValue(response["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("inspection response = %#v", response)
	}
}

func TestNativeShimInitializesPublishedStatus(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "busy", want: "busy"},
		{input: "waiting", want: "waiting"},
		{input: "shell", want: "shell"},
		{input: "invalid", want: "idle"},
		{want: "idle"},
	} {
		d := newDaemon(map[string]string{
			"session-id": "status-" + defaultString(test.input, "empty"), "cwd": root,
			"status": test.input, "data-dir": filepath.Join(root, "state"),
			"claude-config-dir": filepath.Join(root, "claude"), "codex-home": filepath.Join(root, "codex"),
			"runtime-dir": filepath.Join(root, "run"),
		})
		if d.status != test.want {
			t.Fatalf("initial status %q = %q, want %q", test.input, d.status, test.want)
		}
	}
}

func TestNativeShimShutdownCannotBeUndoneBySelectedHeartbeat(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(map[string]string{
		"session-id": "shutdown-heartbeat-race", "cwd": root,
		"owner-pid": strconv.Itoa(os.Getpid()), "owner-proc-start": readProcStart(os.Getpid()),
		"data-dir": filepath.Join(root, "state"), "claude-config-dir": filepath.Join(root, "claude"),
		"codex-home": filepath.Join(root, "codex"), "runtime-dir": runtimeDir, "heartbeat-ms": "20",
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	d.maintenanceBeforeWrite = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		d.shutdown()
		close(release)
		t.Fatal("native shim heartbeat did not reach the blocked writer")
	}

	// The heartbeat has already selected its write path, but has not acquired
	// d.mu. Shutdown must commit the stopped latch and remove both records first.
	d.shutdown()
	close(release)
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, path := range []string{d.stateFile, d.registryFile} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("native shim record was recreated after shutdown at %s: %v", path, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNativeShimPublishesPrivateStablePeerAndQueuesMessage(t *testing.T) {
	root := t.TempDir()
	sessionID := "native-integration-session"
	dataRoot := filepath.Join(root, "state")
	claudeRoot := filepath.Join(root, "claude")
	codexHome := filepath.Join(root, "codex")
	runtimeDir := filepath.Join(root, "run")
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataRoot)
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", claudeRoot)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	d := newDaemon(map[string]string{
		"session-id":        sessionID,
		"cwd":               root,
		"name":              "native integration",
		"name-source":       "explicit",
		"permission-mode":   "bypassPermissions",
		"data-dir":          dataRoot,
		"claude-config-dir": claudeRoot,
		"codex-home":        codexHome,
		"runtime-dir":       runtimeDir,
		"heartbeat-ms":      "50",
	})
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.shutdown)

	state := readJSONMap(d.stateFile)
	if stringValue(state["socketPath"]) != d.stableSocket || stringValue(state["backendSocketPath"]) != d.backendSocket {
		t.Fatalf("unexpected state: %#v", state)
	}
	if target, err := os.Readlink(d.stableSocket); err != nil || target != d.backendSocket {
		t.Fatalf("stable alias = %q, %v", target, err)
	}
	for _, file := range []string{d.backendSocket, d.registryFile, d.stateFile} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("mode of %s = %o", file, got)
		}
	}
	registry := readJSONMap(d.registryFile)
	if stringValue(registry["entrypoint"]) != "codex" || stringValue(registry["name"]) != "native-integration" {
		t.Fatalf("unexpected registry: %#v", registry)
	}
	messageID := "integration-message"
	frame := map[string]any{
		"type": "user", "msg_id": messageID, "from": "uds:/tmp/claude.sock",
		"message": map[string]any{
			"role": "user",
			"content": wrapNativePeerMessage(
				"uds:/tmp/claude.sock", "claude-session", "peer-a", "bypass",
				messageID, "2026-08-09T18:00:00Z", "INTEGRATION_MESSAGE",
			),
		},
	}
	if err := sendUnixJSON(d.stableSocket, frame, time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(d.pendingDir)
		if len(entries) == 1 {
			body, err := os.ReadFile(filepath.Join(d.pendingDir, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			var item map[string]any
			if json.Unmarshal(body, &item) != nil || stringValue(item["message"]) != "INTEGRATION_MESSAGE" || stringValue(item["id"]) != messageID {
				t.Fatalf("unexpected inbox item: %s", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message did not reach native inbox")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := handleNativeHook(hookInput{Event: "SessionEnd", SessionID: sessionID, Cwd: root}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeShimNameSourcesPreserveExplicitAndFollowCodexTitles(t *testing.T) {
	root := t.TempDir()
	sessionID := "native-name-session"
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(codexHome, "session_index.jsonl")
	if err := os.WriteFile(index, []byte("{\"id\":\"native-name-session\",\"thread_name\":\"first title\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(map[string]string{
		"session-id": sessionID, "cwd": root, "name": "fallback", "name-source": "generated",
		"data-dir": filepath.Join(root, "state"), "claude-config-dir": filepath.Join(root, "claude"),
		"codex-home": codexHome, "runtime-dir": filepath.Join(root, "run"),
	})
	d.mu.Lock()
	d.refreshNameLocked()
	if d.name != "first-title" || d.nameSource != "codex" {
		t.Fatalf("first title = %q (%s)", d.name, d.nameSource)
	}
	d.applyNameLocked("wrapper name", "launch")
	d.applyNameLocked("stale codex title", "codex")
	if d.name != "wrapper-name" || d.nameSource != "launch" {
		t.Fatalf("launch name was overwritten: %q (%s)", d.name, d.nameSource)
	}
	d.applyNameLocked("canonical name", "canonical")
	if d.name != "canonical-name" || d.nameSource != "canonical" {
		t.Fatalf("canonical name = %q (%s)", d.name, d.nameSource)
	}
	d.applyNameLocked("lane name", "lane")
	d.applyNameLocked("stale thread title", "codex")
	d.refreshNameLocked()
	if d.name != "lane-name" || d.nameSource != "lane" {
		t.Fatalf("lane name was overwritten: %q (%s)", d.name, d.nameSource)
	}
	d.mu.Unlock()
}
