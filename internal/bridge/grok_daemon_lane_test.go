package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestGrokDaemonLaneCoordinatorRunsACPWithoutManagerSocket(t *testing.T) {
	root := shortSocketTestRoot(t, "gdl-")
	for _, directory := range []string{"home", "state", "runtime"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_GENERATED_SESSION_ID", "0198f37a-3cbe-7cca-8148-f0a79a63c97b")
	t.Setenv("GROK_FAKE_ANSWER", "in-process Grok lane result")
	t.Setenv("GROK_FAKE_REQUIRE_AGENT_SESSIONS_MCP", "1")
	record := filepath.Join(root, "grok-acp.jsonl")
	t.Setenv("GROK_FAKE_RECORD", record)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeGrok := filepath.Join(root, "fake-grok")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -test.run='^TestGrokFakeProcess$' -- \"$@\"\n", grokDaemonLaneShellQuote(testBinary))
	if err := os.WriteFile(fakeGrok, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", fakeGrok)

	coordinator := newGrokNativeCoordinator()
	adapter := newGrokDaemonAdapter(coordinator)
	t.Cleanup(adapter.Close)
	lane, turn := grokDaemonLaneTestRecords("", "turn-production-actor")
	lane.NativeActor = nil
	lane.Cwd = root
	turn.InputReference = map[string]any{
		"kind": "inline", "content": "exercise the production actor",
		"options": map[string]any{"native": map[string]any{"model": "grok-native", "reasoning_effort": "high"}},
	}
	dispatch, err := adapter.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start in-process Grok lane actor: %v", err)
	}
	if dispatch.DispatchState != "running" || stringValue(dispatch.NativeActor["session_id"]) == "" ||
		intValue(dispatch.NativeActor["worker_pid"]) <= 1 {
		t.Fatalf("in-process Grok dispatch = %#v", dispatch)
	}
	lane.NativeActor = dispatch.NativeActor
	turn.NativeTurnIdentity = dispatch.NativeTurnIdentity
	turn.DispatchState = dispatch.DispatchState

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	terminal, err := adapter.WaitTurn(waitCtx, lane, turn)
	if err != nil || terminal.TerminalOutcome != "completed" ||
		stringValue(terminal.ResultReference["text"]) != "in-process Grok lane result" {
		t.Fatalf("in-process Grok terminal = %#v, %v", terminal, err)
	}
	methods := grokDaemonLaneRecordedMethods(t, record)
	if !reflect.DeepEqual(methods, []string{"initialize", "authenticate", "session/new", "session/prompt"}) {
		t.Fatalf("production Grok ACP methods = %v", methods)
	}
	arguments := grokDaemonLaneRecordedArgv(t, record)
	if grokFakeArgument(arguments, "--model") != "grok-native" ||
		grokFakeArgument(arguments, "--reasoning-effort") != "high" {
		t.Fatalf("production Grok native argv = %v", arguments)
	}
	if err := adapter.Archive(context.Background(), lane); err != nil {
		t.Fatalf("archive in-process Grok lane: %v", err)
	}
	if err := adapter.Cleanup(context.Background(), lane); err != nil {
		t.Fatalf("cleanup in-process Grok lane: %v", err)
	}
	coordinator.mu.Lock()
	remaining := len(coordinator.laneActors)
	coordinator.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("daemon retained %d Grok lane actor(s) after archive", remaining)
	}
	entries, err := filepath.Glob(filepath.Join(root, "runtime", "agent-sessions", "grok-lanes", "*", "*.sock"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("daemon-owned Grok lane published manager socket(s): %v, %v", entries, err)
	}
}

func TestGrokDaemonLaneACPStartLoadAndInterject(t *testing.T) {
	ctx := context.Background()

	t.Run("start new ACP session", func(t *testing.T) {
		client := newGrokDaemonLaneTestClient()
		client.startResult = map[string]any{
			"session_id": "grok-native-new", "native_turn_id": "grok-turn-new",
			"worker_pid": 4101, "worker_proc_start": "worker-start", "worker_strong_start": "worker-strong-start",
		}
		adapter := newGrokDaemonAdapter(client)
		lane, turn := grokDaemonLaneTestRecords("", "turn-new")

		result, err := adapter.StartTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("start Grok daemon lane turn: %v", err)
		}
		if result.LaneSessionID != lane.LaneSessionID || result.DispatchState != "running" ||
			stringValue(result.NativeActor["session_id"]) != "grok-native-new" ||
			intValue(result.NativeActor["worker_pid"]) != 4101 ||
			stringValue(result.NativeActor["worker_strong_start"]) != "worker-strong-start" ||
			stringValue(result.NativeTurnIdentity["native_turn_id"]) != "grok-turn-new" {
			t.Fatalf("Grok new-session dispatch = %#v", result)
		}
		client.requireMethods(t, "initialize", "authenticate", "session/new", "session/prompt")
		if client.startedLane != lane.LaneSessionID || client.startedTurn != turn.TurnID || client.managerLaunches != 0 {
			t.Fatalf("Grok start lane=%q turn=%q manager launches=%d", client.startedLane, client.startedTurn, client.managerLaunches)
		}
	})

	t.Run("load exact ACP session", func(t *testing.T) {
		client := newGrokDaemonLaneTestClient()
		client.startResult = map[string]any{
			"session_id": "grok-native-existing", "native_turn_id": "grok-turn-load",
			"worker_pid": 4101, "worker_proc_start": "worker-start", "worker_strong_start": "worker-strong-start",
		}
		adapter := newGrokDaemonAdapter(client)
		lane, turn := grokDaemonLaneTestRecords("grok-native-existing", "turn-load")

		result, err := adapter.StartTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("load Grok daemon lane turn: %v", err)
		}
		if stringValue(result.NativeActor["session_id"]) != "grok-native-existing" ||
			stringValue(result.NativeTurnIdentity["native_turn_id"]) != "grok-turn-load" {
			t.Fatalf("Grok loaded dispatch = %#v", result)
		}
		client.requireMethods(t, "initialize", "authenticate", "session/load", "session/prompt")
	})

	t.Run("interject admitted peer turn", func(t *testing.T) {
		client := newGrokDaemonLaneTestClient()
		client.startResult = map[string]any{
			"session_id": "grok-native-existing", "native_turn_id": "message-17",
			"worker_pid": 4101, "worker_proc_start": "worker-start", "worker_strong_start": "worker-strong-start",
		}
		adapter := newGrokDaemonAdapter(client)
		lane, turn := grokDaemonLaneTestRecords("grok-native-existing", "turn-interject")
		turn.InputReference = map[string]any{
			"kind": "peer_message", "message_id": "message-17", "content": "peer follow-up",
		}

		result, err := adapter.StartTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("interject Grok daemon lane turn: %v", err)
		}
		if result.DispatchState != "running" || stringValue(result.NativeTurnIdentity["native_turn_id"]) != "message-17" {
			t.Fatalf("Grok interjection dispatch = %#v", result)
		}
		client.requireMethods(t, "_x.ai/interject")
		if client.interjectionID != "message-17" || client.interjectionText != "peer follow-up" {
			t.Fatalf("Grok interjection id=%q text=%q", client.interjectionID, client.interjectionText)
		}
	})
}

func TestGrokDaemonLaneInterruptCollectAndArchiveUseExactEvidence(t *testing.T) {
	ctx := context.Background()
	client := newGrokDaemonLaneTestClient()
	client.terminalResult = map[string]any{
		"session_id": "grok-native-terminal", "native_turn_id": "grok-turn-terminal",
		"terminal_outcome": "completed", "result_reference": map[string]any{
			"kind": "native_result", "text": "fake Grok daemon answer", "stop_reason": "end_turn",
		},
	}
	adapter := newGrokDaemonAdapter(client)
	lane, turn := grokDaemonLaneTestRecords("grok-native-terminal", "turn-terminal")
	turn.DispatchState = "running"
	turn.NativeTurnIdentity = map[string]any{
		"session_id": "grok-native-terminal", "native_turn_id": "grok-turn-terminal",
	}

	if err := adapter.InterruptTurn(ctx, lane, turn); err != nil {
		t.Fatalf("interrupt exact Grok lane turn: %v", err)
	}
	if client.interruptCalls != 1 || client.interruptedSession != "grok-native-terminal" ||
		client.interruptedTurn != "grok-turn-terminal" {
		t.Fatalf("Grok interrupt calls=%d session=%q turn=%q", client.interruptCalls, client.interruptedSession, client.interruptedTurn)
	}

	first, err := adapter.CollectTurn(ctx, lane, turn)
	if err != nil {
		t.Fatalf("collect Grok turn: %v", err)
	}
	second, err := adapter.CollectTurn(ctx, lane, turn)
	if err != nil {
		t.Fatalf("repeat collect Grok turn: %v", err)
	}
	if !reflect.DeepEqual(first, second) || first.TerminalOutcome != "completed" ||
		stringValue(first.ResultReference["text"]) != "fake Grok daemon answer" ||
		stringValue(first.NativeTurnIdentity["native_turn_id"]) != "grok-turn-terminal" {
		t.Fatalf("stable Grok terminal = first %#v second %#v", first, second)
	}
	if lane.CollectionCursor != "" || turn.CollectionRevision != 0 || turn.CollectedAt != 0 {
		t.Fatalf("adapter advanced daemon collection cursor: lane=%#v turn=%#v", lane, turn)
	}

	if err := adapter.Archive(ctx, lane); err != nil {
		t.Fatalf("archive exact Grok lane: %v", err)
	}
	if client.archiveCalls != 1 || client.archivedSession != "grok-native-terminal" {
		t.Fatalf("Grok archive calls=%d session=%q", client.archiveCalls, client.archivedSession)
	}
	if err := adapter.Cleanup(ctx, lane); err != nil {
		t.Fatalf("cleanup exact Grok lane: %v", err)
	}
	if client.cleanupCalls != 1 || client.cleanedLane != lane.LaneSessionID || client.vendorArtifactsRemoved != 0 {
		t.Fatalf("Grok cleanup calls=%d lane=%q vendor removals=%d", client.cleanupCalls, client.cleanedLane, client.vendorArtifactsRemoved)
	}

	client.evidenceErr = daemonpkg.ErrAttachmentEvidenceChanged
	if err := adapter.InterruptTurn(ctx, lane, turn); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed-evidence interrupt error = %v", err)
	}
	if err := adapter.Archive(ctx, lane); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed-evidence archive error = %v", err)
	}
	if client.interruptCalls != 1 || client.archiveCalls != 1 {
		t.Fatalf("changed evidence performed interrupt/archive: %d/%d", client.interruptCalls, client.archiveCalls)
	}
}

func TestGrokDaemonLaneRestartRecordsEvidenceApprovedInterruption(t *testing.T) {
	ctx := context.Background()
	lane, turn := grokDaemonLaneTestRecords("grok-native-restart", "turn-restart")
	turn.DispatchState = "running"
	turn.NativeTurnIdentity = map[string]any{
		"session_id": "grok-native-restart", "native_turn_id": "grok-turn-restart",
	}

	t.Run("supported exact reconnect loads without redispatch", func(t *testing.T) {
		client := newGrokDaemonLaneTestClient()
		client.reconnectResult = map[string]any{
			"reconnectable": true, "session_id": "grok-native-restart", "native_turn_id": "grok-turn-restart",
			"worker_pid": 4101, "worker_proc_start": "worker-start", "worker_strong_start": "worker-strong-start",
		}
		adapter := newGrokDaemonAdapter(client)

		result, err := adapter.ReconnectTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("reconnect Grok turn: %v", err)
		}
		if result.DispatchState != "running" || result.TerminalOutcome != "" ||
			stringValue(result.NativeActor["session_id"]) != "grok-native-restart" ||
			stringValue(result.NativeTurnIdentity["native_turn_id"]) != "grok-turn-restart" {
			t.Fatalf("Grok reconnect result = %#v", result)
		}
		client.requireMethods(t, "initialize", "authenticate", "session/load")
		if client.startCalls != 0 || client.hasMethod("session/prompt") || client.hasMethod("_x.ai/interject") {
			t.Fatalf("restart redispatched accepted Grok turn: starts=%d methods=%v", client.startCalls, client.methods)
		}
	})

	t.Run("proved unattachable ACP pipe becomes one resumable interruption", func(t *testing.T) {
		client := newGrokDaemonLaneTestClient()
		client.reconnectResult = map[string]any{
			"reconnectable": false, "session_id": "grok-native-restart", "native_turn_id": "grok-turn-restart",
			"worker_status": "absent", "worker_pid": 4101, "worker_proc_start": "worker-start",
			"worker_strong_start": "worker-strong-start", "limitation": "grok_acp_stdio_is_not_reattachable",
			"native_transcript": map[string]any{"session_id": "grok-native-restart", "resume_supported": true},
		}
		adapter := newGrokDaemonAdapter(client)

		first, err := adapter.ReconnectTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("record evidence-approved Grok interruption: %v", err)
		}
		second, err := adapter.ReconnectTurn(ctx, lane, turn)
		if err != nil {
			t.Fatalf("repeat evidence-approved Grok interruption: %v", err)
		}
		if !reflect.DeepEqual(first, second) || first.DispatchState != "interrupted" ||
			first.TerminalOutcome != "interrupted" || !boolValue(first.ResultReference["collectable"]) ||
			!boolValue(first.ResultReference["resumable"]) ||
			first.ResultReference["restart_evidence"] != "grok_acp_stdio_is_not_reattachable" {
			t.Fatalf("Grok evidence-approved restart result first=%#v second=%#v", first, second)
		}
		if client.startCalls != 0 || len(client.methods) != 0 {
			t.Fatalf("interrupted restart redispatched Grok work: starts=%d methods=%v", client.startCalls, client.methods)
		}
	})

	t.Run("missing or changed evidence fails closed", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			result map[string]any
		}{
			{name: "missing limitation proof", result: map[string]any{
				"reconnectable": false, "session_id": "grok-native-restart", "native_turn_id": "grok-turn-restart",
			}},
			{name: "changed session identity", result: map[string]any{
				"reconnectable": false, "session_id": "another-native-session", "native_turn_id": "grok-turn-restart",
				"worker_status": "absent", "limitation": "grok_acp_stdio_is_not_reattachable",
				"native_transcript": map[string]any{"session_id": "another-native-session", "resume_supported": true},
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				client := newGrokDaemonLaneTestClient()
				client.reconnectResult = test.result
				result, err := newGrokDaemonAdapter(client).ReconnectTurn(ctx, lane, turn)
				if !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) || result.DispatchState == "interrupted" {
					t.Fatalf("Grok failed-closed restart = %#v, %v", result, err)
				}
				if client.startCalls != 0 || len(client.methods) != 0 {
					t.Fatalf("failed-closed restart touched ACP: starts=%d methods=%v", client.startCalls, client.methods)
				}
			})
		}
	})
}

func grokDaemonLaneRecordedMethods(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	methods := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil || stringValue(row["kind"]) != "request" {
			continue
		}
		request, _ := row["request"].(map[string]any)
		methods = append(methods, stringValue(request["method"]))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return methods
}

func grokDaemonLaneRecordedArgv(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil || stringValue(row["kind"]) != "argv" {
			continue
		}
		arguments := daemonLaneStringSlice(row["args"])
		if containsString(arguments, "stdio") && containsString(arguments, "--no-leader") {
			return arguments
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("Grok daemon lane did not record native ACP argv")
	return nil
}

func grokDaemonLaneShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func grokDaemonLaneTestRecords(nativeSessionID, turnID string) (daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) {
	actor := map[string]any{
		"worker_pid": 4101, "worker_proc_start": "worker-start", "worker_strong_start": "worker-strong-start",
	}
	if nativeSessionID != "" {
		actor["session_id"] = nativeSessionID
	}
	lane := daemonpkg.LaneRecord{
		LaneSessionID: "lane-grok-17", Name: "grok-review", Product: "grok", Cwd: "/workspace",
		PermissionMode: "bypassPermissions", State: "running", NativeActor: actor,
	}
	turn := daemonpkg.LaneTurnRecord{
		TurnID: turnID, LaneSessionID: lane.LaneSessionID, DispatchState: "accepted",
		InputReference: map[string]any{"kind": "inline", "content": "review the change"},
	}
	return lane, turn
}

type grokDaemonLaneTestClient struct {
	session grokDaemonSession

	startResult     map[string]any
	reconnectResult map[string]any
	terminalResult  map[string]any
	evidenceErr     error

	methods                []string
	startCalls             int
	reconnectCalls         int
	interruptCalls         int
	waitCalls              int
	collectCalls           int
	archiveCalls           int
	cleanupCalls           int
	managerLaunches        int
	vendorArtifactsRemoved int
	startedLane            string
	startedTurn            string
	interjectionID         string
	interjectionText       string
	interruptedSession     string
	interruptedTurn        string
	archivedSession        string
	cleanedLane            string
}

func newGrokDaemonLaneTestClient() *grokDaemonLaneTestClient {
	return &grokDaemonLaneTestClient{session: grokDaemonSession{
		SessionID: "grok-native-existing", Cwd: "/workspace", Profile: "grok-profile",
		OwnerPID: 4101, OwnerProcStart: "worker-start", LeaderSessionID: "grok-leader-1",
		ACPReady: true, CoordinatorID: "grok-coordinator-1",
	}}
}

func (client *grokDaemonLaneTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "grok", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *grokDaemonLaneTestClient) ResolveSession(_ context.Context, _ string) (grokDaemonSession, bool, error) {
	return client.session, false, nil
}

func (client *grokDaemonLaneTestClient) ObserveSession(_ context.Context, _ daemonpkg.AttachmentRecord, _ int) (grokDaemonSession, error) {
	return client.session, nil
}

func (client *grokDaemonLaneTestClient) InspectSession(_ context.Context, _ string) (grokDaemonSession, error) {
	return client.session, nil
}

func (client *grokDaemonLaneTestClient) InterjectFrame(_ context.Context, _ string, _ federation.AgentFrame) error {
	return nil
}

func (client *grokDaemonLaneTestClient) StartGrokTurn(_ context.Context, lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.startCalls++
	client.startedLane, client.startedTurn = lane.LaneSessionID, turn.TurnID
	if stringValue(turn.InputReference["kind"]) == "peer_message" {
		client.methods = append(client.methods, "_x.ai/interject")
		client.interjectionID = stringValue(turn.InputReference["message_id"])
		client.interjectionText = stringValue(turn.InputReference["content"])
	} else {
		client.methods = append(client.methods, "initialize", "authenticate")
		if stringValue(lane.NativeActor["session_id"]) == "" {
			client.methods = append(client.methods, "session/new")
		} else {
			client.methods = append(client.methods, "session/load")
		}
		client.methods = append(client.methods, "session/prompt")
	}
	return cloneGrokDaemonLaneMap(client.startResult), nil
}

func (client *grokDaemonLaneTestClient) ReconnectGrokTurn(_ context.Context, _ daemonpkg.LaneRecord, _ daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.reconnectCalls++
	if boolValue(client.reconnectResult["reconnectable"]) {
		client.methods = append(client.methods, "initialize", "authenticate", "session/load")
	}
	return cloneGrokDaemonLaneMap(client.reconnectResult), nil
}

func (client *grokDaemonLaneTestClient) InterruptGrokTurn(_ context.Context, lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) error {
	if client.evidenceErr != nil {
		return client.evidenceErr
	}
	client.interruptCalls++
	client.interruptedSession = stringValue(lane.NativeActor["session_id"])
	client.interruptedTurn = stringValue(turn.NativeTurnIdentity["native_turn_id"])
	return nil
}

func (client *grokDaemonLaneTestClient) CollectGrokTurn(_ context.Context, _ daemonpkg.LaneRecord, _ daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.collectCalls++
	return cloneGrokDaemonLaneMap(client.terminalResult), nil
}

func (client *grokDaemonLaneTestClient) WaitGrokTurn(_ context.Context, _ daemonpkg.LaneRecord, _ daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.waitCalls++
	return cloneGrokDaemonLaneMap(client.terminalResult), nil
}

func (client *grokDaemonLaneTestClient) ArchiveGrokLane(_ context.Context, lane daemonpkg.LaneRecord) error {
	if client.evidenceErr != nil {
		return client.evidenceErr
	}
	client.archiveCalls++
	client.archivedSession = stringValue(lane.NativeActor["session_id"])
	return nil
}

func (client *grokDaemonLaneTestClient) CleanupGrokLane(_ context.Context, lane daemonpkg.LaneRecord) error {
	client.cleanupCalls++
	client.cleanedLane = lane.LaneSessionID
	return nil
}

func (client *grokDaemonLaneTestClient) requireMethods(t *testing.T, methods ...string) {
	t.Helper()
	if !reflect.DeepEqual(client.methods, methods) {
		t.Fatalf("Grok ACP methods = %v, want %v", client.methods, methods)
	}
}

func (client *grokDaemonLaneTestClient) hasMethod(method string) bool {
	for _, candidate := range client.methods {
		if candidate == method {
			return true
		}
	}
	return false
}

func cloneGrokDaemonLaneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneGrokDaemonLaneMap(typed)
		case []any:
			output[key] = append([]any(nil), typed...)
		default:
			output[key] = value
		}
	}
	return output
}
