package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHookDaemonParity freezes the actual event surfaces exposed by each
// vendor. Codex owns daemon-wide hooks; Claude refreshes through its native
// registry, Grok through ACP roster generations, and Qwen through dual-output
// and lifecycle evidence. The products are deliberately not forced into an
// invented common hook model.
func TestHookDaemonParity(t *testing.T) {
	t.Run("codex_bare_and_managed_events", testCodexHookEventParity)
	t.Run("native_product_event_contracts", func(t *testing.T) {
		runLegacyParityReferences(t, legacyParityRepositoryRoot(t), []string{
			"internal/bridge/peer_authorization_test.go:TestOrdinaryHooksAreSilentAndDoNotActivateSupervisor",
			"internal/bridge/peer_authorization_test.go:TestOrdinaryHookCommandWritesNoOutput",
			"internal/bridge/peer_authorization_test.go:TestUserPromptLateAttachesPreparedCodex0148OwnerWithDistinctSessionFamily",
			"internal/bridge/peer_authorization_test.go:TestHookRejectsMismatchedSupervisorWithoutRestart",
			"internal/bridge/launch_test.go:TestPreparedLaunchRollsBackNameFailure",
			"internal/launcher/claude_peer_test.go:TestReadClaudeNativePeerRecordRequiresExactLiveIdentityAndSocket",
			"internal/launcher/claude_peer_test.go:TestClaudePeerSharedRegistryRegistersAndRestoresPreferences",
			"internal/bridge/grok_test.go:TestGrokHostRefreshesRuntimePermissionMode",
			"internal/bridge/grok_test.go:TestGrokRosterPushWinsOneReconciliationWindowAndOldGenerationIsIgnored",
			"internal/bridge/grok_test.go:TestGrokHostPermissionPublishFailureRemainsDirtyAndRetries",
			"internal/bridge/qwen_test.go:TestQwenDualOutputAdmissionRequiresExactFirstSessionStart",
			"internal/bridge/qwen_test.go:TestQwenInputWriterAppendsOneCompleteSubmitAndAttestsBodyCursor",
			"internal/bridge/qwen_cleanup_test.go:TestQwenCleanupRejectsRecycledLifecycleIdentity",
			"internal/federator/qwen_registration_test.go:TestQwenPreparedLaunchRollbackRestoresCatalog",
		})
	})
}

func testCodexHookEventParity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("AGENT_SESSIONS_SUPERVISOR_SOCKET", filepath.Join(root, "run", "absent-supervisor.sock"))

	threadID := "00000000-0000-0000-0000-00000000d017"
	paths := resolveNativePaths()
	writeTestAttachedInteractiveOwner(t, paths, threadID)
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "hook-parity", "name-source": "explicit",
		"data-dir": paths.dataRoot, "claude-config-dir": paths.claudeRoot,
		"codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
		"owner-pid": strconv.Itoa(os.Getpid()), "owner-proc-start": readProcStart(os.Getpid()),
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shim.shutdown()
		time.Sleep(25 * time.Millisecond)
	})

	start, err := handleNativeHook(hookInput{Event: "SessionStart", SessionID: threadID, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	startSpecific, _ := start["hookSpecificOutput"].(map[string]any)
	if stringValue(startSpecific["hookEventName"]) != "SessionStart" ||
		!strings.Contains(stringValue(startSpecific["additionalContext"]), "peer messaging is active") {
		t.Fatalf("managed SessionStart output = %#v", start)
	}

	if err := enqueueNativeInboxItem(paths, threadID, map[string]any{
		"type": "message", "id": "hook-prompt", "message": "PROMPT_EVENT_MESSAGE",
	}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	prompt, err := handleNativeHook(hookInput{
		Event: "UserPromptSubmit", SessionID: threadID, Cwd: root, PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatal(err)
	}
	promptSpecific, _ := prompt["hookSpecificOutput"].(map[string]any)
	if stringValue(promptSpecific["hookEventName"]) != "UserPromptSubmit" ||
		!strings.Contains(stringValue(promptSpecific["additionalContext"]), "PROMPT_EVENT_MESSAGE") {
		t.Fatalf("managed UserPromptSubmit output = %#v", prompt)
	}
	assertHookParityState(t, shim.stateFile, "busy", "bypassPermissions")

	if err := enqueueNativeInboxItem(paths, threadID, map[string]any{
		"type": "message", "id": "hook-stop", "message": "STOP_EVENT_MESSAGE",
	}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	stop, err := handleNativeHook(hookInput{
		Event: "Stop", SessionID: threadID, Cwd: root, PermissionMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(stop["decision"]) != "block" || !strings.Contains(stringValue(stop["reason"]), "STOP_EVENT_MESSAGE") {
		t.Fatalf("managed Stop output = %#v", stop)
	}
	assertHookParityState(t, shim.stateFile, "idle", "default")
}

func assertHookParityState(t *testing.T, statePath, status, permission string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state := readJSONMap(statePath)
		if stringValue(state["status"]) == status && stringValue(state["permissionMode"]) == permission {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hook state = %#v, want status=%s permission=%s", readJSONMap(statePath), status, permission)
}
