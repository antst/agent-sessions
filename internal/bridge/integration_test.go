package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
