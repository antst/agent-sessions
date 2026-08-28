package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

var errQwenDaemonLaneEvidenceChanged = errors.New("qwen daemon lane evidence changed")

func TestQwenDaemonNativeLaneActorRunsACPWithoutManager(t *testing.T) {
	fake := newFakeQwenProcess(t)
	fake.installEnvironment(t)
	t.Setenv(qwenLaneExecutableEnv, fake.Paths.Executable)
	home, runtimeDir := filepath.Join(t.TempDir(), "qwen-home"), filepath.Join(t.TempDir(), "qwen-runtime")
	selectedHome, selectedRuntime := filepath.Join(t.TempDir(), "selected-qwen-home"), filepath.Join(t.TempDir(), "selected-qwen-runtime")
	for _, directory := range []string{home, runtimeDir, selectedHome, selectedRuntime} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QWEN_HOME", home)
	t.Setenv("QWEN_RUNTIME_DIR", runtimeDir)
	priorReadiness := qwenDaemonReadinessCheck
	qwenDaemonReadinessCheck = func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error) {
		return qwenreadiness.Report{Ready: true, Version: qwenFakeVersion()}, nil
	}
	t.Cleanup(func() { qwenDaemonReadinessCheck = priorReadiness })

	coordinator := newQwenNativeCoordinator()
	adapter := newQwenDaemonAdapter(coordinator)
	lane := daemonpkg.LaneRecord{
		LaneSessionID: "qwen-native-daemon-lane", Name: "qwen-native", Product: "qwen",
		Cwd: t.TempDir(), PermissionMode: "yolo", State: daemonpkg.LaneStateRunning,
	}
	turn := daemonpkg.LaneTurnRecord{
		TurnID: "qwen-native-turn", LaneSessionID: lane.LaneSessionID,
		DispatchState: daemonpkg.LaneDispatchAccepted,
		InputReference: map[string]any{
			"kind": "inline", "content": "exercise native ACP",
			"options": map[string]any{"native": map[string]any{
				"profile": "selected-profile", "qwen_home_set": true, "qwen_home": selectedHome,
				"qwen_runtime_dir_set": true, "qwen_runtime_dir": selectedRuntime,
			}},
		},
	}
	dispatched, err := adapter.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start native Qwen daemon lane: %v", err)
	}
	if dispatched.DispatchState != daemonpkg.LaneDispatchRunning ||
		stringValue(dispatched.NativeActor["qwen_session_id"]) == "" ||
		intValue(dispatched.NativeActor["worker_pid"]) <= 1 ||
		stringValue(dispatched.NativeTurnIdentity["native_turn_id"]) != turn.TurnID {
		t.Fatalf("native Qwen dispatch = %#v", dispatched)
	}
	started := qwenTestWaitForJSONL(t, fake.Paths.Records, time.Second, func(row map[string]any) bool {
		return stringValue(row["kind"]) == "start" && containsString(daemonLaneStringSlice(row["args"]), "--acp")
	})
	arguments := daemonLaneStringSlice(started["args"])
	environment := mapValue(started["env"])
	if grokFakeArgument(arguments, "--approval-mode") != "yolo" ||
		stringValue(environment["QWEN_HOME"]) != selectedHome || stringValue(environment["QWEN_RUNTIME_DIR"]) != selectedRuntime {
		t.Fatalf("native Qwen argv/env = %v %#v", arguments, environment)
	}
	for _, obsolete := range []string{"manager_pid", "manager_socket", "control_socket", "messaging_socket"} {
		if _, exists := dispatched.NativeActor[obsolete]; exists {
			t.Fatalf("native Qwen dispatch retained obsolete %q evidence: %#v", obsolete, dispatched.NativeActor)
		}
	}
	lane.NativeActor = cloneQwenLaneMap(dispatched.NativeActor)
	turn.NativeTurnIdentity = cloneQwenLaneMap(dispatched.NativeTurnIdentity)
	ctx, cancel := context.WithTimeout(context.Background(), qwenTestLifecycleTimeout)
	defer cancel()
	terminal, err := adapter.WaitTurn(ctx, lane, turn)
	if err != nil {
		t.Fatalf("wait for native Qwen daemon lane: %v", err)
	}
	if terminal.TerminalOutcome != daemonpkg.LaneDispatchCompleted ||
		terminal.ResultReference["content"] != "fake Qwen answer" {
		t.Fatalf("native Qwen terminal = %#v", terminal)
	}
	if err := adapter.Cleanup(context.Background(), lane); err != nil {
		t.Fatalf("cleanup native Qwen daemon lane: %v", err)
	}
	coordinator.mu.Lock()
	remaining := len(coordinator.lanes)
	coordinator.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("native Qwen coordinator retained %d lane actor(s)", remaining)
	}
	adapter.Close()
	qwenTestPoll(t, time.Second, "native Qwen ACP worker exit", func() (bool, error) {
		return exactProcessIdentityStatus(
			intValue(dispatched.NativeActor["worker_pid"]),
			stringValue(dispatched.NativeActor["worker_proc_start"]),
		).Status == processIdentityStale, nil
	})
}

func TestQwenDaemonLaneStartsACPAndCollectsExactNativeEvents(t *testing.T) {
	client := newQwenDaemonLaneTestClient()
	client.startResult = map[string]any{
		"qwen_session_id": "qwen-native-1", "native_turn_id": "qwen-turn-1",
		"event_cursor": "event-0", "worker_pid": 8101, "worker_proc_start": "start-8101",
	}
	client.collectResult = map[string]any{
		"events": []any{
			map[string]any{
				"method": "session/update", "params": map[string]any{
					"sessionId": "qwen-native-1", "turnId": "qwen-turn-1",
					"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "first "}},
				},
			},
			map[string]any{
				"method": "session/update", "params": map[string]any{
					"sessionId": "qwen-native-1", "turnId": "qwen-turn-1",
					"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer"}},
				},
			},
		},
		"response": map[string]any{"sessionId": "qwen-native-1", "turnId": "qwen-turn-1", "stopReason": "end_turn"},
	}
	adapter := newQwenDaemonAdapter(client)
	lane, turn := qwenDaemonLaneFixture()

	dispatched, err := adapter.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start Qwen daemon lane turn: %v", err)
	}
	if dispatched.LaneSessionID != lane.LaneSessionID || dispatched.DispatchState != daemonpkg.LaneDispatchRunning ||
		dispatched.NativeActor["qwen_session_id"] != "qwen-native-1" ||
		dispatched.NativeTurnIdentity["native_turn_id"] != "qwen-turn-1" ||
		dispatched.NativeTurnIdentity["event_cursor"] != "event-0" {
		t.Fatalf("Qwen dispatch result = %#v", dispatched)
	}
	if client.startCalls != 1 || client.startedLane != lane.LaneSessionID || client.startedTurn != turn.TurnID || client.managerLaunches != 0 {
		t.Fatalf("Qwen start calls=%d lane=%q turn=%q manager launches=%d", client.startCalls, client.startedLane, client.startedTurn, client.managerLaunches)
	}

	turn.NativeTurnIdentity = cloneQwenLaneMap(dispatched.NativeTurnIdentity)
	terminal, err := adapter.CollectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("collect Qwen daemon lane turn: %v", err)
	}
	if terminal.TerminalOutcome != daemonpkg.LaneDispatchCompleted || terminal.ResultReference["content"] != "first answer" ||
		terminal.NativeTurnIdentity["native_turn_id"] != "qwen-turn-1" || client.collectCalls != 1 {
		t.Fatalf("Qwen terminal result = %#v, collect calls=%d", terminal, client.collectCalls)
	}

	client.collectResult = cloneQwenLaneMap(client.collectResult)
	events := client.collectResult["events"].([]any)
	changed := cloneQwenLaneMap(events[0].(map[string]any))
	changedParams := cloneQwenLaneMap(changed["params"].(map[string]any))
	changedParams["sessionId"] = "another-qwen-session"
	changed["params"] = changedParams
	events[0] = changed
	client.collectResult["events"] = events
	if _, err := adapter.CollectTurn(context.Background(), lane, turn); err == nil {
		t.Fatalf("changed Qwen event identity error = %v, want exact-evidence rejection", err)
	}
}

func TestQwenDaemonLaneInterruptArchiveAndCleanupUseExactNativeIdentity(t *testing.T) {
	client := newQwenDaemonLaneTestClient()
	adapter := newQwenDaemonAdapter(client)
	lane, turn := qwenDaemonLaneFixture()

	if err := adapter.InterruptTurn(context.Background(), lane, turn); err != nil {
		t.Fatalf("interrupt Qwen daemon lane turn: %v", err)
	}
	if client.interruptCalls != 1 || client.interruptedSession != "qwen-native-1" || client.interruptedTurn != "qwen-turn-1" {
		t.Fatalf("Qwen interrupt calls=%d session=%q turn=%q", client.interruptCalls, client.interruptedSession, client.interruptedTurn)
	}

	if err := adapter.Archive(context.Background(), lane); err != nil {
		t.Fatalf("archive Qwen daemon lane: %v", err)
	}
	if client.archiveCalls != 1 || client.archivedSession != "qwen-native-1" {
		t.Fatalf("Qwen archive calls=%d session=%q", client.archiveCalls, client.archivedSession)
	}
	if err := adapter.Cleanup(context.Background(), lane); err != nil {
		t.Fatalf("clean Qwen daemon lane artifacts: %v", err)
	}
	if client.cleanupCalls != 1 || client.cleanedLane != lane.LaneSessionID || client.vendorArtifactsRemoved != 0 {
		t.Fatalf("Qwen cleanup calls=%d lane=%q vendor removals=%d", client.cleanupCalls, client.cleanedLane, client.vendorArtifactsRemoved)
	}

	client.cleanupErr = errQwenDaemonLaneEvidenceChanged
	if err := adapter.Cleanup(context.Background(), lane); !errors.Is(err, errQwenDaemonLaneEvidenceChanged) {
		t.Fatalf("changed Qwen cleanup identity error = %v", err)
	}
	if client.vendorArtifactsRemoved != 0 {
		t.Fatalf("changed Qwen cleanup removed %d vendor artifact(s)", client.vendorArtifactsRemoved)
	}
}

func TestQwenDaemonLaneRestartReconnectsExactTurnWithoutRedispatch(t *testing.T) {
	client := newQwenDaemonLaneTestClient()
	client.reconnectResult = map[string]any{
		"reconnectable": true, "qwen_session_id": "qwen-native-1", "native_turn_id": "qwen-turn-1",
		"event_cursor": "event-17", "worker_pid": 8101, "worker_proc_start": "start-8101",
	}
	adapter := newQwenDaemonAdapter(client)
	lane, turn := qwenDaemonLaneFixture()

	reconnected, err := adapter.ReconnectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("reconnect Qwen daemon lane turn: %v", err)
	}
	if reconnected.DispatchState != daemonpkg.LaneDispatchRunning || reconnected.TerminalOutcome != "" ||
		reconnected.NativeActor["qwen_session_id"] != "qwen-native-1" ||
		reconnected.NativeTurnIdentity["native_turn_id"] != "qwen-turn-1" ||
		reconnected.NativeTurnIdentity["event_cursor"] != "event-17" {
		t.Fatalf("Qwen reconnect result = %#v", reconnected)
	}
	if client.reconnectCalls != 1 || client.startCalls != 0 || client.managerLaunches != 0 {
		t.Fatalf("Qwen reconnect calls=%d start calls=%d manager launches=%d", client.reconnectCalls, client.startCalls, client.managerLaunches)
	}
}

func TestQwenDaemonLaneRestartRecordsEvidenceApprovedInterruption(t *testing.T) {
	client := newQwenDaemonLaneTestClient()
	client.reconnectResult = map[string]any{
		"reconnectable":   false,
		"qwen_session_id": "qwen-native-1", "native_turn_id": "qwen-turn-1",
		"worker_status": "absent", "worker_pid": 8101, "worker_proc_start": "start-8101",
		"limitation":        "qwen_acp_stdio_is_not_reattachable",
		"native_transcript": map[string]any{"session_id": "qwen-native-1", "resume_supported": true},
	}
	adapter := newQwenDaemonAdapter(client)
	lane, turn := qwenDaemonLaneFixture()

	reconnected, err := adapter.ReconnectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("record evidence-approved Qwen interruption: %v", err)
	}
	if reconnected.DispatchState != daemonpkg.LaneDispatchInterrupted || reconnected.TerminalOutcome != daemonpkg.LaneDispatchInterrupted ||
		!boolValue(reconnected.ResultReference["collectable"]) || !boolValue(reconnected.ResultReference["resumable"]) ||
		reconnected.NativeTurnIdentity["native_turn_id"] != "qwen-turn-1" {
		t.Fatalf("evidence-approved Qwen interruption = %#v", reconnected)
	}
	if client.startCalls != 0 || client.managerLaunches != 0 {
		t.Fatalf("Qwen interruption redispatched=%d or launched manager=%d", client.startCalls, client.managerLaunches)
	}

	client.reconnectResult = map[string]any{
		"reconnectable": false, "qwen_session_id": "qwen-native-1", "native_turn_id": "qwen-turn-1",
	}
	if result, err := adapter.ReconnectTurn(context.Background(), lane, turn); err == nil ||
		result.DispatchState == daemonpkg.LaneDispatchInterrupted || result.TerminalOutcome == daemonpkg.LaneDispatchInterrupted {
		t.Fatalf("Qwen restart without limitation evidence = %#v, %v; want fail-closed non-terminal result", result, err)
	}

	client.reconnectResult = map[string]any{
		"reconnectable":   false,
		"qwen_session_id": "different-native-session", "native_turn_id": "qwen-turn-1",
		"worker_status": "absent", "limitation": "qwen_acp_stdio_is_not_reattachable",
		"native_transcript": map[string]any{"session_id": "different-native-session", "resume_supported": true},
	}
	if result, err := adapter.ReconnectTurn(context.Background(), lane, turn); err == nil ||
		result.DispatchState == daemonpkg.LaneDispatchInterrupted {
		t.Fatalf("changed Qwen restart evidence = %#v, %v; want exact-evidence rejection", result, err)
	}
}

func qwenDaemonLaneFixture() (daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) {
	lane := daemonpkg.LaneRecord{
		LaneSessionID: "qwen-lane-1", Name: "qwen-review", Product: "qwen", Cwd: "/work",
		PermissionMode: "default", State: daemonpkg.LaneStateRunning, ActiveTurnID: "turn-1",
		NativeActor: map[string]any{
			"qwen_session_id": "qwen-native-1", "worker_pid": 8101,
			"worker_proc_start": "start-8101", "worker_strong_start": "strong-8101",
			"event_path": "/qwen/events/qwen-native-1.jsonl", "archive_session_id": "qwen-native-1",
		},
	}
	turn := daemonpkg.LaneTurnRecord{
		TurnID: "turn-1", LaneSessionID: lane.LaneSessionID, DispatchState: daemonpkg.LaneDispatchAccepted,
		InputReference: map[string]any{"prompt": "review the change"},
		NativeTurnIdentity: map[string]any{
			"qwen_session_id": "qwen-native-1", "native_turn_id": "qwen-turn-1", "event_cursor": "event-0",
		},
	}
	return lane, turn
}

type qwenDaemonLaneTestClient struct {
	session qwenDaemonSession

	startResult     map[string]any
	reconnectResult map[string]any
	collectResult   map[string]any
	cleanupErr      error

	startCalls, reconnectCalls, interruptCalls, collectCalls, archiveCalls, cleanupCalls int
	managerLaunches, vendorArtifactsRemoved                                              int
	startedLane, startedTurn                                                             string
	interruptedSession, interruptedTurn                                                  string
	archivedSession, cleanedLane                                                         string
}

func newQwenDaemonLaneTestClient() *qwenDaemonLaneTestClient {
	return &qwenDaemonLaneTestClient{session: qwenDaemonSession{
		SessionID: "qwen-native-1", Cwd: "/work", Profile: "qwen-profile", PID: 8101,
		ProcStart: "start-8101", ParentPID: 8001, EventPath: "/qwen/events/qwen-native-1.jsonl",
		InputPath: "/qwen/input/qwen-native-1.jsonl", ReadinessPath: "/qwen/ready.json",
		Ready: true, DualOutput: true,
	}}
}

func (client *qwenDaemonLaneTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "qwen", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *qwenDaemonLaneTestClient) ResolveSession(_ context.Context, _ string) (qwenDaemonSession, bool, error) {
	return client.session, false, nil
}

func (client *qwenDaemonLaneTestClient) ObserveSession(_ context.Context, _ daemonpkg.AttachmentRecord, _ int) (qwenDaemonSession, error) {
	return client.session, nil
}

func (client *qwenDaemonLaneTestClient) InspectSession(_ context.Context, _ string) (qwenDaemonSession, error) {
	return client.session, nil
}

func (client *qwenDaemonLaneTestClient) WriteInput(_ context.Context, _ string, _ federation.AgentFrame) error {
	return nil
}

func (client *qwenDaemonLaneTestClient) StartQwenTurn(_ context.Context, lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.startCalls++
	client.startedLane, client.startedTurn = lane.LaneSessionID, turn.TurnID
	return cloneQwenLaneMap(client.startResult), nil
}

func (client *qwenDaemonLaneTestClient) ReconnectQwenTurn(_ context.Context, _ daemonpkg.LaneRecord, _ daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.reconnectCalls++
	return cloneQwenLaneMap(client.reconnectResult), nil
}

func (client *qwenDaemonLaneTestClient) InterruptQwenTurn(_ context.Context, lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) error {
	client.interruptCalls++
	client.interruptedSession = stringValue(lane.NativeActor["qwen_session_id"])
	client.interruptedTurn = stringValue(turn.NativeTurnIdentity["native_turn_id"])
	return nil
}

func (client *qwenDaemonLaneTestClient) CollectQwenTurn(_ context.Context, _ daemonpkg.LaneRecord, _ daemonpkg.LaneTurnRecord) (map[string]any, error) {
	client.collectCalls++
	return cloneQwenLaneMap(client.collectResult), nil
}

func (client *qwenDaemonLaneTestClient) ArchiveQwenLane(_ context.Context, lane daemonpkg.LaneRecord) error {
	client.archiveCalls++
	client.archivedSession = stringValue(lane.NativeActor["archive_session_id"])
	return nil
}

func (client *qwenDaemonLaneTestClient) CleanupQwenLane(_ context.Context, lane daemonpkg.LaneRecord) error {
	client.cleanupCalls++
	client.cleanedLane = lane.LaneSessionID
	return client.cleanupErr
}

func cloneQwenLaneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneQwenLaneMap(typed)
		case []any:
			output[key] = append([]any(nil), typed...)
		default:
			output[key] = value
		}
	}
	return output
}
