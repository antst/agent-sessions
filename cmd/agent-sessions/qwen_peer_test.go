package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/launcher"
)

const testQwenNativeSessionID = "12345678-1234-4234-8234-123456789abc"

func TestQwenLaunchIdentityComesOnlyFromFirstNativeSessionStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if _, ok := qwenLaunchSessionID(path); ok {
		t.Fatal("missing native event stream produced an identity")
	}
	partial := `{"session_id":"` + testQwenNativeSessionID + `","type":"system","subtype":"session_start"}`
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := qwenLaunchSessionID(path); ok {
		t.Fatal("partial native event produced an identity")
	}
	if err := os.WriteFile(path, []byte(partial+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := qwenLaunchSessionID(path); !ok || got != testQwenNativeSessionID {
		t.Fatalf("native identity = %q/%v", got, ok)
	}
	wrongFirst := `{"sessionId":"` + testQwenNativeSessionID + `","type":"system","subtype":"other"}` + "\n" + partial + "\n"
	if err := os.WriteFile(path, []byte(wrongFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := qwenLaunchSessionID(path); ok {
		t.Fatal("a later event replaced the first product selection event")
	}
}

func TestQwenLauncherDeliversOverItsOwnedInput(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(input, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{
		"message_id": "message-1", "body": "hello",
		"from": map[string]any{"uuid": "parent", "name": "parent", "product": "codex", "groups": []string{"team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qwenLauncherLiveCall(context.Background(), input, "message.deliver", params); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("native Qwen input = %q", body)
	}
}

func TestQwenLauncherConfirmsProductIdentityBeforeChildExit(t *testing.T) {
	root := t.TempDir()
	events := filepath.Join(root, "events.jsonl")
	input := filepath.Join(root, "input.jsonl")
	for _, path := range []string{events, input} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "qwen")
	body := "#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"" + testQwenNativeSessionID + "\",\"type\":\"system\",\"subtype\":\"session_start\"}' >> \"$" + launcher.QwenEventsFileEnv + "\"\nsleep 0.1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	err := runQwenNativePeer(context.Background(), launcher.QwenNativeLaunch{
		Executable: script, Environment: []string{launcher.QwenEventsFileEnv + "=" + events},
		Cwd: root, QwenHome: filepath.Join(root, "qwen-home"), InputPath: input, EventsPath: events,
		Groups: []string{"project"},
	})
	if err != nil {
		t.Fatal(err)
	}
}
