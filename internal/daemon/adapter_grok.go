package daemon

import (
	"context"
	"errors"
	"strings"
)

// GrokAdapterConfig retains Grok's private-leader and native-owner callbacks
// at the product edge of the shared attachment lifecycle.
type GrokAdapterConfig struct {
	Prepare  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Refresh  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Detach   func(context.Context, ManagedAttachment) error
	Rollback func(context.Context, ManagedAttachment) error
}

// NewGrokAttachmentAdapter validates the exact TUI/leader pairing without
// inventing daemon-child ancestry for Grok-owned MCP processes.
//
//nolint:gocyclo // Grok's owner/leader evidence requires independent fail-closed authorization gates.
func NewGrokAttachmentAdapter(config GrokAdapterConfig) AttachmentAdapter {
	return AttachmentAdapter{
		Prepare: config.Prepare,
		Adopt: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) (NativeEvidence, error) {
			expected := attachment.ExpectedEvidence
			if !exactIdentityEqual(expected.Process, observed.Process) || len(expected.Ancestry) != 1 ||
				len(observed.Ancestry) != 1 || !exactIdentityEqual(expected.Ancestry[0], observed.Ancestry[0]) ||
				expected.SocketPath == "" || expected.SocketPath != observed.SocketPath ||
				strings.TrimSpace(observed.ThreadID) == "" {
				return NativeEvidence{}, errors.New("grok native attachment evidence changed")
			}
			return cloneEvidence(observed), nil
		},
		Refresh: config.Refresh,
		Authorize: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) error {
			if attachment.Product != "grok" || observed.ThreadID != attachment.NativeSessionID ||
				!identityCurrentlyMatches(attachment.Evidence.Process) || len(attachment.Evidence.Ancestry) != 1 ||
				!identityCurrentlyMatches(attachment.Evidence.Ancestry[0]) || !identityCurrentlyMatches(observed.Process) {
				return errors.New("grok actor does not match the managed session")
			}
			// Grok releases have hosted plugin MCPs under either the attached
			// TUI or its private leader. Both are exact live daemon-corroborated
			// identities for this one launch, so either ancestry is authoritative.
			if ancestryContains(observed.Ancestry, attachment.Evidence.Process) ||
				ancestryContains(observed.Ancestry, attachment.Evidence.Ancestry[0]) {
				return nil
			}
			return errors.New("grok actor ancestry does not match its TUI or private leader")
		},
		Detach: config.Detach, Rollback: config.Rollback,
	}
}
