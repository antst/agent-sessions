package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
)

// CodexAdapterConfig supplies only the native operations that cannot live in
// the shared attachment transaction.
type CodexAdapterConfig struct {
	Prepare  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Refresh  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Detach   func(context.Context, ManagedAttachment) error
	Rollback func(context.Context, ManagedAttachment) error
}

// NewCodexAttachmentAdapter preserves Codex's distinct App Server/thread and
// terminal-owner evidence while using the shared durable lifecycle.
func NewCodexAttachmentAdapter(config CodexAdapterConfig) AttachmentAdapter {
	return AttachmentAdapter{
		Prepare: config.Prepare,
		Adopt: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) (NativeEvidence, error) {
			if !sameCodexAttachmentEvidence(attachment.ExpectedEvidence, observed) {
				return NativeEvidence{}, errors.New("codex native attachment evidence changed")
			}
			return cloneEvidence(observed), nil
		},
		Refresh: config.Refresh,
		Authorize: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) error {
			return authorizeCodexActor(attachment, observed)
		},
		Detach: config.Detach, Rollback: config.Rollback,
	}
}

func sameCodexAttachmentEvidence(expected, observed NativeEvidence) bool {
	return expected.ThreadID != "" && expected.ThreadID == observed.ThreadID &&
		expected.SocketPath != "" && expected.SocketPath == observed.SocketPath &&
		exactIdentityEqual(expected.Process, observed.Process) &&
		len(expected.Ancestry) > 0 && len(observed.Ancestry) > 0 &&
		exactIdentityEqual(expected.Ancestry[0], observed.Ancestry[0])
}

func authorizeCodexActor(attachment ManagedAttachment, observed NativeEvidence) error {
	if attachment.Product != "codex" || strings.TrimSpace(observed.ThreadID) == "" ||
		observed.ThreadID != attachment.NativeSessionID || observed.ThreadID != attachment.ID ||
		observed.SocketPath == "" || observed.SocketPath != attachment.Evidence.SocketPath ||
		!identityCurrentlyMatches(observed.Process) {
		return errors.New("codex actor does not match the managed thread")
	}
	owner := attachment.Evidence.Process
	appServer := firstIdentity(attachment.Evidence.Ancestry)
	if !identityCurrentlyMatches(owner) || !identityCurrentlyMatches(appServer) {
		return errors.New("codex attachment owner is no longer live")
	}
	if ancestryContains(observed.Ancestry, owner) || ancestryContains(observed.Ancestry, appServer) {
		return nil
	}
	return errors.New("codex actor ancestry does not match its TUI or App Server")
}

func firstIdentity(values []procinfo.Identity) procinfo.Identity {
	if len(values) == 0 {
		return procinfo.Identity{}
	}
	return values[0]
}

func ancestryContains(values []procinfo.Identity, expected procinfo.Identity) bool {
	for _, value := range values {
		if exactIdentityEqual(value, expected) {
			return true
		}
	}
	return false
}

func exactIdentityEqual(left, right procinfo.Identity) bool {
	return left.PID > 1 && left.PID == right.PID && left.Start != "" && left.Start == right.Start &&
		(left.StrongStart == "" || right.StrongStart == "" || left.StrongStart == right.StrongStart)
}

func identityCurrentlyMatches(identity procinfo.Identity) bool {
	if identity.PID <= 1 || identity.Start == "" {
		return false
	}
	return procinfo.ObserveIdentity(identity).Status == procinfo.IdentityMatches
}
