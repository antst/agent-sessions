package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func withCurrentTestLaneOwner(options laneOptions) laneOptions {
	options.ownerPID = os.Getpid()
	options.ownerProcStart = readProcStart(os.Getpid())
	return options
}

func writeTestActiveCodexLane(t *testing.T, paths nativePaths, threadID string) {
	t.Helper()
	if err := writeJSONAtomic(laneStatePath(paths, threadID), laneState{
		Type: "codex-peer-lane", ThreadID: threadID, SessionID: threadID,
		Name: "test-lane", Status: "idle", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
}

func writeTestAttachedInteractiveOwner(t *testing.T, paths nativePaths, threadID string) {
	t.Helper()
	if err := writeInteractiveOwnerRecord(paths, interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "test-owner-" + sessionKey(threadID),
		OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLaneResumePreservesPersistentLifecyclePolicy(t *testing.T) {
	state := laneState{
		Persistent: true, OwnerPID: 91, OwnerProcStart: "old", OwnerSessionID: "old-session",
		NotifyTarget: "session:old-session",
	}
	applyLaneLifecycleOptions(&state, laneOptions{laneCommonOptions: laneCommonOptions{ownerPID: 92, ownerProcStart: "new"}})
	if !state.Persistent || state.OwnerPID != 0 || state.OwnerProcStart != "" ||
		state.OwnerSessionID != "" || state.NotifyTarget != "session:old-session" {
		t.Fatalf("implicit resume changed persistent lifecycle = %+v", state)
	}
	applyLaneLifecycleOptions(&state, laneOptions{laneCommonOptions: laneCommonOptions{persistent: true, persistentSet: true}})
	if !state.Persistent || state.OwnerPID != 0 || state.OwnerProcStart != "" || state.OwnerSessionID != "" {
		t.Fatalf("persistent resume lifecycle = %+v", state)
	}
}

func TestLaneFeatureOptionsAreExplicitAndResumeTargetsExistingThread(t *testing.T) {
	options, err := parseLaneArgs([]string{
		"start", "--name", "schema-lane", "--persistent", "--notify", "orchestrator", "--schema", "result.schema.json", "--worktree", "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.notifyTarget != "orchestrator" || options.schemaFile != "result.schema.json" || !options.worktree {
		t.Fatalf("feature options = %+v", options)
	}
	resumed, err := parseLaneArgs([]string{"resume", "schema-lane", "--persistent", "--notify", "orchestrator", "-"})
	if err != nil || resumed.target != "schema-lane" || resumed.command != "resume" {
		t.Fatalf("resume options = %+v, %v", resumed, err)
	}
}

func TestPrivateRuntimeDirectoryRejectsSymlinkAndRepairsMode(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(parent, "victim")
	if err := os.Mkdir(victim, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "ccp-1000")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateRuntimeDir(link); err == nil {
		t.Fatal("attacker-controlled runtime symlink was accepted")
	}
	owned := filepath.Join(parent, "owned")
	if err := os.Mkdir(owned, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateRuntimeDir(owned); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(owned)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private runtime mode = %v, err = %v", info.Mode().Perm(), err)
	}
}

func TestStableSessionSocketRefusesSameNameSocketAndSymlinkSubstitution(t *testing.T) {
	root := shortSocketTestRoot(t, "ss-")
	path := filepath.Join(root, "session-collision.sock")
	target := filepath.Join(root, "unrelated-target")
	if err := os.WriteFile(target, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenPrivateSessionSocket(path); err == nil {
		_ = listener.Close()
		t.Fatal("same-name symlink was replaced by a stable session socket")
	}
	if destination, err := os.Readlink(path); err != nil || destination != target {
		t.Fatalf("same-name symlink changed: target=%q err=%v", destination, err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "preserve\n" {
		t.Fatalf("symlink target changed: body=%q err=%v", body, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	existing, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = existing.Close() })
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if listener, listenErr := listenPrivateSessionSocket(path); listenErr == nil {
		_ = listener.Close()
		t.Fatal("same-name live socket was replaced by a stable session socket")
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSocket == 0 || !os.SameFile(before, after) {
		t.Fatalf("same-name live socket changed: before=%v after=%v err=%v", before, after, err)
	}
}

func TestNativeProfilesUseDistinctSupervisorState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", "")
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", "")
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-a"))
	first := resolveNativePaths()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-b"))
	second := resolveNativePaths()
	if first.profileKey == second.profileKey || first.supervisorSock == second.supervisorSock || first.supervisorState == second.supervisorState {
		t.Fatalf("profiles share supervisor state: first=%+v second=%+v", first, second)
	}
	if !strings.Contains(first.supervisorSock, first.profileKey) || !strings.Contains(second.supervisorState, second.profileKey) {
		t.Fatal("profile key is not reflected in supervisor paths")
	}
}

func TestHookInboxLeavesOverflowQueued(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root}
	sessionID := "overflow-session"
	pending := filepath.Join(root, "sessions", sessionKey(sessionID), "inbox", "pending")
	if err := writeJSONAtomic(filepath.Join(pending, "001-small.json"), map[string]any{"message": "small", "from": "peer"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(pending, "002-large.json"), map[string]any{"message": strings.Repeat("x", 10_000), "from": "peer"}); err != nil {
		t.Fatal(err)
	}
	messages, queued, err := consumeNativeInboxLimited(paths, sessionID, hookAdditionalContextLimit)
	if err != nil || len(messages) != 1 || !queued {
		t.Fatalf("limited consume = %d messages queued=%v err=%v", len(messages), queued, err)
	}
	if _, err := os.Stat(filepath.Join(pending, "001-small.json")); !os.IsNotExist(err) {
		t.Fatalf("acknowledged message remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pending, "002-large.json")); err != nil {
		t.Fatalf("overflow message was lost: %v", err)
	}
	remainder, err := consumeNativeInbox(paths, sessionID)
	if err != nil || len(remainder) != 1 || len(stringValue(remainder[0]["message"])) != 10_000 {
		t.Fatalf("full inbox did not recover overflow: %#v, %v", remainder, err)
	}
}

func TestPersistLaneWaitStatePreservesSupervisorTimeoutOutcome(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	threadID := "thread-timeout-merge"
	latest := laneState{
		Type: "codex-peer-lane", ThreadID: threadID, SessionID: threadID,
		Name: "timeout-merge", Status: "interrupted", TurnID: "turn-timeout",
		TimedOutTurnID: "turn-timeout", TerminalOutcome: "timed_out", DeadlineAt: 1234,
	}
	if err := recordLaneState(paths, latest); err != nil {
		t.Fatal(err)
	}
	staleCollector := latest
	staleCollector.TimedOutTurnID = ""
	staleCollector.TerminalOutcome = ""
	staleCollector.DeadlineAt = 0
	staleCollector.Status = "interrupted"
	staleCollector.CollectedTurnID = staleCollector.TurnID
	if err := persistLaneWaitState(paths, staleCollector, true); err != nil {
		t.Fatal(err)
	}
	got, err := readLaneStateFile(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.TerminalOutcome != "timed_out" || got.TimedOutTurnID != "turn-timeout" || got.DeadlineAt != 1234 || got.CollectedTurnID != "turn-timeout" {
		t.Fatalf("durable timeout was overwritten by stale collector: %+v", got)
	}
}

func TestPersistLaneWaitStatePreservesArchivedLifecycle(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	threadID := "thread-archived-merge"
	latest := laneState{
		Type: "codex-peer-lane", ThreadID: threadID, SessionID: threadID, Name: "archived-merge",
		Status: "archived", TurnID: "turn-final", LatestTurnID: "turn-final",
		PendingTurnIDs: []string{"turn-final"}, PendingQueueVer: 1, AutoArchive: true,
	}
	if err := recordLaneState(paths, latest); err != nil {
		t.Fatal(err)
	}
	if err := markRetiredThread(paths, threadID); err != nil {
		t.Fatal(err)
	}
	collector := latest
	collector.Status = "completed"
	collector.CollectedTurnID = "turn-final"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	got, err := readLaneStateFile(paths, threadID)
	if err != nil || got.Status != "archived" || got.AutoArchiveAt != 0 || got.CollectedTurnID != "turn-final" || len(got.PendingTurnIDs) != 0 {
		t.Fatalf("stale collector corrupted archived lifecycle while acknowledging: %+v, %v", got, err)
	}
}

func TestLaneItemDeduplicatesProviderAndProjectionIDs(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	seen := map[string]bool{}
	provider := json.RawMessage(`{"id":"provider-abc","type":"agentMessage","phase":"final_answer","text":"RESULT"}`)
	projected := json.RawMessage(`{"id":"item-7","type":"agentMessage","phase":"final_answer","text":"RESULT"}`)
	secondProvider := json.RawMessage(`{"id":"provider-def","type":"agentMessage","phase":"final_answer","text":"RESULT"}`)
	for _, item := range []json.RawMessage{provider, projected, secondProvider} {
		if err := emitLaneItem(item, "turn-dedupe", seen); err != nil {
			t.Fatal(err)
		}
	}
	_ = write.Close()
	os.Stdout = original
	body, _ := io.ReadAll(read)
	_ = read.Close()
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("cross-namespace duplicate was not collapsed without preserving distinct providers: %s", body)
	}
}

func TestBoundedLanePromptRejectsRatherThanTruncates(t *testing.T) {
	if _, err := readBoundedLanePrompt(strings.NewReader("12345"), 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized prompt error = %v", err)
	}
	body, err := readBoundedLanePrompt(strings.NewReader("1234"), 4)
	if err != nil || string(body) != "1234" {
		t.Fatalf("boundary prompt = %q, %v", body, err)
	}
}

func TestSessionScopedPeerToolsRejectForeignCallerIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	callerID := "00000000-0000-0000-0000-000000000211"
	foreignID := "00000000-0000-0000-0000-000000000212"
	writeTestAttachedInteractiveOwner(t, resolveNativePaths(), callerID)
	_, err := callNativePeerTool("identity", map[string]any{"session_id": foreignID}, callerID)
	if err == nil || !strings.Contains(err.Error(), "cannot act as") {
		t.Fatalf("foreign session id was accepted: %v", err)
	}
	_, err = callNativePeerTool("identity", map[string]any{"session_id": callerID}, "")
	if err == nil || !strings.Contains(err.Error(), "inactive outside an attested peer session") {
		t.Fatalf("inactive trusted-local session was accepted: %v", err)
	}
}

func TestInteractivePeerToolUsesAttestedInteractiveOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", root)
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	sessionID := "00000000-0000-0000-0000-000000000213"
	socket := filepath.Join(root, "peer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	statePath := filepath.Join(root, "sessions", sessionKey(sessionID), "state.json")
	if err := writeJSONAtomic(statePath, map[string]any{
		"sessionId": sessionID, "name": "trusted-local-peer", "socketPath": socket,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestAttachedInteractiveOwner(t, resolveNativePaths(), sessionID)
	result, err := callNativePeerTool("identity", map[string]any{
		"session_id": sessionID,
	}, sessionID)
	if err != nil || !strings.Contains(stringValue(result["text"]), "trusted-local-peer") {
		t.Fatalf("trusted-local identity = %#v, %v", result, err)
	}
}

func TestLaneSetupArchivesThreadWhenNamingFails(t *testing.T) {
	useBridgeTestAgent(t)
	archived := make(chan string, 1)
	fake, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": "thread-name-fail", "sessionId": "thread-name-fail", "cwd": t.TempDir()}}, nil
		case "thread/name/set":
			return nil, errors.New("name rejected")
		case "thread/archive":
			params := request["params"].(map[string]any)
			archived <- stringValue(params["threadId"])
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "missing-supervisor.sock"))
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("brief"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{name: "name-fail", cwd: root, promptFile: prompt}}), false); err == nil {
		t.Fatal("name failure unexpectedly succeeded")
	}
	select {
	case got := <-archived:
		if got != "thread-name-fail" {
			t.Fatalf("archived thread = %q", got)
		}
	case <-time.After(time.Second):
		methods := []string{}
		for len(fake.requests) > 0 {
			methods = append(methods, stringValue((<-fake.requests)["method"]))
		}
		t.Fatalf("persistent thread was not archived after setup failure; methods=%v", methods)
	}
}

func TestLaneSetupRetainsManageableStateWhenRollbackArchiveFails(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	threadID := "thread-rollback-fail"
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID, "sessionId": threadID, "cwd": root}}, nil
		case "thread/name/set":
			return nil, errors.New("name rejected")
		case "thread/archive":
			return nil, errors.New("archive unavailable")
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "missing-supervisor.sock"))
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("brief"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{name: "recoverable-setup", cwd: root, promptFile: prompt}}), false); err == nil {
		t.Fatal("name failure unexpectedly succeeded")
	}
	state, err := resolveLaneState(resolveNativePaths(), "recoverable-setup")
	if err != nil || state.ThreadID != threadID || state.Status != "failed" {
		t.Fatalf("failed rollback state = %+v, %v", state, err)
	}
}

func TestLaneSetupDeletesUnmaterializedThreadWhenRollbackFindsNoRollout(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000041"
	deleted := make(chan string, 1)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID, "sessionId": threadID, "cwd": root}}, nil
		case "thread/name/set":
			return nil, errors.New("name rejected")
		case "thread/archive":
			return nil, errors.New("no rollout found for thread id " + threadID)
		case "thread/delete":
			params := request["params"].(map[string]any)
			deleted <- stringValue(params["threadId"])
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "missing-supervisor.sock"))
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("brief"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{name: "no-rollout", cwd: root, promptFile: prompt}}), false); err == nil {
		t.Fatal("name failure unexpectedly succeeded")
	}
	select {
	case got := <-deleted:
		if got != threadID {
			t.Fatalf("deleted thread = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("unmaterialized lane thread was not deleted")
	}
	paths := resolveNativePaths()
	if _, err := os.Stat(laneStatePath(paths, threadID)); !os.IsNotExist(err) {
		t.Fatalf("unmaterialized lane retained active state: %v", err)
	}
	if !readRetiredThreads(paths)[threadID] {
		t.Fatal("unmaterialized lane deletion did not persist retirement")
	}
}

func TestFailedLaneStartRemovesProvisionalWorktree(t *testing.T) {
	useBridgeTestAgent(t)
	repository := t.TempDir()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked"}, {"-c", "commit.gpgsign=false", "commit", "-m", "base"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("brief"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startLaneNative(withCurrentTestLaneOwner(laneOptions{
		laneCommonOptions: laneCommonOptions{name: "duplicate", cwd: repository, promptFile: prompt}, worktree: true,
	}), false); err == nil {
		t.Fatal("lane start unexpectedly succeeded without an App Server")
	}
	output, err := exec.Command("git", "-C", repository, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(output), "worktree ") != 1 {
		t.Fatalf("failed lane left a registered worktree:\n%s", output)
	}
}

func TestWorktreeCreationCleansMissingSourceSubdirectory(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(filepath.Join(repository, "untracked-subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, command...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", command, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"add", "tracked"}, {"-c", "commit.gpgsign=false", "commit", "-qm", "initial"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, command...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", command, err, output)
		}
	}
	paths := nativePaths{profileRoot: filepath.Join(root, "state")}
	if _, err := createLaneWorktree(paths, "missing-subdir", filepath.Join(repository, "untracked-subdir")); err == nil {
		t.Fatal("worktree unexpectedly accepted a cwd absent from HEAD")
	}
	output, err := exec.Command("git", "-C", repository, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(output), "worktree "); count != 1 {
		t.Fatalf("failed worktree remained registered (%d entries):\n%s", count, output)
	}
}

func TestWaitReloadsDurableTimeoutBeforeTerminalClassification(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	state := laneState{Type: "codex-peer-lane", ThreadID: "thread-timeout-live", SessionID: "thread-timeout-live", TurnID: "turn-timeout-live", Status: "in_progress"}
	latest := state
	latest.Status = "interrupted"
	latest.TimedOutTurnID = latest.TurnID
	latest.TerminalOutcome = "timed_out"
	if err := recordLaneState(paths, latest); err != nil {
		t.Fatal(err)
	}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{"id": state.TurnID, "status": "interrupted", "durationMs": 1}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	status, code, collected, err := waitForLaneTurnWithPolicy(client, &state, time.Second, false, false)
	if err != nil || status != "timed_out" || code != 124 || !collected {
		t.Fatalf("durable timeout classification = status %q code %d collected=%v err=%v", status, code, collected, err)
	}
}

func TestRunReleasesLaneNameLockBeforeWaitingForTurn(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "thread/name/set", "turn/interrupt":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": "thread-parallel", "sessionId": "thread-parallel", "cwd": root}}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-parallel", "status": "inProgress"}}, nil
		case "thread/turns/list":
			waitingOnce.Do(func() { close(waiting) })
			return map[string]any{"data": []map[string]any{{"id": "turn-parallel", "status": "inProgress"}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	supervisorSocket := filepath.Join(root, "supervisor.sock")
	listener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				var request map[string]any
				_ = json.Unmarshal(line, &request)
				response := `{"ok":true}`
				if stringValue(request["action"]) == "register" {
					response = `{"ok":true,"state":{"socketPath":"/tmp/parallel.sock"}}`
				}
				_, _ = connection.Write([]byte(response + "\n"))
			}()
		}
	}()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("wait"), 0600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		_, _ = startLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{name: "parallel", cwd: root, promptFile: prompt, timeout: 800 * time.Millisecond}}), true)
		close(finished)
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("lane never entered collection wait")
	}
	paths := resolveNativePaths()
	acquired := make(chan struct{})
	go func() {
		lock, lockErr := lockLaneNames(paths)
		if lockErr == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			close(acquired)
		}
	}()
	lockWasReleased := false
	select {
	case <-acquired:
		lockWasReleased = true
	case <-time.After(300 * time.Millisecond):
	}
	<-finished
	if !lockWasReleased {
		t.Fatal("run held the global lane-name lock while waiting for the model turn")
	}
}

func TestResumeReleasesLaneNameLockBeforeWaitingForTurn(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000338"
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "thread/name/set", "turn/interrupt":
			return map[string]any{}, nil
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "sessionId": threadID, "name": "resume-parallel", "cwd": root,
			}}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-resume-parallel", "status": "inProgress"}}, nil
		case "thread/turns/list":
			waitingOnce.Do(func() { close(waiting) })
			return map[string]any{"data": []map[string]any{{"id": "turn-resume-parallel", "status": "inProgress"}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	supervisorSocket := filepath.Join(root, "supervisor.sock")
	listener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				_, _ = bufio.NewReader(connection).ReadBytes('\n')
				_, _ = connection.Write([]byte(`{"ok":true,"state":{"socketPath":"/tmp/resume-parallel.sock"}}` + "\n"))
			}()
		}
	}()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	if err := recordLaneState(paths, laneState{
		Type: "codex-peer-lane", Name: "resume-parallel", ThreadID: threadID,
		SessionID: threadID, Cwd: root, Status: "completed", TurnID: "turn-before-resume",
	}); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(root, "prompt-resume")
	if err := os.WriteFile(prompt, []byte("resume"), 0600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		_, _ = resumeLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{target: threadID, promptFile: prompt, timeout: 800 * time.Millisecond}}))
		close(finished)
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("resume never reached turn wait")
	}
	acquired := make(chan *os.File, 1)
	go func() {
		lock, _ := lockLaneNames(paths)
		acquired <- lock
	}()
	select {
	case lock := <-acquired:
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	case <-time.After(300 * time.Millisecond):
		t.Fatal("resume held the lane-name lock while waiting for the model turn")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("resume test did not finish")
	}
}

func TestWakeLedgerMakesRetriesIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	var starts atomic.Int32
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "00000000-0000-0000-0000-000000000111", "status": map[string]any{"type": "idle"}}}, nil
		case "turn/start":
			starts.Add(1)
			return map[string]any{"turn": map[string]any{"id": "turn-wake-once", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	supervisor, err := newNativeSupervisor("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		supervisor.clientMu.Lock()
		if supervisor.client != nil {
			supervisor.client.close()
		}
		supervisor.clientMu.Unlock()
	})
	threadID := "00000000-0000-0000-0000-000000000111"
	writeTestActiveCodexLane(t, supervisor.paths, threadID)
	item := map[string]any{"id": "wake-id-once", "message": "hello", "from": "peer", "receivedAt": "first"}
	first, err := supervisor.queueWake(threadID, item)
	if err != nil || first != "accepted" {
		t.Fatalf("first wake = %q, %v", first, err)
	}
	retry := map[string]any{"id": "wake-id-once", "message": "hello", "from": "peer", "receivedAt": "second"}
	second, err := supervisor.queueWake(threadID, retry)
	if err != nil || !containsString([]string{"accepted", "in_flight", "started", "delivered"}, second) {
		t.Fatalf("retried wake = %q, %v", second, err)
	}
	conflict, err := supervisor.queueWake(threadID, map[string]any{
		"id": "wake-id-once", "message": "changed payload", "from": "peer",
	})
	if err != nil || conflict != "conflict" {
		t.Fatalf("conflicting transport-id reuse = %q, %v", conflict, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		record := readWakeRecord(supervisor.paths, threadID, "wake-id-once")
		if record != nil && record.State == "delivered" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatalf("same wake started %d turns", starts.Load())
	}
	record := readWakeRecord(supervisor.paths, threadID, "wake-id-once")
	if record == nil || record.State != "delivered" {
		t.Fatalf("wake record = %+v", record)
	}
}

func TestSlowWakeIsJournaledBeforeAppServerDelivery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	threadID := "00000000-0000-0000-0000-000000000119"
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "status": map[string]any{"type": "idle"}}}, nil
		case "turn/start":
			started <- struct{}{}
			<-release
			return map[string]any{"turn": map[string]any{"id": "turn-slow", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	supervisor, err := newNativeSupervisor("test")
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"id": "wake-id-slow", "message": "slow wake", "from": "peer"}
	writeTestActiveCodexLane(t, supervisor.paths, threadID)
	begin := time.Now()
	delivery, err := supervisor.queueWake(threadID, item)
	if err != nil || delivery != "accepted" {
		t.Fatalf("queue slow wake = %q, %v", delivery, err)
	}
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("wake acknowledgement waited for App Server: %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow App Server mutation never started")
	}
	if messages, err := consumeNativeInbox(supervisor.paths, threadID); err != nil || len(messages) != 0 {
		t.Fatalf("in-flight wake was also queued: %#v, %v", messages, err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record := readWakeRecord(supervisor.paths, threadID, stringValue(item["id"]))
		if record != nil && record.State == "delivered" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("slow wake never reached delivered state")
}

func TestWakeFallbackIsDeduplicatedAcrossShimAndSupervisor(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state")}
	threadID := "00000000-0000-0000-0000-000000000120"
	item := map[string]any{"id": "wake-cross-owner", "message": "one fallback"}
	if err := enqueueNativeInboxItem(paths, threadID, item, 1); err != nil {
		t.Fatal(err)
	}
	if err := enqueueNativeInboxItem(paths, threadID, item, 2); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "inbox", "pending")
	entries, err := os.ReadDir(pending)
	if err != nil || len(entries) != 1 {
		t.Fatalf("fallback entries = %d, %v", len(entries), err)
	}
}

func TestFailedWakeQueuesExactlyOnceAndSurvivesShimRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "00000000-0000-0000-0000-000000000222", "status": map[string]any{"type": "idle"}}}, nil
		case "turn/start":
			return nil, errors.New("start failed")
		case "thread/turns/list":
			return map[string]any{"data": []any{}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	supervisor, err := newNativeSupervisor("test")
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-000000000222"
	writeTestActiveCodexLane(t, supervisor.paths, threadID)
	item := map[string]any{"id": "wake-id-fallback", "message": "fallback", "from": "peer"}
	if _, err := supervisor.queueWake(threadID, item); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		record := readWakeRecord(supervisor.paths, threadID, "wake-id-fallback")
		if record != nil && record.State == "queued" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := supervisor.queueWake(threadID, item); err != nil {
		t.Fatal(err)
	}
	messages, err := consumeNativeInbox(supervisor.paths, threadID)
	if err != nil || len(messages) != 1 || stringValue(messages[0]["id"]) != "wake-id-fallback" {
		t.Fatalf("fallback inbox = %#v, %v", messages, err)
	}
	if more, err := consumeNativeInbox(supervisor.paths, threadID); err != nil || len(more) != 0 {
		t.Fatalf("wake fallback duplicated after acknowledgement: %#v, %v", more, err)
	}
}

func TestWakeLedgerRecoversInFlightAfterSupervisorRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	threadID := "00000000-0000-0000-0000-000000000777"
	messageID := "wake-recovery-id"
	item := map[string]any{"id": messageID, "message": "already delivered", "from": "peer"}
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-already-started", "status": "inProgress",
				"items": []map[string]any{{"id": "user-item", "type": "userMessage", "text": peerMessageText(item)}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	supervisor, err := newNativeSupervisor("test")
	if err != nil {
		t.Fatal(err)
	}
	record := wakeRecord{
		SessionID: threadID, MessageID: messageID, State: "in_flight",
		Item: item, CreatedAt: time.Now().UnixMilli(),
	}
	record.Fingerprint = wakeItemFingerprint(item)
	record.DeliveryFingerprint = sessionKey(peerMessageText(item))
	if err := writeWakeRecord(supervisor.paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor.finishWakeRecovery(record)
	got := readWakeRecord(supervisor.paths, threadID, messageID)
	if got == nil || got.State != "delivered" || got.TurnID != "turn-already-started" {
		t.Fatalf("recovered wake record = %+v", got)
	}
	messages, err := consumeNativeInbox(supervisor.paths, threadID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("recovered delivered wake was also queued: %#v, %v", messages, err)
	}
}

func TestWakeIdentityWithoutTransportIDIsStableAcrossShimRestart(t *testing.T) {
	root := t.TempDir()
	args := map[string]string{
		"session-id": "00000000-0000-0000-0000-000000000333", "cwd": root,
		"data-dir": filepath.Join(root, "state"), "codex-home": filepath.Join(root, "codex"),
		"claude-config-dir": filepath.Join(root, "claude"), "runtime-dir": filepath.Join(root, "run"),
	}
	frame := map[string]any{"type": "user", "from": "peer", "message": map[string]any{"content": "same content"}}
	newDaemon(args).handleUser(frame)
	newDaemon(args).handleUser(frame)
	pending := filepath.Join(args["data-dir"], "sessions", sessionKey(args["session-id"]), "inbox", "pending")
	entries, err := os.ReadDir(pending)
	if err != nil || len(entries) != 1 {
		t.Fatalf("derived wake identity produced %d inbox files: %v", len(entries), err)
	}
}

func TestHookRegisterFailureReusesPublishedSupervisorShim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	supervisorSocket := filepath.Join(root, "supervisor.sock")
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	paths := resolveNativePaths()
	threadID := "00000000-0000-0000-0000-000000000444"
	writeTestAttachedInteractiveOwner(t, paths, threadID)
	shimSocket := filepath.Join(root, "shim.sock")
	shimListener, err := net.Listen("unix", shimSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shimListener.Close() })
	go func() {
		for {
			connection, acceptErr := shimListener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	supervisorListener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisorListener.Close() })
	go func() {
		for {
			connection, acceptErr := supervisorListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				var request map[string]any
				_ = json.Unmarshal(line, &request)
				if stringValue(request["action"]) == "register" {
					_ = writeJSONAtomic(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json"), map[string]any{
						"sessionId": threadID, "socketPath": shimSocket, "name": "published-shim",
					})
					_, _ = connection.Write([]byte(`{"ok":false,"error":"subscription failed"}` + "\n"))
					return
				}
				_, _ = connection.Write([]byte(`{"ok":true}` + "\n"))
			}()
		}
	}()
	state, err := ensureHookShim(paths, hookInput{Event: "SessionStart", SessionID: threadID, Cwd: root}, liveInteractiveOwnerRecord(paths, threadID))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(state["socketPath"]) != shimSocket || stringValue(state["name"]) != "published-shim" {
		t.Fatalf("hook did not reuse supervisor-published shim: %#v", state)
	}
}

func TestRenamePeerReturnsSanitizedDiscoverableName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	threadID := "00000000-0000-0000-0000-000000000555"
	writeTestAttachedInteractiveOwner(t, paths, threadID)
	socket := filepath.Join(root, "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan map[string]any, 1)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				if len(line) == 0 {
					return
				}
				var frame map[string]any
				if json.Unmarshal(line, &frame) == nil {
					received <- frame
				}
			}()
		}
	}()
	if err := writeJSONAtomic(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json"), map[string]any{
		"sessionId": threadID, "socketPath": socket,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := callNativePeerTool("rename_session", map[string]any{"session_id": threadID, "name": "Reviewer / A"}, threadID)
	if err != nil {
		t.Fatal(err)
	}
	data := result["data"].(map[string]any)
	if stringValue(data["name"]) != "Reviewer-A" || !strings.Contains(stringValue(result["text"]), "Reviewer-A") {
		t.Fatalf("rename response did not report sanitized target: %#v", result)
	}
	select {
	case frame := <-received:
		if stringValue(frame["name"]) != "Reviewer-A" {
			t.Fatalf("shim rename target = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("shim did not receive rename")
	}
}

func TestStructuredSubagentSourceDoesNotAbortDecode(t *testing.T) {
	var page struct {
		Data []appThread `json:"data"`
	}
	body := []byte(`{"data":[{"id":"sub","source":{"subAgent":{"kind":"review"}}},{"id":"root","source":"appServer"}]}`)
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || rootThreadSource(page.Data[0].Source) || !rootThreadSource(page.Data[1].Source) {
		t.Fatalf("decoded sources = %#v", page.Data)
	}
}

func TestLaneNameLockSerializesPublishWindow(t *testing.T) {
	paths := nativePaths{dataRoot: t.TempDir()}
	first, err := lockLaneNames(paths)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *os.File, 1)
	errs := make(chan error, 1)
	go func() {
		second, lockErr := lockLaneNames(paths)
		if lockErr != nil {
			errs <- lockErr
			return
		}
		acquired <- second
	}()
	select {
	case second := <-acquired:
		_ = second.Close()
		t.Fatal("second lane crossed the name reservation window")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = syscall.Flock(int(first.Fd()), syscall.LOCK_UN)
	_ = first.Close()
	select {
	case second := <-acquired:
		_ = syscall.Flock(int(second.Fd()), syscall.LOCK_UN)
		_ = second.Close()
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("second lane never acquired released name lock")
	}
}

func TestFastCompletedLaneReplaysFinalItem(t *testing.T) {
	isolateNativeLaneTest(t)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-fast", "status": "completed", "items": []map[string]any{{
					"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "FAST_FINAL",
				}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	state := laneState{ThreadID: "thread-fast", TurnID: "turn-fast"}
	status, code, collected, waitErr := waitForLaneTurn(client, &state, time.Second)
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if waitErr != nil || code != 0 || status != "completed" {
		t.Fatalf("fast wait = status %q code %d err %v", status, code, waitErr)
	}
	if !collected {
		t.Fatal("fast terminal turn was not acknowledged as collected")
	}
	text := string(output)
	if answer := strings.Index(text, "FAST_FINAL"); answer < 0 || answer > strings.Index(text, "turn.completed") {
		t.Fatalf("final item was not replayed before completion: %s", text)
	}
}

func TestCompletedLaneRecoversFinalAnswerFromSupervisorSpool(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	state := laneState{ThreadID: "thread-spooled", TurnID: "turn-spooled", Status: "in_progress"}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := persistLaneItem(paths, state.ThreadID, state.TurnID, map[string]any{
		"id": "spooled-answer", "type": "agentMessage", "phase": "final_answer", "text": "SPOOLED_FINAL",
	}); err != nil {
		t.Fatal(err)
	}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "thread/turns/list" {
			return map[string]any{"data": []map[string]any{{"id": state.TurnID, "status": "completed", "items": []any{}}}}, nil
		}
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	status, code, collected, waitErr := waitForLaneTurn(client, &state, time.Second)
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if waitErr != nil || code != 0 || status != "completed" || !collected {
		t.Fatalf("spooled wait = status %q code %d collected=%v err=%v", status, code, collected, waitErr)
	}
	text := string(output)
	if answer := strings.Index(text, "SPOOLED_FINAL"); answer < 0 || answer > strings.Index(text, "turn.completed") {
		t.Fatalf("spooled answer was not replayed before completion: %s", text)
	}
}

func TestLaneItemSpoolWriterAndAcknowledgementAreSerialized(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	for index := 0; index < 64; index++ {
		threadID := fmt.Sprintf("thread-spool-race-%d", index)
		turnID := fmt.Sprintf("turn-spool-race-%d", index)
		state := laneState{ThreadID: threadID, TurnID: turnID, Status: "completed"}
		if err := recordLaneState(paths, state); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			errs <- persistLaneItem(paths, threadID, turnID, map[string]any{
				"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "RESULT",
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			acknowledged := state
			acknowledged.CollectedTurnID = turnID
			errs <- persistLaneWaitState(paths, acknowledged, true)
		}()
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(laneItemSpoolDir(paths, threadID, turnID))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("iteration %d left %d orphaned spool items", index, len(entries))
		}
	}
}

func TestTerminalOutputFailureDoesNotAcknowledgeCollection(t *testing.T) {
	isolateNativeLaneTest(t)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "thread/turns/list" {
			return map[string]any{"data": []map[string]any{{
				"id": "turn-undelivered", "status": "completed",
				"items": []map[string]any{{"id": "item-undelivered", "type": "agentMessage", "phase": "final_answer", "text": "MUST_REPLAY"}},
			}}}, nil
		}
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	original := os.Stdout
	os.Stdout = write
	state := laneState{ThreadID: "thread-undelivered", TurnID: "turn-undelivered", CollectedTurnID: "turn-prior"}
	_, _, collected, waitErr := waitForLaneTurn(client, &state, time.Second)
	os.Stdout = original
	_ = write.Close()
	if waitErr == nil || collected {
		t.Fatalf("broken output = collected=%v err=%v", collected, waitErr)
	}
	if state.CollectedTurnID != "turn-prior" {
		t.Fatalf("broken output advanced cursor: %+v", state)
	}
}

func TestLaneReadyFollowsInitialTurnAndRegistration(t *testing.T) {
	useBridgeTestAgent(t)
	methods := make(chan string, 16)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods <- method
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{
				"id": "thread-ready", "sessionId": "thread-ready", "cwd": t.TempDir(), "source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "thread/name/set":
			return map[string]any{}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-ready", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	supervisorSocket := filepath.Join(root, "supervisor.sock")
	listener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		line, _ := bufio.NewReader(connection).ReadBytes('\n')
		var request map[string]any
		_ = json.Unmarshal(line, &request)
		methods <- stringValue(request["action"])
		_, _ = connection.Write([]byte(`{"ok":true,"state":{"socketPath":"/tmp/lane-ready.sock"}}` + "\n"))
	}()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	promptFile := filepath.Join(root, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("briefing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, startErr := startLaneNative(withCurrentTestLaneOwner(laneOptions{
		laneCommonOptions: laneCommonOptions{command: "start", name: "ready-lane", cwd: root, promptFile: promptFile},
	}), false)
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if startErr != nil || code != 0 {
		t.Fatalf("start lane = code %d err %v", code, startErr)
	}
	gotMethods := []string{}
	for len(gotMethods) < 4 {
		select {
		case method := <-methods:
			gotMethods = append(gotMethods, method)
		case <-time.After(time.Second):
			t.Fatalf("method sequence timed out: %v", gotMethods)
		}
	}
	wantMethods := []string{"thread/start", "thread/name/set", "turn/start", "register"}
	if strings.Join(gotMethods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("startup method order = %v, want %v", gotMethods, wantMethods)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"type":"thread.started"`) ||
		!strings.Contains(lines[1], `"type":"turn.started"`) || !strings.Contains(lines[2], `"type":"lane.ready"`) {
		t.Fatalf("startup event order = %s", output)
	}
}

func TestSupervisorStopTimeoutDoesNotPermitReplacement(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "supervisor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(connection net.Conn) {
				defer func() { _ = connection.Close() }()
				buffer := make([]byte, maxFrameBytes)
				_, _ = connection.Read(buffer)
				_, _ = connection.Write([]byte("{\"ok\":true,\"stopping\":true}\n"))
			}(conn)
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	if err := stopExistingNativeSupervisor(socket, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("live old supervisor did not block replacement: %v", err)
	}
	if !probeUnixSocket(socket, 50*time.Millisecond) {
		t.Fatal("old supervisor was unexpectedly displaced")
	}
}

func TestSupervisorStopCommandWaitsForExactProcessExit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	socket := filepath.Join(root, "supervisor.sock")
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", socket)

	child := exec.Command("sleep", "0.35")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	procStart := ""
	deadline := time.Now().Add(time.Second)
	for procStart == "" && time.Now().Before(deadline) {
		procStart = readProcStart(child.Process.Pid)
		time.Sleep(10 * time.Millisecond)
	}
	if procStart == "" {
		t.Fatal("could not capture supervisor fixture process identity")
	}

	paths := resolveNativePaths()
	if err := writeJSONAtomic(paths.supervisorState, map[string]any{
		"pid": child.Process.Pid, "procStart": procStart,
	}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		_, _ = bufio.NewReader(connection).ReadBytes('\n')
		_, _ = connection.Write([]byte(`{"ok":true,"stopping":true}` + "\n"))
		_ = connection.Close()
		_ = listener.Close()
		_ = os.Remove(socket)
	}()

	started := time.Now()
	if exit := runSupervisorCommand([]string{"stop"}); exit != 0 {
		t.Fatalf("supervisor stop exit = %d", exit)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("supervisor stop returned before exact process exit: %s", elapsed)
	}
	<-done
}

func TestNamespacedSupervisorRetiresResponsiveLegacyInstance(t *testing.T) {
	runtimeRoot := t.TempDir()
	legacySocket := filepath.Join(bridgeRuntimeRoot(runtimeRoot, os.Getuid()), "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(legacySocket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", legacySocket)
	if err != nil {
		t.Fatal(err)
	}
	appServerSocket := filepath.Join(t.TempDir(), "app-server.sock")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			line, _ := bufio.NewReader(connection).ReadBytes('\n')
			var request map[string]any
			_ = json.Unmarshal(line, &request)
			if stringValue(request["action"]) == "stop" {
				_, _ = connection.Write([]byte(`{"ok":true,"stopping":true}` + "\n"))
				_ = connection.Close()
				_ = listener.Close()
				return
			}
			response, _ := json.Marshal(map[string]any{
				"ok": true, "implementation": "go", "appServerSocket": appServerSocket,
			})
			_, _ = connection.Write(append(response, '\n'))
			_ = connection.Close()
		}
	}()
	paths := nativePaths{
		runtimeDir: runtimeRoot, appServerSock: appServerSocket,
		supervisorSock: filepath.Join(bridgeRuntimeRoot(runtimeRoot, os.Getuid()), "supervisor-profile.sock"),
	}
	if err := stopLegacyNativeSupervisor(paths); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("legacy supervisor was not stopped")
	}
	if probeUnixSocket(legacySocket, 25*time.Millisecond) {
		t.Fatal("legacy supervisor remained reachable")
	}
}

func TestLaneSchemaValidatesFinalAnswer(t *testing.T) {
	answer := `{"ok":true}`
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-schema", "status": "completed", "items": []map[string]any{{
					"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": answer,
				}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	state := laneState{
		ThreadID: "thread-schema", TurnID: "turn-schema",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"const":true}},"required":["ok"],"additionalProperties":false}`),
	}
	paths := nativePaths{profileRoot: t.TempDir()}
	if err := validateLaneTurnOutput(client, paths, state, state.TurnID); err != nil {
		t.Fatalf("valid schema result rejected: %v", err)
	}
	answer = `{"ok":false}`
	if err := validateLaneTurnOutput(client, paths, state, state.TurnID); err == nil {
		t.Fatal("invalid schema result accepted")
	}
}

func TestSupervisorRetriesInvalidDetachedSchemaTurn(t *testing.T) {
	turnStarts := make(chan map[string]any, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-invalid", "status": "completed", "items": []map[string]any{{
					"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": `{"ok":false}`,
				}},
			}}}, nil
		case "turn/start":
			turnStarts <- request["params"].(map[string]any)
			return map[string]any{"turn": map[string]any{"id": "turn-corrected", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		appServerSock: socket,
	}
	state := laneState{
		Type: "codex-peer-lane", Name: "schema-lane", ThreadID: "thread-schema-retry", SessionID: "thread-schema-retry",
		Cwd: root, Status: "completed", TurnID: "turn-invalid",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"const":true}},"required":["ok"]}`),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), activeTurns: map[string]string{}, subscribed: map[string]bool{}, shims: map[string]map[string]any{},
	}
	action, _, err := supervisor.processLaneSchemaTerminal(state.ThreadID, state.TurnID)
	if err != nil || action != laneSchemaRetried {
		t.Fatalf("schema retry = %v, %v", action, err)
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-corrected" || updated.SchemaAttempts != 1 {
		t.Fatalf("updated schema lane = %+v, %v", updated, err)
	}
	select {
	case params := <-turnStarts:
		if _, ok := params["outputSchema"].(map[string]any); !ok {
			t.Fatalf("retry omitted output schema: %#v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("schema retry did not start a corrective turn")
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestSchemaRetryPreservesOlderAndNewerCollectionDebt(t *testing.T) {
	turnStarts := make(chan struct{}, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-wake-invalid", "status": "completed", "items": []map[string]any{{
					"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": `{"ok":false}`,
				}},
			}}}, nil
		case "turn/start":
			turnStarts <- struct{}{}
			return map[string]any{"turn": map[string]any{"id": "turn-corrected", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "schema-debt", ThreadID: "thread-schema-debt", SessionID: "thread-schema-debt",
		Status: "completed", TurnID: "turn-owed", LatestTurnID: "turn-newer",
		PendingTurnIDs: []string{"turn-owed", "turn-wake-invalid", "turn-newer"},
		OutputSchema:   json.RawMessage(`{"type":"object","properties":{"ok":{"const":true}},"required":["ok"]}`),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, done: make(chan struct{}), activeTurns: map[string]string{}, subscribed: map[string]bool{}, shims: map[string]map[string]any{}}
	action, _, err := supervisor.processLaneSchemaTerminal(state.ThreadID, "turn-wake-invalid")
	if err != nil || action != laneSchemaRetried {
		t.Fatalf("schema action for middle pending turn = %v, %v", action, err)
	}
	select {
	case <-turnStarts:
	case <-time.After(time.Second):
		t.Fatal("schema retry did not start after older result was acknowledged")
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-owed" || updated.LatestTurnID != "turn-corrected" ||
		!reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-owed", "turn-corrected", "turn-newer"}) {
		t.Fatalf("schema retry did not replace its draft in collection order: %+v, %v", updated, err)
	}
	collector := updated
	collector.TurnID = "turn-owed"
	collector.Status = "completed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	updated, err = readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-corrected" {
		t.Fatalf("older acknowledgement did not advance to correction: %+v, %v", updated, err)
	}
	collector = updated
	collector.TurnID = "turn-corrected"
	collector.Status = "completed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	updated, err = readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-newer" {
		t.Fatalf("correction acknowledgement stranded newer turn: %+v, %v", updated, err)
	}
	collector = updated
	collector.TurnID = "turn-newer"
	collector.Status = "completed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	updated, err = readLaneStateFile(paths, state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	normalizeLanePendingTurns(&updated)
	if len(updated.PendingTurnIDs) != 0 || updated.TurnID != updated.CollectedTurnID {
		t.Fatalf("collected correction was resurrected after queue drained: %+v", updated)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestSchemaCorrectionDoesNotRevertConcurrentAcknowledgement(t *testing.T) {
	turnStartEntered := make(chan struct{})
	releaseTurnStart := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTurnStart) }) }
	defer release()
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-invalid", "status": "completed", "items": []map[string]any{{
					"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": `{"ok":false}`,
				}},
			}}}, nil
		case "turn/start":
			close(turnStartEntered)
			<-releaseTurnStart
			return map[string]any{"turn": map[string]any{"id": "turn-corrected", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "schema-concurrent", ThreadID: "thread-schema-concurrent", SessionID: "thread-schema-concurrent",
		Status: "completed", TurnID: "turn-invalid", LatestTurnID: "turn-newer",
		PendingTurnIDs: []string{"turn-invalid", "turn-newer"}, PendingQueueVer: 1,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"const":true}},"required":["ok"]}`),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, done: make(chan struct{}), activeTurns: map[string]string{}, subscribed: map[string]bool{}, shims: map[string]map[string]any{}}
	type result struct {
		action laneSchemaAction
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		action, _, err := supervisor.processLaneSchemaTerminal(state.ThreadID, "turn-invalid")
		resultCh <- result{action: action, err: err}
	}()
	select {
	case <-turnStartEntered:
	case <-time.After(time.Second):
		t.Fatal("schema correction did not reach blocked turn/start")
	}
	collector := state
	collector.Status = "completed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	acknowledged, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || acknowledged.CollectedTurnID != "turn-invalid" ||
		!reflect.DeepEqual(acknowledged.PendingTurnIDs, []string{"turn-newer"}) {
		t.Fatalf("concurrent acknowledgement was not persisted: %+v, %v", acknowledged, err)
	}
	release()
	select {
	case got := <-resultCh:
		if got.err != nil || got.action != laneSchemaRetried {
			t.Fatalf("schema correction = %v, %v", got.action, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("schema correction did not finish")
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.CollectedTurnID != "turn-invalid" || updated.TurnID != "turn-newer" ||
		!reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-newer", "turn-corrected"}) {
		t.Fatalf("schema correction reverted concurrent acknowledgement: %+v, %v", updated, err)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestResumeLaneStartsNewTurnOnSameTranscript(t *testing.T) {
	useBridgeTestAgent(t)
	methods := make(chan string, 16)
	root := t.TempDir()
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods <- method
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": "thread-resume", "sessionId": "thread-resume", "name": "resume-lane", "cwd": root,
				"source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-followup", "status": "inProgress"}}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-followup", "status": "completed", "items": []map[string]any{{
					"id": "followup-answer", "type": "agentMessage", "phase": "final_answer", "text": "SAME_TRANSCRIPT",
				}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	supervisorSocket := filepath.Join(root, "supervisor-resume.sock")
	listener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		line, _ := bufio.NewReader(connection).ReadBytes('\n')
		var request map[string]any
		_ = json.Unmarshal(line, &request)
		methods <- stringValue(request["action"])
		_, _ = connection.Write([]byte(`{"ok":true,"state":{"socketPath":"/tmp/lane-resume.sock"}}` + "\n"))
	}()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	if err := recordLaneState(paths, laneState{
		Type: "codex-peer-lane", Name: "resume-lane", ThreadID: "thread-resume", SessionID: "thread-resume",
		Cwd: root, Status: "completed", TurnID: "turn-original", CollectedTurnID: "turn-original",
	}); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(root, "followup.txt")
	if err := os.WriteFile(promptFile, []byte("clarify the prior verdict\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, resumeErr := resumeLaneNative(withCurrentTestLaneOwner(laneOptions{laneCommonOptions: laneCommonOptions{command: "resume", target: "resume-lane", promptFile: promptFile}}))
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if resumeErr != nil || code != 0 || !strings.Contains(string(output), "SAME_TRANSCRIPT") {
		t.Fatalf("resume = code %d err %v output %s", code, resumeErr, output)
	}
	seen := map[string]bool{}
	for len(methods) > 0 {
		seen[<-methods] = true
	}
	if !seen["thread/resume"] || !seen["turn/start"] || seen["thread/start"] {
		t.Fatalf("resume methods = %#v", seen)
	}
}

func TestResumeStartFailureRestoresOriginalLaneState(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": "thread-resume-failure", "sessionId": "thread-resume-failure", "name": "resume-failure", "cwd": root,
				"source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "turn/start":
			return nil, errors.New("authoritative turn start rejection")
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	original := laneState{
		Type: "codex-peer-lane", Name: "resume-failure", ThreadID: "thread-resume-failure", SessionID: "thread-resume-failure",
		Cwd: root, Status: "completed", TurnID: "turn-original", LatestTurnID: "turn-original",
		CollectedTurnID: "turn-original", TerminalTurnID: "turn-original", TerminalOutcome: "completed",
		Persistent: true, AutoArchive: false, PendingQueueVer: 1,
	}
	if err := recordLaneState(paths, original); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(root, "followup.txt")
	if err := os.WriteFile(promptFile, []byte("follow up\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, resumeErr := resumeLaneNative(laneOptions{
		laneCommonOptions: laneCommonOptions{command: "resume", target: original.Name, promptFile: promptFile, persistent: true, autoArchive: false},
	})
	if resumeErr == nil || code == 0 {
		t.Fatalf("rejected resume = code %d err %v", code, resumeErr)
	}
	restored, err := readLaneStateFile(paths, original.ThreadID)
	if err != nil || restored.Status != original.Status || restored.TurnID != original.TurnID ||
		restored.LatestTurnID != original.LatestTurnID || restored.CollectedTurnID != original.CollectedTurnID ||
		restored.TerminalOutcome != original.TerminalOutcome || !restored.Persistent || restored.AutoArchive {
		t.Fatalf("failed resume did not restore original state: %+v, %v", restored, err)
	}
}

func TestResumeUncertainStartPreservesObservedTurn(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	var observer *nativeSupervisor
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": "thread-resume-uncertain", "sessionId": "thread-resume-uncertain", "name": "resume-uncertain", "cwd": root,
				"source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "turn/start":
			// Model a response lost after App Server accepted the request: the
			// notification path has already made the new turn durable.
			if err := observer.recordLaneTurnStarted("thread-resume-uncertain", "turn-observed"); err != nil {
				return nil, err
			}
			return nil, errors.New("turn start response lost")
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	observer = &nativeSupervisor{paths: paths}
	original := laneState{
		Type: "codex-peer-lane", Name: "resume-uncertain", ThreadID: "thread-resume-uncertain", SessionID: "thread-resume-uncertain",
		Cwd: root, Status: "completed", TurnID: "turn-original", LatestTurnID: "turn-original",
		CollectedTurnID: "turn-original", TerminalTurnID: "turn-original", TerminalOutcome: "completed",
		Persistent: true, AutoArchive: false, PendingQueueVer: 1,
	}
	if err := recordLaneState(paths, original); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(root, "followup.txt")
	if err := os.WriteFile(promptFile, []byte("follow up\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, resumeErr := resumeLaneNative(laneOptions{
		laneCommonOptions: laneCommonOptions{command: "resume", target: original.Name, promptFile: promptFile, persistent: true, autoArchive: false},
	})
	if resumeErr == nil || code == 0 {
		t.Fatalf("uncertain resume = code %d err %v", code, resumeErr)
	}
	updated, err := readLaneStateFile(paths, original.ThreadID)
	if err != nil || updated.Status != "in_progress" || updated.TurnID != "turn-observed" ||
		updated.LatestTurnID != "turn-observed" || !reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-observed"}) ||
		updated.CollectedTurnID != "" {
		t.Fatalf("observed uncertain resume was rolled back: %+v, %v", updated, err)
	}
}

func TestResumePostStartCommitFailurePreservesKnownTurn(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	original := laneState{
		Type: "codex-peer-lane", Name: "resume-known", ThreadID: "thread-resume-known", SessionID: "thread-resume-known",
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", CollectedTurnID: "turn-old", PendingQueueVer: 1,
	}
	placeholder := original
	placeholder.Status = "starting"
	placeholder.TurnID = ""
	placeholder.LatestTurnID = ""
	placeholder.CollectedTurnID = ""
	placeholder.PendingTurnIDs = nil
	if err := recordLaneState(paths, placeholder); err != nil {
		t.Fatal(err)
	}
	observed := placeholder
	observed.Status = "in_progress"
	observed.TurnID = "turn-known"
	observed.LatestTurnID = "turn-known"
	observed.PendingTurnIDs = []string{"turn-known"}
	restored, err := restoreLaneAfterFailedResume(paths, original, observed)
	if err != nil || restored {
		t.Fatalf("known accepted turn was treated as rollback-safe: restored=%v err=%v", restored, err)
	}
	updated, err := readLaneStateFile(paths, original.ThreadID)
	if err != nil || updated.TurnID != "turn-known" || updated.LatestTurnID != "turn-known" ||
		!reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-known"}) {
		t.Fatalf("known accepted turn was not preserved: %+v, %v", updated, err)
	}
}

func TestTerminalNoticeJobIsDurableAndDeduplicated(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), claudeRoot: filepath.Join(root, "claude")}
	state := laneState{
		Type: "codex-peer-lane", Name: "notice-lane", ThreadID: "thread-notice", SessionID: "thread-notice",
		Cwd: root, Status: "in_progress", TurnID: "turn-notice", NotifyTarget: "orchestrator",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	supervisor.queueLaneTerminalNotice(state.ThreadID, map[string]any{"id": state.TurnID, "status": "completed"})
	supervisor.queueLaneTerminalNotice(state.ThreadID, map[string]any{"id": state.TurnID, "status": "completed"})
	for _, terminal := range []struct {
		turnID string
		status string
	}{{"turn-failed", "failed"}, {"turn-interrupted", "interrupted"}} {
		supervisor.queueLaneTerminalNotice(state.ThreadID, map[string]any{"id": terminal.turnID, "status": terminal.status})
	}
	entries, err := os.ReadDir(filepath.Join(paths.profileRoot, "notices"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("durable notice entries = %d, %v", len(entries), err)
	}
	statuses := map[string]bool{}
	for _, entry := range entries {
		job := readJSONMap(filepath.Join(paths.profileRoot, "notices", entry.Name()))
		if stringValue(job["threadId"]) != state.ThreadID || stringValue(job["target"]) != "orchestrator" {
			t.Fatalf("notice job = %#v", job)
		}
		statuses[stringValue(job["status"])] = true
	}
	for _, status := range []string{"completed", "failed", "interrupted"} {
		if !statuses[status] {
			t.Fatalf("terminal notice status %q was not persisted: %#v", status, statuses)
		}
	}
	completedID := sessionKey("lane-terminal\x00" + state.ThreadID + "\x00" + state.TurnID)
	completed := readJSONMap(filepath.Join(paths.profileRoot, "notices", completedID+".json"))
	if stringValue(completed["outcome"]) != "completed" || intValue(completed["exit"]) != 0 {
		t.Fatalf("completed notice lacks normalized exit detail: %#v", completed)
	}
}

func TestSupervisorArchivesTerminalLaneWhenParentExits(t *testing.T) {
	root := t.TempDir()
	threadID := "thread-owner-exited"
	archived := make(chan string, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "status": "idle"}}, nil
		case "thread/archive":
			params := request["params"].(map[string]any)
			archived <- stringValue(params["threadId"])
			return map[string]any{}, nil
		case "thread/unsubscribe":
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "owned", ThreadID: threadID, SessionID: threadID,
		Status: "completed", TurnID: "turn-done", OwnerPID: 1_000_000_000, OwnerProcStart: "definitely-dead",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	supervisor.reconcileLaneLifecycles(client)
	select {
	case got := <-archived:
		if got != threadID {
			t.Fatalf("archived thread = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ownerless terminal lane was not archived")
	}
	updated, err := readLaneStateFile(paths, threadID)
	if err != nil || updated.Status != "archived" || !readRetiredThreads(paths)[threadID] {
		t.Fatalf("archived ownerless state = %+v, %v", updated, err)
	}
}

func TestSupervisorInterruptsRunningLaneWhenParentExitsButKeepsPersistentLane(t *testing.T) {
	root := t.TempDir()
	interrupted := make(chan string, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			params := request["params"].(map[string]any)
			return map[string]any{"thread": map[string]any{
				"id": stringValue(params["threadId"]), "status": "active",
			}}, nil
		case "turn/interrupt":
			params := request["params"].(map[string]any)
			interrupted <- stringValue(params["turnId"])
		}
		return map[string]any{}, nil
	})
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	for _, state := range []laneState{
		{Type: "codex-peer-lane", Name: "owned", ThreadID: "thread-owned", Status: "completed", TurnID: "turn-owed", LatestTurnID: "turn-owned", OwnerPID: 1_000_000_000, OwnerProcStart: "definitely-dead"},
		{Type: "codex-peer-lane", Name: "persistent", ThreadID: "thread-persistent", Status: "in_progress", TurnID: "turn-persistent", Persistent: true},
		{Type: "codex-peer-lane", Name: "live-owner", ThreadID: "thread-live-owner", Status: "completed", TurnID: "turn-live-owner", OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid())},
	} {
		if err := recordLaneState(paths, state); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths}
	supervisor.reconcileLaneLifecycles(client)
	select {
	case got := <-interrupted:
		if got != "turn-owned" {
			t.Fatalf("interrupted turn = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("running ownerless lane was not interrupted")
	}
	select {
	case extra := <-interrupted:
		t.Fatalf("persistent lane was interrupted: %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestArchiveCancellationMakesUndeliverableNoticeNonBlocking(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-4000-8000-000000000011"
	noticeID := sessionKey("lane-terminal\x00" + threadID + "\x00turn-a")
	pending := filepath.Join(paths.profileRoot, "notices", noticeID+".json")
	if err := writeJSONAtomic(pending, map[string]any{
		"noticeId": noticeID, "threadId": threadID, "turnId": "turn-a", "target": "offline-peer",
	}); err != nil {
		t.Fatal(err)
	}
	dropped, err := cancelLaneNotices(paths, threadID, "lane archived")
	if err != nil || dropped != 1 {
		t.Fatalf("cancelLaneNotices = %d, %v", dropped, err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending notice survived cancellation: %v", err)
	}
	cancelled := readJSONMap(filepath.Join(paths.profileRoot, "notices-cancelled", noticeID+".json"))
	if stringValue(cancelled["threadId"]) != threadID || stringValue(cancelled["reason"]) != "lane archived" {
		t.Fatalf("notice cancellation audit = %#v", cancelled)
	}
}

func TestSupervisorAutoArchivesIdleLaneAfterGracePeriod(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000021"
	archived := make(chan string, 1)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "status": "idle"}}, nil
		case "thread/archive":
			params := request["params"].(map[string]any)
			archived <- stringValue(params["threadId"])
		}
		return map[string]any{}, nil
	})
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: appSocket}
	state := laneState{
		Type: "codex-peer-lane", Name: "auto-archive", ThreadID: threadID, SessionID: threadID,
		Status: "completed", TurnID: "turn-final", LatestTurnID: "turn-final",
		AutoArchive: true, AutoArchiveAt: time.Now().Add(-time.Second).UnixMilli(), Persistent: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, appSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	supervisor.reconcileLaneLifecycles(client)
	select {
	case got := <-archived:
		if got != threadID {
			t.Fatalf("archived thread = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expired auto-archive grace did not retire idle lane")
	}
	updated, err := readLaneStateFile(paths, threadID)
	if err != nil || updated.Status != "archived" || updated.AutoArchiveAt != 0 || !readRetiredThreads(paths)[threadID] {
		t.Fatalf("auto-archived state = %+v, %v", updated, err)
	}
}

func TestLatestTerminalTurnSchedulesAndNewTurnCancelsAutoArchive(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	state := laneState{
		Type: "codex-peer-lane", ThreadID: "thread-auto-schedule", Status: "completed",
		TurnID: "turn-owed", LatestTurnID: "turn-latest", PendingTurnIDs: []string{"turn-owed", "turn-latest"},
		PendingQueueVer: 1, AutoArchive: true, AutoArchiveDelayMS: 2500,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, activeTurns: map[string]string{state.ThreadID: "turn-latest"}}
	supervisor.handleLaneTurnCompleted(state.ThreadID, map[string]any{"id": "turn-latest", "status": "completed"})
	scheduled, err := readLaneStateFile(paths, state.ThreadID)
	remaining := time.Until(time.UnixMilli(scheduled.AutoArchiveAt))
	if err != nil || remaining < 2*time.Second || remaining > 3*time.Second {
		t.Fatalf("latest terminal turn did not schedule grace expiry: %+v, %v", scheduled, err)
	}
	if err := supervisor.recordLaneTurnStarted(state.ThreadID, "turn-new"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || cancelled.AutoArchiveAt != 0 || cancelled.LatestTurnID != "turn-new" {
		t.Fatalf("new turn did not cancel auto-archive: %+v, %v", cancelled, err)
	}
}

func TestDeferredSchemaTurnDoesNotAutoArchiveBeforeCorrection(t *testing.T) {
	turnStarts := make(chan struct{}, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{
				{"id": "turn-corrected", "status": "completed", "items": []map[string]any{{
					"id": "corrected", "type": "agentMessage", "phase": "final_answer", "text": `{"ok":true}`,
				}}},
				{"id": "turn-invalid", "status": "completed", "items": []map[string]any{{
					"id": "draft", "type": "agentMessage", "phase": "final_answer", "text": `{"ok":false}`,
				}}},
			}}, nil
		case "turn/start":
			turnStarts <- struct{}{}
			return map[string]any{"turn": map[string]any{"id": "turn-corrected", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "deferred-schema", ThreadID: "thread-deferred-schema", SessionID: "thread-deferred-schema",
		Status: "completed", TurnID: "turn-owed", LatestTurnID: "turn-invalid",
		PendingTurnIDs: []string{"turn-owed", "turn-invalid"}, PendingQueueVer: 1, AutoArchive: true,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"const":true}},"required":["ok"]}`),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, done: make(chan struct{}), activeTurns: map[string]string{}, subscribed: map[string]bool{}, shims: map[string]map[string]any{}}
	supervisor.handleLaneTurnCompleted(state.ThreadID, map[string]any{"id": "turn-invalid", "status": "completed"})
	deferred, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || deferred.AutoArchiveAt != 0 {
		t.Fatalf("deferred invalid turn armed auto-archive: %+v, %v", deferred, err)
	}
	collector := deferred
	collector.TurnID = "turn-owed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	action, _, err := supervisor.processLaneSchemaTerminal(state.ThreadID, "turn-invalid")
	if err != nil || action != laneSchemaRetried {
		t.Fatalf("deferred schema correction = %v, %v", action, err)
	}
	select {
	case <-turnStarts:
	case <-time.After(time.Second):
		t.Fatal("deferred invalid turn did not start a correction")
	}
	correcting, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || correcting.AutoArchiveAt != 0 || correcting.LatestTurnID != "turn-corrected" {
		t.Fatalf("in-flight correction has an archive deadline: %+v, %v", correcting, err)
	}
	action, _, err = supervisor.processLaneSchemaTerminal(state.ThreadID, "turn-corrected")
	if err != nil || action != laneSchemaNone {
		t.Fatalf("corrected schema terminal = %v, %v", action, err)
	}
	corrected, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || corrected.AutoArchiveAt < time.Now().Add(50*time.Second).UnixMilli() {
		t.Fatalf("validated correction did not arm auto-archive: %+v, %v", corrected, err)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestLifecycleArchiveRevalidatesAfterThreadRead(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000024"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	archived := make(chan struct{}, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			latest, err := readLaneStateFile(paths, threadID)
			if err != nil {
				return nil, err
			}
			latest.LatestTurnID = "turn-new"
			latest.AutoArchiveAt = 0
			latest.Status = "in_progress"
			if err := recordLaneState(paths, latest); err != nil {
				return nil, err
			}
			return map[string]any{"thread": map[string]any{"id": threadID, "status": "idle"}}, nil
		case "thread/archive":
			archived <- struct{}{}
		}
		return map[string]any{}, nil
	})
	paths.appServerSock = socket
	state := laneState{
		Type: "codex-peer-lane", Name: "revalidated", ThreadID: threadID, SessionID: threadID,
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", AutoArchive: true,
		AutoArchiveAt: time.Now().Add(-time.Second).UnixMilli(), Persistent: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	supervisor.reconcileLaneLifecycles(client)
	select {
	case <-archived:
		t.Fatal("lane was archived after a new turn cancelled its deadline")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLifecycleArchiveSerializesNewTurnCommit(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000026"
	archived := make(chan struct{}, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "status": "idle"}}, nil
		case "thread/archive":
			archived <- struct{}{}
		}
		return map[string]any{}, nil
	})
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "serialized", ThreadID: threadID, SessionID: threadID,
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", AutoArchive: true,
		AutoArchiveAt: time.Now().Add(-time.Second).UnixMilli(), Persistent: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	lifecycleLock, err := lockLaneLifecycle(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	done := make(chan error, 1)
	go func() { done <- supervisor.archiveLifecycleLane(client, state, "test", false) }()
	select {
	case <-archived:
		t.Fatal("archive bypassed a new-turn lifecycle lock")
	case err := <-done:
		t.Fatalf("archive returned while lifecycle lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	latest, err := readLaneStateFile(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	latest.LatestTurnID = "turn-new"
	latest.AutoArchiveAt = 0
	latest.Status = "in_progress"
	if err := recordLaneState(paths, latest); err != nil {
		t.Fatal(err)
	}
	unlockLaneLifecycle(lifecycleLock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("archive did not resume after lifecycle lock release")
	}
	select {
	case <-archived:
		t.Fatal("archive ignored the new turn committed under lifecycle lock")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLifecycleArchivePreservesConcurrentCollectorState(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000025"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "status": "idle"}}, nil
		case "thread/archive":
			latest, err := readLaneStateFile(paths, threadID)
			if err != nil {
				return nil, err
			}
			latest.CollectedTurnID = "turn-final"
			latest.PendingTurnIDs = nil
			latest.TerminalOutcome = "completed"
			latest.TerminalTurnID = "turn-final"
			if err := recordLaneState(paths, latest); err != nil {
				return nil, err
			}
		}
		return map[string]any{}, nil
	})
	paths.appServerSock = socket
	state := laneState{
		Type: "codex-peer-lane", Name: "merge-on-archive", ThreadID: threadID, SessionID: threadID,
		Status: "completed", TurnID: "turn-final", LatestTurnID: "turn-final",
		PendingTurnIDs: []string{"turn-final"}, PendingQueueVer: 1, AutoArchive: true,
		AutoArchiveAt: time.Now().Add(-time.Second).UnixMilli(), Persistent: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	if err := supervisor.archiveLifecycleLane(client, state, "test", false); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, threadID)
	if err != nil || updated.Status != "archived" || updated.CollectedTurnID != "turn-final" ||
		len(updated.PendingTurnIDs) != 0 || updated.TerminalOutcome != "completed" {
		t.Fatalf("archive overwrote concurrent collector state: %+v, %v", updated, err)
	}
}

func TestTerminalRecoveryArmsAutoArchiveWithoutNotifyTarget(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000027"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-final", "status": "completed", "completedAt": time.Now().Unix(),
				"items": []map[string]any{{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "done"}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "no-notify-recovery", ThreadID: threadID, SessionID: threadID,
		Status: "in_progress", TurnID: "turn-final", LatestTurnID: "turn-final",
		PendingTurnIDs: []string{"turn-final"}, PendingQueueVer: 1, AutoArchive: true, AutoArchiveDelayMS: 2500, Persistent: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	supervisor.reconcileLaneTerminalNotices(client)
	updated, err := readLaneStateFile(paths, threadID)
	remaining := time.Until(time.UnixMilli(updated.AutoArchiveAt))
	if err != nil || remaining < 2*time.Second || remaining > 3*time.Second || updated.TerminalOutcome != "completed" {
		t.Fatalf("terminal recovery did not arm no-notify lane: %+v, %v", updated, err)
	}
	entries, err := os.ReadDir(filepath.Join(paths.profileRoot, "notices"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no-notify recovery queued %d notices", len(entries))
	}
}

func TestTerminalRecoverySkipsArchivedLanes(t *testing.T) {
	var lists atomic.Int32
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "thread/turns/list" {
			lists.Add(1)
		}
		return map[string]any{}, nil
	})
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	state := laneState{
		Type: "codex-peer-lane", Name: "archived-recovery", ThreadID: "thread-archived-recovery", SessionID: "thread-archived-recovery",
		Status: "archived", TurnID: "turn-final", LatestTurnID: "turn-final", AutoArchive: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	supervisor := nativeSupervisor{paths: paths}
	supervisor.reconcileLaneTerminalNotices(client)
	if lists.Load() != 0 {
		t.Fatalf("terminal recovery queried %d archived lanes", lists.Load())
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.Status != "archived" || updated.AutoArchiveAt != 0 {
		t.Fatalf("archived state changed during terminal recovery: %+v, %v", updated, err)
	}
}

func TestDelayedTerminalCallbackDoesNotRewriteArchivedLane(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", Name: "delayed-terminal", ThreadID: "thread-delayed-terminal", SessionID: "thread-delayed-terminal",
		Status: "archived", TurnID: "turn-final", LatestTurnID: "turn-final", NotifyTarget: "orchestrator",
		AutoArchive: true, PendingTurnIDs: []string{"turn-final"}, PendingQueueVer: 1,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := markRetiredThread(paths, state.ThreadID); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, activeTurns: map[string]string{state.ThreadID: state.TurnID}}
	supervisor.handleLaneTurnCompleted(state.ThreadID, map[string]any{"id": state.TurnID, "status": "completed"})
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.Status != "archived" || updated.AutoArchiveAt != 0 {
		t.Fatalf("delayed terminal rewrote archived state: %+v, %v", updated, err)
	}
	entries, err := os.ReadDir(filepath.Join(paths.profileRoot, "notices"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("delayed terminal queued %d post-archive notices", len(entries))
	}
}

func TestArchiveByRawThreadIDDoesNotFabricateLaneMetadata(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-4000-8000-000000000023"
	archived := make(chan string, 1)
	retireRequests := make(chan struct{}, 2)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "thread/archive" {
			params := request["params"].(map[string]any)
			archived <- stringValue(params["threadId"])
		}
		return map[string]any{}, nil
	})
	supervisorSocket := filepath.Join(root, "supervisor.sock")
	listener, err := net.Listen("unix", supervisorSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				var request map[string]any
				_ = json.Unmarshal(line, &request)
				response := `{"ok":true}`
				if stringValue(request["action"]) == "flush_notices" {
					response = `{"ok":true,"pending":0}`
				} else if stringValue(request["action"]) == "retire" {
					retireRequests <- struct{}{}
				}
				_, _ = connection.Write([]byte(response + "\n"))
			}()
		}
	}()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, archiveErr := archiveLaneNative(laneOptions{laneCommonOptions: laneCommonOptions{target: threadID}})
	secondCode, secondErr := archiveLaneNative(laneOptions{laneCommonOptions: laneCommonOptions{target: threadID}})
	_ = write.Close()
	os.Stdout = original
	_, _ = io.ReadAll(read)
	_ = read.Close()
	if archiveErr != nil || code != 0 {
		t.Fatalf("raw thread archive = code %d err %v", code, archiveErr)
	}
	if secondErr != nil || secondCode != 0 {
		t.Fatalf("idempotent raw thread archive = code %d err %v", secondCode, secondErr)
	}
	select {
	case got := <-archived:
		if got != threadID {
			t.Fatalf("archived thread = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("raw interactive thread was not archived")
	}
	if !readRetiredThreads(paths)[threadID] {
		t.Fatal("raw interactive thread archive did not persist retirement")
	}
	if len(retireRequests) != 2 {
		t.Fatalf("archive did not reassert supervisor retirement: %d requests", len(retireRequests))
	}
	if _, err := os.Stat(laneStatePath(paths, threadID)); !os.IsNotExist(err) {
		t.Fatalf("raw interactive archive fabricated lane metadata: %v", err)
	}
}

func TestSupervisorEnforcesDurableLaneDeadlineAndPreservesOutcome(t *testing.T) {
	requests := make(chan string, 8)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "" {
			requests <- method
		}
		return map[string]any{}, nil
	})
	root := t.TempDir()
	paths := nativePaths{appServerSock: socket, profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-4000-8000-000000000012"
	state := laneState{
		Type: "codex-peer-lane", Name: "deadline-lane", ThreadID: threadID, SessionID: threadID,
		Cwd: root, Status: "in_progress", TurnID: "turn-timeout", NotifyTarget: "orchestrator",
		DeadlineAt: time.Now().Add(-time.Second).UnixMilli(),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, done: make(chan struct{}), activeTurns: map[string]string{}, subscribed: map[string]bool{}}
	supervisor.enforceLaneDeadlines()
	updated, err := readLaneStateFile(paths, threadID)
	if err != nil || updated.TimedOutTurnID != state.TurnID || updated.TerminalOutcome != "timed_out" {
		t.Fatalf("durable timeout state = %+v, %v", updated, err)
	}
	seenInterrupt := false
	for len(requests) > 0 {
		if <-requests == "turn/interrupt" {
			seenInterrupt = true
		}
	}
	if !seenInterrupt {
		t.Fatal("expired detached lane was not interrupted")
	}
	supervisor.recordLaneTurnTerminal(threadID, state.TurnID, "interrupted")
	updated, err = readLaneStateFile(paths, threadID)
	if err != nil || updated.DeadlineAt != 0 || updated.TerminalOutcome != "timed_out" {
		t.Fatalf("terminal timeout state = %+v, %v", updated, err)
	}
	supervisor.queueLaneTerminalNotice(threadID, map[string]any{"id": state.TurnID, "status": "interrupted"})
	noticeID := sessionKey("lane-terminal\x00" + threadID + "\x00" + state.TurnID)
	job := readJSONMap(filepath.Join(paths.profileRoot, "notices", noticeID+".json"))
	if stringValue(job["outcome"]) != "timed_out" || intValue(job["exit"]) != 124 {
		t.Fatalf("timeout notice = %#v", job)
	}
}

func TestSupervisorRestartReconstructsMissedTerminalNotice(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			params, _ := request["params"].(map[string]any)
			if stringValue(params["threadId"]) == "thread-transient" {
				return map[string]any{"data": []map[string]any{{
					"id": "turn-transient", "status": "interrupted", "startedAt": int64(10),
				}}}, nil
			}
			return map[string]any{"data": []map[string]any{{
				"id": "turn-missed", "status": "failed", "completedAt": int64(20), "durationMs": int64(10),
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	root := t.TempDir()
	paths := nativePaths{
		profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"),
		claudeRoot: filepath.Join(root, "claude"),
	}
	state := laneState{
		Type: "codex-peer-lane", Name: "missed-lane", ThreadID: "thread-missed", SessionID: "thread-missed",
		Cwd: root, Status: "in_progress", TurnID: "turn-missed", NotifyTarget: "orchestrator",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	supervisor.reconcileLaneTerminalNotices(client)
	noticeID := sessionKey("lane-terminal\x00" + state.ThreadID + "\x00" + state.TurnID)
	job := readJSONMap(filepath.Join(paths.profileRoot, "notices", noticeID+".json"))
	if stringValue(job["status"]) != "failed" || stringValue(job["target"]) != "orchestrator" {
		t.Fatalf("reconstructed terminal notice = %#v", job)
	}
	transient := laneState{
		Type: "codex-peer-lane", Name: "transient-lane", ThreadID: "thread-transient", SessionID: "thread-transient",
		Cwd: root, Status: "in_progress", TurnID: "turn-transient", NotifyTarget: "orchestrator",
	}
	if err := recordLaneState(paths, transient); err != nil {
		t.Fatal(err)
	}
	supervisor.reconcileLaneTerminalNotices(client)
	transientNoticeID := sessionKey("lane-terminal\x00" + transient.ThreadID + "\x00" + transient.TurnID)
	if transientJob := readJSONMap(filepath.Join(paths.profileRoot, "notices", transientNoticeID+".json")); transientJob != nil {
		t.Fatalf("transient projection reconstructed a false notice: %#v", transientJob)
	}
}

func TestPeerWakeTurnAdvancesDurableLaneCursor(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", Name: "wake-lane", ThreadID: "thread-wake", SessionID: "thread-wake",
		Cwd: root, Status: "completed", TurnID: "turn-initial", CollectedTurnID: "turn-initial",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	if err := supervisor.recordLaneTurnStarted(state.ThreadID, "turn-wake"); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-wake" || updated.LatestTurnID != "turn-wake" || updated.CollectedTurnID != "turn-initial" || updated.Status != "in_progress" {
		t.Fatalf("wake start projection = %+v, %v", updated, err)
	}
	supervisor.recordLaneTurnTerminal(state.ThreadID, "turn-wake", "completed")
	updated, err = readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.Status != "completed" {
		t.Fatalf("wake terminal projection = %+v, %v", updated, err)
	}
}

func TestObservedWakeRecoveryAdoptsDurableLaneTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", Name: "recovered-wake", ThreadID: "thread-recovered-wake", SessionID: "thread-recovered-wake",
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", CollectedTurnID: "turn-old",
		PendingQueueVer: 1, AutoArchive: true, AutoArchiveAt: time.Now().Add(time.Second).UnixMilli(),
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	if err := supervisor.adoptObservedWakeTurn(state.ThreadID, "turn-recovered"); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-recovered" || updated.LatestTurnID != "turn-recovered" ||
		updated.AutoArchiveAt != 0 || !reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-recovered"}) {
		t.Fatalf("observed wake was not adopted: %+v, %v", updated, err)
	}
}

func TestFailedWakeRestoresAutoArchiveGrace(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	priorDeadline := time.Now().Add(20 * time.Second).UnixMilli()
	state := laneState{
		Type: "codex-peer-lane", Name: "wake-fallback", ThreadID: "thread-wake-fallback", SessionID: "thread-wake-fallback",
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", CollectedTurnID: "turn-old",
		PendingQueueVer: 1, AutoArchive: true, AutoArchiveDelayMS: 2500,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	record := wakeRecord{
		SessionID: state.ThreadID, MessageID: "wake-fallback-id",
		PriorLatestTurnID: state.LatestTurnID, PriorAutoArchiveAt: priorDeadline,
	}
	supervisor.restoreLaneAutoArchive(record, false)
	restored, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || restored.AutoArchiveAt != priorDeadline {
		t.Fatalf("exact prior grace was not restored: %+v, %v", restored, err)
	}
	restored.AutoArchiveAt = 0
	if err := recordLaneState(paths, restored); err != nil {
		t.Fatal(err)
	}
	supervisor.restoreLaneAutoArchive(record, true)
	renewed, err := readLaneStateFile(paths, state.ThreadID)
	remaining := time.Until(time.UnixMilli(renewed.AutoArchiveAt))
	if err != nil || remaining < 2*time.Second || remaining > 3*time.Second {
		t.Fatalf("queued fallback did not receive a fresh grace: %+v, %v", renewed, err)
	}
	renewed.AutoArchiveAt = 0
	renewed.LatestTurnID = "turn-new"
	if err := recordLaneState(paths, renewed); err != nil {
		t.Fatal(err)
	}
	supervisor.restoreLaneAutoArchive(record, true)
	newer, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || newer.AutoArchiveAt != 0 {
		t.Fatalf("failed wake restored grace over a newer turn: %+v, %v", newer, err)
	}
}

func TestQueuedWakeRecoveryRestoresAutoArchiveGrace(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", Name: "queued-wake", ThreadID: "thread-queued-wake", SessionID: "thread-queued-wake",
		Status: "completed", TurnID: "turn-old", LatestTurnID: "turn-old", CollectedTurnID: "turn-old",
		PendingQueueVer: 1, AutoArchive: true, AutoArchiveDelayMS: 2500,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	record := wakeRecord{
		SessionID: state.ThreadID, MessageID: "queued-wake-id", State: "queued", Delivery: "queued",
		PriorLatestTurnID: state.LatestTurnID, PriorAutoArchiveAt: time.Now().Add(20 * time.Second).UnixMilli(),
		Item: map[string]any{"id": "queued-wake-id", "message": "queued"}, CreatedAt: time.Now().UnixMilli(),
	}
	if err := writeWakeRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	supervisor.recoverWakeRecords()
	updated, err := readLaneStateFile(paths, state.ThreadID)
	remaining := time.Until(time.UnixMilli(updated.AutoArchiveAt))
	if err != nil || remaining < 2*time.Second || remaining > 3*time.Second {
		t.Fatalf("queued wake recovery did not restore grace: %+v, %v", updated, err)
	}
}

func TestWaitCollectsTerminalTurnStrandedByPeerWake(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	threadID := "thread-wake-debt"
	state := laneState{
		Type: "codex-peer-lane", Name: "wake-debt", ThreadID: threadID, SessionID: threadID,
		Cwd: root, Status: "interrupted", TurnID: "turn-owed", LatestTurnID: "turn-owed",
		TimedOutTurnID: "turn-owed", TerminalOutcome: "timed_out", TerminalTurnID: "turn-owed",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths}
	if err := supervisor.recordLaneTurnStarted(threadID, "turn-wake-new"); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TurnID != "turn-owed" || updated.LatestTurnID != "turn-wake-new" || updated.TerminalOutcome != "timed_out" {
		t.Fatalf("peer wake overwrote uncollected debt: %+v", updated)
	}
	completedAt, duration := int64(2), int64(1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{
				{"id": "turn-wake-new", "status": "inProgress", "items": []any{}},
				{"id": "turn-owed", "status": "interrupted", "completedAt": completedAt, "durationMs": duration,
					"items": []map[string]any{{"id": "owed-final", "type": "agentMessage", "phase": "final_answer", "text": "OWED_FINAL"}}},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	status, code, collected, waitErr := waitForLaneTurnWithPolicy(client, &updated, time.Second, false, false)
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if waitErr != nil || status != "timed_out" || code != 124 || !collected {
		t.Fatalf("owed turn collection = status %q code %d collected=%v err=%v", status, code, collected, waitErr)
	}
	if answer := strings.Index(string(output), "OWED_FINAL"); answer < 0 || answer > strings.Index(string(output), "turn.completed") {
		t.Fatalf("owed final answer was not emitted before terminal: %s", output)
	}
	if err := persistLaneWaitState(paths, updated, true); err != nil {
		t.Fatal(err)
	}
	final, err := readLaneStateFile(paths, threadID)
	if err != nil || final.CollectedTurnID != "turn-owed" || final.LatestTurnID != "turn-wake-new" {
		t.Fatalf("owed cursor acknowledgement = %+v, %v", final, err)
	}
}

func TestInterruptPreservesOlderUncollectedTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", ThreadID: "thread-interrupt-debt", SessionID: "thread-interrupt-debt",
		Status: "interrupted", TurnID: "turn-owed", LatestTurnID: "turn-active",
		TimedOutTurnID: "turn-owed", TerminalOutcome: "timed_out", TerminalTurnID: "turn-owed",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := recordLaneInterrupted(paths, state.ThreadID, "turn-active"); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TurnID != "turn-owed" || updated.LatestTurnID != "turn-active" || updated.TerminalTurnID != "turn-owed" || updated.TerminalOutcome != "timed_out" {
		t.Fatalf("interrupt overwrote older collection debt: %+v", updated)
	}
}

func TestCollectingNewerTurnReplacesOlderDurableOutcome(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", ThreadID: "thread-new-outcome", SessionID: "thread-new-outcome",
		Status: "interrupted", TurnID: "turn-old", LatestTurnID: "turn-new", CollectedTurnID: "turn-old",
		TimedOutTurnID: "turn-old", TerminalOutcome: "timed_out", TerminalTurnID: "turn-old",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	collector := state
	collector.TurnID = "turn-new"
	collector.Status = "completed"
	if err := persistLaneWaitState(paths, collector, true); err != nil {
		t.Fatal(err)
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CollectedTurnID != "turn-new" || updated.TerminalTurnID != "turn-new" || updated.TerminalOutcome != "completed" || updated.TimedOutTurnID != "" {
		t.Fatalf("new collection retained stale terminal outcome: %+v", updated)
	}
}

func TestAcknowledgementClearsOnlyCollectedTurnSpool(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := laneState{
		Type: "codex-peer-lane", ThreadID: "thread-spool-queue", SessionID: "thread-spool-queue",
		Status: "completed", TurnID: "turn-first", LatestTurnID: "turn-second",
		PendingTurnIDs: []string{"turn-first", "turn-second"},
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	for _, turnID := range state.PendingTurnIDs {
		if err := persistLaneItem(paths, state.ThreadID, turnID, map[string]any{
			"id": turnID + "-answer", "type": "agentMessage", "phase": "final_answer", "text": turnID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := persistLaneWaitState(paths, state, true); err != nil {
		t.Fatal(err)
	}
	if items := readLaneItemSpool(paths, state.ThreadID, "turn-first"); len(items) != 0 {
		t.Fatalf("acknowledged turn retained %d spool items", len(items))
	}
	if items := readLaneItemSpool(paths, state.ThreadID, "turn-second"); len(items) != 1 {
		t.Fatalf("later turn spool was cleared with older acknowledgement: %d items", len(items))
	}
	updated, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil || updated.TurnID != "turn-second" || !reflect.DeepEqual(updated.PendingTurnIDs, []string{"turn-second"}) {
		t.Fatalf("collection queue did not advance one turn: %+v, %v", updated, err)
	}
}

func TestArchivedLaneStatusIsLocalAndRetainedForResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	state := laneState{
		Type: "codex-peer-lane", Name: "archived-lane", ThreadID: "thread-archived", SessionID: "thread-archived",
		Cwd: root, Status: "archived", TurnID: "turn-final", CollectedTurnID: "turn-final",
		TimedOutTurnID: "turn-final", TerminalOutcome: "timed_out",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := markRetiredThread(paths, state.ThreadID); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, statusErr := statusLaneNative(laneOptions{laneCommonOptions: laneCommonOptions{target: state.Name}})
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if statusErr != nil || code != 0 || !strings.Contains(string(output), `"status":"archived"`) ||
		!strings.Contains(string(output), `"outcome":"timed_out"`) || !strings.Contains(string(output), `"exit":124`) {
		t.Fatalf("archived local status = code %d err %v output %s", code, statusErr, output)
	}
	if _, err := readLaneStateFile(paths, state.ThreadID); err != nil {
		t.Fatalf("archived resume metadata was removed: %v", err)
	}
}

func TestLaneMetricSurvivesDetachedCollector(t *testing.T) {
	paths := nativePaths{dataRoot: t.TempDir()}
	threadID, turnID := "thread-metric", "turn-metric"
	usage := map[string]any{"total": map[string]any{"inputTokens": 12, "outputTokens": 3, "totalTokens": 15}}
	if err := writeJSONAtomic(laneMetricPath(paths, threadID, turnID), map[string]any{
		"threadId": threadID, "turnId": turnID, "tokenUsage": usage,
	}); err != nil {
		t.Fatal(err)
	}
	got := normalizeLaneUsage(readLaneMetric(paths, threadID, turnID)).(map[string]any)
	if got["input_tokens"] != float64(12) || got["total_tokens"] != float64(15) {
		t.Fatalf("persisted accounting = %#v", got)
	}
	started, completed, duration := int64(100), int64(103), int64(3000)
	accounting := laneAccounting(appTurn{StartedAt: &started, CompletedAt: &completed, DurationMS: &duration}, usage)
	costAvailable, costAvailabilityPresent := accounting["cost_available"].(bool)
	if accounting["duration_ms"] != &duration || accounting["started_at"] != &started ||
		accounting["completed_at"] != &completed || accounting["cost"] != nil || !costAvailabilityPresent || costAvailable {
		t.Fatalf("terminal accounting = %#v", accounting)
	}
}

func TestLaneWorktreeUsesCurrentRepositoryHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"},
	}
	for _, arguments := range commands {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "marker"), []byte("current\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repository, "add", "marker").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", repository, "-c", "commit.gpgsign=false", "commit", "-qm", "base").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	// Exercise the same lexical/physical path split as Darwin's /tmp ->
	// /private/tmp alias while keeping the regression portable to Linux.
	repositoryAlias := filepath.Join(t.TempDir(), "repository-alias")
	if err := os.Symlink(repository, repositoryAlias); err != nil {
		t.Fatal(err)
	}
	paths := nativePaths{dataRoot: filepath.Join(t.TempDir(), "state")}
	worktree, err := createLaneWorktree(paths, "isolated", repositoryAlias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = exec.Command("git", "-C", repository, "worktree", "remove", "--force", worktree).CombinedOutput()
	})
	body, err := os.ReadFile(filepath.Join(worktree, "marker"))
	if err != nil || string(body) != "current\n" {
		t.Fatalf("worktree did not use current HEAD: %q, %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "isolated-change"), []byte("lane only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "isolated-change")); !os.IsNotExist(err) {
		t.Fatalf("worktree write leaked into the caller checkout: %v", err)
	}
}

func TestDetachedCollectorSignalDoesNotInterruptTurn(t *testing.T) {
	state := laneState{ThreadID: "thread-detached-signal", TurnID: "turn-detached-signal", Status: "in_progress"}
	interrupts := 0
	status := laneWaitSignalStatus(state, false, func() { interrupts++ })
	if status != "in_progress" || interrupts != 0 {
		t.Fatalf("detached signal = status %q interrupts=%d", status, interrupts)
	}
	status = laneWaitSignalStatus(state, true, func() { interrupts++ })
	if status != "interrupted" || interrupts != 1 {
		t.Fatalf("owned signal = status %q interrupts=%d", status, interrupts)
	}
}

func TestReconcilePreservesAuthoritativePermissionMode(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), claudeRoot: filepath.Join(root, "claude"),
		codexHome: filepath.Join(root, "codex"), runtimeDir: filepath.Join(root, "run"),
		supervisorSock: filepath.Join(root, "supervisor.sock"),
	}
	threadID := "00000000-0000-0000-0000-000000000902"
	writeTestActiveCodexLane(t, paths, threadID)
	socket := filepath.Join(root, "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stateFile := filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json")
	if err := writeJSONAtomic(stateFile, map[string]any{
		"sessionId": threadID, "cwd": root, "name": "permission-peer", "nameSource": "explicit",
		"permissionMode": "default", "socketPath": socket, "stableSocket": socket, "status": "idle",
	}); err != nil {
		t.Fatal(err)
	}
	s := nativeSupervisor{paths: paths, shims: map[string]map[string]any{}, procStart: readProcStart(os.Getpid())}
	if _, err := s.ensureShim(map[string]any{"sessionId": threadID, "cwd": root, "name": "permission-peer", "status": "idle"}); err != nil {
		t.Fatal(err)
	}
	state := readJSONMap(stateFile)
	if got := stringValue(state["permissionMode"]); got != "default" {
		t.Fatalf("reconciliation widened permission mode to %q", got)
	}
}

func TestQueuedWakeRecoveryRecreatesInboxExactlyOnce(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-0000-0000-000000000903"
	writeTestActiveCodexLane(t, paths, threadID)
	item := map[string]any{"type": "message", "id": "wake-recovery-message", "message": "RECOVER_QUEUED_WAKE"}
	record := wakeRecord{
		SessionID: threadID, MessageID: stringValue(item["id"]), Fingerprint: wakeItemFingerprint(item),
		State: "queueing", Delivery: "queued", Item: item, CreatedAt: 123,
	}
	if err := writeWakeRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	s := nativeSupervisor{paths: paths, procStart: readProcStart(os.Getpid())}
	s.recoverWakeRecords()
	messages, err := consumeNativeInbox(paths, threadID)
	if err != nil || len(messages) != 1 || stringValue(messages[0]["message"]) != "RECOVER_QUEUED_WAKE" {
		t.Fatalf("recovered inbox = %#v err=%v", messages, err)
	}
	if got := readWakeRecord(paths, threadID, record.MessageID); got == nil || got.State != "fallback_delivered" {
		t.Fatalf("recovered wake ledger = %+v", got)
	}
	s.recoverWakeRecords()
	messages, err = consumeNativeInbox(paths, threadID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("recovery duplicated consumed wake = %#v err=%v", messages, err)
	}
}

func TestSessionEndIsInertWithoutAttachmentIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	paths := resolveNativePaths()
	managedID := "00000000-0000-0000-0000-000000000904"
	if err := ensurePrivateRuntimeDir(filepath.Dir(paths.supervisorSock)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.supervisorSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan map[string]any, 1)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			scanner := bufio.NewScanner(connection)
			if scanner.Scan() {
				var request map[string]any
				if json.Unmarshal(scanner.Bytes(), &request) == nil {
					received <- request
					_, _ = connection.Write([]byte("{\"ok\":true}\n"))
					_ = connection.Close()
					return
				}
			}
			_ = connection.Close()
		}
	}()
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(managedID), "state.json")
	if err := writeJSONAtomic(statePath, map[string]any{
		"sessionId": managedID, "supervisorSocket": paths.supervisorSock,
	}); err != nil {
		t.Fatal(err)
	}
	output, err := handleNativeHook(hookInput{Event: "SessionEnd", SessionID: managedID, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if output != nil {
		t.Fatalf("SessionEnd output = %#v", output)
	}
	select {
	case request := <-received:
		t.Fatalf("SessionEnd mutated supervisor state: %#v", request)
	case <-time.After(100 * time.Millisecond):
	}

	standaloneID := "00000000-0000-0000-0000-000000000905"
	standalone := newDaemon(map[string]string{
		"session-id": standaloneID, "cwd": root, "name": "standalone", "data-dir": paths.dataRoot,
		"claude-config-dir": paths.claudeRoot, "codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
	})
	if err := standalone.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(standalone.shutdown)
	if _, err := handleNativeHook(hookInput{Event: "SessionEnd", SessionID: standaloneID, Cwd: root}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-standalone.done:
		t.Fatal("SessionEnd stopped a shim without attachment identity")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSupervisorReleaseStopsShimAndParksInteractiveThread(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000906"
	operations := make(chan string, 2)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/archive", "thread/unarchive":
			params, _ := request["params"].(map[string]any)
			operations <- stringValue(request["method"]) + ":" + stringValue(params["threadId"])
			if stringValue(request["method"]) == "thread/unarchive" {
				return map[string]any{"thread": map[string]any{"id": threadID, "status": map[string]any{"type": "notLoaded"}}}, nil
			}
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
		runtimeDir: filepath.Join(root, "run"), appServerSock: appSocket,
		supervisorSock: filepath.Join(root, "run", "supervisor.sock"),
	}
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "managed", "data-dir": paths.dataRoot,
		"claude-config-dir": paths.claudeRoot, "codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
		"supervisor-socket": paths.supervisorSock,
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shim.shutdown)
	state := readJSONMap(shim.stateFile)
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{threadID: state},
		activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
		releasing: map[string]int64{}, retired: map[string]bool{},
	}
	if err := supervisor.releaseInteractiveThread(threadID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shim.done:
	case <-time.After(time.Second):
		t.Fatal("release did not stop the interactive shim")
	}
	for _, want := range []string{"thread/archive:" + threadID, "thread/unarchive:" + threadID} {
		select {
		case got := <-operations:
			if got != want {
				t.Fatalf("release operation = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("release did not perform %s", want)
		}
	}
	if !supervisor.interactiveReleasePending(threadID) {
		t.Fatal("release did not suppress reconciliation during SessionEnd")
	}
	if pending := supervisor.reconcileInteractiveReleases(); !pending[threadID] || !supervisor.interactiveReleasePending(threadID) {
		t.Fatalf("release grace did not cover delayed archive notifications: %#v", pending)
	}
	archivedParams, _ := json.Marshal(map[string]any{"threadId": threadID})
	supervisor.handleNotification(rpcNotification{Method: "thread/archived", Params: archivedParams})
	if supervisor.isRetired(threadID) {
		t.Fatal("transient SessionEnd archive created a durable retirement marker")
	}
	supervisor.releasing[threadID] = time.Now().Add(-time.Second).UnixMilli()
	if pending := supervisor.reconcileInteractiveReleases(); len(pending) != 0 || supervisor.interactiveReleasePending(threadID) {
		t.Fatalf("expired release grace was not cleared: %#v", pending)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestSupervisorReapsShimAfterCorrelatedTUIOwnerExits(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000908"
	operations := make(chan string, 2)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/archive", "thread/unarchive":
			params, _ := request["params"].(map[string]any)
			operations <- stringValue(request["method"]) + ":" + stringValue(params["threadId"])
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
		runtimeDir: filepath.Join(root, "run"), appServerSock: appSocket,
		supervisorSock: filepath.Join(root, "run", "supervisor.sock"),
	}
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "managed-owner", "data-dir": paths.dataRoot,
		"claude-config-dir": paths.claudeRoot, "codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
		"supervisor-socket": paths.supervisorSock,
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shim.shutdown)
	owner := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "dead_owner_request", OwnerPID: 1_000_000_000,
		OwnerProcStart: "dead", UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), owner); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{threadID: readJSONMap(shim.stateFile)},
		activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
		releasing: map[string]int64{}, retired: map[string]bool{},
	}
	supervisor.reconcileExitedInteractiveOwners()
	select {
	case <-shim.done:
	case <-time.After(time.Second):
		t.Fatal("dead interactive TUI owner did not stop its shim")
	}
	for _, want := range []string{"thread/archive:" + threadID, "thread/unarchive:" + threadID} {
		select {
		case got := <-operations:
			if got != want {
				t.Fatalf("owner-exit operation = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("owner exit did not perform %s", want)
		}
	}
	if current := readInteractiveOwner(paths, threadID); current != nil {
		t.Fatalf("dead interactive owner record survived release: %#v", current)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestSupervisorSynchronouslyReleasesOnlyStaleAttachedOwner(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-00000000090a"
	operations := make(chan string, 2)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/archive", "thread/unarchive":
			operations <- stringValue(request["method"])
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
		runtimeDir: filepath.Join(root, "run"), appServerSock: appSocket,
	}
	stale := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "stale-owner", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), stale); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, releasing: map[string]int64{}, retired: map[string]bool{},
	}
	if err := supervisor.releaseStaleInteractiveOwner(threadID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"thread/archive", "thread/unarchive"} {
		select {
		case got := <-operations:
			if got != want {
				t.Fatalf("release operation = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s", want)
		}
	}
	if owner := readInteractiveOwner(paths, threadID); owner != nil {
		t.Fatalf("stale attached owner survived synchronous release: %#v", owner)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestSupervisorInteractiveOwnerRecordRejectsStaleRelease(t *testing.T) {
	paths := nativePaths{dataRoot: filepath.Join(t.TempDir(), "state"), profileRoot: filepath.Join(t.TempDir(), "profile")}
	threadID := "00000000-0000-0000-0000-000000000909"
	current := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "current_owner_request", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), current); err != nil {
		t.Fatal(err)
	}
	stale := current
	stale.RequestID = "stale_owner_request"
	supervisor := nativeSupervisor{paths: paths, done: make(chan struct{}), releasing: map[string]int64{}, retired: map[string]bool{}}
	supervisor.ownerMu.Lock()
	err := supervisor.releaseInteractiveThreadLocked(threadID, &stale)
	supervisor.ownerMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := readInteractiveOwner(paths, threadID); !sameInteractiveOwner(got, &current) {
		t.Fatalf("stale owner exit replaced or removed current owner: %#v", got)
	}
}

func TestNativeSupervisorMatchRequiresExactRuntimeIdentity(t *testing.T) {
	paths := nativePaths{appServerSock: filepath.Join(t.TempDir(), "app-server.sock")}
	status := map[string]any{
		"ready": true, "implementation": "go", "pluginVersion": "v1",
		"runtimeIdentity": "sha256:current", "appServerSocket": paths.appServerSock,
	}
	if !nativeSupervisorMatches(status, paths, "v1", "sha256:current") {
		t.Fatal("matching supervisor identity was rejected")
	}
	for _, mutate := range []func(map[string]any){
		func(row map[string]any) { delete(row, "runtimeIdentity") },
		func(row map[string]any) { row["runtimeIdentity"] = "sha256:old" },
		func(row map[string]any) { row["pluginVersion"] = "v0" },
		func(row map[string]any) { row["ready"] = false },
	} {
		candidate := map[string]any{}
		for key, value := range status {
			candidate[key] = value
		}
		mutate(candidate)
		if nativeSupervisorMatches(candidate, paths, "v1", "sha256:current") {
			t.Fatalf("stale supervisor identity was accepted: %#v", candidate)
		}
	}
}

func TestSupervisorSessionReleaseDoesNotStopCodexLane(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000907"
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
		runtimeDir: filepath.Join(root, "run"), appServerSock: filepath.Join(root, "missing-appserver.sock"),
		supervisorSock: filepath.Join(root, "run", "supervisor.sock"),
	}
	if err := recordLaneState(paths, laneState{
		Type: "codex-peer-lane", ThreadID: threadID, SessionID: threadID, Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "lane", "data-dir": paths.dataRoot,
		"claude-config-dir": paths.claudeRoot, "codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
		"supervisor-socket": paths.supervisorSock,
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shim.shutdown)
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}),
		shims:       map[string]map[string]any{threadID: readJSONMap(shim.stateFile)},
		activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
		releasing: map[string]int64{}, retired: map[string]bool{},
	}
	if err := supervisor.releaseInteractiveThread(threadID); err != nil {
		t.Fatal(err)
	}
	if !probeUnixSocket(shim.stableSocket, 250*time.Millisecond) {
		t.Fatal("SessionEnd release stopped a Codex lane shim")
	}
	if supervisor.interactiveReleasePending(threadID) {
		t.Fatal("SessionEnd release suppressed Codex lane reconciliation")
	}
}
