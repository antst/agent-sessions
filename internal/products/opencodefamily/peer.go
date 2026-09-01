package opencodefamily

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const ComponentProtocol = component.ContractRevision

// ComponentGateway is the process-local observer/router supplied by composition.
// It awaits delivery.accept or native rename confirmation; queueing a frame
// locally is not success.
type ComponentGateway interface {
	Deliver(context.Context, string, string, productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error)
	Rename(context.Context, string, string, string) (productruntime.NativeName, error)
}

type PeerConfig struct {
	ProductID          string
	Executable         string
	IntegrationVersion string
	Deps               productruntime.HostDeps
	Gateway            ComponentGateway
	BuildLaunch        func(context.Context, productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error)
	Prepare            func(context.Context, daemon.ManagedAttachment) (daemon.NativeEvidence, error)
	Detach             func(context.Context, daemon.ManagedAttachment) error
}

type PeerDriver struct {
	config PeerConfig
}

func NewPeerDriver(config PeerConfig) (*PeerDriver, error) {
	if config.ProductID == "" || config.Executable == "" || config.IntegrationVersion == "" ||
		config.Deps.Generation == 0 || config.Deps.Components == nil || config.Deps.Processes == nil ||
		config.Gateway == nil || config.BuildLaunch == nil {
		return nil, productruntime.ErrProtocol
	}
	return &PeerDriver{config: config}, nil
}

func (driver *PeerDriver) AttachmentAdapter(productruntime.HostDeps) (daemon.AttachmentAdapter, error) {
	return daemon.AttachmentAdapter{
		Prepare: func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if attachment.Product != driver.config.ProductID || attachment.ComponentProtocol != "" && attachment.ComponentProtocol != ComponentProtocol {
				return daemon.NativeEvidence{}, productruntime.ErrProtocol
			}
			if driver.config.Prepare != nil {
				return driver.config.Prepare(ctx, attachment)
			}
			return daemon.NativeEvidence{ArtifactType: "component", ArtifactRevision: driver.config.IntegrationVersion}, nil
		},
		Adopt: func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
			if err := driver.validateEvidence(ctx, attachment, observed); err != nil {
				return daemon.NativeEvidence{}, err
			}
			return observed, nil
		},
		Refresh: func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if err := driver.validateEvidence(ctx, attachment, attachment.Evidence); err != nil {
				return daemon.NativeEvidence{}, err
			}
			view, err := driver.config.Deps.Components.LookupComponent(ctx, attachment.ID, attachment.NativeSessionID)
			if err != nil || view.AttachmentID != attachment.ID || view.NativeSessionID != attachment.NativeSessionID ||
				view.Generation != driver.config.Deps.Generation || view.BindingID == "" {
				return daemon.NativeEvidence{}, productruntime.ErrStale
			}
			return attachment.Evidence, nil
		},
		Authorize: func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) error {
			if !reflect.DeepEqual(observed.Process, attachment.Evidence.Process) || observed.ThreadID != attachment.Evidence.ThreadID {
				return productruntime.ErrUnauthorized
			}
			return driver.validateEvidence(ctx, attachment, observed)
		},
		Detach: func(ctx context.Context, attachment daemon.ManagedAttachment) error {
			if driver.config.Detach != nil {
				return driver.config.Detach(ctx, attachment)
			}
			return nil
		},
		Rollback: func(ctx context.Context, attachment daemon.ManagedAttachment) error {
			if driver.config.Detach != nil {
				return driver.config.Detach(ctx, attachment)
			}
			return nil
		},
	}, nil
}

func (driver *PeerDriver) validateEvidence(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) error {
	if observed.Process.PID <= 1 || observed.Process.Start == "" || observed.Process.StrongStart == "" ||
		observed.ArtifactType != "component" || observed.ArtifactRevision != driver.config.IntegrationVersion {
		return productruntime.ErrUnauthorized
	}
	observation, err := driver.config.Deps.Processes.ObserveIdentity(ctx, observed.Process)
	if err != nil || observation.Status != procinfo.IdentityMatches {
		return productruntime.ErrStale
	}
	executable, err := driver.config.Deps.Processes.Executable(ctx, observed.Process)
	if err != nil || filepath.Base(executable) != filepath.Base(driver.config.Executable) {
		return productruntime.ErrUnauthorized
	}
	if attachment.NativeSessionID != "" && observed.ThreadID != "" && attachment.NativeSessionID != observed.ThreadID {
		return productruntime.ErrAmbiguousSession
	}
	return nil
}

func (driver *PeerDriver) BuildLaunch(ctx context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	if request.ProductID != driver.config.ProductID || strings.TrimSpace(request.AttachmentID) == "" || !validDirectory(request.Cwd) {
		return productruntime.NativeCommand{}, productruntime.ErrProtocol
	}
	return driver.config.BuildLaunch(ctx, request)
}

func (driver *PeerDriver) Rename(ctx context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	if attachment.Product != driver.config.ProductID || !validNativeID(attachment.NativeSessionID, "ses_") ||
		strings.TrimSpace(name) == "" || len([]byte(name)) > maxTitleBytes {
		return productruntime.NativeName{}, productruntime.ErrProtocol
	}
	view, err := driver.config.Deps.Components.LookupComponent(ctx, attachment.ID, attachment.NativeSessionID)
	if err != nil || view.AttachmentID != attachment.ID || view.NativeSessionID != attachment.NativeSessionID ||
		view.Generation != driver.config.Deps.Generation || view.BindingID == "" {
		return productruntime.NativeName{}, productruntime.ErrStale
	}
	result, err := driver.config.Gateway.Rename(ctx, view.BindingID, attachment.NativeSessionID, name)
	if err != nil {
		return productruntime.NativeName{}, err
	}
	if !result.NativeConfirmed || result.Applied != name {
		return productruntime.NativeName{}, productruntime.ErrAmbiguousSession
	}
	return result, nil
}

func (driver *PeerDriver) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	if attachment.Product != driver.config.ProductID || attachment.State != "attached" ||
		attachment.DaemonGeneration != driver.config.Deps.Generation || !validNativeID(attachment.NativeSessionID, "ses_") ||
		strings.TrimSpace(request.DeliveryID) == "" || len(request.Body) == 0 || len(request.Body) > maxResultBytes ||
		request.Mode != productruntime.DeliveryIdleWake && request.Mode != productruntime.DeliveryBusySteer && request.Mode != productruntime.DeliveryBusyFollow {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	view, err := driver.config.Deps.Components.LookupComponent(ctx, attachment.ID, attachment.NativeSessionID)
	if err != nil || view.AttachmentID != attachment.ID || view.NativeSessionID != attachment.NativeSessionID ||
		view.Generation != driver.config.Deps.Generation || view.BindingID == "" {
		return productruntime.NativeAcceptance{}, productruntime.ErrStale
	}
	accepted, err := driver.config.Gateway.Deliver(ctx, view.BindingID, attachment.NativeSessionID, request)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if accepted.NativeSessionID != attachment.NativeSessionID || accepted.NativeMessageID == "" || accepted.AcceptedAt.IsZero() || accepted.AcceptedAt.After(time.Now().Add(time.Minute)) {
		return productruntime.NativeAcceptance{}, productruntime.ErrAmbiguousSession
	}
	return accepted, nil
}

// ComponentEnv reports the live component socket and product/session identity.
func ComponentEnv(request productruntime.PeerLaunchRequest) ([]productruntime.EnvVar, []productruntime.SensitiveEnvVar) {
	return []productruntime.EnvVar{
		{Name: "AGENT_SESSIONS_PRODUCT_ID", Value: request.ProductID},
		{Name: "AGENT_SESSIONS_ATTACHMENT_ID", Value: request.AttachmentID},
		{Name: "AGENT_SESSIONS_COMPONENT_VERSION", Value: ComponentProtocol},
	}, nil
}

// ExactParentVerifier resolves a connector against its current component
// session. It is host-owned because product packages do not own live routing.
type ExactParentVerifier interface {
	VerifyComponentParent(context.Context, string, productruntime.ConnectorAttempt) (productruntime.ComponentSessionView, error)
}

type ParentAttester struct {
	productID string
	verify    ExactParentVerifier
}

func NewParentAttester(productID string, verify ExactParentVerifier) (*ParentAttester, error) {
	if productID == "" || verify == nil {
		return nil, productruntime.ErrProtocol
	}
	return &ParentAttester{productID: productID, verify: verify}, nil
}

func (attester *ParentAttester) Attest(ctx context.Context, attempt productruntime.ConnectorAttempt) (productruntime.ParentBinding, error) {
	if attempt.ProductID != attester.productID || attempt.ComponentBindingID == "" ||
		!validNativeID(attempt.ClaimedNativeSessionID, "ses_") || attempt.PeerCredential.PID <= 1 ||
		attempt.ProcessIdentity.PID != attempt.PeerCredential.PID || attempt.ProcessIdentity.Start == "" || attempt.ProcessIdentity.StrongStart == "" {
		return productruntime.ParentBinding{}, productruntime.ErrUnauthorized
	}
	view, err := attester.verify.VerifyComponentParent(ctx, attester.productID, attempt)
	if err != nil {
		return productruntime.ParentBinding{}, errors.Join(productruntime.ErrUnauthorized, err)
	}
	if view.BindingID != attempt.ComponentBindingID || view.NativeSessionID != attempt.ClaimedNativeSessionID ||
		view.AttachmentID == "" || view.Generation == 0 {
		return productruntime.ParentBinding{}, productruntime.ErrUnauthorized
	}
	return productruntime.ParentBinding{AttachmentID: view.AttachmentID, NativeSessionID: view.NativeSessionID, Verified: true}, nil
}

// NativeEvidenceForComponent packages host-observed evidence. Callers must pass
// an independently captured process identity, never values declared by the
// component bootstrap payload or environment.
func NativeEvidenceForComponent(process procinfo.Identity, executable, nativeSessionID, integrationVersion string) daemon.NativeEvidence {
	return daemon.NativeEvidence{
		Process: process, Executable: executable, ThreadID: nativeSessionID,
		ArtifactType: "component", ArtifactRevision: integrationVersion,
	}
}

var (
	_ productruntime.PeerDriver     = (*PeerDriver)(nil)
	_ productruntime.MessageDriver  = (*PeerDriver)(nil)
	_ productruntime.ParentAttester = (*ParentAttester)(nil)
)
