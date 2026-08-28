package bridge

import (
	"context"
	"errors"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

var (
	_ daemonpkg.LaneAdapter          = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneStartAdapter     = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneReconnectAdapter = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneInterruptAdapter = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneCollectAdapter   = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneWaitAdapter      = (*qwenDaemonAdapter)(nil)
	_ daemonpkg.LaneCleanupAdapter   = (*qwenDaemonAdapter)(nil)
)

// Dispatch preserves the transitional LaneAdapter contract while the daemon
// prefers StartTurn for every newly accepted turn.
func (adapter *qwenDaemonAdapter) Dispatch(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	return adapter.StartTurn(ctx, lane, turn)
}

// StartTurn opens or resumes one Qwen ACP session and dispatches exactly the
// already-accepted daemon turn. The vendor worker is owned by the in-process
// coordinator; no Agent Sessions lane manager or listener is started.
func (adapter *qwenDaemonAdapter) StartTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	if err := validateQwenDaemonLane(lane, turn, false); err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	raw, err := client.StartQwenTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	actor, identity, err := qwenDaemonLaneEvidence(lane, turn, raw)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	return daemonpkg.LaneDispatchResult{
		LaneSessionID: lane.LaneSessionID, NativeActor: actor,
		NativeTurnIdentity: identity, DispatchState: daemonpkg.LaneDispatchRunning,
	}, nil
}

// ReconnectTurn re-attaches only when the exact in-memory ACP actor survived.
// A replacement daemon may instead record the one documented interruption,
// but only from exact dead-worker and resumable-native-session evidence.
func (adapter *qwenDaemonAdapter) ReconnectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneReconnectResult, error) {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	if err := validateQwenDaemonLane(lane, turn, true); err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	raw, err := client.ReconnectQwenTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	actor, identity, err := qwenDaemonLaneEvidence(lane, turn, raw)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	if reconnectable, _ := raw["reconnectable"].(bool); reconnectable {
		outcome := strings.TrimSpace(stringValue(raw["terminal_outcome"]))
		state := daemonpkg.LaneDispatchRunning
		if outcome != "" {
			state = outcome
		}
		return daemonpkg.LaneReconnectResult{
			NativeActor: actor, NativeTurnIdentity: identity, DispatchState: state,
			TerminalOutcome: outcome, ResultReference: cloneQwenDaemonLaneMap(mapValue(raw["result_reference"])),
		}, nil
	}
	transcript := mapValue(raw["native_transcript"])
	limitation := strings.TrimSpace(stringValue(raw["limitation"]))
	if stringValue(raw["worker_status"]) != "absent" ||
		limitation != "qwen_acp_stdio_is_not_reattachable" ||
		stringValue(transcript["session_id"]) != qwenDaemonLaneSessionID(lane) ||
		!boolValue(transcript["resume_supported"]) {
		return daemonpkg.LaneReconnectResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return daemonpkg.LaneReconnectResult{
		NativeActor: actor, NativeTurnIdentity: identity,
		DispatchState: daemonpkg.LaneDispatchInterrupted, TerminalOutcome: daemonpkg.LaneDispatchInterrupted,
		ResultReference: map[string]any{
			"collectable": true, "resumable": true, "restart_evidence": limitation,
			"native_session_id": qwenDaemonLaneSessionID(lane),
		},
	}, nil
}

// InterruptTurn cancels only the exact native Qwen session/turn pair.
func (adapter *qwenDaemonAdapter) InterruptTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return err
	}
	if err := validateQwenDaemonLane(lane, turn, true); err != nil {
		return err
	}
	return client.InterruptQwenTurn(ctx, lane, turn)
}

// CollectTurn maps Qwen's exact session/update stream and prompt response into
// stable terminal metadata. The daemon, not this adapter, advances collection.
func (adapter *qwenDaemonAdapter) CollectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	if err := validateQwenDaemonLane(lane, turn, true); err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	raw, err := client.CollectQwenTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	return qwenDaemonTerminalResult(lane, turn, raw)
}

// WaitTurn blocks on the actor-owned ACP prompt response and returns the same
// stable terminal projection as collection. It gives the daemon a callback
// seam for Complete/notices without creating a watcher process or listener.
func (adapter *qwenDaemonAdapter) WaitTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	return adapter.CollectTurn(ctx, lane, turn)
}

// Archive invokes only Qwen's supported native history archive contract.
func (adapter *qwenDaemonAdapter) Archive(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return err
	}
	if err := validateQwenDaemonLaneActor(lane); err != nil {
		return err
	}
	return client.ArchiveQwenLane(ctx, lane)
}

// Cleanup retires exact Agent Sessions-owned ACP resources and never removes a
// Qwen transcript, profile, credential, or other vendor-owned artifact.
func (adapter *qwenDaemonAdapter) Cleanup(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.qwenLaneClient()
	if err != nil {
		return err
	}
	if err := validateQwenDaemonLaneActor(lane); err != nil {
		return err
	}
	return client.CleanupQwenLane(ctx, lane)
}

func (adapter *qwenDaemonAdapter) qwenLaneClient() (qwenDaemonLaneClient, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	client, ok := adapter.client.(qwenDaemonLaneClient)
	if !ok {
		return nil, errors.New("qwen daemon coordinator does not implement lane ACP operations")
	}
	return client, nil
}

func validateQwenDaemonLane(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord, requireNative bool) error {
	if lane.Product != "qwen" || strings.TrimSpace(lane.LaneSessionID) == "" ||
		strings.TrimSpace(lane.Cwd) == "" || turn.LaneSessionID != lane.LaneSessionID ||
		strings.TrimSpace(turn.TurnID) == "" {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	if requireNative && (qwenDaemonLaneSessionID(lane) == "" ||
		strings.TrimSpace(stringValue(turn.NativeTurnIdentity["native_turn_id"])) == "") {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}

func validateQwenDaemonLaneActor(lane daemonpkg.LaneRecord) error {
	if lane.Product != "qwen" || strings.TrimSpace(lane.LaneSessionID) == "" ||
		strings.TrimSpace(lane.Cwd) == "" || qwenDaemonLaneSessionID(lane) == "" {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}

func qwenDaemonLaneEvidence(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	raw map[string]any,
) (map[string]any, map[string]any, error) {
	sessionID := defaultString(stringValue(raw["qwen_session_id"]), stringValue(raw["session_id"]))
	turnID := strings.TrimSpace(stringValue(raw["native_turn_id"]))
	expectedSession := qwenDaemonLaneSessionID(lane)
	expectedTurn := strings.TrimSpace(stringValue(turn.NativeTurnIdentity["native_turn_id"]))
	if sessionID == "" || turnID == "" || expectedSession != "" && sessionID != expectedSession ||
		expectedTurn != "" && turnID != expectedTurn {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor := map[string]any{"qwen_session_id": sessionID, "archive_session_id": sessionID}
	for _, key := range []string{
		"worker_pid", "worker_proc_start", "worker_strong_start", "profile", "cwd", "permission_mode",
		"qwen_home_set", "qwen_home", "qwen_runtime_dir_set", "qwen_runtime_dir",
	} {
		if value, ok := raw[key]; ok {
			actor[key] = value
		} else if value, ok := lane.NativeActor[key]; ok {
			actor[key] = value
		}
	}
	identity := map[string]any{"qwen_session_id": sessionID, "native_turn_id": turnID}
	if cursor := stringValue(raw["event_cursor"]); cursor != "" {
		identity["event_cursor"] = cursor
	} else if cursor := stringValue(turn.NativeTurnIdentity["event_cursor"]); cursor != "" {
		identity["event_cursor"] = cursor
	}
	return actor, identity, nil
}

func qwenDaemonTerminalResult( //nolint:gocyclo // Exact ACP event, response, and terminal evidence are validated together.
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	raw map[string]any,
) (daemonpkg.LaneTerminalResult, error) {
	expectedSession := qwenDaemonLaneSessionID(lane)
	expectedTurn := stringValue(turn.NativeTurnIdentity["native_turn_id"])
	if sessionID := defaultString(stringValue(raw["qwen_session_id"]), stringValue(raw["session_id"])); sessionID != "" && sessionID != expectedSession {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if turnID := stringValue(raw["native_turn_id"]); turnID != "" && turnID != expectedTurn {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	var content strings.Builder
	for _, item := range sliceValue(raw["events"]) {
		event := mapValue(item)
		if stringValue(event["method"]) != "session/update" {
			continue
		}
		params := mapValue(event["params"])
		if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != expectedSession {
			return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
		}
		if turnID := stringValue(params["turnId"]); turnID != "" && turnID != expectedTurn {
			return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
		}
		update := mapValue(params["update"])
		if stringValue(update["sessionUpdate"]) == "agent_message_chunk" {
			content.WriteString(defaultString(stringValue(mapValue(update["content"])["text"]), stringValue(update["text"])))
		}
	}
	response := mapValue(raw["response"])
	if sessionID := stringValue(response["sessionId"]); sessionID != "" && sessionID != expectedSession {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if turnID := stringValue(response["turnId"]); turnID != "" && turnID != expectedTurn {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	outcome := defaultString(stringValue(raw["terminal_outcome"]), daemonpkg.LaneDispatchCompleted)
	result := cloneQwenDaemonLaneMap(mapValue(raw["result_reference"]))
	if result == nil {
		result = map[string]any{}
	}
	if text := defaultString(stringValue(raw["content"]), content.String()); text != "" {
		result["content"] = text
	}
	if stop := defaultString(stringValue(raw["stop_reason"]), stringValue(response["stopReason"])); stop != "" {
		result["stop_reason"] = stop
	}
	identity := cloneQwenDaemonLaneMap(turn.NativeTurnIdentity)
	if cursor := stringValue(raw["event_cursor"]); cursor != "" {
		identity["event_cursor"] = cursor
	}
	return daemonpkg.LaneTerminalResult{
		TerminalOutcome: outcome, ResultReference: result, NativeTurnIdentity: identity,
	}, nil
}

func qwenDaemonLaneSessionID(lane daemonpkg.LaneRecord) string {
	for _, key := range []string{"qwen_session_id", "session_id", "archive_session_id"} {
		if value := strings.TrimSpace(stringValue(lane.NativeActor[key])); value != "" {
			return value
		}
	}
	return ""
}

func cloneQwenDaemonLaneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneQwenDaemonLaneMap(typed)
		case []any:
			output[key] = append([]any(nil), typed...)
		default:
			output[key] = value
		}
	}
	return output
}

func sliceValue(value any) []any {
	values, _ := value.([]any)
	return values
}
