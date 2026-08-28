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
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// RunHostInstallCLI performs the offline host-role invocation used by
// install-all. It cannot select, install, or restart the hub role.
//
//nolint:gocyclo // The one role entry point keeps parsing, staging hooks, aliases, service, and readiness visibly ordered.
func RunHostInstallCLI(ctx context.Context, args []string) error {
	values, err := parseHostInstallOptions(args)
	if err != nil {
		return err
	}
	if values["--role"] != "host" {
		return errors.New("host installer accepts only --role host")
	}
	sourceRoot, prefix, version := values["--source-root"], values["--prefix"], values["--version"]
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix || prefix == string(filepath.Separator) {
		return errors.New("host install prefix must be a clean absolute non-root path")
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	if err != nil {
		return err
	}
	hooks, err := NewDurableHostInstallHooks(nil, filepath.Join(layout.TransactionRoot, "connectors.json"))
	if err != nil {
		return err
	}
	service, err := newInstalledHostService(prefix)
	if err != nil {
		return err
	}
	gate := &productionHostMigrationGate{
		stateRoot: paths.StateRoot,
		retryDebt: func(ctx context.Context, state *StateStore, observedAt int64) (LegacyMigrationDebtRetryResult, error) {
			observer, observerErr := newProductionLegacyRetirementLifecycle(paths, state)
			if observerErr != nil {
				return LegacyMigrationDebtRetryResult{}, observerErr
			}
			return RetrySelectedLegacyMigrationDebt(ctx, state, observer, observedAt)
		},
	}
	buildLifecycle := func(existingOnly bool) (*HostInstallLifecycle, error) {
		var state *StateStore
		var openErr error
		if existingOnly {
			state, openErr = OpenStateStoreExisting(paths.StateRoot, defaultMaximumStateRecordBytes)
		} else {
			state, openErr = OpenStateStore(paths.StateRoot, defaultMaximumStateRecordBytes)
		}
		if openErr != nil {
			return nil, openErr
		}
		legacyLifecycle, lifecycleErr := newProductionLegacyRetirementLifecycle(paths, state)
		if lifecycleErr != nil {
			return nil, lifecycleErr
		}
		retirement, lifecycleErr := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
			State: state, Lifecycle: legacyLifecycle,
		})
		if lifecycleErr != nil {
			return nil, lifecycleErr
		}
		firstMigration, lifecycleErr := NewFirstMigrationLifecycle(FirstMigrationLifecycleOptions{
			State: state, Retirement: retirement, Inspect: gate.Inspect,
		})
		if lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return NewHostInstallLifecycle(hooks, firstMigration, hostInstallReadiness(paths.ControlEndpoint))
	}
	var lifecycle *HostInstallLifecycle
	if _, openErr := OpenStateStoreExisting(paths.StateRoot, defaultMaximumStateRecordBytes); openErr == nil {
		lifecycle, err = buildLifecycle(true)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(openErr) {
		return openErr
	}
	roleHooks := &hostInstallRoleHooks{
		lifecycle: lifecycle, initialize: buildLifecycle, migrationGate: gate,
		prefix: prefix, stateRoot: paths.StateRoot, runtimeEndpoint: paths.ControlEndpoint,
	}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: roleHooks})
	if err != nil {
		return err
	}
	return recoverThenInstallHostSource(ctx, engine, sourceRoot, version, func() error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		drivers, err := NewNativeConnectorDrivers(NativeConnectorOptions{
			CodexExecutable: values["--codex"], ClaudeExecutable: values["--claude"],
			GrokExecutable: values["--grok"], QwenExecutable: values["--qwen"],
			GrokUserPluginRoot: filepath.Join(home, ".grok", "plugins", "agent-sessions"),
		})
		if err != nil {
			return err
		}
		return hooks.ConfigureDrivers(drivers)
	})
}

type hostReleaseTransaction interface {
	Recover(context.Context) error
	Install(context.Context, releaseinstall.InstallRequest) (releaseinstall.InstallResult, error)
}

func recoverThenInstallHost(
	ctx context.Context,
	engine hostReleaseTransaction,
	request releaseinstall.InstallRequest,
) error {
	// Finish or roll back the exact durable release transaction before a new
	// request can replace its FromRelease provenance.
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover host install transaction: %w", err)
	}
	_, err := engine.Install(ctx, request)
	return err
}

func recoverThenInstallHostSource(
	ctx context.Context,
	engine hostReleaseTransaction,
	sourceRoot string,
	version string,
	configureDrivers func() error,
) error {
	// Recovery is authority over an unfinished prior transaction. It uses the
	// selected immutable release and durable connector provenance, and must
	// finish before this invocation reads its new source or resolves its native
	// product executables.
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover host install transaction: %w", err)
	}
	if !filepath.IsAbs(sourceRoot) || filepath.Clean(sourceRoot) != sourceRoot || sourceRoot == string(filepath.Separator) {
		return errors.New("host install source root must be a clean absolute non-root path")
	}
	identity, err := releaseinstall.ContentIdentity(sourceRoot)
	if err != nil {
		return err
	}
	if configureDrivers == nil {
		return errors.New("host install native connector configuration is unavailable")
	}
	if err := configureDrivers(); err != nil {
		return err
	}
	_, err = engine.Install(ctx, releaseinstall.InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: sourceRoot, Executable: "agent-sessions",
	})
	return err
}

type hostInstallRoleHooks struct {
	lifecycle       *HostInstallLifecycle
	initialize      func(bool) (*HostInstallLifecycle, error)
	migrationGate   *productionHostMigrationGate
	prefix          string
	stateRoot       string
	runtimeEndpoint string
	restoreService  func() error
	restoreAliases  func() error
}

type productionHostMigrationGate struct {
	mu         sync.Mutex
	stateRoot  string
	inspect    func(context.Context) (FirstMigrationInspection, error)
	request    releaseinstall.InstallRequest
	inspection FirstMigrationInspection
	resume     bool
	ready      bool
	final      bool
	retryDebt  func(context.Context, *StateStore, int64) (LegacyMigrationDebtRetryResult, error)
}

// Preflight performs the sole production first-migration observation while
// releaseinstall holds the cross-process role lock. No unified state or
// release transaction exists until this method succeeds.
func (hooks *hostInstallRoleHooks) Preflight(
	ctx context.Context,
	request releaseinstall.InstallRequest,
) error {
	if hooks == nil || hooks.migrationGate == nil {
		return nil
	}
	return hooks.migrationGate.Preflight(ctx, request)
}

// Preflight captures the exact migration inspection under the release lock.
func (gate *productionHostMigrationGate) Preflight( //nolint:gocyclo // Recovery, debt refresh, and fresh inspection stay one lock-held admission decision.
	ctx context.Context,
	request releaseinstall.InstallRequest,
) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if existing, err := OpenStateStoreExisting(gate.stateRoot, defaultMaximumStateRecordBytes); err == nil {
		if current, _, currentErr := existing.ReadCurrentMigration(ctx); currentErr == nil {
			journal, _, journalErr := existing.ReadMigration(ctx, current.MigrationID)
			if journalErr != nil {
				return journalErr
			}
			if journal.FreshInventoryRequired {
				// The selected journal is historical rollback evidence, not recovery
				// authority. Continue into a fresh production inspection below.
				goto inspectFresh
			}
			if (journal.State == MigrationStateBlockedUnknownIdentity || journal.State == MigrationStateDebt) &&
				gate.retryDebt != nil {
				if _, retryErr := gate.retryDebt(ctx, existing, time.Now().UnixMilli()); retryErr != nil {
					return retryErr
				}
				journal, _, journalErr = existing.ReadMigration(ctx, current.MigrationID)
				if journalErr != nil {
					return journalErr
				}
				if journal.FreshInventoryRequired {
					goto inspectFresh
				}
			}
			switch journal.State {
			case MigrationStateLegacyAbsenceVerified, MigrationStateAdopting,
				MigrationStateAuthorityCommitted, MigrationStateRetiringLegacyArtifacts, MigrationStateComplete:
			case MigrationStateInventorying, MigrationStateBlockedActivePeerOrLane,
				MigrationStateBlockedLiveAuthority, MigrationStateBlockedUnknownIdentity,
				MigrationStateDebt, MigrationStateRetryRequired:
				return fmt.Errorf("selected first migration %q requires recovery from state %q", current.MigrationID, journal.State)
			default:
				return fmt.Errorf("selected first migration %q has unsupported state %q", current.MigrationID, journal.State)
			}
			gate.request, gate.inspection, gate.resume, gate.ready, gate.final =
				request, FirstMigrationInspection{}, true, true, true
			return nil
		} else if !os.IsNotExist(currentErr) {
			return currentErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}

inspectFresh:
	inspect := gate.inspect
	if inspect == nil {
		inspect = InspectProductionFirstMigration
	}
	inspection, err := inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect first migration: %w", err)
	}
	if inspection.Required {
		if _, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: inspection.Candidates}); err != nil {
			return err
		}
	}
	gate.request, gate.inspection, gate.resume, gate.ready, gate.final = request, inspection, false, true, false
	return nil
}

// FinalInspect repeats the exact production inventory immediately before the
// first migration can create unified state or stop an authority. The operator
// maintenance window is the cross-version admission exclusion boundary: this
// second observation detects a legacy launch that violated that window, while
// never pretending the new release lock is understood by old launchers.
func (gate *productionHostMigrationGate) FinalInspect(
	ctx context.Context,
	request releaseinstall.InstallRequest,
) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.ready || request.Version != gate.request.Version ||
		request.ContentIdentity != gate.request.ContentIdentity || request.Executable != gate.request.Executable {
		return errors.New("final first migration inspection lacks its exact lock-held preflight")
	}
	if gate.resume {
		gate.final = true
		return nil
	}
	inspect := gate.inspect
	if inspect == nil {
		inspect = InspectProductionFirstMigration
	}
	inspection, err := inspect(ctx)
	if err != nil {
		return fmt.Errorf("repeat first migration inspection before mutation: %w", err)
	}
	if inspection.Required {
		if _, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: inspection.Candidates}); err != nil {
			return err
		}
	}
	gate.inspection, gate.final = inspection, true
	return nil
}

// Inspect returns only the exact inspection authorized by the lock-held gate.
func (gate *productionHostMigrationGate) Inspect(
	_ context.Context,
	request releaseinstall.InstallRequest,
) (FirstMigrationInspection, error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.ready || !gate.final || gate.resume || request.Version != gate.request.Version ||
		request.ContentIdentity != gate.request.ContentIdentity || request.Executable != gate.request.Executable {
		return FirstMigrationInspection{}, errors.New("first migration prepare lacks its exact lock-held preflight")
	}
	return gate.inspection, nil
}

func (gate *productionHostMigrationGate) migrationBinding(ctx context.Context) (string, error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.ready {
		return "", errors.New("host migration binding lacks its lock-held preflight")
	}
	if !gate.resume {
		return gate.inspection.MigrationID, nil
	}
	state, err := OpenStateStoreExisting(gate.stateRoot, defaultMaximumStateRecordBytes)
	if err != nil {
		return "", err
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if err != nil {
		return "", err
	}
	journal, _, err := state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return "", err
	}
	if journal.State == MigrationStateComplete {
		return "", nil
	}
	return current.MigrationID, nil
}

func (hooks *hostInstallRoleHooks) ensureLifecycle(existingOnly bool) error {
	if hooks.lifecycle != nil {
		return nil
	}
	if hooks.initialize == nil {
		return errors.New("host install role hooks require an initialized lifecycle")
	}
	lifecycle, err := hooks.initialize(existingOnly)
	if err != nil {
		return err
	}
	hooks.lifecycle = lifecycle
	return nil
}

// Prepare implements releaseinstall.RoleHooks under the role install lock.
func (hooks *hostInstallRoleHooks) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if hooks == nil {
		return errors.New("host install role hooks require a lifecycle")
	}
	if hooks.migrationGate != nil {
		if err := hooks.migrationGate.FinalInspect(ctx, request); err != nil {
			return err
		}
	}
	if err := hooks.ensureLifecycle(false); err != nil {
		return err
	}
	var err error
	migrationID := ""
	if hooks.migrationGate != nil {
		migrationID, err = hooks.migrationGate.migrationBinding(ctx)
		if err != nil {
			return err
		}
	}
	migrationTargetIdentity := ""
	if migrationID != "" {
		migrationTargetIdentity, err = stagedHostExecutableIdentity(request)
		if err != nil {
			return err
		}
	}
	surface, err := captureHostSurfaceRollback(
		hooks.prefix, hooks.stateRoot, migrationID, migrationTargetIdentity,
	)
	if err != nil {
		return err
	}
	if err := saveHostSurfaceRollback(surface); err != nil {
		return err
	}
	if err := hooks.lifecycle.Prepare(ctx, request); err != nil {
		return errors.Join(err, hooks.rollbackSurface())
	}
	restoreService, err := installHostServiceDefinition(request.SourceRoot, hooks.prefix, hooks.stateRoot)
	if err != nil {
		return errors.Join(err, hooks.lifecycle.Rollback(ctx))
	}
	hooks.restoreService = restoreService
	restoreAliases, err := installHostAliases(hooks.prefix)
	if err != nil {
		return errors.Join(err, hooks.rollbackSurface(), hooks.lifecycle.Rollback(ctx))
	}
	hooks.restoreAliases = restoreAliases
	return nil
}

// FailureDisposition prevents release rollback after first-migration
// successor authority has durably crossed its irreversible commit boundary.
func (hooks *hostInstallRoleHooks) FailureDisposition(
	ctx context.Context,
	_ releaseinstall.Phase,
) (releaseinstall.FailureDisposition, error) {
	record, err := loadHostSurfaceRollback(hooks.prefix, hooks.stateRoot)
	if err != nil {
		return releaseinstall.FailureDispositionRollForward, err
	}
	if record.MigrationID == "" {
		return releaseinstall.FailureDispositionRollback, nil
	}
	state, err := OpenStateStoreExisting(hooks.stateRoot, defaultMaximumStateRecordBytes)
	if err != nil {
		return releaseinstall.FailureDispositionRollForward, err
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if err != nil || current.MigrationID != record.MigrationID {
		if err == nil {
			err = errors.New("release rollback migration binding no longer matches the current selector")
		}
		return releaseinstall.FailureDispositionRollForward, err
	}
	journal, _, err := state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return releaseinstall.FailureDispositionRollForward, err
	}
	if journal.TargetRuntimeIdentity != record.MigrationTargetIdentity {
		return releaseinstall.FailureDispositionRollForward,
			errors.New("release rollback migration target identity changed")
	}
	if migrationStateHasCommittedAuthority(journal.State) && journal.SuccessorStateDurable &&
		journal.MaintenanceWindowState == MaintenanceWindowLegacyAbsenceVerified && journal.AuthorityGeneration > 0 {
		return releaseinstall.FailureDispositionRollForward, nil
	}
	return releaseinstall.FailureDispositionRollback, nil
}

// Ready implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	if err := hooks.ensureLifecycle(true); err != nil {
		return err
	}
	return hooks.lifecycle.Ready(ctx, release)
}

// Commit implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Commit(ctx context.Context) error {
	if err := hooks.ensureLifecycle(true); err != nil {
		return err
	}
	if err := hooks.lifecycle.Commit(ctx); err != nil {
		return err
	}
	if err := removeHostSurfaceRollback(hooks.prefix); err != nil {
		return err
	}
	hooks.restoreAliases, hooks.restoreService = nil, nil
	return nil
}

// Rollback implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Rollback(ctx context.Context) error {
	if err := hooks.ensureLifecycle(true); err != nil {
		if os.IsNotExist(err) {
			return hooks.rollbackSurface()
		}
		return errors.Join(hooks.rollbackSurface(), err)
	}
	return errors.Join(hooks.rollbackSurface(), hooks.lifecycle.Rollback(ctx))
}

// Remove implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Remove(ctx context.Context) error {
	removeSurface, err := prepareHostSurfaceRemoval(hooks.prefix, hooks.runtimeEndpoint)
	if err != nil {
		return err
	}
	if err := hooks.lifecycle.Remove(ctx); err != nil {
		return err
	}
	return removeSurface(ctx)
}

func (hooks *hostInstallRoleHooks) rollbackSurface() error {
	var result error
	if hooks.restoreAliases != nil {
		result = errors.Join(result, hooks.restoreAliases())
		hooks.restoreAliases = nil
	}
	if hooks.restoreService != nil {
		result = errors.Join(result, hooks.restoreService())
		hooks.restoreService = nil
	}
	record, err := loadHostSurfaceRollback(hooks.prefix, hooks.stateRoot)
	if err == nil {
		if restoreErr := restoreHostSurfaceRollback(record); restoreErr != nil {
			result = errors.Join(result, restoreErr)
		} else {
			result = errors.Join(result, removeHostSurfaceRollback(hooks.prefix))
		}
	} else if !os.IsNotExist(err) {
		result = errors.Join(result, err)
	}
	return result
}

func parseHostInstallOptions(args []string) (map[string]string, error) {
	allowed := map[string]bool{
		"--role": true, "--source-root": true, "--prefix": true, "--version": true,
		"--codex": true, "--claude": true, "--grok": true, "--qwen": true,
	}
	values := map[string]string{}
	for len(args) != 0 {
		if len(args) < 2 || !allowed[args[0]] || args[1] == "" || values[args[0]] != "" {
			return nil, errors.New("usage: lifecycle install --role host --source-root ROOT --prefix PREFIX --version VERSION [--codex PATH --claude PATH --grok PATH --qwen PATH]")
		}
		values[args[0]], args = args[1], args[2:]
	}
	for _, required := range []string{"--role", "--source-root", "--prefix", "--version"} {
		if values[required] == "" {
			return nil, fmt.Errorf("host install requires %s", required)
		}
	}
	return values, nil
}

func installHostAliases(prefix string) (func() error, error) {
	return installHostAliasesWithHooks(prefix, nil)
}

func installHostAliasesWithHooks(
	prefix string, mutationHooks *hostSurfaceMutationHooks,
) (func() error, error) {
	binRoot := filepath.Join(prefix, "bin")
	target := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	names := hostAliasNames()
	snapshots := make([]hostSurfaceAliasRollback, 0, len(names))
	for _, name := range names {
		path := filepath.Join(binRoot, name)
		snapshot, err := snapshotHostSurfaceAlias(path, nil)
		if err != nil {
			return nil, errors.Join(err, restoreHostAliases(snapshots))
		}
		snapshots = append(snapshots, snapshot)
		expected := &hostSurfaceEntryExpectation{present: snapshot.Present, identity: snapshot.observed}
		if err := replaceHostSurfaceAliasExpected(path, target, mutationHooks, expected); err != nil {
			return nil, errors.Join(err, restoreHostAliases(snapshots))
		}
	}
	return func() error { return restoreHostAliases(snapshots) }, nil
}

func hostAliasNames() []string {
	return append([]string{"agent-sessions"}, productcatalog.Catalog().HostAliases...)
}

func replaceAlias(path, target string) error {
	return replaceHostSurfaceAlias(path, target, nil)
}

func restoreHostAliases(snapshots []hostSurfaceAliasRollback) error {
	var result error
	for index := len(snapshots) - 1; index >= 0; index-- {
		result = errors.Join(result, restoreHostSurfaceAlias(snapshots[index], nil))
	}
	return result
}

func hostInstallReadiness(expectedEndpoint string) HostReadinessProbe {
	return func(ctx context.Context, release releaseinstall.InstalledRelease) error {
		expectedIdentity, err := executableSHA256(release.Executable)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(20 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			body, queryErr := QueryAdmin(ctx, "runtime.status")
			if queryErr == nil {
				var status HostStatusProjection
				queryErr = json.Unmarshal(body, &status)
				if queryErr == nil && status.RuntimeVersion == release.Version && status.RuntimeIdentity == expectedIdentity &&
					status.Endpoint == expectedEndpoint && status.Generation > 0 {
					return nil
				}
				if queryErr == nil {
					queryErr = errors.New("running host identity does not match selected release")
				}
			}
			last = queryErr
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return fmt.Errorf("host service did not publish exact readiness: %w", last)
	}
}

func executableSHA256(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // Exact immutable selected release executable.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
