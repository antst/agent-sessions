package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestManagedCodexHookRefreshesDaemonWithoutSupervisorOrShim(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "legacy-state"))
	t.Setenv(daemonpkg.InternalProductEnvironment, "codex")
	t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, "attachment-codex-1")
	t.Setenv(daemonpkg.InternalCapabilityEnvironment, "capability-codex-1")
	threadID := "00000000-0000-0000-0000-000000000901"
	sessionID := "00000000-0000-0000-0000-000000000902"
	t.Setenv(daemonpkg.InternalSessionIDEnvironment, threadID)
	transcript := writeTestCodexHookTranscript(t, nativePaths{codexHome: codexHome}, threadID, sessionID)

	previous := queryDaemonCodexHook
	t.Cleanup(func() { queryDaemonCodexHook = previous })
	queryDaemonCodexHook = func(
		_ context.Context,
		identity daemonpkg.LocalControlIdentity,
		operation string,
		payload any,
	) (daemonpkg.LocalControlResult, error) {
		if identity.Role != daemonpkg.LocalControlHook || identity.AttachmentID != "attachment-codex-1" ||
			identity.SessionID != threadID || identity.NativeActor["thread_id"] != threadID {
			t.Fatalf("hook identity = %#v", identity)
		}
		refresh, ok := payload.(daemonpkg.AttachmentRefreshRequest)
		if operation != "attachment.refresh" || !ok || refresh.SessionID != threadID {
			t.Fatalf("hook request = %q %#v", operation, payload)
		}
		body, _ := json.Marshal(daemonpkg.AttachmentRecord{AttachmentID: identity.AttachmentID, SessionID: threadID, Name: "managed-codex"})
		return daemonpkg.LocalControlResult{Generation: 7, Result: body}, nil
	}

	output, err := handleNativeHook(hookInput{
		Event: "SessionStart", SessionID: sessionID, TranscriptPath: transcript, Cwd: root,
	})
	if err != nil {
		t.Fatalf("managed daemon hook: %v", err)
	}
	specific, _ := output["hookSpecificOutput"].(map[string]any)
	if specific["hookEventName"] != "SessionStart" {
		t.Fatalf("managed hook output = %#v", output)
	}
	paths := resolveNativePaths()
	for _, legacy := range []string{paths.supervisorSock, filepath.Join(paths.dataRoot, "sessions")} {
		if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
			t.Fatalf("managed hook created legacy authority %s: %v", legacy, err)
		}
	}
}

func TestCodexHistoryReadinessDetectsNontrivialBlankProjection(t *testing.T) {
	root := t.TempDir()
	small := filepath.Join(root, "small.jsonl")
	large := filepath.Join(root, "large.jsonl")
	if err := os.WriteFile(small, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, make([]byte, 128*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if !codexThreadHistoryReady(appThread{Path: small}) {
		t.Fatal("fresh zero-turn rollout was rejected")
	}
	if codexThreadHistoryReady(appThread{Path: large}) {
		t.Fatal("nontrivial rollout with no App Server turns was accepted")
	}
	if !codexThreadHistoryReady(appThread{Path: large, Turns: []appTurn{{ID: "turn-1"}}}) {
		t.Fatal("projected rollout was rejected")
	}
}
