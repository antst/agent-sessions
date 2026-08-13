package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isolateNativeLaneTest(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
}

func TestNativeLaneCLIRequiresNameAndLeavesPolicyOptional(t *testing.T) {
	if _, err := parseLaneArgs([]string{"run", "-"}); err == nil {
		t.Fatal("run without --name succeeded")
	}
	options, err := parseLaneArgs([]string{"run", "--name", "review-a", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if options.name != "review-a" || options.model != "" || options.effort != "" || options.sandbox != "" || options.approvalPolicy != "" || options.web != nil {
		t.Fatalf("unexpected defaults: %+v", options)
	}
	listed, err := parseLaneArgs([]string{"list", "--all", "--mine", "--json"})
	if err != nil || !listed.all || !listed.mine || listed.command != "list" {
		t.Fatalf("list options = %+v, %v", listed, err)
	}
	if _, err := parseLaneArgs([]string{"status", "lane", "--mine"}); err == nil {
		t.Fatal("status accepted list-only --mine")
	}
	doctor, err := parseLaneArgs([]string{"doctor", "--json"})
	if err != nil || doctor.command != "doctor" {
		t.Fatalf("doctor options = %+v, %v", doctor, err)
	}
}

func TestNativeLaneNotifyFlagsAreExplicit(t *testing.T) {
	disabled, err := parseLaneArgs([]string{"start", "--name", "review-a", "--no-notify", "-"})
	if err != nil || !disabled.disableNotify {
		t.Fatalf("disabled notify options = %+v, %v", disabled, err)
	}
	if _, err := parseLaneArgs([]string{"start", "--name", "review-a", "--notify", "parent", "--no-notify", "-"}); err == nil {
		t.Fatal("conflicting notify options were accepted")
	}
	if _, err := parseLaneArgs([]string{"start", "--name", "review-a", "--notify", "parent", "-"}); err == nil {
		t.Fatal("non-persistent explicit notify was accepted")
	}
	if resumed, err := parseLaneArgs([]string{"resume", "review-a", "--notify", "parent", "-"}); err != nil || !resumed.notifyExplicit {
		t.Fatalf("persistent resume notification override was rejected before durable state resolution: %#v, %v", resumed, err)
	}
	persistent, err := parseLaneArgs([]string{"start", "--name", "review-a", "--persistent", "--notify", "parent", "-"})
	if err != nil || !persistent.persistent || persistent.notifyTarget != "parent" {
		t.Fatalf("persistent notify options = %+v, %v", persistent, err)
	}
	autoArchive, err := parseLaneArgs([]string{"start", "--name", "review-a", "-"})
	if err != nil || !autoArchive.autoArchive || autoArchive.autoArchiveDelay != time.Minute {
		t.Fatalf("default auto-archive options = %+v, %v", autoArchive, err)
	}
	customArchive, err := parseLaneArgs([]string{"start", "--name", "review-a", "--auto-archive-after", "2.5", "-"})
	if err != nil || !customArchive.autoArchive || customArchive.autoArchiveDelay != 2500*time.Millisecond || !customArchive.autoArchiveCustom {
		t.Fatalf("custom auto-archive options = %+v, %v", customArchive, err)
	}
	retained, err := parseLaneArgs([]string{"start", "--name", "review-a", "--no-auto-archive", "-"})
	if err != nil || retained.autoArchive {
		t.Fatalf("disabled auto-archive options = %+v, %v", retained, err)
	}
	for _, invalid := range []string{"0", "0.0001", "NaN", "Inf", "9.223372036854776e9", "1e20"} {
		if _, err := parseLaneArgs([]string{"start", "--name", "review-a", "--auto-archive-after", invalid, "-"}); err == nil {
			t.Fatalf("invalid auto-archive delay %q was accepted", invalid)
		}
	}
	if _, err := parseLaneArgs([]string{"start", "--name", "review-a", "--timeout", "9.223372036854776e9", "-"}); err == nil {
		t.Fatal("overflowing turn timeout was accepted")
	}
	if _, err := parseLaneArgs([]string{"start", "--name", "review-a", "--auto-archive-after", "5", "--no-auto-archive", "-"}); err == nil {
		t.Fatal("conflicting auto-archive options were accepted")
	}
	if _, err := parseLaneArgs([]string{"wait", "review-a", "--auto-archive-after", "5"}); err == nil {
		t.Fatal("wait accepted a lane lifecycle configuration flag")
	}
}

func TestResumeLifecycleOptionsPreservePersistentPolicyUnlessExplicitlyChanged(t *testing.T) {
	state := laneState{Persistent: true, NotifyTarget: "session:old", AutoArchive: false, AutoArchiveDelayMS: 7_000}
	applyLaneLifecycleOptions(&state, laneOptions{ownerPID: 42, ownerProcStart: "start", ownerSessionID: "session:new", notifyTarget: "session:new", autoArchive: true, autoArchiveDelay: 3 * time.Minute})
	if !state.Persistent || state.NotifyTarget != "session:old" || state.OwnerPID != 0 || state.OwnerSessionID != "" || state.AutoArchive || state.AutoArchiveDelayMS != 7_000 {
		t.Fatalf("implicit resume changed persistent lifecycle = %+v", state)
	}
	state = laneState{NotifyTarget: "session:old", AutoArchive: true, AutoArchiveDelayMS: 7_000}
	applyLaneLifecycleOptions(&state, laneOptions{ownerPID: 42, ownerProcStart: "start", ownerSessionID: "session:new", notifyTarget: "session:new", autoArchive: true, autoArchiveDelay: 3 * time.Minute})
	if state.NotifyTarget != "session:new" || state.OwnerPID != 42 || state.OwnerSessionID != "session:new" || state.Persistent || !state.AutoArchive || state.AutoArchiveDelayMS != 7_000 {
		t.Fatalf("parent-owned lifecycle = %+v", state)
	}
	applyLaneLifecycleOptions(&state, laneOptions{persistent: true, persistentSet: true, notifyTarget: "coordinator", notifyExplicit: true})
	if state.NotifyTarget != "coordinator" || !state.Persistent || state.OwnerPID != 0 || state.OwnerSessionID != "" {
		t.Fatalf("persistent lifecycle = %+v", state)
	}
	applyLaneLifecycleOptions(&state, laneOptions{autoArchive: true, autoArchiveDelay: 3 * time.Minute, autoArchiveCustom: true})
	if !state.AutoArchive || state.AutoArchiveDelayMS != 180_000 {
		t.Fatalf("explicit auto-archive lifecycle = %+v", state)
	}
	applyLaneLifecycleOptions(&state, laneOptions{noAutoArchiveSet: true})
	if state.AutoArchive {
		t.Fatalf("explicit no-auto-archive lifecycle = %+v", state)
	}
}

func TestWaitArchivedLaneExplainsPriorAnswerIsUnrecoverable(t *testing.T) {
	isolateNativeLaneTest(t)
	paths := resolveNativePaths()
	state := laneState{
		Type: "codex-peer-lane", Name: "archived-result", ThreadID: "thread-archived-result",
		SessionID: "thread-archived-result", Status: "archived", AutoArchive: true,
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	code, err := waitLaneNative(laneOptions{target: state.ThreadID})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "wait cannot recover an uncollected prior turn") ||
		!strings.Contains(err.Error(), "resume starts a new follow-up turn") {
		t.Fatalf("archived wait result = code %d, err %v", code, err)
	}
}

func TestReadLaneStatesRejectsSpoofedFilenameAndType(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	directory := filepath.Join(profileDataRoot(paths), "lanes")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	valid := laneState{Type: "codex-peer-lane", ThreadID: "thread-valid", SessionID: "thread-valid", Name: "valid"}
	if err := writeJSONAtomic(filepath.Join(directory, sessionKey(valid.ThreadID)+".json"), valid); err != nil {
		t.Fatal(err)
	}
	spoofed := valid
	spoofed.ThreadID, spoofed.SessionID, spoofed.Name = "thread-spoofed", "thread-spoofed", "spoofed"
	if err := writeJSONAtomic(filepath.Join(directory, "attacker.json"), spoofed); err != nil {
		t.Fatal(err)
	}
	foreign := valid
	foreign.Type, foreign.ThreadID, foreign.SessionID = "foreign", "thread-foreign", "thread-foreign"
	if err := writeJSONAtomic(filepath.Join(directory, sessionKey(foreign.ThreadID)+".json"), foreign); err != nil {
		t.Fatal(err)
	}
	states := readLaneStates(paths)
	if len(states) != 1 || states[0].ThreadID != valid.ThreadID {
		t.Fatalf("lane discovery accepted spoofed durable state: %#v", states)
	}
}

func TestInferClaudeParentNotifyTargetRequiresLiveCorroboratedAncestor(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{claudeRoot: filepath.Join(root, "claude")}
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", paths.claudeRoot)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CODEX_THREAD_ID", "")
	socket := filepath.Join(root, "claude.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	pid := os.Getpid()
	sessionID := "claude-session-test"
	if err := writeJSONAtomic(filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(pid)+".json"), peerSession{
		SessionID: sessionID, ProcStart: readProcStart(pid), MessagingSocketPath: socket,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessionID)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	t.Setenv("CLAUDE_PID", strconv.Itoa(pid))
	if got := inferClaudeParentNotifyTarget(paths, pid); got != "session:"+sessionID {
		t.Fatalf("inferred target = %q", got)
	}
	owned := withLaneLaunchContext(laneOptions{command: "start"})
	if owned.ownerPID != pid || owned.ownerSessionID != sessionID || owned.notifyTarget != "session:"+sessionID || owned.persistent {
		t.Fatalf("automatic parent-owned context = %+v", owned)
	}
	persistent := withLaneLaunchContext(laneOptions{command: "start", persistent: true})
	if persistent.ownerPID != 0 || persistent.ownerSessionID != "" || persistent.notifyTarget != "" || !persistent.persistent {
		t.Fatalf("persistent context = %+v", persistent)
	}
	mine := withLaneLaunchContext(laneOptions{command: "list", mine: true})
	if mine.ownerPID != pid || mine.ownerProcStart != readProcStart(pid) || mine.ownerSessionID != sessionID {
		t.Fatalf("mine context = %+v", mine)
	}
	if got := inferClaudeParentNotifyTarget(paths, 1); got != "" {
		t.Fatalf("inherited environment without matching ancestry inferred %q", got)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", filepath.Join(root, "other.sock"))
	if got := inferClaudeParentNotifyTarget(paths, pid); got != "" {
		t.Fatalf("mismatched socket inferred %q", got)
	}
	unresolved := withLaneLaunchContext(laneOptions{command: "list", mine: true})
	if unresolved.ownerPID != 0 || unresolved.ownerProcStart != "" {
		t.Fatalf("unresolved --mine fell back to a transient parent: %+v", unresolved)
	}
	fallback := withLaneLaunchContext(laneOptions{command: "start"})
	if fallback.ownerPID != 0 || fallback.ownerProcStart != "" {
		t.Fatalf("ordinary shell unexpectedly became a lifecycle owner: %+v", fallback)
	}
}

func TestProcessHasAncestorWalksToParent(t *testing.T) {
	if runtimeGOOS() != "linux" && runtimeGOOS() != "darwin" {
		t.Skip("process ancestry is supported on Linux and macOS")
	}
	parent := os.Getppid()
	if parent <= 1 {
		t.Skip("test process has no inspectable parent")
	}
	if !processHasAncestor(os.Getpid(), parent) {
		t.Fatalf("pid %d did not resolve parent %d", os.Getpid(), parent)
	}
}

func TestNativeLaneCLIExposesOrchestratorPolicy(t *testing.T) {
	options, err := parseLaneArgs([]string{
		"start", "-n", "implementer", "-C", "/work/tree", "-m", "gpt-test", "--effort", "high",
		"--sandbox", "workspace-write", "--approval-policy", "never", "--web",
		"-c", "features.alpha=true", "-c", "label=\"packet\"", "--timeout", "12.5", "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	params, err := laneThreadStartParams(options)
	if err != nil {
		t.Fatal(err)
	}
	config := params["config"].(map[string]any)
	expected := map[string]any{
		"features": map[string]any{"alpha": true, "code_mode_host": false}, "label": "packet",
		"tools": map[string]any{"web_search": true},
	}
	if !reflect.DeepEqual(config, expected) {
		t.Fatalf("config = %#v, want %#v", config, expected)
	}
	turn := laneTurnStartParams(options, "thread-a", "prompt")
	if turn["effort"] != "high" || turn["approvalPolicy"] != "never" {
		t.Fatalf("turn policy = %#v", turn)
	}
	if turn["sandboxPolicy"].(map[string]any)["type"] != "workspaceWrite" {
		t.Fatalf("sandbox policy = %#v", turn["sandboxPolicy"])
	}
}

func TestNativeLaneNormalizesAppServerVocabulary(t *testing.T) {
	if got := snakeCaseNative("agentMessage"); got != "agent_message" {
		t.Fatalf("snakeCaseNative(agentMessage) = %q", got)
	}
	if got := normalizeStatus("inProgress"); got != "in_progress" {
		t.Fatalf("normalizeStatus(inProgress) = %q", got)
	}
}

func TestLaneTerminalRequiresCompletionEvidence(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	state := laneState{ThreadID: "thread-evidence", TurnID: "turn-evidence", Status: "in_progress"}
	started := int64(10)
	transient := appTurn{ID: state.TurnID, Status: "interrupted", StartedAt: &started}
	if status, collectable := laneCollectableTerminal(paths, state, transient); status != "interrupted" || collectable {
		t.Fatalf("transient terminal = %q collectable=%v", status, collectable)
	}
	completed := int64(20)
	failed := appTurn{ID: state.TurnID, Status: "failed", CompletedAt: &completed}
	if status, collectable := laneCollectableTerminal(paths, state, failed); status != "failed" || !collectable {
		t.Fatalf("persisted failed terminal = %q collectable=%v", status, collectable)
	}
	if err := persistLaneTerminalObservation(paths, state.ThreadID, map[string]any{
		"id": state.TurnID, "status": "interrupted",
	}); err != nil {
		t.Fatal(err)
	}
	if status, collectable := laneCollectableTerminal(paths, state, transient); status != "interrupted" || !collectable {
		t.Fatalf("observed terminal = %q collectable=%v", status, collectable)
	}
}

func TestLaneListIncludesArchivedOnlyWhenRequested(t *testing.T) {
	root := t.TempDir()
	ownerPID, ownerProcStart := os.Getpid(), readProcStart(os.Getpid())
	active := laneState{Type: "codex-peer-lane", Name: "active-lane", ThreadID: "thread-active", SessionID: "thread-active", Status: "in_progress", OwnerPID: ownerPID, OwnerProcStart: ownerProcStart}
	archived := laneState{Type: "codex-peer-lane", Name: "archived-lane", ThreadID: "thread-archived", SessionID: "thread-archived", Status: "archived", OwnerPID: ownerPID, OwnerProcStart: ownerProcStart}
	foreign := laneState{Type: "codex-peer-lane", Name: "foreign-lane", ThreadID: "thread-foreign", SessionID: "thread-foreign", Status: "idle", OwnerPID: ownerPID, OwnerProcStart: "different-process-start"}
	persistent := laneState{Type: "codex-peer-lane", Name: "persistent-lane", ThreadID: "thread-persistent", SessionID: "thread-persistent", Status: "idle", Persistent: true, OwnerPID: ownerPID, OwnerProcStart: ownerProcStart}
	invalid := laneState{Name: "partial-test-fixture", ThreadID: "thread-invalid", Status: "completed"}
	t.Setenv("CLAUDE_PEER_DATA_DIR", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	// resolveNativePaths adds a profile directory, so write the fixture there.
	resolved := resolveNativePaths()
	for _, state := range []laneState{active, archived, foreign, persistent, invalid} {
		if err := recordLaneState(resolved, state); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(options laneOptions) string {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = write
		code, listErr := listLanesNative(options)
		_ = write.Close()
		os.Stdout = original
		body, _ := io.ReadAll(read)
		_ = read.Close()
		if code != 0 || listErr != nil {
			t.Fatalf("list options=%+v: code=%d err=%v", options, code, listErr)
		}
		return string(body)
	}
	current := capture(laneOptions{})
	if !strings.Contains(current, "active-lane") || strings.Contains(current, "archived-lane") ||
		strings.Contains(current, "partial-test-fixture") {
		t.Fatalf("active list = %s", current)
	}
	all := capture(laneOptions{all: true})
	if !strings.Contains(all, "active-lane") || !strings.Contains(all, "archived-lane") ||
		!strings.Contains(all, `"contract_version":2`) {
		t.Fatalf("all list = %s", all)
	}
	mine := capture(laneOptions{mine: true, ownerPID: ownerPID, ownerProcStart: ownerProcStart})
	if !strings.Contains(mine, "active-lane") || strings.Contains(mine, "foreign-lane") || strings.Contains(mine, "persistent-lane") || strings.Contains(mine, "archived-lane") {
		t.Fatalf("mine list = %s", mine)
	}
	mineAll := capture(laneOptions{mine: true, all: true, ownerPID: ownerPID, ownerProcStart: ownerProcStart})
	if !strings.Contains(mineAll, "active-lane") || !strings.Contains(mineAll, "archived-lane") || strings.Contains(mineAll, "foreign-lane") || strings.Contains(mineAll, "persistent-lane") {
		t.Fatalf("mine all list = %s", mineAll)
	}
}

func TestArchiveInterruptsActiveLaneAndPersistsTerminalOutcome(t *testing.T) {
	for _, test := range []struct {
		name             string
		historyStatuses  []string
		interruptFailure bool
		completionProof  bool
		wantInterrupt    bool
		wantArchive      bool
		wantOutcome      string
	}{
		{name: "active turn", historyStatuses: []string{"inProgress"}, wantInterrupt: true, wantArchive: true, wantOutcome: "interrupted"},
		{name: "interrupted before local persistence", historyStatuses: []string{"interrupted"}, completionProof: true, wantArchive: true, wantOutcome: "interrupted"},
		{name: "natural completion races interrupt", historyStatuses: []string{"inProgress", "completed"}, interruptFailure: true, completionProof: true, wantInterrupt: true, wantArchive: true, wantOutcome: "completed"},
		{name: "failed interrupt leaves turn active", historyStatuses: []string{"inProgress", "inProgress"}, interruptFailure: true, wantInterrupt: true},
		{name: "projection-only interrupted row is refused", historyStatuses: []string{"interrupted"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			threadID, turnID := "thread-archive-active", "turn-archive-active"
			methods := make(chan string, 8)
			listCalls := 0
			_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				method := stringValue(request["method"])
				if method != "initialize" {
					methods <- method
				}
				switch method {
				case "thread/turns/list":
					index := listCalls
					if index >= len(test.historyStatuses) {
						index = len(test.historyStatuses) - 1
					}
					listCalls++
					status := test.historyStatuses[index]
					turn := map[string]any{"id": turnID, "status": status}
					if test.completionProof && statusTerminal(normalizeStatus(status)) {
						turn["durationMs"] = 1
					}
					if test.completionProof && normalizeStatus(status) == "completed" {
						turn["items"] = []map[string]any{{"id": "item-final", "type": "agentMessage", "phase": "final_answer", "text": "done"}}
					}
					return map[string]any{"data": []map[string]any{turn}}, nil
				case "turn/interrupt":
					if test.interruptFailure {
						return nil, errors.New("turn already changed")
					}
					return map[string]any{}, nil
				case "thread/archive":
					return map[string]any{}, nil
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
						response := map[string]any{"ok": true}
						if stringValue(request["action"]) == "flush_notices" {
							response["pending"] = 0
						}
						body, _ := json.Marshal(response)
						_, _ = connection.Write(append(body, '\n'))
					}()
				}
			}()
			t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", appSocket)
			t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
			t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
			t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			paths := resolveNativePaths()
			state := laneState{
				Type: "codex-peer-lane", Name: "archive-active", ThreadID: threadID, SessionID: threadID,
				Status: "in_progress", TurnID: turnID, LatestTurnID: turnID,
				PendingTurnIDs: []string{turnID}, PendingQueueVer: 1,
			}
			if err := recordLaneState(paths, state); err != nil {
				t.Fatal(err)
			}
			code, archiveErr := archiveLaneNative(laneOptions{target: threadID})
			if test.wantArchive && (code != 0 || archiveErr != nil) {
				t.Fatalf("archive active lane = code %d err %v", code, archiveErr)
			}
			if !test.wantArchive && (code == 0 || archiveErr == nil) {
				t.Fatalf("archive unexpectedly succeeded = code %d err %v", code, archiveErr)
			}
			seen := []string{}
			for len(methods) > 0 {
				seen = append(seen, <-methods)
			}
			interruptIndex, archiveIndex := -1, -1
			for index, method := range seen {
				if method == "turn/interrupt" {
					interruptIndex = index
				}
				if method == "thread/archive" {
					archiveIndex = index
				}
			}
			if (test.wantArchive && archiveIndex < 0) || (!test.wantArchive && archiveIndex >= 0) ||
				(test.wantInterrupt && interruptIndex < 0) || (!test.wantInterrupt && interruptIndex >= 0) ||
				(archiveIndex >= 0 && interruptIndex > archiveIndex) {
				t.Fatalf("archive method order = %#v", seen)
			}
			latest, err := readLaneStateFile(paths, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantArchive && (latest.Status != "archived" || latest.TerminalTurnID != turnID || latest.TerminalOutcome != test.wantOutcome) {
				t.Fatalf("archived active lane state = %#v", latest)
			}
			if !test.wantArchive && (latest.Status != "in_progress" || latest.TerminalOutcome != "") {
				t.Fatalf("refused archive mutated active lane state = %#v", latest)
			}
		})
	}
}

func TestArchivePreservesTimedOutTurnOutcome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	state := laneState{
		Type: "codex-peer-lane", ThreadID: "thread-archive-timeout", SessionID: "thread-archive-timeout",
		Status: "timed_out", TurnID: "turn-archive-timeout", LatestTurnID: "turn-archive-timeout",
		PendingTurnIDs: []string{"turn-archive-timeout"}, PendingQueueVer: 1,
		TerminalTurnID: "turn-archive-timeout", TerminalOutcome: "timed_out", TimedOutTurnID: "turn-archive-timeout",
	}
	if err := recordLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := recordLaneArchiveTerminal(paths, state.ThreadID, state.TurnID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	latest, err := readLaneStateFile(paths, state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.TerminalOutcome != "timed_out" || laneExitCode(latest.TerminalOutcome) != 124 {
		t.Fatalf("timed-out archive state = %#v", latest)
	}
}

func TestLaneDoctorReportsContractWhenServicesAreUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", filepath.Join(root, "missing-app-server.sock"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "missing-supervisor.sock"))
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, doctorErr := doctorLaneNative()
	_ = write.Close()
	os.Stdout = original
	body, _ := io.ReadAll(read)
	_ = read.Close()
	if doctorErr != nil || code != 1 {
		t.Fatalf("doctor unavailable = code %d err %v", code, doctorErr)
	}
	text := string(body)
	if !strings.Contains(text, `"type":"lane.doctor"`) ||
		!strings.Contains(text, `"contract_version":2`) ||
		!strings.Contains(text, `"appserver_reachable":false`) ||
		!strings.Contains(text, `"supervisor_reachable":false`) {
		t.Fatalf("doctor report = %s", text)
	}
}

func TestLaneTurnCursorCollectsEveryTurnOnceInChronologicalOrder(t *testing.T) {
	turns := []appTurn{
		{ID: "turn-3", Status: "completed"},
		{ID: "turn-2", Status: "completed"},
		{ID: "turn-1", Status: "completed"},
	}
	cases := []struct {
		preferred string
		collected string
		want      string
	}{
		{preferred: "turn-1", want: "turn-1"},
		{preferred: "turn-3", collected: "turn-1", want: "turn-2"},
		{preferred: "turn-3", collected: "turn-2", want: "turn-3"},
		{collected: "turn-3", want: ""},
		{want: "turn-3"},
	}
	for _, test := range cases {
		turn := selectLaneTurn(turns, test.preferred, test.collected)
		got := ""
		if turn != nil {
			got = turn.ID
		}
		if got != test.want {
			t.Fatalf("selectLaneTurn(%q, %q) = %q, want %q", test.preferred, test.collected, got, test.want)
		}
	}
}

func TestLaneQueueSelectionSkipsSupersededSchemaDraft(t *testing.T) {
	turns := []appTurn{
		{ID: "turn-correction", Status: "completed"},
		{ID: "turn-rejected-draft", Status: "completed"},
		{ID: "turn-collected", Status: "completed"},
	}
	state := laneState{
		TurnID:          "turn-correction",
		LatestTurnID:    "turn-correction",
		CollectedTurnID: "turn-collected",
		PendingTurnIDs:  []string{"turn-correction"},
		PendingQueueVer: 1,
	}
	turn := selectLaneTurnForState(turns, state)
	if turn == nil || turn.ID != "turn-correction" {
		t.Fatalf("queue selected %#v, want correction turn", turn)
	}
	state.PendingTurnIDs = nil
	if turn := selectLaneTurnForState(turns, state); turn != nil {
		t.Fatalf("drained queue resurrected history turn %#v", turn)
	}
}

func TestWaitForLaneTurnAdoptsPeerWakeAfterPriorTurnWasCollected(t *testing.T) {
	isolateNativeLaneTest(t)
	paths := resolveNativePaths()
	initialState := laneState{
		ThreadID: "thread-lane", TurnID: "turn-old", LatestTurnID: "turn-old",
		CollectedTurnID: "turn-old", PendingQueueVer: 1, Status: "completed",
	}
	if err := recordLaneState(paths, initialState); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var wakeCompleted atomic.Bool
	fake, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			if wakeCompleted.Load() {
				return map[string]any{"data": []map[string]any{{
					"id": "turn-wake", "status": "completed",
					"items": []map[string]any{{"id": "item-final", "type": "agentMessage", "phase": "final_answer", "text": "WAKE_RESULT"}},
				}, {"id": "turn-old", "status": "completed", "items": []any{}}}}, nil
			}
			return map[string]any{"data": []map[string]any{{"id": "turn-old", "status": "completed", "items": []any{}}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fake.afterResponse = func(conn net.Conn, request map[string]any) {
		if stringValue(request["method"]) != "thread/turns/list" {
			return
		}
		once.Do(func() {
			wakeState := initialState
			wakeState.TurnID = "turn-wake"
			wakeState.LatestTurnID = "turn-wake"
			wakeState.PendingTurnIDs = []string{"turn-wake"}
			wakeState.Status = "in_progress"
			if recordLaneState(paths, wakeState) != nil {
				return
			}
			notifications := []map[string]any{
				{"method": "turn/started", "params": map[string]any{
					"threadId": "thread-lane", "turn": map[string]any{"id": "turn-wake", "status": "inProgress"},
				}},
				{"method": "item/completed", "params": map[string]any{
					"threadId": "thread-lane", "turnId": "turn-wake",
					"item": map[string]any{"id": "item-final", "type": "agentMessage", "phase": "final_answer", "text": "WAKE_RESULT"},
				}},
				{"method": "turn/completed", "params": map[string]any{
					"threadId": "thread-lane", "turn": map[string]any{"id": "turn-wake", "status": "completed"},
				}},
			}
			wakeCompleted.Store(true)
			for _, notification := range notifications {
				body, _ := json.Marshal(notification)
				if writeTestFrame(conn, body) != nil {
					return
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	state := initialState
	status, code, collected, err := waitForLaneTurn(client, &state, 2*time.Second)
	if err != nil || code != 0 || status != "completed" || state.TurnID != "turn-wake" {
		t.Fatalf("wake collection = status %q code %d turn %q err %v", status, code, state.TurnID, err)
	}
	if !collected {
		t.Fatal("wake turn was not acknowledged as collected")
	}
}

func TestWaitTimeoutNeverAcknowledgesUnemittedTurn(t *testing.T) {
	isolateNativeLaneTest(t)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "thread/turns/list" {
			return map[string]any{"data": []any{}}, nil
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
	state := laneState{ThreadID: "thread-wait", CollectedTurnID: "turn-old"}
	status, code, collected, waitErr := waitForLaneTurn(client, &state, 20*time.Millisecond)
	if waitErr == nil || code != 124 || status != "" || collected {
		t.Fatalf("empty wait timeout = status %q code %d collected=%v err=%v", status, code, collected, waitErr)
	}
	if state.CollectedTurnID != "turn-old" || state.TurnID != "" {
		t.Fatalf("timeout advanced collection cursor: %+v", state)
	}
}

func TestCollectionTimeoutDoesNotInterruptActiveLane(t *testing.T) {
	isolateNativeLaneTest(t)
	var interruptCalls atomic.Int32
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": "turn-active", "status": "inProgress", "items": []any{},
			}}}, nil
		case "turn/interrupt":
			interruptCalls.Add(1)
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
	state := laneState{
		ThreadID: "thread-active", TurnID: "turn-active", Status: "in_progress",
		CollectedTurnID: "turn-prior", DeadlineAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	status, code, collected, waitErr := waitForLaneTurnWithPolicy(client, &state, 20*time.Millisecond, false, false)
	if waitErr == nil || code != 124 || status != "" || collected {
		t.Fatalf("collection timeout = status %q code %d collected=%v err=%v", status, code, collected, waitErr)
	}
	if interruptCalls.Load() != 0 {
		t.Fatalf("collection timeout interrupted the lane %d times", interruptCalls.Load())
	}
	if state.CollectedTurnID != "turn-prior" || state.TurnID != "turn-active" ||
		state.Status != "in_progress" || state.TerminalOutcome != "" || state.TimedOutTurnID != "" {
		t.Fatalf("collection timeout mutated lane outcome or cursor: %+v", state)
	}
}

func TestTransientInterruptedProjectionIsNotCollected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	var completed atomic.Bool
	var once sync.Once
	fake, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) != "thread/turns/list" {
			return map[string]any{}, nil
		}
		if completed.Load() {
			completedAt := int64(20)
			duration := int64(10)
			return map[string]any{"data": []map[string]any{{
				"id": "turn-racy", "status": "completed", "completedAt": completedAt, "durationMs": duration,
				"items": []map[string]any{{
					"id": "answer-racy", "type": "agentMessage", "phase": "final_answer", "text": "RACY_FINAL",
				}},
			}}}, nil
		}
		// This is the exact bad App Server shape: a just-started turn is
		// projected as interrupted, but has no completion metadata or items.
		return map[string]any{"data": []map[string]any{{
			"id": "turn-racy", "status": "interrupted", "startedAt": int64(10), "items": []any{},
		}}}, nil
	})
	fake.afterResponse = func(conn net.Conn, request map[string]any) {
		if stringValue(request["method"]) != "thread/turns/list" {
			return
		}
		once.Do(func() {
			completed.Store(true)
			for _, notification := range []map[string]any{
				{"method": "item/completed", "params": map[string]any{
					"threadId": "thread-racy", "turnId": "turn-racy",
					"item": map[string]any{
						"id": "answer-racy", "type": "agentMessage", "phase": "final_answer", "text": "RACY_FINAL",
					},
				}},
				{"method": "turn/completed", "params": map[string]any{
					"threadId": "thread-racy", "turn": map[string]any{"id": "turn-racy", "status": "completed"},
				}},
			} {
				body, _ := json.Marshal(notification)
				if writeTestFrame(conn, body) != nil {
					return
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	state := laneState{ThreadID: "thread-racy", TurnID: "turn-racy", Status: "in_progress"}
	status, code, collected, waitErr := waitForLaneTurn(client, &state, time.Second)
	_ = write.Close()
	os.Stdout = original
	output, _ := io.ReadAll(read)
	_ = read.Close()
	if waitErr != nil || code != 0 || status != "completed" || !collected {
		t.Fatalf("racy wait = status %q code %d collected=%v err=%v", status, code, collected, waitErr)
	}
	text := string(output)
	if !strings.Contains(text, "RACY_FINAL") || strings.Contains(text, `"outcome":"interrupted"`) {
		t.Fatalf("transient interrupted row was acknowledged: %s", text)
	}
}

func TestRetiredLaneMarkersSurviveSupervisorReplacement(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state")}
	threadID := "retired-thread"
	if err := markRetiredThread(paths, threadID); err != nil {
		t.Fatal(err)
	}
	retired := readRetiredThreads(paths)
	if !retired[threadID] {
		t.Fatalf("retired marker was not loaded: %#v", retired)
	}
	supervisor := nativeSupervisor{paths: paths, retired: retired}
	if !supervisor.isRetired(threadID) {
		t.Fatal("replacement supervisor did not retain the retired thread")
	}
	supervisor.clearRetired(threadID)
	if supervisor.isRetired(threadID) {
		t.Fatal("unarchive did not clear the retired thread")
	}
	if _, err := os.Stat(retiredThreadPath(paths, threadID)); !os.IsNotExist(err) {
		t.Fatalf("retired marker survived clear: %v", err)
	}
}

func TestSupervisorRefusesShimForExternallyRetiredThread(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "run"),
	}
	threadID := "00000000-0000-4000-8000-000000000022"
	if err := markRetiredThread(paths, threadID); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{
		paths: paths, retired: map[string]bool{}, shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{},
	}
	if !supervisor.isRetired(threadID) {
		t.Fatal("supervisor did not import an externally written retirement marker")
	}
	if _, err := supervisor.ensureShim(map[string]any{"sessionId": threadID, "cwd": root}); err == nil {
		t.Fatal("supervisor published a shim for a durably retired thread")
	}
	supervisor.handleThreadStarted(appThread{ID: threadID})
	if !readRetiredThreads(paths)[threadID] || !supervisor.isRetired(threadID) {
		t.Fatal("a delayed thread/started event cleared a newer retirement marker")
	}
}
