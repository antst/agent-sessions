package pifamily

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	EnvComponentSocket  = "AGENT_SESSIONS_COMPONENT_SOCKET"
	EnvProductID        = "AGENT_SESSIONS_PRODUCT_ID"
	EnvAttachmentID     = "AGENT_SESSIONS_ATTACHMENT_ID"
	EnvComponentVersion = "AGENT_SESSIONS_COMPONENT_VERSION"
)

type PeerConfig struct {
	Quirks          Quirks
	Executable      string
	ExtensionPath   string
	ComponentSocket string
	Runtime         *ComponentRuntime
}

type PeerDriver struct {
	config PeerConfig
}

func NewPeerDriver(config PeerConfig) (*PeerDriver, error) {
	if err := config.Quirks.Validate(); err != nil {
		return nil, err
	}
	if config.Executable == "" {
		config.Executable = config.Quirks.Executable
	}
	if config.Runtime == nil || !filepath.IsAbs(config.ExtensionPath) || filepath.Clean(config.ExtensionPath) != config.ExtensionPath ||
		!filepath.IsAbs(config.ComponentSocket) || filepath.Clean(config.ComponentSocket) != config.ComponentSocket {
		return nil, errors.New("Pi-family peer requires component runtime, extension path, and a clean absolute component socket")
	}
	return &PeerDriver{config: config}, nil
}

func (driver *PeerDriver) AttachmentAdapter(deps productruntime.HostDeps) (daemon.AttachmentAdapter, error) {
	if deps.Components == nil || deps.Processes == nil {
		return daemon.AttachmentAdapter{}, errors.New("Pi-family attachment adapter requires component and process lookups")
	}
	prepare := func(_ context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
		if attachment.Product != driver.config.Quirks.ProductID || attachment.ID == "" {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: wrong Pi-family attachment", productruntime.ErrNativeRejected)
		}
		return daemon.NativeEvidence{ArtifactPath: driver.config.ExtensionPath, ArtifactRevision: IntegrationVersion}, nil
	}
	adopt := func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
		if !exactProcess(observed.Process) {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: component process identity is incomplete", productruntime.ErrUnauthorized)
		}
		view, err := deps.Components.LookupComponent(ctx, attachment.ID, attachment.NativeSessionID)
		if err != nil {
			return daemon.NativeEvidence{}, err
		}
		if view.AttachmentID != attachment.ID || view.NativeSessionID == "" ||
			(attachment.NativeSessionID != "" && attachment.NativeSessionID != view.NativeSessionID) {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: component selected a foreign native session", productruntime.ErrAmbiguousSession)
		}
		executable, err := deps.Processes.Executable(ctx, observed.Process)
		if err != nil || executable == "" {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: component executable could not be corroborated", productruntime.ErrUnauthorized)
		}
		observed.Executable = executable
		observed.ThreadID = view.NativeSessionID
		observed.ArtifactPath = driver.config.ExtensionPath
		observed.ArtifactRevision = IntegrationVersion
		return observed, nil
	}
	refresh := func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
		observation, err := deps.Processes.ObserveIdentity(ctx, attachment.Evidence.Process)
		if err != nil || observation.Status != procinfo.IdentityMatches {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: component process is stale", productruntime.ErrStale)
		}
		view, err := deps.Components.LookupComponent(ctx, attachment.ID, attachment.NativeSessionID)
		if err != nil || view.NativeSessionID != attachment.NativeSessionID {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: component session is stale", productruntime.ErrStale)
		}
		return attachment.Evidence, nil
	}
	authorize := func(_ context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) error {
		if attachment.Product != driver.config.Quirks.ProductID || attachment.NativeSessionID == "" ||
			attachment.Evidence.Process != observed.Process || attachment.Evidence.ThreadID != observed.ThreadID ||
			observed.ThreadID != attachment.NativeSessionID || observed.ArtifactPath != driver.config.ExtensionPath ||
			observed.ArtifactRevision != IntegrationVersion {
			return fmt.Errorf("%w: Pi-family native evidence changed", productruntime.ErrUnauthorized)
		}
		return nil
	}
	return daemon.AttachmentAdapter{
		Prepare: prepare, Adopt: adopt, Refresh: refresh, Authorize: authorize,
		Detach:   func(context.Context, daemon.ManagedAttachment) error { return nil },
		Rollback: func(context.Context, daemon.ManagedAttachment) error { return nil },
	}, nil
}

func (driver *PeerDriver) BuildLaunch(_ context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	if request.ProductID != driver.config.Quirks.ProductID || request.AttachmentID == "" || strings.TrimSpace(request.Cwd) == "" {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: managed Pi-family launch is incomplete", productruntime.ErrNativeRejected)
	}
	for _, argument := range request.Args {
		if reservedArgument(argument) || strings.IndexByte(argument, 0) >= 0 {
			return productruntime.NativeCommand{}, fmt.Errorf("%w: peer argument %q overrides managed component setup", productruntime.ErrUnsupportedPolicy, argument)
		}
	}
	environment := make([]productruntime.EnvVar, 0, len(request.Env)+4)
	for _, variable := range request.Env {
		switch variable.Name {
		case EnvComponentSocket, EnvProductID, EnvAttachmentID, EnvComponentVersion:
			return productruntime.NativeCommand{}, fmt.Errorf("%w: caller attempted to override managed component environment", productruntime.ErrUnauthorized)
		default:
			environment = append(environment, variable)
		}
	}
	environment = append(environment,
		productruntime.EnvVar{Name: EnvComponentSocket, Value: driver.config.ComponentSocket},
		productruntime.EnvVar{Name: EnvProductID, Value: request.ProductID},
		productruntime.EnvVar{Name: EnvAttachmentID, Value: request.AttachmentID},
		productruntime.EnvVar{Name: EnvComponentVersion, Value: component.ContractRevision},
	)
	arguments := driver.config.Quirks.extensionArguments(driver.config.ExtensionPath)
	arguments = append(arguments, request.Args...)
	return productruntime.NativeCommand{
		Path: driver.config.Executable, Args: arguments, Env: environment, Cwd: request.Cwd,
	}, nil
}

func (driver *PeerDriver) Rename(ctx context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	return driver.config.Runtime.Rename(ctx, attachment, name)
}

type MessageDriver struct{ runtime *ComponentRuntime }

func NewMessageDriver(runtime *ComponentRuntime) (*MessageDriver, error) {
	if runtime == nil {
		return nil, errors.New("Pi-family message driver requires component runtime")
	}
	return &MessageDriver{runtime: runtime}, nil
}

func (driver *MessageDriver) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return driver.runtime.Deliver(ctx, attachment, request)
}

func exactProcess(identity procinfo.Identity) bool {
	return identity.PID > 1 && identity.Start != "" && identity.StrongStart != ""
}
