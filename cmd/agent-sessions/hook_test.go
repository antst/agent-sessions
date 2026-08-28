package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltCanonicalHostDispatchesEveryShippedCodexHookEvent(t *testing.T) {
	repository := filepath.Join("..", "..")
	hooksBody, err := os.ReadFile(filepath.Join(repository, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	const shippedCommand = `"command": "\"$PLUGIN_ROOT/scripts/native-entry\" hook"`
	if got := strings.Count(string(hooksBody), shippedCommand); got != 3 {
		t.Fatalf("shipped canonical hook command count = %d, want 3", got)
	}

	root := t.TempDir()
	binary := filepath.Join(root, "agent-sessions")
	build := exec.Command("go", "build", "-o", binary, "./cmd/agent-sessions")
	build.Dir = repository
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build canonical agent-sessions: %v: %s", buildErr, output)
	}
	codexHome := filepath.Join(root, "codex")
	threadID := "00000000-0000-0000-0000-000000000901"
	sessionID := "00000000-0000-0000-0000-000000000902"
	transcriptDir := filepath.Join(codexHome, "sessions", "2026", "08", "27")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(transcriptDir, "rollout-test-"+threadID+".jsonl")
	metadata, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-27T00:00:00Z", "type": "session_meta",
		"payload": map[string]any{"id": threadID, "session_id": sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, append(metadata, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	environment := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "AGENT_SESSIONS_INTERNAL_") &&
			!strings.HasPrefix(entry, "CODEX_HOME=") &&
			!strings.HasPrefix(entry, "CLAUDE_PEER_DATA_DIR=") &&
			!strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment,
		"AGENT_SESSIONS_HOST_BINARY="+binary,
		"CODEX_HOME="+codexHome,
		"CLAUDE_PEER_DATA_DIR="+filepath.Join(root, "legacy-state"),
		"XDG_RUNTIME_DIR="+filepath.Join(root, "runtime"),
	)
	entry := filepath.Join(repository, "scripts", "native-entry")
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		t.Run(event, func(t *testing.T) {
			input, marshalErr := json.Marshal(map[string]any{
				"hook_event_name": event, "session_id": sessionID,
				"transcript_path": transcript, "cwd": root,
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			command := exec.Command(entry, "hook")
			command.Env = environment
			command.Stdin = bytes.NewReader(input)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if runErr := command.Run(); runErr != nil {
				t.Fatalf("canonical %s hook dispatch: %v; stdout=%q stderr=%q", event, runErr, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("ordinary %s hook was not a silent no-op: stdout=%q stderr=%q", event, stdout.String(), stderr.String())
			}
		})
	}
	for _, forbidden := range []string{filepath.Join(root, "legacy-state"), filepath.Join(root, "runtime")} {
		if _, err := os.Lstat(forbidden); !os.IsNotExist(err) {
			t.Fatalf("ordinary canonical hooks created legacy runtime path %s: %v", forbidden, err)
		}
	}
}
