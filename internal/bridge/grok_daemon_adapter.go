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
}

type grokDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	ResolveSession(context.Context, string) (grokDaemonSession, bool, error)
	InspectSession(context.Context, string) (grokDaemonSession, error)
	InterjectFrame(context.Context, string, federation.AgentFrame) error
}

// PrepareInteractive returns the direct Grok vendor handoff for one validated launch intent.
func (adapter *grokDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type grokDaemonAdapter struct{ client grokDaemonClient }

func newGrokDaemonAdapter(client grokDaemonClient) *grokDaemonAdapter {
	return &grokDaemonAdapter{client: client}
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

//nolint:dupl // Product-specific evidence and readiness rules intentionally remain explicit adapter contracts.
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
				) {
				return nil, daemonpkg.ErrAttachmentEvidenceChanged
			}
			if !session.ACPReady {
				return nil, ErrGrokACPUnavailable
			}
			return map[string]any{
				"session_id": session.SessionID, "owner_pid": session.OwnerPID,
				"owner_proc_start": session.OwnerProcStart, "leader_session_id": session.LeaderSessionID,
				"profile": session.Profile, "cwd": session.Cwd, "acp_ready": true,
			}, nil
		})
}
