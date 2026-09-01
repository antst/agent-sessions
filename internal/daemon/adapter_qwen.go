package daemon

import (
	"context"
	"errors"
	"strings"
)

// QwenAdapterConfig retains the presence-sensitive profile and dual-output
// artifact callbacks at the product boundary.
type QwenAdapterConfig struct {
	Prepare  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Refresh  func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Detach   func(context.Context, ManagedAttachment) error
	Rollback func(context.Context, ManagedAttachment) error
}

// NewQwenAttachmentAdapter validates the native owner plus exact dual-output
// artifacts before granting addressability.
func NewQwenAttachmentAdapter(config QwenAdapterConfig) AttachmentAdapter {
	return AttachmentAdapter{
		Prepare: config.Prepare,
		Adopt: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) (NativeEvidence, error) {
			expected := attachment.ExpectedEvidence
			if !exactIdentityEqual(expected.Process, observed.Process) || expected.ThreadID == "" ||
				expected.ThreadID != observed.ThreadID || expected.ArtifactPath == "" ||
				expected.ArtifactPath != observed.ArtifactPath || expected.RegistryPath == "" ||
				expected.RegistryPath != observed.RegistryPath || strings.TrimSpace(observed.ArtifactRevision) == "" {
				return NativeEvidence{}, errors.New("qwen native attachment evidence changed")
			}
			return cloneEvidence(observed), nil
		},
		Refresh: config.Refresh,
		Authorize: func(_ context.Context, attachment ManagedAttachment, observed NativeEvidence) error {
			if attachment.Product != "qwen" || observed.ThreadID != attachment.NativeSessionID ||
				!identityCurrentlyMatches(attachment.Evidence.Process) || !identityCurrentlyMatches(observed.Process) {
				return errors.New("qwen actor does not match the managed session")
			}
			if exactIdentityEqual(observed.Process, attachment.Evidence.Process) ||
				ancestryContains(observed.Ancestry, attachment.Evidence.Process) {
				return nil
			}
			return errors.New("qwen actor ancestry does not match its native TUI")
		},
		Detach: config.Detach, Rollback: config.Rollback,
	}
}
