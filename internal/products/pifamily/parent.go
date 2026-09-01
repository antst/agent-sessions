package pifamily

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

// ParentAttester accepts the registered component process or one exact
// foreground connector child. A deeper descendant chain is deliberately
// rejected so a native subagent cannot inherit and impersonate TUI authority.
type ParentAttester struct {
	quirks    Quirks
	runtime   *ComponentRuntime
	bindings  BindingSource
	processes productruntime.ProcessInspector
}

func NewParentAttester(quirks Quirks, runtime *ComponentRuntime, bindings BindingSource, processes productruntime.ProcessInspector) (*ParentAttester, error) {
	if err := quirks.Validate(); err != nil {
		return nil, err
	}
	if runtime == nil || bindings == nil || processes == nil {
		return nil, errors.New("Pi-family parent attester requires component runtime, bindings, and process inspection")
	}
	return &ParentAttester{quirks: quirks, runtime: runtime, bindings: bindings, processes: processes}, nil
}

func (attester *ParentAttester) Attest(ctx context.Context, attempt productruntime.ConnectorAttempt) (productruntime.ParentBinding, error) {
	if ctx == nil || attempt.ProductID != attester.quirks.ProductID ||
		!exactProcess(attempt.ProcessIdentity) ||
		strings.TrimSpace(attempt.ComponentBindingID) == "" ||
		strings.TrimSpace(attempt.ClaimedNativeSessionID) == "" {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: Pi-family connector evidence is incomplete", productruntime.ErrUnauthorized)
	}
	var exactBinding *componentBindingWitness
	for _, binding := range attester.bindings.Bindings() {
		if binding.BindingID != attempt.ComponentBindingID {
			continue
		}
		if exactBinding != nil {
			return productruntime.ParentBinding{}, fmt.Errorf("%w: duplicate component binding witness", productruntime.ErrUnauthorized)
		}
		copy := componentBindingWitness{
			attachmentID: binding.AttachmentID, productID: binding.ProductID,
			process: binding.ProcessIdentity,
		}
		exactBinding = &copy
	}
	if exactBinding == nil || exactBinding.productID != attester.quirks.ProductID || !exactProcess(exactBinding.process) {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: connector does not match the authenticated component binding", productruntime.ErrUnauthorized)
	}
	observation, err := attester.processes.ObserveIdentity(ctx, attempt.ProcessIdentity)
	if err != nil || observation.Status != procinfo.IdentityMatches {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: component process identity is not live", productruntime.ErrUnauthorized)
	}
	parentObservation, err := attester.processes.ObserveIdentity(ctx, exactBinding.process)
	if err != nil || parentObservation.Status != procinfo.IdentityMatches {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: registered component process identity is not live", productruntime.ErrUnauthorized)
	}
	direct, err := attester.processes.DescendsFrom(ctx, attempt.ProcessIdentity, exactBinding.process, 1)
	if err != nil || !direct {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: connector is not the registered component or its direct child", productruntime.ErrUnauthorized)
	}
	identity, attachmentID, ok := attester.runtime.identityForBinding(attempt.ComponentBindingID)
	if !ok || attachmentID != exactBinding.attachmentID || identity.nativeSessionID != attempt.ClaimedNativeSessionID {
		return productruntime.ParentBinding{}, fmt.Errorf("%w: claimed native session does not match the live component session", productruntime.ErrUnauthorized)
	}
	return productruntime.ParentBinding{AttachmentID: attachmentID, NativeSessionID: identity.nativeSessionID, Verified: true}, nil
}

type componentBindingWitness struct {
	attachmentID string
	productID    string
	process      procinfo.Identity
}

var _ productruntime.ParentAttester = (*ParentAttester)(nil)
