package bridge

import (
	"context"
	"errors"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

// ErrClaudeSyntheticServiceUnavailable reports a missing vendor-required local service projection.
var ErrClaudeSyntheticServiceUnavailable = errors.New("claude synthetic Agent Sessions service is unavailable")

type claudeDaemonSession struct {
	SessionID        string
	Name             string
	Cwd              string
	Profile          string
	PID              int
	ProcStart        string
	Socket           string
	SyntheticService bool
}

type claudeDaemonClient interface {
	ResolveSession(context.Context, string) (claudeDaemonSession, bool, error)
	InspectSession(context.Context, string) (claudeDaemonSession, error)
	DeliverFrame(context.Context, string, federation.AgentFrame) error
}

type claudeDaemonAdapter struct{ client claudeDaemonClient }

func newClaudeDaemonAdapter(client claudeDaemonClient) *claudeDaemonAdapter {
	return &claudeDaemonAdapter{client: client}
}

// ResolveSelection maps an exact UUID or unique Claude name to one native session.
func (adapter *claudeDaemonAdapter) ResolveSelection(ctx context.Context, selector string) (claudeDaemonSession, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(selector) == "" {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	session, ambiguous, err := adapter.client.ResolveSession(ctx, selector)
	if err != nil {
		return claudeDaemonSession{}, err
	}
	if ambiguous {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentAmbiguous
	}
	if session.SessionID == "" {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return session, nil
}

// Corroborate proves that a prepared Claude attachment selected the expected native session.
func (adapter *claudeDaemonAdapter) Corroborate(
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

// Reconnect revalidates an already attached Claude session after daemon recovery.
func (adapter *claudeDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver forwards one already-admitted frame through the corroborated Claude socket.
func (adapter *claudeDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	actor, err := adapter.Reconnect(ctx, destination)
	if err != nil {
		return err
	}
	return adapter.client.DeliverFrame(ctx, stringValue(actor["socket"]), frame)
}

//nolint:dupl // Product-specific evidence and readiness rules intentionally remain explicit adapter contracts.
func (adapter *claudeDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	sessionID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return inspectDaemonActor(ctx, sessionID, "Claude", true, adapter.client.InspectSession,
		func(session claudeDaemonSession) (map[string]any, error) {
			if !matchesDaemonSession(record, sessionID, session.SessionID, session.Cwd, session.Profile) ||
				!matchesDaemonActorEvidence(record.NativeActor, evidence,
					daemonActorField{key: "pid", value: session.PID},
					daemonActorField{key: "proc_start", value: session.ProcStart},
					daemonActorField{key: "socket", value: session.Socket},
				) {
				return nil, daemonpkg.ErrAttachmentEvidenceChanged
			}
			if !session.SyntheticService {
				return nil, ErrClaudeSyntheticServiceUnavailable
			}
			return map[string]any{
				"session_id": session.SessionID, "pid": session.PID, "proc_start": session.ProcStart,
				"profile": session.Profile, "cwd": session.Cwd, "socket": session.Socket,
				"synthetic_service": true,
			}, nil
		})
}
