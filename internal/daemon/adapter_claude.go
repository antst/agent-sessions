package daemon

import (
	"context"
	"errors"
	"strings"
)

// ClaudeAdapterConfig supplies the exact native operations retained at the
// product boundary. Claude's PID row, messaging socket, and process ancestry
// remain authoritative; the shared engine owns only lifecycle state.
type ClaudeAdapterConfig struct {
	Prepare  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Refresh  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Detach   func(context.Context, ManagedAttachment) error
	Rollback func(context.Context, ManagedAttachment) error
}

// NewClaudeAttachmentAdapter preserves Claude's distinct PID-row/socket
// evidence while using the shared durable lifecycle.
func NewClaudeAttachmentAdapter(config ClaudeAdapterConfig) AttachmentAdapter {
	return AttachmentAdapter{
		Prepare: config.Prepare,
		Adopt: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) (NativeEvidence, error) {
			expected := attachment.ExpectedEvidence
			if !exactIdentityEqual(expected.Process, observed.Process) ||
				expected.RegistryPath == "" || expected.RegistryPath != observed.RegistryPath ||
				expected.SocketPath == "" || expected.SocketPath != observed.SocketPath ||
				strings.TrimSpace(observed.ThreadID) == "" || observed.ThreadID != attachment.NativeSessionID {
				return NativeEvidence{}, errors.New("claude native attachment evidence changed")
			}
			return cloneEvidence(observed), nil
		},
		Refresh: config.Refresh,
		Authorize: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) error {
			return authorizeClaudeActor(attachment, observed)
		},
		Detach: config.Detach, Rollback: config.Rollback,
	}
}

func authorizeClaudeActor(attachment ManagedAttachment, observed NativeEvidence) error {
	if attachment.Product != "claude" || strings.TrimSpace(attachment.NativeSessionID) == "" ||
		observed.ThreadID != attachment.NativeSessionID || observed.SocketPath != attachment.Evidence.SocketPath ||
		!identityCurrentlyMatches(attachment.Evidence.Process) || !identityCurrentlyMatches(observed.Process) {
		return errors.New("claude actor does not match the managed session")
	}
	owner := attachment.Evidence.Process
	if exactIdentityEqual(observed.Process, owner) || ancestryContains(observed.Ancestry, owner) {
		return nil
	}
	return errors.New("claude actor ancestry does not match its native TUI")
}
