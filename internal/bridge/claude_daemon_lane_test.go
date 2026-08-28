package bridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestClaudeDaemonLaneNativeWorkerArgvConsumesNormalizedOptions(t *testing.T) {
	lane, turn := claudeDaemonLaneFixture()
	turn.InputReference["options"] = map[string]any{"native": map[string]any{
		"model": "claude-opus", "effort": "high", "max_budget_usd": "2.5",
		"tools": "Bash", "tools_set": true, "allowed_tools": "Read", "allowed_tools_set": true,
		"disallowed_tools": "Write", "disallowed_tools_set": true,
		"output_schema": map[string]any{"type": "object"},
	}}
	state, _, err := claudeDaemonLaneWorkerState(lane, turn, "/profiles/claude", "claude-native-1")
	if err != nil {
		t.Fatal(err)
	}
	arguments := claudeLaneWorkerArgs(state)
	for flag, want := range map[string]string{
		"--model": "claude-opus", "--effort": "high", "--max-budget-usd": "2.5",
		"--tools": "Bash", "--allowedTools": "Read,SendMessage,ListAgents", "--disallowedTools": "Write",
	} {
		if got := grokFakeArgument(arguments, flag); got != want {
			t.Fatalf("Claude native %s = %q, want %q; argv=%v", flag, got, want, arguments)
		}
	}
	if schema := grokFakeArgument(arguments, "--json-schema"); !strings.Contains(schema, `"type":"object"`) {
		t.Fatalf("Claude native schema = %q; argv=%v", schema, arguments)
	}
}

func TestClaudeDaemonLaneStartsStreamWorkerAndCollectsStableTerminal(t *testing.T) {
	client := newClaudeDaemonLaneTestClient()
	client.startResult = map[string]any{
		"session_id": "claude-native-1", "native_turn_id": "claude-turn-1", "stream_id": "stream-1",
		"worker_pid": 6101, "worker_proc_start": "start-6101", "worker_strong_start": "strong-6101",
		"permission_mode": "dontAsk",
	}
	client.collectResult = map[string]any{
		"frames": []any{
			map[string]any{"type": "system", "subtype": "init", "session_id": "claude-native-1"},
			map[string]any{"type": "assistant", "session_id": "claude-native-1", "message": map[string]any{"content": "first answer"}},
			map[string]any{
				"type": "result", "subtype": "success", "session_id": "claude-native-1",
				"native_turn_id": "claude-turn-1", "result": "first answer", "exit_code": 0,
			},
		},
	}
	adapter := newClaudeDaemonAdapter(client)
	lane, turn := claudeDaemonLaneFixture()

	dispatched, err := adapter.StartTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("start Claude daemon lane turn: %v", err)
	}
	if dispatched.LaneSessionID != lane.LaneSessionID || dispatched.DispatchState != "running" ||
		dispatched.NativeActor["session_id"] != "claude-native-1" ||
		dispatched.NativeActor["worker_pid"] != 6101 ||
		dispatched.NativeActor["worker_proc_start"] != "start-6101" ||
		dispatched.NativeActor["worker_strong_start"] != "strong-6101" ||
		dispatched.NativeTurnIdentity["native_turn_id"] != "claude-turn-1" ||
		dispatched.NativeTurnIdentity["stream_id"] != "stream-1" {
		t.Fatalf("Claude dispatch result = %#v", dispatched)
	}
	for _, obsolete := range []string{"manager_pid", "manager_socket", "control_socket", "shim_pid", "shim_socket"} {
		if _, exists := dispatched.NativeActor[obsolete]; exists {
			t.Fatalf("Claude dispatch retained obsolete %q evidence: %#v", obsolete, dispatched.NativeActor)
		}
	}
	if client.startCalls != 1 || client.startedLane.LaneSessionID != lane.LaneSessionID ||
		client.startedTurn.TurnID != turn.TurnID || client.startedTurn.InputReference["prompt"] != "review the change" ||
		client.managerLaunches != 0 {
		t.Fatalf("Claude start calls=%d lane=%#v turn=%#v manager launches=%d",
			client.startCalls, client.startedLane, client.startedTurn, client.managerLaunches)
	}

	lane.NativeActor = cloneClaudeDaemonLaneMap(dispatched.NativeActor)
	turn.NativeTurnIdentity = cloneClaudeDaemonLaneMap(dispatched.NativeTurnIdentity)
	terminal, err := adapter.CollectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("collect Claude daemon lane turn: %v", err)
	}
	if terminal.TerminalOutcome != "completed" || terminal.ResultReference["content"] != "first answer" ||
		terminal.NativeTurnIdentity["native_turn_id"] != "claude-turn-1" || client.collectCalls != 1 {
		t.Fatalf("Claude terminal result = %#v, collect calls=%d", terminal, client.collectCalls)
	}

	client.collectResult = cloneClaudeDaemonLaneMap(client.collectResult)
	frames := client.collectResult["frames"].([]any)
	changed := cloneClaudeDaemonLaneMap(frames[1].(map[string]any))
	changed["session_id"] = "another-claude-session"
	frames[1] = changed
	client.collectResult["frames"] = frames
	if _, err := adapter.CollectTurn(context.Background(), lane, turn); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Claude stream identity error = %v, want exact-evidence rejection", err)
	}
}

func TestClaudeDaemonLaneCollectBlocksUntilNativeActorTerminalAndIsStable(t *testing.T) {
	lane, turn := claudeDaemonLaneFixture()
	actor := &claudeDaemonLaneActor{
		laneSessionID: lane.LaneSessionID, sessionID: "claude-native-1", cwd: lane.Cwd,
		permission: lane.PermissionMode, workerPID: 6101, workerStart: "start-6101",
		workerStrong: "strong-6101", stopped: make(chan struct{}),
		current: &claudeDaemonStreamTurn{
			id: "claude-turn-1", streamID: "stream-1", done: make(chan struct{}),
		},
	}
	coordinator := newClaudeNativeCoordinator()
	coordinator.laneActors[lane.LaneSessionID] = actor
	adapter := newClaudeDaemonAdapter(coordinator)

	collected := make(chan daemonpkg.LaneTerminalResult, 1)
	failures := make(chan error, 1)
	go func() {
		terminal, err := adapter.CollectTurn(context.Background(), lane, turn)
		if err != nil {
			failures <- err
			return
		}
		collected <- terminal
	}()
	select {
	case terminal := <-collected:
		t.Fatalf("Claude collection returned before native terminal: %#v", terminal)
	case err := <-failures:
		t.Fatalf("Claude collection failed before native terminal: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	actor.recordFrame(map[string]any{
		"type": "result", "subtype": "success", "session_id": "claude-native-1",
		"result": "stable answer", "is_error": false,
	})
	var first daemonpkg.LaneTerminalResult
	select {
	case first = <-collected:
	case err := <-failures:
		t.Fatalf("collect Claude native terminal: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Claude native terminal did not release blocking collection")
	}
	if first.TerminalOutcome != daemonpkg.LaneTerminalCompleted || first.ResultReference["content"] != "stable answer" {
		t.Fatalf("Claude blocking collection = %#v", first)
	}
	repeated, err := adapter.CollectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("repeat Claude native collection: %v", err)
	}
	if !reflect.DeepEqual(repeated, first) {
		t.Fatalf("repeat Claude collection = %#v, want stable %#v", repeated, first)
	}
}

func TestClaudeDaemonLanePreservesNativePermissionModes(t *testing.T) {
	for _, permissionMode := range []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"} {
		t.Run(permissionMode, func(t *testing.T) {
			client := newClaudeDaemonLaneTestClient()
			client.startResult = map[string]any{
				"session_id": "claude-native-1", "native_turn_id": "claude-turn-1", "stream_id": "stream-1",
				"worker_pid": 6201, "worker_proc_start": "start-6201", "worker_strong_start": "strong-6201",
				"permission_mode": permissionMode,
			}
			lane, turn := claudeDaemonLaneFixture()
			lane.PermissionMode = permissionMode
			if _, err := newClaudeDaemonAdapter(client).StartTurn(context.Background(), lane, turn); err != nil {
				t.Fatalf("start Claude %s stream worker: %v", permissionMode, err)
			}
			if client.startedLane.PermissionMode != permissionMode {
				t.Fatalf("Claude permission normalized to %q", client.startedLane.PermissionMode)
			}
		})
	}

	client := newClaudeDaemonLaneTestClient()
	lane, turn := claudeDaemonLaneFixture()
	lane.PermissionMode = "fullAuto"
	if _, err := newClaudeDaemonAdapter(client).StartTurn(context.Background(), lane, turn); err == nil {
		t.Fatal("unsupported Claude permission mode was accepted")
	}
	if client.startCalls != 0 {
		t.Fatalf("unsupported permission dispatched %d Claude stream worker(s)", client.startCalls)
	}
}

func TestClaudeDaemonLaneMapsStreamTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		subtype string
		outcome string
		exit    int
	}{
		{name: "native failure", subtype: "error_during_execution", outcome: "failed", exit: 1},
		{name: "native interrupt", subtype: "interrupted", outcome: "interrupted", exit: 130},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClaudeDaemonLaneTestClient()
			client.collectResult = map[string]any{"frames": []any{map[string]any{
				"type": "result", "subtype": test.subtype, "session_id": "claude-native-1",
				"native_turn_id": "claude-turn-1", "is_error": true, "exit_code": test.exit,
			}}}
			lane, turn := claudeDaemonLaneFixture()
			terminal, err := newClaudeDaemonAdapter(client).CollectTurn(context.Background(), lane, turn)
			if err != nil {
				t.Fatalf("collect Claude %s terminal: %v", test.subtype, err)
			}
			if terminal.TerminalOutcome != test.outcome || terminal.ResultReference["subtype"] != test.subtype ||
				terminal.NativeTurnIdentity["native_turn_id"] != "claude-turn-1" {
				t.Fatalf("Claude %s terminal = %#v", test.subtype, terminal)
			}
		})
	}
}

func TestClaudeDaemonLaneInterruptArchiveAndCleanupUseExactNativeIdentity(t *testing.T) {
	client := newClaudeDaemonLaneTestClient()
	adapter := newClaudeDaemonAdapter(client)
	lane, turn := claudeDaemonLaneFixture()

	if err := adapter.InterruptTurn(context.Background(), lane, turn); err != nil {
		t.Fatalf("interrupt Claude daemon lane turn: %v", err)
	}
	if client.interruptCalls != 1 || client.interruptedSession != "claude-native-1" ||
		client.interruptedTurn != "claude-turn-1" {
		t.Fatalf("Claude interrupt calls=%d session=%q turn=%q",
			client.interruptCalls, client.interruptedSession, client.interruptedTurn)
	}

	if err := adapter.Archive(context.Background(), lane); err != nil {
		t.Fatalf("archive Claude daemon lane: %v", err)
	}
	if client.archiveCalls != 1 || client.archivedSession != "claude-native-1" {
		t.Fatalf("Claude archive calls=%d session=%q", client.archiveCalls, client.archivedSession)
	}
	if err := adapter.Cleanup(context.Background(), lane); err != nil {
		t.Fatalf("clean Claude daemon lane artifacts: %v", err)
	}
	if client.cleanupCalls != 1 || client.cleanedLane.LaneSessionID != lane.LaneSessionID ||
		client.vendorArtifactsRemoved != 0 {
		t.Fatalf("Claude cleanup calls=%d lane=%q vendor removals=%d",
			client.cleanupCalls, client.cleanedLane.LaneSessionID, client.vendorArtifactsRemoved)
	}

	client.cleanupErr = daemonpkg.ErrAttachmentEvidenceChanged
	changed := lane
	changed.NativeActor = cloneClaudeDaemonLaneMap(lane.NativeActor)
	changed.NativeActor["worker_strong_start"] = "recycled-6401"
	if err := adapter.Cleanup(context.Background(), changed); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Claude cleanup identity error = %v", err)
	}
	if client.vendorArtifactsRemoved != 0 {
		t.Fatalf("changed Claude cleanup removed %d vendor artifact(s)", client.vendorArtifactsRemoved)
	}
}

func TestClaudeDaemonLaneRestartReconnectsSupportedStreamWithoutRedispatch(t *testing.T) {
	client := newClaudeDaemonLaneTestClient()
	client.reconnectResult = map[string]any{
		"reconnectable": true, "session_id": "claude-native-1", "native_turn_id": "claude-turn-1",
		"stream_id": "stream-17", "worker_pid": 6101, "worker_proc_start": "start-6101",
		"worker_strong_start": "strong-6101", "reconnect_token": "stream-17",
	}
	adapter := newClaudeDaemonAdapter(client)
	lane, turn := claudeDaemonLaneFixture()

	reconnected, err := adapter.ReconnectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("reconnect supported Claude stream: %v", err)
	}
	if reconnected.DispatchState != "running" || reconnected.TerminalOutcome != "" ||
		reconnected.NativeActor["session_id"] != "claude-native-1" ||
		reconnected.NativeTurnIdentity["native_turn_id"] != "claude-turn-1" ||
		reconnected.NativeTurnIdentity["stream_id"] != "stream-17" {
		t.Fatalf("Claude reconnect result = %#v", reconnected)
	}
	if client.reconnectCalls != 1 || client.startCalls != 0 || client.managerLaunches != 0 {
		t.Fatalf("Claude reconnect calls=%d starts=%d manager launches=%d",
			client.reconnectCalls, client.startCalls, client.managerLaunches)
	}
}

func TestClaudeDaemonLaneRestartRecordsEvidenceApprovedInterruption(t *testing.T) {
	client := newClaudeDaemonLaneTestClient()
	client.reconnectResult = map[string]any{
		"reconnectable": false, "session_id": "claude-native-1", "native_turn_id": "claude-turn-1",
		"stream_id": "stream-1", "worker_status": "absent", "worker_pid": 6101,
		"worker_proc_start": "start-6101", "worker_strong_start": "strong-6101",
		"limitation":        "claude_stream_stdio_is_not_reattachable",
		"native_transcript": map[string]any{"session_id": "claude-native-1", "resume_supported": true},
	}
	adapter := newClaudeDaemonAdapter(client)
	lane, turn := claudeDaemonLaneFixture()

	reconnected, err := adapter.ReconnectTurn(context.Background(), lane, turn)
	if err != nil {
		t.Fatalf("record evidence-approved Claude interruption: %v", err)
	}
	if reconnected.DispatchState != "interrupted" || reconnected.TerminalOutcome != "interrupted" ||
		!boolValue(reconnected.ResultReference["collectable"]) || !boolValue(reconnected.ResultReference["resumable"]) ||
		reconnected.ResultReference["restart_evidence"] != "claude_stream_stdio_is_not_reattachable" ||
		reconnected.NativeTurnIdentity["native_turn_id"] != "claude-turn-1" {
		t.Fatalf("evidence-approved Claude interruption = %#v", reconnected)
	}
	if client.startCalls != 0 || client.managerLaunches != 0 {
		t.Fatalf("Claude interruption redispatched=%d or launched manager=%d", client.startCalls, client.managerLaunches)
	}

	client.reconnectResult = map[string]any{
		"reconnectable": false, "session_id": "claude-native-1", "native_turn_id": "claude-turn-1",
	}
	if result, err := adapter.ReconnectTurn(context.Background(), lane, turn); err == nil ||
		result.DispatchState == "interrupted" || result.TerminalOutcome == "interrupted" {
		t.Fatalf("Claude restart without limitation evidence = %#v, %v; want fail-closed non-terminal result", result, err)
	}

	client.reconnectResult = map[string]any{
		"reconnectable": false, "session_id": "different-claude-session", "native_turn_id": "claude-turn-1",
		"worker_status": "absent", "limitation": "claude_stream_stdio_is_not_reattachable",
		"native_transcript": map[string]any{"session_id": "different-claude-session", "resume_supported": true},
	}
	if result, err := adapter.ReconnectTurn(context.Background(), lane, turn); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) ||
		result.DispatchState == "interrupted" {
		t.Fatalf("changed Claude restart evidence = %#v, %v; want exact-evidence rejection", result, err)
	}
}

func claudeDaemonLaneFixture() (daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) {
	lane := daemonpkg.LaneRecord{
		LaneSessionID: "claude-lane-1", Name: "claude-review", Product: "claude", Cwd: "/work",
		PermissionMode: "dontAsk", State: "running", ActiveTurnID: "turn-1",
		NativeActor: map[string]any{
			"session_id": "claude-native-1", "worker_pid": 6101,
			"worker_proc_start": "start-6101", "worker_strong_start": "strong-6101",
			"stream_id": "stream-1", "profile": "/profiles/claude", "permission_mode": "dontAsk",
		},
	}
	turn := daemonpkg.LaneTurnRecord{
		TurnID: "turn-1", LaneSessionID: lane.LaneSessionID, DispatchState: "accepted",
		InputReference: map[string]any{"prompt": "review the change"},
		NativeTurnIdentity: map[string]any{
			"session_id": "claude-native-1", "native_turn_id": "claude-turn-1", "stream_id": "stream-1",
		},
	}
	return lane, turn
}

type claudeDaemonLaneTestClient struct {
	session claudeDaemonSession

	startResult     map[string]any
	reconnectResult map[string]any
	collectResult   map[string]any
	cleanupErr      error

	startCalls, reconnectCalls, interruptCalls, collectCalls, archiveCalls, cleanupCalls int
	managerLaunches, vendorArtifactsRemoved                                              int
	startedLane, cleanedLane                                                             daemonpkg.LaneRecord
	startedTurn                                                                          daemonpkg.LaneTurnRecord
	interruptedSession, interruptedTurn                                                  string
	archivedSession                                                                      string
}

func newClaudeDaemonLaneTestClient() *claudeDaemonLaneTestClient {
	return &claudeDaemonLaneTestClient{session: claudeDaemonSession{
		SessionID: "claude-native-1", Cwd: "/work", Profile: "/profiles/claude", PID: 6101,
		ProcStart: "start-6101", Socket: "/tmp/claude-native-1.sock", SyntheticService: true,
	}}
}

func (client *claudeDaemonLaneTestClient) PrepareInteractive(
	_ context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "claude", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *claudeDaemonLaneTestClient) ResolveSession(
	_ context.Context,
	_, _ string,
) (claudeDaemonSession, bool, error) {
	return client.session, false, nil
}

func (client *claudeDaemonLaneTestClient) ObserveSession(
	_ context.Context,
	_ string,
	_ int,
) (claudeDaemonSession, error) {
	return client.session, nil
}

func (client *claudeDaemonLaneTestClient) InspectSession(
	_ context.Context,
	_, _ string,
) (claudeDaemonSession, error) {
	return client.session, nil
}

func (client *claudeDaemonLaneTestClient) DeliverFrame(
	_ context.Context,
	_ string,
	_ federation.AgentFrame,
) error {
	return nil
}

func (client *claudeDaemonLaneTestClient) StartClaudeTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	client.startCalls++
	client.startedLane, client.startedTurn = lane, turn
	return cloneClaudeDaemonLaneMap(client.startResult), nil
}

func (client *claudeDaemonLaneTestClient) ReconnectClaudeTurn(
	_ context.Context,
	_ daemonpkg.LaneRecord,
	_ daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	client.reconnectCalls++
	return cloneClaudeDaemonLaneMap(client.reconnectResult), nil
}

func (client *claudeDaemonLaneTestClient) InterruptClaudeTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	client.interruptCalls++
	client.interruptedSession = stringValue(lane.NativeActor["session_id"])
	client.interruptedTurn = stringValue(turn.NativeTurnIdentity["native_turn_id"])
	return nil
}

func (client *claudeDaemonLaneTestClient) CollectClaudeTurn(
	_ context.Context,
	_ daemonpkg.LaneRecord,
	_ daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	client.collectCalls++
	return cloneClaudeDaemonLaneMap(client.collectResult), nil
}

func (client *claudeDaemonLaneTestClient) ArchiveClaudeLane(
	_ context.Context,
	lane daemonpkg.LaneRecord,
) error {
	client.archiveCalls++
	client.archivedSession = stringValue(lane.NativeActor["session_id"])
	return nil
}

func (client *claudeDaemonLaneTestClient) CleanupClaudeLane(
	_ context.Context,
	lane daemonpkg.LaneRecord,
) error {
	client.cleanupCalls++
	client.cleanedLane = lane
	return client.cleanupErr
}

func cloneClaudeDaemonLaneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneClaudeDaemonLaneMap(typed)
		case []any:
			output[key] = append([]any(nil), typed...)
		default:
			output[key] = value
		}
	}
	return output
}

func TestClaudeDaemonLaneClientFixtureCoversNativeOperations(t *testing.T) {
	client := newClaudeDaemonLaneTestClient()
	want := []string{"start", "reconnect", "interrupt", "collect", "archive", "cleanup"}
	got := []string{}
	if _, ok := reflect.TypeOf(client).MethodByName("StartClaudeTurn"); ok {
		got = append(got, "start")
	}
	if _, ok := reflect.TypeOf(client).MethodByName("ReconnectClaudeTurn"); ok {
		got = append(got, "reconnect")
	}
	if _, ok := reflect.TypeOf(client).MethodByName("InterruptClaudeTurn"); ok {
		got = append(got, "interrupt")
	}
	if _, ok := reflect.TypeOf(client).MethodByName("CollectClaudeTurn"); ok {
		got = append(got, "collect")
	}
	if _, ok := reflect.TypeOf(client).MethodByName("ArchiveClaudeLane"); ok {
		got = append(got, "archive")
	}
	if _, ok := reflect.TypeOf(client).MethodByName("CleanupClaudeLane"); ok {
		got = append(got, "cleanup")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Claude daemon lane fixture methods = %v, want %v", got, want)
	}
}
