package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

var (
	// ErrQwenDualOutputUnavailable reports missing native event/input admission evidence.
	ErrQwenDualOutputUnavailable = errors.New("qwen dual-output admission is unavailable")
	// ErrQwenReadinessUnavailable reports a missing or stale native readiness artifact.
	ErrQwenReadinessUnavailable = errors.New("qwen readiness is unavailable")
)

type qwenDaemonSession struct {
	SessionID      string
	Name           string
	Cwd            string
	Profile        string
	Status         string
	PermissionMode string
	PID            int
	ProcStart      string
	ParentPID      int
	EventPath      string
	InputPath      string
	ReadinessPath  string
	Ready          bool
	DualOutput     bool
	CoordinatorID  string
}

type qwenDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	ResolveSession(context.Context, string) (qwenDaemonSession, bool, error)
	ObserveSession(context.Context, daemonpkg.AttachmentRecord, int) (qwenDaemonSession, error)
	InspectSession(context.Context, string) (qwenDaemonSession, error)
	WriteInput(context.Context, string, federation.AgentFrame) error
}

// PrepareInteractive returns the direct Qwen vendor handoff for one validated launch intent.
func (adapter *qwenDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type qwenDaemonAdapter struct{ client qwenDaemonClient }

func newQwenDaemonAdapter(client qwenDaemonClient) *qwenDaemonAdapter {
	return &qwenDaemonAdapter{client: client}
}

// NewQwenDaemonAdapter constructs the in-process Qwen dual-output coordinator.
func NewQwenDaemonAdapter() *qwenDaemonAdapter {
	return newQwenDaemonAdapter(newQwenNativeCoordinator())
}

// Close releases daemon descriptors without stopping vendor-owned Qwen processes.
func (adapter *qwenDaemonAdapter) Close() {
	if coordinator, ok := adapter.client.(*qwenNativeCoordinator); ok {
		coordinator.close()
	}
}

// ResolveSelection maps an exact UUID or unique Qwen name to one native session.
func (adapter *qwenDaemonAdapter) ResolveSelection(ctx context.Context, selector string) (qwenDaemonSession, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(selector) == "" {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	session, ambiguous, err := adapter.client.ResolveSession(ctx, selector)
	if err != nil {
		return qwenDaemonSession{}, err
	}
	if ambiguous {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentAmbiguous
	}
	if session.SessionID == "" {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return session, nil
}

// ObserveConnector adopts the exact Qwen process and first dual-output event
// selected by a daemon-prepared connector ancestry.
//
//nolint:dupl // Product-specific observation remains explicit at the adapter boundary.
func (adapter *qwenDaemonAdapter) ObserveConnector(
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
	return session.SessionID, qwenSessionActor(session), nil
}

// Corroborate proves that a prepared Qwen attachment selected the expected native actor.
func (adapter *qwenDaemonAdapter) Corroborate(
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

// Reconnect revalidates an already attached Qwen session after daemon recovery.
func (adapter *qwenDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver writes one already-admitted frame to the corroborated Qwen input artifact.
func (adapter *qwenDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	actor, err := adapter.Reconnect(ctx, destination)
	if err != nil {
		return err
	}
	return adapter.client.WriteInput(ctx, stringValue(actor["input_path"]), frame)
}

func (adapter *qwenDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	sessionID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(sessionID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	session, err := adapter.client.InspectSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("inspect Qwen native session: %w", err)
	}
	if !matchesDaemonSession(record, sessionID, session.SessionID, session.Cwd, session.Profile) ||
		!matchesDaemonActorEvidence(record.NativeActor, evidence,
			daemonActorField{key: "pid", value: session.PID},
			daemonActorField{key: "proc_start", value: session.ProcStart},
			daemonActorField{key: "parent_pid", value: session.ParentPID},
			daemonActorField{key: "event_path", value: session.EventPath},
			daemonActorField{key: "input_path", value: session.InputPath},
			daemonActorField{key: "readiness_path", value: session.ReadinessPath},
			daemonActorField{key: "coordinator_id", value: session.CoordinatorID},
		) {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if !session.DualOutput {
		return nil, ErrQwenDualOutputUnavailable
	}
	if !session.Ready {
		return nil, ErrQwenReadinessUnavailable
	}
	return qwenSessionActor(session), nil
}

func qwenSessionActor(session qwenDaemonSession) map[string]any {
	return map[string]any{
		"session_id": session.SessionID, "pid": session.PID, "proc_start": session.ProcStart,
		"parent_pid": session.ParentPID, "profile": session.Profile, "cwd": session.Cwd,
		"status": session.Status, "permission_mode": session.PermissionMode,
		"event_path": session.EventPath, "input_path": session.InputPath,
		"readiness_path": session.ReadinessPath, "coordinator_id": session.CoordinatorID,
		"dual_output": session.DualOutput, "ready": session.Ready,
	}
}
