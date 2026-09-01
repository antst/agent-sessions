package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestHookEventsAreSilentWhenDaemonIsUnavailable(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", stateRoot)

	store, err := daemonpkg.OpenState(stateRoot, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-4000-8000-00000000babe"
	if _, err := store.Commit(0, daemonpkg.Catalog{
		Attachments: map[string]daemonpkg.ManagedAttachment{
			threadID: {
				ID: threadID, Product: "codex", NativeSessionID: threadID,
				State: "attached", DaemonGeneration: 7,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		t.Run(event, func(t *testing.T) {
			input := bytes.NewBufferString(fmt.Sprintf(
				`{"hook_event_name":%q,"session_id":%q,"cwd":%q}`,
				event, threadID, t.TempDir(),
			))
			var output bytes.Buffer
			if err := runHookInput(context.Background(), "codex", input, &output); err != nil {
				t.Fatalf("daemon-unavailable hook error = %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("daemon-unavailable hook output = %q", output.String())
			}
		})
	}
}
