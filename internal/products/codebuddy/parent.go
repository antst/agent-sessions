package codebuddy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type ParentAttester struct {
	processes   ProcessProbe
	attachments ActiveAttachmentSource
	maxDepth    int
}

func NewParentAttester(processes ProcessProbe, attachments ActiveAttachmentSource, maxDepth int) (*ParentAttester, error) {
	if processes == nil || attachments == nil || maxDepth < 1 || maxDepth > 256 {
		return nil, ErrInvalidConfiguration
	}
	return &ParentAttester{processes: processes, attachments: attachments, maxDepth: maxDepth}, nil
}

func (attester *ParentAttester) Attest(ctx context.Context, attempt productruntime.ConnectorAttempt) (productruntime.ParentBinding, error) {
	if attester == nil || attester.processes == nil || attester.attachments == nil || ctx == nil ||
		attempt.ProductID != ProductID || !validIdentity(attempt.ProcessIdentity) || strings.TrimSpace(attempt.ComponentBindingID) != "" {
		return productruntime.ParentBinding{}, productruntime.ErrUnauthorized
	}
	observation, err := attester.processes.ObserveIdentity(ctx, attempt.ProcessIdentity)
	if err != nil || observation.Status != procinfo.IdentityMatches {
		return productruntime.ParentBinding{}, productruntime.ErrUnauthorized
	}
	attachments, err := attester.attachments.ActiveCodeBuddyAttachments(ctx)
	if err != nil {
		return productruntime.ParentBinding{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	var matches []daemon.ManagedAttachment
	for _, attachment := range attachments {
		if attachment.Product != ProductID || attachment.State != "attached" || !validNativeID(attachment.NativeSessionID) || !validIdentity(attachment.Evidence.Process) {
			continue
		}
		nativeObservation, observeErr := attester.processes.ObserveIdentity(ctx, attachment.Evidence.Process)
		if observeErr != nil || nativeObservation.Status != procinfo.IdentityMatches {
			continue
		}
		descends, ancestryErr := attester.processes.DescendsFrom(ctx, attempt.ProcessIdentity, attachment.Evidence.Process, attester.maxDepth)
		if ancestryErr == nil && descends {
			matches = append(matches, attachment)
		}
	}
	if len(matches) != 1 {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: connector ancestry matched %d CodeBuddy sessions", productruntime.ErrUnauthorized, len(matches))
	}
	match := matches[0]
	if claim := strings.TrimSpace(attempt.ClaimedNativeSessionID); claim != "" && claim != match.NativeSessionID {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: model-supplied native session claim does not corroborate ancestry", productruntime.ErrUnauthorized)
	}
	return productruntime.ParentBinding{AttachmentID: match.ID, NativeSessionID: match.NativeSessionID, Verified: true}, nil
}

var _ productruntime.ParentAttester = (*ParentAttester)(nil)
