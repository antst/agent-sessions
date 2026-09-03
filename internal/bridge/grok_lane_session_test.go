package bridge

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGrokNativeID = "01a06515-4dd7-7fe3-b0fa-63749ce9e1c7"

func TestGrokNativePrimaryUsesProductNamingAndSelection(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "grok")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$GROK_FAKE_TEST_BINARY\" -test.run='^TestGrokFakeProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "requests.jsonl")
	environment := append(os.Environ(),
		"AGENT_SESSIONS_GROK_FAKE_PROCESS=1",
		"GROK_FAKE_TEST_BINARY="+testBinary,
		"GROK_FAKE_RECORD="+record,
		"GROK_FAKE_GENERATED_SESSION_ID="+testGrokNativeID,
		"GROK_FAKE_YOLO=1",
	)
	primary, err := OpenGrokNativePrimary(
		context.Background(), wrapper, root, filepath.Join(root, "leader.sock"), environment, io.Discard, []string{"--tools", "shell"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	nativeID, err := primary.OpenSession(context.Background(), root, map[string]any{
		"name": "agent_sessions", "command": "/usr/bin/agent-sessions", "args": []any{"connector", "grok"}, "env": []any{},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if nativeID != testGrokNativeID {
		t.Fatalf("native id = %q", nativeID)
	}
	if err := primary.SetModel(context.Background(), "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if err := primary.SetMode(context.Background(), "medium"); err != nil {
		t.Fatal(err)
	}
	prompt, err := primary.StartPrompt(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	result, err := prompt.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "fake Grok lane answer" || result.StopReason != "end_turn" {
		t.Fatalf("result = %#v", result)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	requests := string(body)
	for _, required := range []string{
		`"method":"session/new"`, `"method":"session/set_model"`,
		`"method":"session/set_mode"`, `"method":"session/prompt"`,
		`"--yolo"`, `"--tools"`,
	} {
		if !strings.Contains(requests, required) {
			t.Fatalf("record missing %s: %s", required, requests)
		}
	}
}
