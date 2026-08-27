package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const defaultMaximumStateRecordBytes = 2 * 1024 * 1024

// BuildVersion is populated by release builds and remains explicit in
// development binaries.
var BuildVersion = "development"

// RunForeground runs the one service-manager-owned host authority in the
// foreground. It never forks or supervises another daemon image.
func RunForeground(ctx context.Context) error {
	return RunForegroundWithOptions(ctx, ForegroundOptions{})
}

// ForegroundOptions supplies in-process product adapters from the executable
// composition root. The daemon package stays independent of vendor protocol
// implementations while still owning their one runtime generation.
type ForegroundOptions struct {
	AttachmentAdapters map[string]AttachmentAdapter
	DeliveryAdapters   map[string]DeliveryAdapter
}

// RunForegroundWithOptions runs the canonical foreground authority with
// callable product adapters embedded in the same process.
func RunForegroundWithOptions(ctx context.Context, options ForegroundOptions) error {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return err
	}
	configuration, err := loadOrCreateDefaultConfiguration(paths)
	if err != nil {
		return err
	}
	state, err := OpenStateStore(paths.StateRoot, defaultMaximumStateRecordBytes)
	if err != nil {
		return err
	}
	priorOwner, recoveredCrash, err := priorControlOwner(ctx, state)
	if err != nil {
		return err
	}
	identity := procinfo.Read(os.Getpid())
	if identity.Status != procinfo.Known || identity.Start == "" || identity.StrongStart == "" {
		return errors.New("foreground daemon process identity is not exactly observable")
	}
	runtimeIdentity, err := currentExecutableIdentity()
	if err != nil {
		return err
	}
	manager, unit := hostServiceIdentity()
	recoveryHooks, err := ComposeRecoveryHooks(RecoveryStep{
		Stage: RecoveryTransactions,
		Run: func(recoveryContext context.Context, candidate *Runtime) error {
			return candidate.RecoverDurableState(recoveryContext)
		},
	})
	if err != nil {
		return err
	}
	host, err := NewRuntime(RuntimeOptions{
		Paths: paths, Configuration: configuration, State: state,
		RuntimeVersion: BuildVersion, RuntimeIdentity: runtimeIdentity, PID: os.Getpid(),
		ProcStart: identity.Start, StrongStart: identity.StrongStart,
		ServiceManager: manager, ServiceUnit: unit, RecoveryHooks: recoveryHooks,
		AttachmentAdapters: options.AttachmentAdapters, DeliveryAdapters: options.DeliveryAdapters,
	})
	if err != nil {
		return err
	}
	endpoint, err := acquireControlEndpoint(controlEndpointOptions{
		endpoint: paths.ControlEndpoint, priorIdentity: priorOwner,
	})
	if err != nil {
		return fmt.Errorf("acquire sole host control endpoint: %w", err)
	}
	defer func() { _ = endpoint.Close() }()
	if err := host.Start(ctx); err != nil {
		return err
	}
	observability := newHostDiagnosticWriter(os.Stdout, os.Stderr)
	emitForegroundStarted(observability, host, paths, identity, runtimeIdentity, priorOwner, recoveredCrash)
	server := newControlServer(controlServerConfig{
		Generation: host.Generation(), RuntimeVersion: BuildVersion, OwnerUID: os.Getuid(),
		AuthorizeHello: authorizeForegroundHello(host), Dispatch: runtimeControlDispatch(host),
		ObserveRequest: observability.observeControl,
	})
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.serve(ctx, endpoint) }()
	select {
	case <-ctx.Done():
		observability.emit(diagnostics.OutputNormal, "host.stopping", map[string]any{
			"role": "daemon", "state": "stopping", "generation": host.Generation(),
		})
		stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(host.Stop(stopContext), <-serveResult)
	case err := <-serveResult:
		observability.emit(diagnostics.OutputError, "host.control_failed", map[string]any{
			"role": "daemon", "state": "failed", "generation": host.Generation(),
			"error_code": "control_server_failed", "cause_detail": err,
		})
		stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(err, host.Stop(stopContext))
	}
}

func emitForegroundStarted(
	observability *hostDiagnosticWriter,
	host *Runtime,
	paths ProductionPaths,
	identity procinfo.Info,
	runtimeIdentity string,
	priorOwner *procinfo.Process,
	recoveredCrash bool,
) {
	observability.emit(diagnostics.OutputNormal, "host.ready", map[string]any{
		"role": "daemon", "state": "ready", "generation": host.Generation(),
		"runtime_version": BuildVersion, "runtime_identity": runtimeIdentity,
		"pid": os.Getpid(), "proc_start": identity.Start, "endpoint": paths.ControlEndpoint,
	})
	stages := host.CompletedRecoveryStages()
	stageNames := make([]string, 0, len(stages))
	for _, stage := range stages {
		stageNames = append(stageNames, string(stage))
	}
	observability.emit(diagnostics.OutputDebug, "host.recovery", map[string]any{
		"role": "daemon", "state": "complete", "generation": host.Generation(), "recovery_stage": stageNames,
	})
	observability.emit(diagnostics.OutputMetric, "host.metrics", map[string]any{
		"role": "daemon", "state": "disabled", "operation": "observability.export",
	})
	observability.emit(diagnostics.OutputTrace, "host.traces", map[string]any{
		"role": "daemon", "state": "disabled", "operation": "observability.export",
	})
	if recoveredCrash {
		observability.emit(diagnostics.OutputCrashReport, "host.crash_recovered", map[string]any{
			"role": "daemon", "state": "recovered", "generation": host.Generation(),
			"identity": priorOwner.PID, "error_code": "prior_authority_exited",
		})
	}
}

func priorControlOwner(ctx context.Context, state *StateStore) (*procinfo.Process, bool, error) {
	prior, _, err := state.ReadRuntime(ctx)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load prior host authority for control endpoint: %w", err)
	}
	owner := &procinfo.Process{PID: prior.PID, Info: procinfo.Info{
		Status: procinfo.Known, Start: prior.ProcStart, StrongStart: prior.StrongStart,
	}}
	current := procinfo.Read(prior.PID)
	recoveredCrash := prior.State != HostRuntimeStopping && (current.Status == procinfo.Absent ||
		(current.Status == procinfo.Known && (current.Start != prior.ProcStart || current.StrongStart != prior.StrongStart)))
	return owner, recoveredCrash, nil
}

//nolint:gocyclo // Role, bare-connector, attachment, and exact-evidence gates intentionally fail closed in one boundary.
func authorizeForegroundHello(runtime *Runtime) func(context.Context, controlPeerEvidence, controlHello) (controlPrincipal, *controlError) {
	return func(ctx context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
		if peer.UID != os.Getuid() {
			return controlPrincipal{}, &controlError{Code: "different_user", Message: "control client belongs to another OS user"}
		}
		principal := controlPrincipal{Role: hello.Role, Product: hello.Product, AttachmentID: hello.AttachmentID}
		switch hello.Role {
		case controlRoleAdmin, controlRoleService, controlRoleLauncher:
			return principal, nil
		case controlRoleConnector, controlRoleHook:
			if hello.Role == controlRoleConnector && hello.AttachmentID == "" && hello.SessionID == "" &&
				hello.Capability == "" && len(hello.NativeActor) == 0 {
				return principal, nil
			}
			registry := runtime.attachmentRegistry()
			if registry == nil {
				return controlPrincipal{}, &controlError{Code: "runtime_recovering", Message: "attachment authority is not ready", Retryable: true}
			}
			record, ok := registry.attachmentByID(hello.AttachmentID)
			if !ok || record.Product != hello.Product || hello.Capability == "" {
				return controlPrincipal{}, &controlError{Code: "attachment_not_attested", Message: "connector does not identify one prepared attachment"}
			}
			var err error
			if record.State == AttachmentStatePrepared && hello.SessionID == "" {
				_, err = registry.PreparedConnector(hello.Product, hello.AttachmentID, hello.Capability)
				if err != nil {
					return controlPrincipal{}, controlFailure(err)
				}
				return principal, nil
			}
			if record.State == AttachmentStatePrepared {
				record, err = registry.Adopt(ctx, AttachmentAdoptRequest{
					AttachmentID: record.AttachmentID, Capability: hello.Capability,
					SessionID: hello.SessionID, NativeActor: hello.NativeActor,
				})
			} else {
				record, err = registry.AttestConnector(ctx, ConnectorAttestation{
					Product: hello.Product, AttachmentID: hello.AttachmentID,
					SessionID: hello.SessionID, Capability: hello.Capability, NativeActor: hello.NativeActor,
				})
			}
			if err != nil {
				return controlPrincipal{}, controlFailure(err)
			}
			principal.SessionID = record.SessionID
			principal.Attested = true
			return principal, nil
		default:
			return controlPrincipal{}, &controlError{Code: "role_not_ready", Message: "workflow role is not available in this daemon generation", Retryable: true}
		}
	}
}

func loadOrCreateDefaultConfiguration(paths ProductionPaths) (DaemonConfig, error) {
	configuration, err := LoadDaemonConfig(paths.ConfigurationFile, paths)
	if err == nil {
		return configuration, nil
	}
	if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		return DaemonConfig{}, err
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return DaemonConfig{}, errors.New("cannot establish the host identity")
	}
	digest := sha256.Sum256([]byte(hostname + "\x00" + strconv.Itoa(os.Getuid())))
	now := time.Now().UnixMilli()
	configuration = DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: hex.EncodeToString(digest[:16]), HostName: hostname,
		StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: now,
	}
	if err := writeDefaultConfiguration(paths.ConfigurationFile, configuration); err != nil {
		return DaemonConfig{}, err
	}
	return configuration, nil
}

func writeDefaultConfiguration(path string, configuration DaemonConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(configuration)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func currentExecutableIdentity() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(executable) //nolint:gosec // The kernel-selected current executable is the runtime identity input.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func hostServiceIdentity() (string, string) {
	if runtime.GOOS == "darwin" {
		return "launchd-user", "net.antst.agent-sessions"
	}
	return "systemd-user", "agent-sessions.service"
}
