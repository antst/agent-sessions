package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/statestore"
)

// MigrationInspector performs the read-only first-migration gate before a
// unified host release is selected. It is also the no-migration compatibility
// implementation of HostMigrationLifecycle used by clean install and remove.
type MigrationInspector func(context.Context, releaseinstall.InstallRequest) error

// Prepare implements HostMigrationLifecycle for an inspection-only gate.
func (inspect MigrationInspector) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	return inspect(ctx, request)
}

// BeforeReady is a no-op when inspection determined no first migration exists.
func (MigrationInspector) BeforeReady(context.Context, releaseinstall.InstalledRelease) error {
	return nil
}

// Rollback is a no-op because an inspection-only gate performs no mutation.
func (MigrationInspector) Rollback(context.Context) error { return nil }

// HostMigrationLifecycle is the host-only transaction seam composed by the
// role-neutral release installer. BeforeReady must finish exact recovery and
// retirement before the hook may report unified-daemon readiness.
type HostMigrationLifecycle interface {
	// Prepare inventories, verifies the operator-held maintenance window,
	// selects the journal, and adopts exact legacy metadata.
	Prepare(context.Context, releaseinstall.InstallRequest) error
	// BeforeReady commits successor authority and retires exact legacy artifacts.
	BeforeReady(context.Context, releaseinstall.InstalledRelease) error
	// Rollback records the failed maintenance-window attempt after the release
	// transaction has stopped its candidate. It performs no service lifecycle action.
	Rollback(context.Context) error
}

// FirstMigrationInspection is the already-inventoried, read-only input for
// one first migration. Required=false identifies a clean or already-unified
// estate and requires every other field to be empty.
type FirstMigrationInspection struct {
	Required         bool
	MigrationID      string
	ExpectedRevision statestore.Revision
	FromVersions     []string
	Candidates       []LegacyRuntimeCandidate
	PriorAuthority   LegacyPriorAuthority
	Adoption         LegacyAdoptionRequest
}

// FirstMigrationInspector inventories and classifies the bounded legacy
// sources. The lifecycle independently reruns the global quiescence gate
// before it permits any durable or external mutation.
type FirstMigrationInspector func(
	context.Context,
	releaseinstall.InstallRequest,
) (FirstMigrationInspection, error)

// InspectProductionFirstMigration performs the reusable pre-daemon read-only
// inspection over T080's shipped closed source list. It never opens or creates
// unified state. Incomplete legacy evidence is returned as an Unknown
// candidate so T081 can project actionable debt without authorizing mutation.
func InspectProductionFirstMigration(ctx context.Context) (FirstMigrationInspection, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return FirstMigrationInspection{}, err
	}
	stateHome, err := absoluteEnvironmentPath("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return FirstMigrationInspection{}, err
	}
	runtimeDir := ""
	if runtime.GOOS == "linux" {
		runtimeDir, err = absoluteEnvironmentPath("XDG_RUNTIME_DIR", "")
		if err != nil {
			return FirstMigrationInspection{}, err
		}
	}
	options := LegacyInventoryOptions{
		Platform: runtime.GOOS, UID: os.Getuid(), HomeDir: filepath.Clean(home), StateHome: stateHome,
		RuntimeDir: runtimeDir, SystemTempDir: filepath.Clean(os.TempDir()),
	}
	if runtime.GOOS == "darwin" {
		options.RecordedRuntimeRoots, err = productionRecordedRuntimeRoots(stateHome, os.Getuid())
		if err != nil {
			return FirstMigrationInspection{}, err
		}
	}
	sources, err := LegacyInventorySources(options)
	if err != nil {
		return FirstMigrationInspection{}, err
	}
	return inspectProductionFirstMigrationSources(ctx, sources, time.Now().UnixMilli())
}

// ProductionFirstMigrationInspector adapts the reusable offline inspection to
// the install transaction without introducing a second collector.
func ProductionFirstMigrationInspector(
	ctx context.Context,
	_ releaseinstall.InstallRequest,
) (FirstMigrationInspection, error) {
	return InspectProductionFirstMigration(ctx)
}

func inspectProductionFirstMigrationSources(
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
) (FirstMigrationInspection, error) {
	return collectProductionFirstMigration(ctx, sources, observedAt)
}

func productionLegacyCandidateKind(source LegacyInventorySource) string {
	if source.Kind == "service" {
		return "host_agent_service_job"
	}
	switch {
	case strings.HasPrefix(source.ID, "bridge-") || strings.HasPrefix(source.ID, "recorded-bridge-"):
		return "supervisor"
	case strings.HasPrefix(source.ID, "federator-"):
		return "host_federation_agent"
	case strings.HasPrefix(source.ID, "grok-"):
		return "grok_host"
	default:
		return "legacy_state_owner"
	}
}

// FirstMigrationLifecycleOptions composes T080/T081 inspection evidence with
// the authoritative T082 adoption and T083 retirement engines.
type FirstMigrationLifecycleOptions struct {
	State      *StateStore
	Retirement *LegacyRetirementEngine
	Inspect    FirstMigrationInspector
}

// FirstMigrationLifecycle integrates exactly one selected migration journal
// with the host install transaction. The release install lock is the external
// serialization authority; the mutex also makes direct in-process retries safe.
type FirstMigrationLifecycle struct {
	mu         sync.Mutex
	state      *StateStore
	retirement *LegacyRetirementEngine
	inspect    FirstMigrationInspector
	activeID   string
}

// NewFirstMigrationLifecycle validates the concrete install integration.
func NewFirstMigrationLifecycle(options FirstMigrationLifecycleOptions) (*FirstMigrationLifecycle, error) {
	if options.State == nil || options.Retirement == nil || options.Inspect == nil {
		return nil, errors.New("first migration lifecycle requires state, retirement, and inspection")
	}
	return &FirstMigrationLifecycle{
		state: options.State, retirement: options.Retirement, inspect: options.Inspect,
	}, nil
}

// Prepare refuses blockers without mutation, stages the complete adoption in
// memory, creates and selects the exact journal, then records the operator-proven
// absence of legacy authorities before committing the adopted successor state.
// No live handoff or installer-owned legacy lifecycle action exists.
func (migration *FirstMigrationLifecycle) Prepare( //nolint:gocyclo // One locked transaction boundary keeps mutation order explicit.
	ctx context.Context,
	request releaseinstall.InstallRequest,
) error {
	migration.mu.Lock()
	defer migration.mu.Unlock()
	if resumed, err := migration.resumeSelectedPrepare(ctx); resumed || err != nil {
		return err
	}
	inspection, err := migration.inspect(ctx, request)
	if err != nil {
		return fmt.Errorf("inspect legacy estate: %w", err)
	}
	if !inspection.Required {
		if !emptyFirstMigrationInspection(inspection) {
			return errors.New("optional first migration inspection contains migration authority")
		}
		migration.activeID = ""
		return nil
	}
	if !durableRecordID.MatchString(inspection.MigrationID) || len(inspection.Candidates) == 0 {
		return errors.New("first migration inspection has incomplete exact identity")
	}
	if _, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: inspection.Candidates}); err != nil {
		return err
	}
	adoption, err := StageLegacyAdoption(ctx, inspection.Adoption)
	if err != nil {
		return fmt.Errorf("stage legacy adoption: %w", err)
	}
	targetIdentity, err := stagedHostExecutableIdentity(request)
	if err != nil {
		return err
	}
	journal, _, err := migration.retirement.PrepareRetirement(ctx, LegacyRetirementRequest{
		MigrationID: inspection.MigrationID, ExpectedRevision: inspection.ExpectedRevision,
		FromVersions: inspection.FromVersions, TargetRuntimeIdentity: targetIdentity,
		Candidates: inspection.Candidates, PriorAuthority: inspection.PriorAuthority,
	})
	if err != nil {
		return fmt.Errorf("prepare legacy retirement journal: %w", err)
	}
	if err := migration.state.savePreparedFirstMigration(ctx, journal.MigrationID, adoption); err != nil {
		return fmt.Errorf("persist prepared legacy adoption: %w", err)
	}
	if err := migration.selectCurrent(ctx, journal.MigrationID); err != nil {
		return errors.Join(err, migration.retirementRollback(ctx, journal.MigrationID))
	}
	migration.activeID = journal.MigrationID
	if _, err := migration.retirement.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID); err != nil {
		return errors.Join(fmt.Errorf("accept operator-stopped legacy estate: %w", err), migration.retirementRollback(ctx, journal.MigrationID))
	}
	if err := migration.state.commitPreparedFirstMigration(ctx, journal.MigrationID); err != nil {
		return errors.Join(fmt.Errorf("commit staged legacy adoption: %w", err), migration.retirementRollback(ctx, journal.MigrationID))
	}
	return nil
}

func (migration *FirstMigrationLifecycle) resumeSelectedPrepare(ctx context.Context) (bool, error) {
	current, _, err := migration.state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	journal, _, err := migration.state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return true, err
	}
	if journal.FreshInventoryRequired {
		// A release transaction terminated this attempt. A later install must
		// obtain fresh inventory; it must never replay the old journal.
		if err := migration.state.rollbackSelectedPreparedFirstMigration(ctx, current.MigrationID); err != nil {
			return true, err
		}
		return false, nil
	}
	switch journal.State {
	case MigrationStateComplete:
		migration.activeID = ""
		return true, nil
	case MigrationStateLegacyAbsenceVerified:
		if _, err := migration.retirement.AcceptVerifiedLegacyAbsence(ctx, current.MigrationID); err != nil {
			return true, fmt.Errorf("accept selected operator-stopped estate: %w", err)
		}
		fallthrough
	case MigrationStateAdopting:
		if err := migration.state.commitPreparedFirstMigration(ctx, current.MigrationID); err != nil {
			return true, fmt.Errorf("resume prepared legacy adoption: %w", err)
		}
		migration.activeID = current.MigrationID
		return true, nil
	case MigrationStateRetryRequired:
		return false, nil
	case MigrationStateAuthorityCommitted, MigrationStateRetiringLegacyArtifacts:
		migration.activeID = current.MigrationID
		return true, nil
	case MigrationStateInventorying, MigrationStateBlockedActivePeerOrLane,
		MigrationStateBlockedLiveAuthority, MigrationStateBlockedUnknownIdentity, MigrationStateDebt:
		return true, fmt.Errorf("selected first migration %q requires recovery from state %q", current.MigrationID, journal.State)
	}
	return true, fmt.Errorf("selected first migration %q has unsupported state %q", current.MigrationID, journal.State)
}

// BeforeReady recovers only the exact selected journal. It commits successor
// authority from the running candidate's durable identity and retires legacy
// artifacts before the install readiness hook can return success.
func (migration *FirstMigrationLifecycle) BeforeReady(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
) error {
	migration.mu.Lock()
	defer migration.mu.Unlock()
	current, _, err := migration.state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		if migration.activeID != "" {
			return errors.New("prepared first migration lost its authoritative current selector")
		}
		return nil
	}
	if err != nil {
		return err
	}
	journal, _, err := migration.state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	if journal.State == MigrationStateComplete {
		migration.activeID = ""
		return nil
	}
	targetIdentity, err := installedHostExecutableIdentity(release)
	if err != nil {
		return err
	}
	runtime, _, err := migration.state.ReadRuntime(ctx)
	if err != nil {
		return fmt.Errorf("read candidate runtime authority: %w", err)
	}
	if runtime.State != HostRuntimeReady || runtime.RuntimeIdentity != targetIdentity ||
		journal.TargetRuntimeIdentity != targetIdentity {
		return errors.New("candidate runtime is not the exact durable migration successor")
	}
	if err := CommitFirstMigrationAuthority(
		ctx, migration.state, current.MigrationID, targetIdentity, runtime.Generation, runtime.CommittedAt,
	); err != nil {
		return err
	}
	result, err := migration.retirement.Recover(ctx, current.MigrationID)
	if err != nil {
		return fmt.Errorf("recover exact legacy retirement: %w", err)
	}
	if !result.Complete || !result.ArtifactsRetired {
		return errors.New("legacy retirement did not reach complete before readiness")
	}
	migration.activeID = ""
	return nil
}

// Rollback terminates this migration attempt after the enclosing release
// transaction has durably stopped its candidate. It performs no service
// lifecycle action and never restarts a legacy authority. A completed migration
// deliberately cannot regress to split runtime ownership.
func (migration *FirstMigrationLifecycle) Rollback(ctx context.Context) error {
	migration.mu.Lock()
	defer migration.mu.Unlock()
	current, _, err := migration.state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	journal, _, err := migration.state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	if journal.State == MigrationStateComplete {
		migration.activeID = ""
		return nil
	}
	err = migration.retirementRollback(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	journal, revision, err := migration.state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	if journal.FreshInventoryRequired {
		if err := migration.state.rollbackSelectedPreparedFirstMigration(ctx, current.MigrationID); err != nil {
			return err
		}
		migration.activeID = ""
		return nil
	}
	if !journal.RollbackCompleted || journal.State != MigrationStateRetryRequired {
		return errors.New("release rollback did not restore a terminal pre-migration authority")
	}
	journal.FreshInventoryRequired = true
	journal.Revision++
	journal.UpdatedAt = migration.retirement.timestamp(journal.UpdatedAt)
	if _, err := migration.state.CompareAndSwapMigration(ctx, revision, journal); err != nil {
		return err
	}
	if err := migration.state.rollbackSelectedPreparedFirstMigration(ctx, current.MigrationID); err != nil {
		return err
	}
	migration.activeID = ""
	return nil
}

func (migration *FirstMigrationLifecycle) selectCurrent(ctx context.Context, migrationID string) error {
	current, revision, err := migration.state.ReadCurrentMigration(ctx)
	if err == nil {
		journal, _, journalErr := migration.state.ReadMigration(ctx, current.MigrationID)
		if journalErr != nil {
			return journalErr
		}
		if current.MigrationID == migrationID {
			if journal.FreshInventoryRequired {
				return errors.New("fresh migration inspection reused a terminally rolled-back migration ID")
			}
			return nil
		}
		if !journal.FreshInventoryRequired {
			return fmt.Errorf("current migration %q conflicts with inspected migration %q", current.MigrationID, migrationID)
		}
		_, err = migration.state.SelectCurrentMigration(ctx, revision, MigrationCurrent{
			SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
		})
		return err
	}
	if !os.IsNotExist(err) {
		return err
	}
	_, err = migration.state.SelectCurrentMigration(ctx, revision, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
	})
	if errors.Is(err, statestore.ErrRevisionConflict) {
		current, _, readErr := migration.state.ReadCurrentMigration(ctx)
		if readErr == nil && current.MigrationID == migrationID {
			return nil
		}
	}
	return err
}

func (migration *FirstMigrationLifecycle) retirementRollback(ctx context.Context, migrationID string) error {
	_, err := migration.retirement.RollbackMaintenanceWindowBeforeReady(ctx, migrationID)
	return err
}

func emptyFirstMigrationInspection(inspection FirstMigrationInspection) bool {
	return inspection.MigrationID == "" && inspection.ExpectedRevision == 0 && len(inspection.FromVersions) == 0 &&
		len(inspection.Candidates) == 0 && reflectLegacyPriorAuthorityEmpty(inspection.PriorAuthority) &&
		reflectLegacyAdoptionRequestEmpty(inspection.Adoption)
}

func reflectLegacyPriorAuthorityEmpty(authority LegacyPriorAuthority) bool {
	return reflect.DeepEqual(authority, LegacyPriorAuthority{})
}

func reflectLegacyAdoptionRequestEmpty(request LegacyAdoptionRequest) bool {
	return request.SourceRevision == "" && request.HostID == "" && len(request.Sessions) == 0 &&
		len(request.Names) == 0 && len(request.Deliveries) == 0 && len(request.DeliveryCursors) == 0 &&
		len(request.DeliveryNotices) == 0 && len(request.Preparations) == 0 && request.Configuration == nil &&
		len(request.Lanes) == 0 &&
		len(request.Turns) == 0 && len(request.Notices) == 0 &&
		reflect.DeepEqual(request.Hub, FederationStateRecord{}) && len(request.Debt) == 0 && len(request.ExcludedPaths) == 0
}

func stagedHostExecutableIdentity(request releaseinstall.InstallRequest) (string, error) {
	if request.Executable != "agent-sessions" || !filepath.IsAbs(request.SourceRoot) {
		return "", errors.New("first migration requires an immutable staged host release")
	}
	return hostExecutableIdentity(filepath.Join(request.SourceRoot, "bin", request.Executable))
}

func installedHostExecutableIdentity(release releaseinstall.InstalledRelease) (string, error) {
	if release.Role != releaseinstall.RoleHost || !filepath.IsAbs(release.Executable) ||
		!pathWithin(release.Executable, release.Root) {
		return "", errors.New("first migration readiness requires an exact installed host release")
	}
	return hostExecutableIdentity(release.Executable)
}

func hostExecutableIdentity(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // Exact staged/selected executable is the migration target identity.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// HostReadinessProbe verifies that the selected service reports the exact
// release identity, generation, endpoint, state schema and routing readiness.
type HostReadinessProbe func(context.Context, releaseinstall.InstalledRelease) error

// HostInstallLifecycle composes host-only migration, connector and readiness
// hooks over the role-neutral release transaction engine.
type HostInstallLifecycle struct {
	connectors *HostInstallHooks
	migration  HostMigrationLifecycle
	readiness  HostReadinessProbe
}

// NewHostInstallLifecycle creates the host-specific portion of an install.
func NewHostInstallLifecycle(
	connectors *HostInstallHooks,
	migration HostMigrationLifecycle,
	readiness HostReadinessProbe,
) (*HostInstallLifecycle, error) {
	if connectors == nil || migration == nil || readiness == nil {
		return nil, errors.New("host install lifecycle requires connectors, migration inspection and readiness")
	}
	return &HostInstallLifecycle{connectors: connectors, migration: migration, readiness: readiness}, nil
}

// Prepare runs the no-mutation migration gate before connector preparation.
func (lifecycle *HostInstallLifecycle) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if err := lifecycle.migration.Prepare(ctx, request); err != nil {
		return fmt.Errorf("inspect first migration: %w", err)
	}
	if err := lifecycle.connectors.Prepare(ctx, request); err != nil {
		return errors.Join(err, lifecycle.migration.Rollback(ctx))
	}
	return nil
}

// Ready verifies the immutable executable identity boundary and then invokes
// the exact running-daemon readiness probe.
func (lifecycle *HostInstallLifecycle) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	if release.Role != releaseinstall.RoleHost || !filepath.IsAbs(release.Root) ||
		filepath.Clean(release.Root) != release.Root || !pathWithin(release.Executable, release.Root) {
		return errors.New("host readiness received a non-host or unsafe release identity")
	}
	info, err := os.Lstat(release.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("selected host release executable is missing or not executable")
	}
	if err := lifecycle.readiness(ctx, release); err != nil {
		return err
	}
	return lifecycle.migration.BeforeReady(ctx, release)
}

// Commit commits the four-product connector transaction.
func (lifecycle *HostInstallLifecycle) Commit(ctx context.Context) error {
	return lifecycle.connectors.Commit(ctx)
}

// Rollback restores exact prior connector state.
func (lifecycle *HostInstallLifecycle) Rollback(ctx context.Context) error {
	return errors.Join(lifecycle.connectors.Rollback(ctx), lifecycle.migration.Rollback(ctx))
}

// Remove invokes only supported host connector removal.
func (lifecycle *HostInstallLifecycle) Remove(ctx context.Context) error {
	return lifecycle.connectors.Remove(ctx)
}
