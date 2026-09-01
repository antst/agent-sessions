package dsh

import (
	"context"
	"errors"
	"fmt"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type ParentAttester struct {
	gateway   *CordisGateway
	processes productruntime.ProcessInspector
}

func NewParentAttester(gateway *CordisGateway, processes ...productruntime.ProcessInspector) (*ParentAttester, error) {
	if gateway == nil {
		return nil, errors.New("DSH parent attester requires Cordis gateway")
	}
	if len(processes) > 1 {
		return nil, errors.New("DSH parent attester accepts at most one process inspector")
	}
	attester := &ParentAttester{gateway: gateway}
	if len(processes) == 1 {
		attester.processes = processes[0]
	}
	return attester, nil
}

func (attester *ParentAttester) Attest(ctx context.Context, attempt productruntime.ConnectorAttempt) (productruntime.ParentBinding, error) {
	if attempt.ProductID != ProductID || attempt.ComponentBindingID == "" || attempt.ClaimedNativeSessionID == "" {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: DSH parent attempt is incomplete", productruntime.ErrUnauthorized)
	}
	session, ok := attester.gateway.SessionByBinding(attempt.ComponentBindingID)
	if !ok || session.Binding.ProductID != ProductID || session.NativeID != attempt.ClaimedNativeSessionID {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: DSH_SESSION_ID or exact component process witness did not match", productruntime.ErrUnauthorized)
	}
	if attempt.ProcessIdentity != session.Binding.ProcessIdentity {
		if attester.processes == nil {
			return productruntime.ParentBinding{}, fmt.Errorf("%w: DSH MCP fallback lacks process ancestry inspector", productruntime.ErrUnauthorized)
		}
		observation, err := attester.processes.ObserveIdentity(ctx, attempt.ProcessIdentity)
		if err != nil || observation.Status != procinfo.IdentityMatches {
			return productruntime.ParentBinding{}, fmt.Errorf("%w: DSH connector process is stale", productruntime.ErrUnauthorized)
		}
		descendant, err := attester.processes.DescendsFrom(ctx, attempt.ProcessIdentity, session.Binding.ProcessIdentity, 32)
		if err != nil || !descendant {
			return productruntime.ParentBinding{}, fmt.Errorf("%w: DSH connector is outside the exact managed ancestry", productruntime.ErrUnauthorized)
		}
	}
	return productruntime.ParentBinding{
		AttachmentID: session.Binding.AttachmentID, NativeSessionID: session.NativeID, Verified: true,
	}, nil
}

var _ productruntime.ParentAttester = (*ParentAttester)(nil)
