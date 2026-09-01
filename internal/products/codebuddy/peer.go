package codebuddy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type peerMechanics struct{ config Config }

type verifiedPeer struct {
	claim      WorkerClaim
	process    procinfo.Identity
	executable string
}

type PeerDriver struct{ mechanics *peerMechanics }

type MessageDriver struct {
	mechanics *peerMechanics
	now       func() time.Time
}

func NewPeerDriver(config Config, deps productruntime.HostDeps) (*PeerDriver, *MessageDriver, error) {
	normalized, err := normalizeConfig(config, deps)
	if err != nil {
		return nil, nil, err
	}
	mechanics := &peerMechanics{config: normalized}
	return &PeerDriver{mechanics: mechanics}, &MessageDriver{mechanics: mechanics, now: normalized.Now}, nil
}

func (driver *PeerDriver) AttachmentAdapter(productruntime.HostDeps) (daemon.AttachmentAdapter, error) {
	if driver == nil || driver.mechanics == nil {
		return daemon.AttachmentAdapter{}, ErrInvalidConfiguration
	}
	return daemon.AttachmentAdapter{
		Prepare: func(_ context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if err := validateAttachment(attachment, false); err != nil {
				return daemon.NativeEvidence{}, err
			}
			// CodeBuddy has no component bootstrap and no peer credential. The
			// wrapper child identity arrives as observed launch evidence at Adopt.
			return daemon.NativeEvidence{}, nil
		},
		Adopt: func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
			if err := validateAttachment(attachment, true); err != nil {
				return daemon.NativeEvidence{}, err
			}
			if !validIdentity(observed.Process) {
				return daemon.NativeEvidence{}, fmt.Errorf("%w: wrapper child identity is absent", productruntime.ErrAmbiguousSession)
			}
			peer, err := driver.mechanics.verify(ctx, attachment, observed.Process, false)
			if err != nil {
				return daemon.NativeEvidence{}, err
			}
			return nativeEvidence(peer, observed.Process), nil
		},
		Refresh: func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if err := validateAttachment(attachment, true); err != nil || !validIdentity(attachment.Evidence.Process) {
				return daemon.NativeEvidence{}, productruntime.ErrStale
			}
			peer, err := driver.mechanics.verify(ctx, attachment, attachment.Evidence.Process, true)
			if err != nil {
				return daemon.NativeEvidence{}, err
			}
			return nativeEvidence(peer, attachment.Evidence.Process), nil
		},
		Authorize: func(ctx context.Context, attachment daemon.ManagedAttachment, evidence daemon.NativeEvidence) error {
			if err := validateAttachment(attachment, true); err != nil || !validIdentity(evidence.Process) || !validIdentity(attachment.Evidence.Process) {
				return productruntime.ErrUnauthorized
			}
			observation, err := driver.mechanics.config.Processes.ObserveIdentity(ctx, evidence.Process)
			if err != nil || observation.Status != procinfo.IdentityMatches {
				return productruntime.ErrUnauthorized
			}
			descends, err := driver.mechanics.config.Processes.DescendsFrom(ctx, evidence.Process, attachment.Evidence.Process, driver.mechanics.config.MaxAncestryDepth)
			if err != nil || !descends {
				return productruntime.ErrUnauthorized
			}
			return nil
		},
		Detach:   func(context.Context, daemon.ManagedAttachment) error { return nil },
		Rollback: func(context.Context, daemon.ManagedAttachment) error { return nil },
	}, nil
}

func (driver *PeerDriver) BuildLaunch(_ context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	if driver == nil || driver.mechanics == nil || request.ProductID != ProductID || !validNativeID(request.AttachmentID) ||
		!filepath.IsAbs(request.Cwd) || !request.BootstrapSecret.Empty() || request.BootstrapCapabilityID != "" {
		return productruntime.NativeCommand{}, ErrInvalidConfiguration
	}
	if hasForbiddenLaunchArgument(request.Args) {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: caller supplied an identity or MCP routing flag", productruntime.ErrUnsupportedPolicy)
	}
	environment, err := mergePeerEnvironment(request.Env, request.AttachmentID)
	if err != nil {
		return productruntime.NativeCommand{}, err
	}
	arguments := append([]string(nil), request.Args...)
	arguments = append(arguments,
		"--session-id", request.AttachmentID,
		"--strict-mcp-config",
		"--mcp-config", driver.mechanics.config.MCPConfigPath,
	)
	return productruntime.NativeCommand{
		Path: driver.mechanics.config.Executable, Args: arguments, Env: environment,
		SensitiveEnv: nil, Cwd: filepath.Clean(request.Cwd),
	}, nil
}

func (driver *PeerDriver) Rename(ctx context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	if driver == nil || driver.mechanics == nil || errInvalidAttachment(attachment) || strings.TrimSpace(name) == "" {
		return productruntime.NativeName{}, productruntime.ErrStale
	}
	peer, err := driver.mechanics.verify(ctx, attachment, attachment.Evidence.Process, true)
	if err != nil {
		return productruntime.NativeName{}, err
	}
	client, err := NewPeerClient(peer.claim.Endpoint, driver.mechanics.config.Limits)
	if err != nil {
		return productruntime.NativeName{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	defer client.CloseIdleConnections()
	if err := client.RenameSession(ctx, attachment.NativeSessionID, name); err != nil {
		return productruntime.NativeName{}, err
	}
	return productruntime.NativeName{Applied: strings.TrimSpace(name), NativeConfirmed: true}, nil
}

func (driver *MessageDriver) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	if driver == nil || driver.mechanics == nil || errInvalidAttachment(attachment) || strings.TrimSpace(request.DeliveryID) == "" ||
		!validText(string(request.Body)) || !validDeliveryMode(request.Mode) {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	peer, err := driver.mechanics.verify(ctx, attachment, attachment.Evidence.Process, true)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	client, err := NewPeerClient(peer.claim.Endpoint, driver.mechanics.config.Limits)
	if err != nil {
		return productruntime.NativeAcceptance{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	defer client.CloseIdleConnections()
	if err := client.ReplySession(ctx, attachment.NativeSessionID, string(request.Body)); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	return productruntime.NativeAcceptance{NativeSessionID: attachment.NativeSessionID, AcceptedAt: driver.now().UTC()}, nil
}

func (mechanics *peerMechanics) verify(ctx context.Context, attachment daemon.ManagedAttachment, wrapper procinfo.Identity, requireSameWorker bool) (verifiedPeer, error) {
	if ctx == nil || mechanics == nil || mechanics.config.Registry == nil || mechanics.config.Processes == nil || mechanics.config.SocketOwner == nil {
		return verifiedPeer{}, productruntime.ErrUnavailable
	}
	wrapperObservation, err := mechanics.config.Processes.ObserveIdentity(ctx, wrapper)
	if err != nil || wrapperObservation.Status != procinfo.IdentityMatches {
		return verifiedPeer{}, productruntime.ErrStale
	}
	claim, err := mechanics.config.Registry.FindInteractive(ctx, attachment.NativeSessionID)
	if err != nil {
		return verifiedPeer{}, mapRegistryError(err)
	}
	if filepath.Clean(claim.Cwd) != filepath.Clean(attachment.Cwd) {
		return verifiedPeer{}, fmt.Errorf("%w: worker cwd does not match attachment", productruntime.ErrAmbiguousSession)
	}
	worker, err := mechanics.config.Processes.CaptureIdentity(ctx, claim.PID)
	if err != nil {
		return verifiedPeer{}, errors.Join(productruntime.ErrStale, err)
	}
	if requireSameWorker && worker != wrapper {
		return verifiedPeer{}, fmt.Errorf("%w: worker process identity changed", productruntime.ErrStale)
	}
	if worker != wrapper {
		descends, err := mechanics.config.Processes.DescendsFrom(ctx, worker, wrapper, mechanics.config.MaxAncestryDepth)
		if err != nil || !descends {
			return verifiedPeer{}, fmt.Errorf("%w: worker is outside wrapper ancestry", productruntime.ErrAmbiguousSession)
		}
	}
	executable, err := mechanics.config.Processes.Executable(ctx, worker)
	if err != nil || strings.TrimSpace(executable) == "" {
		return verifiedPeer{}, errors.Join(productruntime.ErrStale, ErrEntrypoint, err)
	}
	argv, err := mechanics.config.Processes.CommandLine(ctx, worker)
	if err != nil || !mechanics.config.Entrypoint(executable, argv) {
		return verifiedPeer{}, errors.Join(productruntime.ErrStale, ErrEntrypoint, err)
	}
	socket, err := mechanics.config.SocketOwner.VerifyOwner(ctx, claim.Endpoint, worker.PID)
	if err != nil || socket.PID != worker.PID || socket.Endpoint != claim.Endpoint || strings.TrimSpace(socket.Identity) == "" {
		return verifiedPeer{}, errors.Join(productruntime.ErrStale, ErrSocketOwner, err)
	}
	client, err := NewPeerClient(claim.Endpoint, mechanics.config.Limits)
	if err != nil {
		return verifiedPeer{}, errors.Join(productruntime.ErrProtocol, err)
	}
	live, liveErr := client.LiveSession(ctx)
	client.CloseIdleConnections()
	if liveErr != nil || live.SessionID != attachment.NativeSessionID {
		return verifiedPeer{}, fmt.Errorf("%w: endpoint is not the selected native session", productruntime.ErrAmbiguousSession)
	}
	if err := mechanics.config.Registry.VerifyUnchanged(ctx, claim); err != nil {
		return verifiedPeer{}, mapRegistryError(err)
	}
	finalObservation, err := mechanics.config.Processes.ObserveIdentity(ctx, worker)
	if err != nil || finalObservation.Status != procinfo.IdentityMatches {
		return verifiedPeer{}, productruntime.ErrStale
	}
	if wrapper != worker {
		finalWrapper, wrapperErr := mechanics.config.Processes.ObserveIdentity(ctx, wrapper)
		if wrapperErr != nil || finalWrapper.Status != procinfo.IdentityMatches {
			return verifiedPeer{}, productruntime.ErrStale
		}
	}
	return verifiedPeer{claim: claim, process: worker, executable: executable}, nil
}

func nativeEvidence(peer verifiedPeer, wrapper procinfo.Identity) daemon.NativeEvidence {
	evidence := daemon.NativeEvidence{
		Process: peer.process, Executable: peer.executable,
		RegistryPath: peer.claim.Registry.Path, RegistryDevice: peer.claim.Registry.Device,
		RegistryInode: peer.claim.Registry.Inode, RegistryPrefix: digestPrefix(peer.claim.Registry.Digest),
		RegistryBytes: peer.claim.Registry.Bytes,
	}
	if wrapper != peer.process {
		evidence.Ancestry = []procinfo.Identity{wrapper}
	}
	return evidence
}

func validateAttachment(attachment daemon.ManagedAttachment, requireSession bool) error {
	if attachment.Product != ProductID || strings.TrimSpace(attachment.ID) == "" || !filepath.IsAbs(attachment.Cwd) {
		return productruntime.ErrAmbiguousSession
	}
	if requireSession && !validNativeID(attachment.NativeSessionID) {
		return productruntime.ErrAmbiguousSession
	}
	return nil
}

func errInvalidAttachment(attachment daemon.ManagedAttachment) bool {
	return validateAttachment(attachment, true) != nil || !validIdentity(attachment.Evidence.Process)
}

func validIdentity(identity procinfo.Identity) bool {
	return identity.PID > 1 && identity.Start != "" && identity.StrongStart != ""
}

func validDeliveryMode(mode productruntime.DeliveryMode) bool {
	return mode == productruntime.DeliveryIdleWake || mode == productruntime.DeliveryBusySteer || mode == productruntime.DeliveryBusyFollow
}

func mapRegistryError(err error) error {
	switch {
	case errors.Is(err, ErrWorkerNotFound):
		return errors.Join(productruntime.ErrStale, err)
	case errors.Is(err, ErrWorkerAmbiguous):
		return errors.Join(productruntime.ErrAmbiguousSession, err)
	case errors.Is(err, productruntime.ErrStale):
		return errors.Join(productruntime.ErrStale, err)
	default:
		return errors.Join(productruntime.ErrProtocol, err)
	}
}

func hasForbiddenLaunchArgument(arguments []string) bool {
	for index, argument := range arguments {
		if argument == "--" {
			return true
		}
		name := strings.SplitN(argument, "=", 2)[0]
		switch name {
		case "--session-id", "--resume", "--mcp-config", "--strict-mcp-config", "--acp", "--acp-transport", "--serve", "--auth", "--port":
			return true
		}
		if (name == "--session-id" || name == "--mcp-config") && index+1 < len(arguments) {
			return true
		}
	}
	return false
}

func mergePeerEnvironment(environment []productruntime.EnvVar, attachmentID string) ([]productruntime.EnvVar, error) {
	result := make([]productruntime.EnvVar, 0, len(environment)+2)
	seen := map[string]bool{}
	for _, variable := range environment {
		name := strings.TrimSpace(variable.Name)
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(variable.Value, 0) || seen[name] {
			return nil, ErrInvalidConfiguration
		}
		if name == SessionIDEnv || name == ProductEnv || name == GatewayPasswordEnv || name == GatewayAuthEnv {
			return nil, fmt.Errorf("%w: caller supplied managed environment %s", productruntime.ErrUnsupportedPolicy, name)
		}
		seen[name] = true
		result = append(result, variable)
	}
	result = append(result,
		productruntime.EnvVar{Name: SessionIDEnv, Value: attachmentID},
		productruntime.EnvVar{Name: ProductEnv, Value: ProductID},
	)
	return result, nil
}

var _ productruntime.PeerDriver = (*PeerDriver)(nil)
var _ productruntime.MessageDriver = (*MessageDriver)(nil)
