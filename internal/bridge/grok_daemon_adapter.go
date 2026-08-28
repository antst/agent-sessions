package bridge

import (
	"context"
	"errors"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

// ErrGrokACPUnavailable reports a roster actor without a usable daemon-owned ACP channel.
var ErrGrokACPUnavailable = errors.New("grok ACP session is unavailable")

type grokDaemonSession struct {
	SessionID       string
	Name            string
	Cwd             string
	Profile         string
	OwnerPID        int
	OwnerProcStart  string
	LeaderSessionID string
	ACPReady        bool
	CoordinatorID   string
}

type grokDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	ResolveSession(context.Context, string) (grokDaemonSession, bool, error)
	ObserveSession(context.Context, daemonpkg.AttachmentRecord, int) (grokDaemonSession, error)
	InspectSession(context.Context, string) (grokDaemonSession, error)
	InterjectFrame(context.Context, string, federation.AgentFrame) error
}

// grokDaemonLaneClient is the product-native half of the shared daemon lane
// contract. Implementations own Grok ACP workers and sessions in process; they
// never publish a lane-manager socket or a second durable lane catalog.
type grokDaemonLaneClient interface {
	StartGrokTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (map[string]any, error)
	ReconnectGrokTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (map[string]any, error)
	InterruptGrokTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) error
	WaitGrokTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (map[string]any, error)
	CollectGrokTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (map[string]any, error)
	ArchiveGrokLane(context.Context, daemonpkg.LaneRecord) error
	CleanupGrokLane(context.Context, daemonpkg.LaneRecord) error
}

// PrepareInteractive returns the direct Grok vendor handoff for one validated launch intent.
func (adapter *grokDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type grokDaemonAdapter struct{ client grokDaemonClient }

var _ daemonpkg.LaneWaitAdapter = (*grokDaemonAdapter)(nil)

func newGrokDaemonAdapter(client grokDaemonClient) *grokDaemonAdapter {
	return &grokDaemonAdapter{client: client}
}

// NewGrokDaemonAdapter constructs the in-process Grok leader and ACP coordinator.
func NewGrokDaemonAdapter() *grokDaemonAdapter {
	return newGrokDaemonAdapter(newGrokNativeCoordinator())
}

// Close stops only vendor processes owned by this daemon generation.
func (adapter *grokDaemonAdapter) Close() {
	if coordinator, ok := adapter.client.(*grokNativeCoordinator); ok {
		coordinator.close()
	}
}

// ResolveSelection maps an exact UUID or unique Grok name to one roster session.
func (adapter *grokDaemonAdapter) ResolveSelection(ctx context.Context, selector string) (grokDaemonSession, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(selector) == "" {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	session, ambiguous, err := adapter.client.ResolveSession(ctx, selector)
	if err != nil {
		return grokDaemonSession{}, err
	}
	if ambiguous {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentAmbiguous
	}
	if session.SessionID == "" {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return session, nil
}

// ObserveConnector binds a Grok attachment to the private leader's one live
// resident session using the connector's exact native process ancestry.
//
//nolint:dupl // Product-specific observation remains explicit at the adapter boundary.
func (adapter *grokDaemonAdapter) ObserveConnector(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence daemonpkg.ConnectorProcessEvidence,
) (string, map[string]any, error) {
	if adapter == nil || adapter.client == nil || evidence.PID <= 1 {
		return "", nil, daemonpkg.ErrAttachmentSelecting
	}
	session, err := adapter.client.ObserveSession(ctx, record, evidence.PID)
	if err != nil {
		return "", nil, err
	}
	if session.Cwd != record.Cwd || !matchesOptionalString(record.ProfileIdentity["profile"], session.Profile) {
		return "", nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return session.SessionID, grokSessionActor(session), nil
}

// Corroborate proves that a prepared Grok attachment selected the expected roster actor.
func (adapter *grokDaemonAdapter) Corroborate(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	sessionID := record.SessionID
	if sessionID == "" {
		sessionID = stringValue(evidence["session_id"])
	}
	if sessionID == "" {
		sessionID = stringValue(record.NativeActor["session_id"])
	}
	return adapter.inspectExact(ctx, record, sessionID, evidence)
}

// Reconnect revalidates an already attached Grok session after daemon recovery.
func (adapter *grokDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver forwards one already-admitted frame through the exact Grok ACP session.
func (adapter *grokDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	if _, err := adapter.Reconnect(ctx, destination); err != nil {
		return err
	}
	return adapter.client.InterjectFrame(ctx, destination.SessionID, frame)
}

// Dispatch is the compatibility projection used by the daemon lane engine.
func (adapter *grokDaemonAdapter) Dispatch(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	return adapter.StartTurn(ctx, lane, turn)
}

// StartTurn delegates one already-committed turn to the in-process Grok ACP
// actor and returns only exact native identities to the daemon authority.
func (adapter *grokDaemonAdapter) StartTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	result, err := client.StartGrokTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	actor, nativeTurn, err := validateGrokLaneDispatchEvidence(lane, turn, result)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	return daemonpkg.LaneDispatchResult{
		LaneSessionID: lane.LaneSessionID, NativeActor: actor,
		NativeTurnIdentity: nativeTurn, DispatchState: daemonpkg.LaneDispatchRunning,
	}, nil
}

// ReconnectTurn never redispatches accepted work. A live in-generation ACP
// actor may continue; a successor daemon may record the one bounded Grok stdio
// interruption only when exact native evidence and transcript resume support
// are both present.
func (adapter *grokDaemonAdapter) ReconnectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneReconnectResult, error) {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	result, err := client.ReconnectGrokTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	actor, nativeTurn, err := validateGrokLaneReconnectEvidence(lane, turn, result)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	if reconnectable, _ := result["reconnectable"].(bool); reconnectable {
		return daemonpkg.LaneReconnectResult{
			NativeActor: actor, NativeTurnIdentity: nativeTurn, DispatchState: daemonpkg.LaneDispatchRunning,
		}, nil
	}
	limitation := stringValue(result["limitation"])
	transcript, _ := result["native_transcript"].(map[string]any)
	resumeSupported, _ := transcript["resume_supported"].(bool)
	if limitation != "grok_acp_stdio_is_not_reattachable" ||
		stringValue(result["worker_status"]) == "" ||
		stringValue(transcript["session_id"]) != stringValue(actor["session_id"]) || !resumeSupported {
		return daemonpkg.LaneReconnectResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return daemonpkg.LaneReconnectResult{
		NativeActor: actor, NativeTurnIdentity: nativeTurn,
		DispatchState: daemonpkg.LaneDispatchInterrupted, TerminalOutcome: daemonpkg.LaneDispatchInterrupted,
		ResultReference: map[string]any{
			"collectable": true, "resumable": true, "restart_evidence": limitation,
		},
	}, nil
}

// InterruptTurn cancels only the exact accepted ACP turn.
func (adapter *grokDaemonAdapter) InterruptTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return err
	}
	return client.InterruptGrokTurn(ctx, lane, turn)
}

// CollectTurn returns stable native terminal metadata. The daemon, not the
// adapter, owns and advances CollectionCursor/CollectionRevision.
func (adapter *grokDaemonAdapter) CollectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	result, err := client.CollectGrokTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	return grokLaneTerminalResult(lane, turn, result)
}

// WaitTurn blocks on the resident actor's terminal signal without advancing
// the daemon-owned collection cursor. The daemon terminal watcher uses this
// optional boundary to commit Complete exactly once when ACP finishes.
func (adapter *grokDaemonAdapter) WaitTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	result, err := client.WaitGrokTurn(ctx, lane, turn)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	return grokLaneTerminalResult(lane, turn, result)
}

func grokLaneTerminalResult(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	result map[string]any,
) (daemonpkg.LaneTerminalResult, error) {
	_, nativeTurn, err := validateGrokLaneResultIdentity(lane, turn, result)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	outcome := stringValue(result["terminal_outcome"])
	if !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, outcome) {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	reference, _ := result["result_reference"].(map[string]any)
	return daemonpkg.LaneTerminalResult{
		TerminalOutcome: outcome, ResultReference: cloneGrokLaneEvidence(reference), NativeTurnIdentity: nativeTurn,
	}, nil
}

// Archive retires the exact native Grok lane actor without deleting the
// vendor-owned transcript.
func (adapter *grokDaemonAdapter) Archive(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return err
	}
	return client.ArchiveGrokLane(ctx, lane)
}

// Cleanup removes only the exact Agent Sessions-owned actor artifacts.
func (adapter *grokDaemonAdapter) Cleanup(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.grokLaneClient()
	if err != nil {
		return err
	}
	return client.CleanupGrokLane(ctx, lane)
}

func (adapter *grokDaemonAdapter) grokLaneClient() (grokDaemonLaneClient, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	client, ok := adapter.client.(grokDaemonLaneClient)
	if !ok {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return client, nil
}

func validateGrokLaneDispatchEvidence(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	result map[string]any,
) (map[string]any, map[string]any, error) {
	actor, nativeTurn, err := validateGrokLaneResultIdentity(lane, turn, result)
	if err != nil {
		return nil, nil, err
	}
	if intValue(actor["worker_pid"]) <= 1 || stringValue(actor["worker_proc_start"]) == "" ||
		stringValue(actor["worker_strong_start"]) == "" {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return actor, nativeTurn, nil
}

func validateGrokLaneReconnectEvidence(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	result map[string]any,
) (map[string]any, map[string]any, error) {
	actor, nativeTurn, err := validateGrokLaneResultIdentity(lane, turn, result)
	if err != nil {
		return nil, nil, err
	}
	if expected := lane.NativeActor["worker_pid"]; expected != nil && intValue(expected) != intValue(actor["worker_pid"]) {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	for _, key := range []string{"worker_proc_start", "worker_strong_start"} {
		if expected := stringValue(lane.NativeActor[key]); expected != "" && expected != stringValue(actor[key]) {
			return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
		}
	}
	return actor, nativeTurn, nil
}

func validateGrokLaneResultIdentity(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
	result map[string]any,
) (map[string]any, map[string]any, error) {
	sessionID := strings.TrimSpace(stringValue(result["session_id"]))
	nativeTurnID := strings.TrimSpace(stringValue(result["native_turn_id"]))
	if sessionID == "" || nativeTurnID == "" {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if expected := stringValue(lane.NativeActor["session_id"]); expected != "" && expected != sessionID {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if expected := stringValue(turn.NativeTurnIdentity["native_turn_id"]); expected != "" && expected != nativeTurnID {
		return nil, nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor := map[string]any{"session_id": sessionID}
	for _, key := range []string{"worker_pid", "worker_proc_start", "worker_strong_start"} {
		if value := result[key]; value != nil {
			actor[key] = value
		} else if value = lane.NativeActor[key]; value != nil {
			actor[key] = value
		}
	}
	return actor, map[string]any{"session_id": sessionID, "native_turn_id": nativeTurnID}, nil
}

func cloneGrokLaneEvidence(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			result[key] = cloneGrokLaneEvidence(typed)
		case []any:
			result[key] = append([]any(nil), typed...)
		default:
			result[key] = value
		}
	}
	return result
}

func (adapter *grokDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	sessionID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return inspectDaemonActor(ctx, sessionID, "Grok", true, adapter.client.InspectSession,
		func(session grokDaemonSession) (map[string]any, error) {
			if !matchesDaemonSession(record, sessionID, session.SessionID, session.Cwd, session.Profile) ||
				!matchesDaemonActorEvidence(record.NativeActor, evidence,
					daemonActorField{key: "owner_pid", value: session.OwnerPID},
					daemonActorField{key: "owner_proc_start", value: session.OwnerProcStart},
					daemonActorField{key: "leader_session_id", value: session.LeaderSessionID},
					daemonActorField{key: "coordinator_id", value: session.CoordinatorID},
				) {
				return nil, daemonpkg.ErrAttachmentEvidenceChanged
			}
			if !session.ACPReady {
				return nil, ErrGrokACPUnavailable
			}
			return grokSessionActor(session), nil
		})
}

func grokSessionActor(session grokDaemonSession) map[string]any {
	return map[string]any{
		"session_id": session.SessionID, "owner_pid": session.OwnerPID,
		"owner_proc_start": session.OwnerProcStart, "leader_session_id": session.LeaderSessionID,
		"coordinator_id": session.CoordinatorID, "profile": session.Profile,
		"cwd": session.Cwd, "acp_ready": session.ACPReady,
	}
}
