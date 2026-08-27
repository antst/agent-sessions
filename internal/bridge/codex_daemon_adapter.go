package bridge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

// ErrCodexHistoryProjectionUnavailable reports a native App Server history-readiness gap.
var ErrCodexHistoryProjectionUnavailable = errors.New("codex thread history projection is unavailable")

type codexDaemonThread struct {
	ID           string
	Cwd          string
	Profile      string
	PID          int
	ProcStart    string
	HistoryReady bool
}

type codexDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	InspectThread(context.Context, string) (codexDaemonThread, error)
	DeliverFrame(context.Context, string, federation.AgentFrame) error
}

// PrepareInteractive returns the direct Codex vendor handoff for one validated launch intent.
func (adapter *codexDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type codexDaemonAdapter struct{ client codexDaemonClient }

func newCodexDaemonAdapter(client codexDaemonClient) *codexDaemonAdapter {
	return &codexDaemonAdapter{client: client}
}

// Corroborate proves that a prepared Codex attachment selected the expected native thread.
func (adapter *codexDaemonAdapter) Corroborate(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	threadID := record.SessionID
	if threadID == "" {
		threadID = stringValue(evidence["thread_id"])
	}
	if threadID == "" {
		threadID = stringValue(record.NativeActor["thread_id"])
	}
	return adapter.inspectExact(ctx, record, threadID, evidence)
}

// Reconnect revalidates an already attached Codex thread after daemon recovery.
func (adapter *codexDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver forwards one already-admitted frame through the exact Codex thread.
func (adapter *codexDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	if _, err := adapter.Reconnect(ctx, destination); err != nil {
		return err
	}
	return adapter.client.DeliverFrame(ctx, destination.SessionID, frame)
}

func (adapter *codexDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	threadID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(threadID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	thread, err := adapter.client.InspectThread(ctx, threadID)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex App Server thread: %w", err)
	}
	if !matchesDaemonSession(record, threadID, thread.ID, thread.Cwd, thread.Profile) ||
		!matchesDaemonActorEvidence(record.NativeActor, evidence,
			daemonActorField{key: "pid", value: thread.PID},
			daemonActorField{key: "proc_start", value: thread.ProcStart},
		) {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if !thread.HistoryReady {
		return nil, fmt.Errorf("%w for %s; run `codex migrate-rollouts --apply` and retry", ErrCodexHistoryProjectionUnavailable, threadID)
	}
	return map[string]any{
		"thread_id": thread.ID, "pid": thread.PID, "proc_start": thread.ProcStart,
		"profile": thread.Profile, "cwd": thread.Cwd, "history_ready": true,
	}, nil
}

type daemonActorField struct {
	key   string
	value any
}

func inspectDaemonActor[T any](
	ctx context.Context,
	sessionID, product string,
	available bool,
	inspect func(context.Context, string) (T, error),
	verify func(T) (map[string]any, error),
) (map[string]any, error) {
	if !available || strings.TrimSpace(sessionID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor, err := inspect(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("inspect %s native session: %w", product, err)
	}
	return verify(actor)
}

func matchesDaemonSession(record daemonpkg.AttachmentRecord, expectedID, observedID, cwd, profile string) bool {
	return observedID == expectedID && cwd == record.Cwd && matchesOptionalString(record.ProfileIdentity["profile"], profile)
}

func matchesDaemonActorEvidence(recorded, supplied map[string]any, fields ...daemonActorField) bool {
	for _, field := range fields {
		if !matchesOptionalValue(recorded[field.key], field.value) || !matchesOptionalValue(supplied[field.key], field.value) {
			return false
		}
	}
	return true
}

func matchesOptionalValue(expected, observed any) bool {
	if expected == nil {
		return true
	}
	switch value := observed.(type) {
	case string:
		return matchesOptionalString(expected, value)
	case int:
		return matchesOptionalNumber(expected, value)
	default:
		return reflect.DeepEqual(expected, observed)
	}
}

func matchesOptionalString(expected any, observed string) bool {
	return expected == nil || stringValue(expected) == "" || stringValue(expected) == observed
}

func matchesOptionalNumber(expected any, observed int) bool {
	if expected == nil {
		return true
	}
	switch value := expected.(type) {
	case int:
		return value == observed
	case int64:
		return value == int64(observed)
	case float64:
		return value == float64(observed)
	default:
		return reflect.DeepEqual(expected, observed)
	}
}
