package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/statestore"
)

var (
	// ErrInjectedMigrationCrash identifies a test-owned durable boundary.
	ErrInjectedMigrationCrash = errors.New("injected migration crash")
	// ErrLegacyRetirementDebt identifies an exact-identity conflict that was
	// durably retained for a later re-observation rather than guessed through.
	ErrLegacyRetirementDebt = errors.New("legacy retirement requires identity debt resolution")
)

// LegacyPriorAuthority is the exact reversible authority state retained until
// the successor is ready. It contains Agent Sessions lifecycle metadata only.
type LegacyPriorAuthority struct {
	Candidate         LegacyRuntimeCandidate `json:"candidate"`
	JournalRevision   uint64                 `json:"journal_revision"`
	ReleaseSelection  string                 `json:"release_selection"`
	StateSelection    string                 `json:"state_selection"`
	ConnectorRevision string                 `json:"connector_revision"`
	ServiceManager    string                 `json:"service_manager"`
	ServiceUnit       string                 `json:"service_unit"`
}

// Validate rejects incomplete prior-authority rollback provenance.
func (authority LegacyPriorAuthority) Validate() error {
	if err := authority.Candidate.Validate(); err != nil {
		return fmt.Errorf("prior candidate: %w", err)
	}
	if authority.JournalRevision == 0 || strings.TrimSpace(authority.ConnectorRevision) == "" ||
		(authority.ServiceManager == "") != (authority.ServiceUnit == "") {
		return errors.New("prior authority has incomplete exact rollback identity")
	}
	if !migrationAbsoluteCleanPath(authority.ReleaseSelection) || !migrationAbsoluteCleanPath(authority.StateSelection) {
		return errors.New("prior authority has incomplete exact rollback selection")
	}
	if authority.ServiceManager == "" && (authority.Candidate.Kind != "supervisor" ||
		authority.Candidate.ProcessExecutable != authority.ReleaseSelection ||
		authority.Candidate.SourcePath != authority.StateSelection || len(authority.Candidate.ProcessArguments) == 0 ||
		len(authority.Candidate.ProcessEnvironment) == 0) {
		return errors.New("native prior authority lacks an exact executable, argument, or state selector")
	}
	return nil
}

// LegacyRetirementLifecycle is the only mutation boundary used by retirement.
// Platform implementations must use supported lifecycle controls and exact
// kernel/service observations; the engine never scans or signals by name.
type LegacyRetirementLifecycle interface {
	// ReattestEndpoint re-observes exact endpoint identity immediately before retirement.
	ReattestEndpoint(context.Context, LegacyRuntimeCandidate) (LegacyRuntimeCandidate, error)
	// RetireEndpoint removes only the re-attested Agent Sessions-owned endpoint/artifact.
	RetireEndpoint(context.Context, LegacyRuntimeCandidate) error
}

// LegacyRetirementEngineOptions binds the durable host store to exact
// lifecycle operations. Now is injectable solely to make journal order stable.
type LegacyRetirementEngineOptions struct {
	State     *StateStore
	Lifecycle LegacyRetirementLifecycle
	Now       func() time.Time
}

// LegacyRetirementEngine performs only the stop/retire/rollback mechanics.
// Install ordering and successor readiness remain owned by T084.
type LegacyRetirementEngine struct {
	state      *StateStore
	lifecycle  LegacyRetirementLifecycle
	now        func() time.Time
	crashPoint MigrationState
}

// LegacyRetirementRequest begins the stop phase from a completed quiescence
// gate. Endpoint retirement is deliberately a separate post-commit call.
type LegacyRetirementRequest struct {
	MigrationID           string
	ExpectedRevision      statestore.Revision
	FromVersions          []string
	TargetRuntimeIdentity string
	Candidates            []LegacyRuntimeCandidate
	PriorAuthority        LegacyPriorAuthority
}

// LegacyRetirementResult is bounded phase/debt metadata; it never asserts
// daemon readiness, which belongs to the install transaction.
type LegacyRetirementResult struct {
	AuthoritiesStopped bool
	ArtifactsRetired   bool
	Complete           bool
	Ready              bool
	Debt               []LegacyMigrationDebt
}

// LegacyRollbackResult describes an exact pre-ready restoration attempt.
type LegacyRollbackResult struct {
	Restored bool
	Debt     []LegacyMigrationDebt
}

// NewLegacyRetirementEngine validates the bounded retirement dependencies.
func NewLegacyRetirementEngine(options LegacyRetirementEngineOptions) (*LegacyRetirementEngine, error) {
	if options.State == nil || options.Lifecycle == nil {
		return nil, errors.New("legacy retirement requires durable state and exact lifecycle operations")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &LegacyRetirementEngine{state: options.State, lifecycle: options.Lifecycle, now: options.Now}, nil
}

// SetCrashPoint injects a crash after the named durable migration state.
func (engine *LegacyRetirementEngine) SetCrashPoint(state MigrationState) { engine.crashPoint = state }

// PrepareAndAcceptVerifiedLegacyAbsence creates the exact maintenance journal
// and accepts it only when every recorded legacy authority is already absent.
// It never invokes a legacy lifecycle mutation.
func (engine *LegacyRetirementEngine) PrepareAndAcceptVerifiedLegacyAbsence(
	ctx context.Context,
	request LegacyRetirementRequest,
) (LegacyRetirementResult, error) {
	journal, _, err := engine.PrepareRetirement(ctx, request)
	if err != nil {
		return LegacyRetirementResult{}, err
	}
	return engine.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID)
}

// PrepareRetirement durably creates the exact journal and candidate records
// without mutating a legacy authority. T084 selects this migration ID as the
// sole recovery root before accepting the verified maintenance window.
func (engine *LegacyRetirementEngine) PrepareRetirement(
	ctx context.Context,
	request LegacyRetirementRequest,
) (MigrationTransaction, statestore.Revision, error) {
	return engine.loadOrCreateRetirement(ctx, request)
}

// AcceptVerifiedLegacyAbsence advances a freshly inspected maintenance-
// window journal without invoking any legacy lifecycle mutation. Every
// candidate must already be classified as absent metadata; a live, unknown,
// or conflicting process fails closed.
func (engine *LegacyRetirementEngine) AcceptVerifiedLegacyAbsence(
	ctx context.Context,
	migrationID string,
) (LegacyRetirementResult, error) {
	journal, revision, err := engine.state.ReadMigration(ctx, migrationID)
	if err != nil {
		return LegacyRetirementResult{}, err
	}
	if journal.FreshInventoryRequired {
		return LegacyRetirementResult{}, errors.New("terminal migration requires fresh inventory")
	}
	if journal.State == MigrationStateAdopting {
		return LegacyRetirementResult{AuthoritiesStopped: true}, nil
	}
	if journal.State != MigrationStateLegacyAbsenceVerified {
		return LegacyRetirementResult{}, fmt.Errorf(
			"migration %q cannot accept operator-stopped authority from %q", migrationID, journal.State,
		)
	}
	for _, candidateID := range journal.Candidates {
		candidate, _, readErr := engine.state.readMigrationCandidate(ctx, migrationID, candidateID)
		if readErr != nil {
			return LegacyRetirementResult{}, readErr
		}
		switch candidate.Classification {
		case LegacyClassificationStale, LegacyClassificationRetired, LegacyClassificationExcluded:
		case LegacyClassificationActiveManagedBlocker, LegacyClassificationLiveLegacyAuthority,
			LegacyClassificationUnknown, LegacyClassificationConflicting:
			return LegacyRetirementResult{}, fmt.Errorf(
				"candidate %q is not proven operator-stopped", candidate.CandidateID,
			)
		default:
			return LegacyRetirementResult{}, fmt.Errorf(
				"candidate %q has unsupported maintenance classification %q",
				candidate.CandidateID, candidate.Classification,
			)
		}
		journal.VerifiedAbsentAuthorities = appendUnique(journal.VerifiedAbsentAuthorities, candidate.CandidateID)
	}
	journal.MaintenanceWindowState = MaintenanceWindowLegacyAbsenceVerified
	journal.State = MigrationStateAdopting
	if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
		return LegacyRetirementResult{}, err
	}
	if engine.crashPoint == MigrationStateAdopting {
		return LegacyRetirementResult{}, ErrInjectedMigrationCrash
	}
	return LegacyRetirementResult{AuthoritiesStopped: true}, nil
}

// RetireArtifacts retires exact endpoints only after the caller has durably
// committed the successor authority and both commit gates validate.
func (engine *LegacyRetirementEngine) RetireArtifacts(
	ctx context.Context,
	migrationID string,
) (LegacyRetirementResult, error) {
	journal, revision, err := engine.state.ReadMigration(ctx, migrationID)
	if err != nil {
		return LegacyRetirementResult{}, err
	}
	return engine.continueRetirement(ctx, journal, revision)
}

// Recover resumes only the phase named by the durable journal. Already
// journaled stops and retirements are not dispatched twice.
func (engine *LegacyRetirementEngine) Recover(
	ctx context.Context,
	migrationID string,
) (LegacyRetirementResult, error) {
	journal, revision, err := engine.state.ReadMigration(ctx, migrationID)
	if err != nil {
		return LegacyRetirementResult{}, err
	}
	switch journal.State {
	case MigrationStateInventorying, MigrationStateBlockedActivePeerOrLane,
		MigrationStateBlockedLiveAuthority, MigrationStateRetryRequired:
		return LegacyRetirementResult{}, fmt.Errorf("migration %q has not passed quiescence", migrationID)
	case MigrationStateLegacyAbsenceVerified:
		return engine.AcceptVerifiedLegacyAbsence(ctx, migrationID)
	case MigrationStateAdopting:
		return LegacyRetirementResult{AuthoritiesStopped: true}, nil
	case MigrationStateAuthorityCommitted, MigrationStateRetiringLegacyArtifacts:
		return engine.continueRetirement(ctx, journal, revision)
	case MigrationStateComplete:
		return LegacyRetirementResult{AuthoritiesStopped: true, ArtifactsRetired: true, Complete: true}, nil
	case MigrationStateDebt, MigrationStateBlockedUnknownIdentity:
		return LegacyRetirementResult{Debt: engine.readJournalDebt(ctx, journal)}, ErrLegacyRetirementDebt
	default:
		return LegacyRetirementResult{}, fmt.Errorf("migration %q is not at a recoverable retirement phase %q", migrationID, journal.State)
	}
}

// RollbackBeforeReady exposes the exact reversible seam consumed by T084. It
// restores a safe precommit state; it does not integrate with installation or
// make a readiness decision itself.
func (engine *LegacyRetirementEngine) RollbackBeforeReady(
	ctx context.Context,
	migrationID string,
) (LegacyRollbackResult, error) {
	journal, revision, err := engine.state.ReadMigration(ctx, migrationID)
	if err != nil {
		return LegacyRollbackResult{}, err
	}
	if journal.State == MigrationStateComplete {
		return LegacyRollbackResult{}, errors.New("completed migration cannot silently restore split authorities")
	}
	if journal.RollbackCompleted && journal.State == MigrationStateRetryRequired {
		return LegacyRollbackResult{Restored: true}, nil
	}
	journal.State = MigrationStateRetryRequired
	journal.SuccessorStateDurable = false
	journal.MaintenanceWindowState = MaintenanceWindowUnverified
	journal.AuthorityGeneration = 0
	journal.VerifiedAbsentAuthorities = nil
	journal.RetiredCandidateIDs = nil
	journal.RollbackCompleted = true
	journal.RollbackCause = "maintenance_window_retry_required"
	if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
		return LegacyRollbackResult{}, err
	}
	return LegacyRollbackResult{Restored: true}, nil
}

// RollbackMaintenanceWindowBeforeReady is the only rollback entry point used
// by the unified installer. It stops only the unified successor and never
// invokes a legacy lifecycle operation.
func (engine *LegacyRetirementEngine) RollbackMaintenanceWindowBeforeReady(
	ctx context.Context,
	migrationID string,
) (LegacyRollbackResult, error) {
	return engine.RollbackBeforeReady(ctx, migrationID)
}

func (engine *LegacyRetirementEngine) loadOrCreateRetirement( //nolint:gocyclo // Validation keeps the one bounded journal/candidate creation boundary explicit.
	ctx context.Context,
	request LegacyRetirementRequest,
) (MigrationTransaction, statestore.Revision, error) {
	if !durableRecordID.MatchString(request.MigrationID) || strings.TrimSpace(request.TargetRuntimeIdentity) == "" {
		return MigrationTransaction{}, 0, errors.New("legacy retirement request has incomplete identity")
	}
	var prior *LegacyPriorAuthority
	if !reflectLegacyPriorAuthorityEmpty(request.PriorAuthority) {
		if err := request.PriorAuthority.Validate(); err != nil {
			return MigrationTransaction{}, 0, err
		}
		value := request.PriorAuthority
		prior = &value
	}
	if len(request.Candidates) == 0 {
		return MigrationTransaction{}, 0, errors.New("legacy retirement request has no classified candidates")
	}
	fromVersions := slices.Clone(request.FromVersions)
	if len(fromVersions) == 0 {
		fromVersions = []string{"legacy-split-runtime"}
	}
	candidateIDs := make([]string, 0, len(request.Candidates))
	seen := make(map[string]struct{}, len(request.Candidates))
	for _, candidate := range request.Candidates {
		if err := candidate.Validate(); err != nil {
			return MigrationTransaction{}, 0, err
		}
		if _, duplicate := seen[candidate.CandidateID]; duplicate {
			return MigrationTransaction{}, 0, fmt.Errorf("legacy retirement repeats candidate %q", candidate.CandidateID)
		}
		seen[candidate.CandidateID] = struct{}{}
		candidateIDs = append(candidateIDs, candidate.CandidateID)
		if err := engine.state.putMigrationCandidate(ctx, request.MigrationID, candidate); err != nil {
			return MigrationTransaction{}, 0, err
		}
	}
	now := engine.timestamp(0)
	journal := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: request.MigrationID,
		FromVersions: fromVersions, TargetRuntimeIdentity: request.TargetRuntimeIdentity,
		State: MigrationStateLegacyAbsenceVerified, Candidates: candidateIDs, PriorAuthority: prior,
		MaintenanceWindowState:    MaintenanceWindowLegacyAbsenceVerified,
		VerifiedAbsentAuthorities: slices.Clone(candidateIDs),
		Revision:                  1, StartedAt: now, UpdatedAt: now,
	}
	revision, err := engine.state.CompareAndSwapMigration(ctx, request.ExpectedRevision, journal)
	if err == nil {
		return journal, revision, nil
	}
	if !errors.Is(err, statestore.ErrRevisionConflict) {
		return MigrationTransaction{}, 0, err
	}
	existing, existingRevision, readErr := engine.state.ReadMigration(ctx, request.MigrationID)
	if readErr != nil {
		return MigrationTransaction{}, 0, readErr
	}
	if existing.TargetRuntimeIdentity != request.TargetRuntimeIdentity || !reflect.DeepEqual(existing.Candidates, candidateIDs) ||
		!reflect.DeepEqual(existing.PriorAuthority, prior) {
		return MigrationTransaction{}, 0, statestore.ErrRevisionConflict
	}
	return existing, existingRevision, nil
}

func (engine *LegacyRetirementEngine) continueRetirement( //nolint:gocyclo // Exact endpoint re-attestation, debt, retirement, and journal order stay together.
	ctx context.Context,
	journal MigrationTransaction,
	revision statestore.Revision,
) (LegacyRetirementResult, error) {
	if journal.State == MigrationStateAuthorityCommitted {
		journal.State = MigrationStateRetiringLegacyArtifacts
		if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
			return LegacyRetirementResult{}, err
		}
	}
	if journal.State != MigrationStateRetiringLegacyArtifacts {
		return LegacyRetirementResult{}, fmt.Errorf("migration %q cannot retire artifacts from %q", journal.MigrationID, journal.State)
	}
	if !journal.SuccessorStateDurable ||
		journal.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified || journal.AuthorityGeneration == 0 {
		return LegacyRetirementResult{}, errors.New("legacy artifact retirement requires committed successor authority")
	}
	for _, candidateID := range journal.Candidates {
		if slices.Contains(journal.RetiredCandidateIDs, candidateID) {
			continue
		}
		candidate, _, err := engine.state.readMigrationCandidate(ctx, journal.MigrationID, candidateID)
		if err != nil {
			return LegacyRetirementResult{}, err
		}
		if retirementCandidateExcluded(candidate) {
			continue
		}
		// Terminal and archived rows are revision-bound provenance already copied
		// into the adopted projection. They are intentionally retained as source
		// evidence and are never treated as disposable authority artifacts.
		if candidate.Classification == LegacyClassificationRetired {
			continue
		}
		if !slices.Contains(journal.VerifiedAbsentAuthorities, candidate.CandidateID) {
			return LegacyRetirementResult{}, fmt.Errorf("legacy candidate %q was not durably verified absent", candidate.CandidateID)
		}
		observed, err := engine.lifecycle.ReattestEndpoint(ctx, candidate)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return LegacyRetirementResult{}, err
			}
			return engine.retirementPhaseDebt(ctx, &journal, &revision, candidate, "unobservable_endpoint_identity", err)
		}
		if legacyRetirementArtifactsProvenAbsent(candidate, observed) {
			// An absent socket/service is not proof that the same authority's
			// journaled record, start lock, or other disposable lifecycle artifact
			// is absent. Let the platform lifecycle re-attest and retire that exact
			// closed set before recording completion; implementations remain
			// idempotent when every artifact is already gone.
			if err := engine.lifecycle.RetireEndpoint(ctx, candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
				return engine.retirementPhaseDebt(ctx, &journal, &revision, candidate, "ambiguous_endpoint_retirement", err)
			}
			journal.RetiredCandidateIDs = append(journal.RetiredCandidateIDs, candidate.CandidateID)
			if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
				return LegacyRetirementResult{}, err
			}
			continue
		}
		if !legacyRetirementArtifactObservationAffirmative(candidate, observed) {
			return engine.retirementPhaseDebt(ctx, &journal, &revision, candidate, "unobservable_endpoint_identity", nil)
		}
		if !sameLegacyRetirementArtifactAuthority(candidate, observed) {
			return engine.retirementPhaseDebt(ctx, &journal, &revision, candidate, "changed_endpoint_identity", nil)
		}
		if err := engine.lifecycle.RetireEndpoint(ctx, candidate); err != nil {
			return engine.retirementPhaseDebt(ctx, &journal, &revision, candidate, "ambiguous_endpoint_retirement", err)
		}
		journal.RetiredCandidateIDs = append(journal.RetiredCandidateIDs, candidate.CandidateID)
		if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
			return LegacyRetirementResult{}, err
		}
	}
	journal.State = MigrationStateComplete
	journal.CompletedAt = engine.timestamp(journal.UpdatedAt)
	if err := engine.saveJournal(ctx, &journal, &revision); err != nil {
		return LegacyRetirementResult{}, err
	}
	return LegacyRetirementResult{AuthoritiesStopped: true, ArtifactsRetired: true, Complete: true}, nil
}

func (engine *LegacyRetirementEngine) saveJournal(
	ctx context.Context,
	journal *MigrationTransaction,
	revision *statestore.Revision,
) error {
	journal.Revision++
	journal.UpdatedAt = engine.timestamp(journal.UpdatedAt)
	next, err := engine.state.CompareAndSwapMigration(ctx, *revision, *journal)
	if err != nil {
		return err
	}
	*revision = next
	return nil
}

func (engine *LegacyRetirementEngine) saveDebtAndJournal(
	ctx context.Context,
	debt LegacyMigrationDebt,
	journal *MigrationTransaction,
	revision *statestore.Revision,
) error {
	if err := engine.state.compareAndSwapMigrationDebt(ctx, 0, debt); err != nil &&
		!errors.Is(err, statestore.ErrRevisionConflict) {
		return err
	}
	return engine.saveJournal(ctx, journal, revision)
}

func (engine *LegacyRetirementEngine) retirementPhaseDebt(
	ctx context.Context,
	journal *MigrationTransaction,
	revision *statestore.Revision,
	candidate LegacyRuntimeCandidate,
	code string,
	cause error,
) (LegacyRetirementResult, error) {
	debt := retirementDebt(candidate, code, engine.timestamp(journal.UpdatedAt))
	journal.State = MigrationStateDebt
	journal.CleanupDebtIDs = appendUnique(journal.CleanupDebtIDs, debt.DebtID)
	if err := engine.saveDebtAndJournal(ctx, debt, journal, revision); err != nil {
		return LegacyRetirementResult{}, err
	}
	return LegacyRetirementResult{AuthoritiesStopped: true, Debt: []LegacyMigrationDebt{debt}},
		legacyRetirementDebtError(candidate, code, cause)
}

func legacyRetirementDebtError(candidate LegacyRuntimeCandidate, code string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: candidate %q requires %s", ErrLegacyRetirementDebt, candidate.CandidateID, code)
	}
	return fmt.Errorf("%w: candidate %q requires %s: %w", ErrLegacyRetirementDebt, candidate.CandidateID, code, cause)
}

func (engine *LegacyRetirementEngine) readJournalDebt(
	ctx context.Context,
	journal MigrationTransaction,
) []LegacyMigrationDebt {
	result := make([]LegacyMigrationDebt, 0, len(journal.CleanupDebtIDs))
	for _, debtID := range journal.CleanupDebtIDs {
		debt, err := engine.state.readMigrationDebt(ctx, debtID)
		if err == nil {
			result = append(result, debt)
		}
	}
	return result
}

func (engine *LegacyRetirementEngine) timestamp(after int64) int64 {
	now := engine.now().UnixMilli()
	if now <= after {
		return after + 1
	}
	return now
}

func retirementCandidateExcluded(candidate LegacyRuntimeCandidate) bool {
	return legacyQuiescenceExcluded(candidate)
}

func sameLegacyProcessAuthority(expected, observed LegacyRuntimeCandidate) bool {
	return legacyProcessObservationAffirmative(expected, observed) &&
		expected.CandidateID == observed.CandidateID && expected.Kind == observed.Kind &&
		expected.RuntimeIdentity == observed.RuntimeIdentity && expected.PID == observed.PID &&
		expected.ProcStart == observed.ProcStart && expected.StrongStart == observed.StrongStart &&
		expected.ProcessExecutable == observed.ProcessExecutable && expected.ProcessArgvRole == observed.ProcessArgvRole &&
		slices.Equal(expected.ProcessArguments, observed.ProcessArguments) &&
		maps.Equal(expected.ProcessEnvironment, observed.ProcessEnvironment) &&
		expected.ServiceManager == observed.ServiceManager && expected.ServiceUnit == observed.ServiceUnit &&
		expected.ServiceExecutable == observed.ServiceExecutable && expected.ServiceArgvRole == observed.ServiceArgvRole
}

func legacyProcessObservationAffirmative(expected, observed LegacyRuntimeCandidate) bool {
	if expected.ServiceManager != "" {
		return observed.ServiceStatus == "loaded" && (expected.PID <= 1 || observed.ProcessStatus == "known")
	}
	return observed.ProcessStatus == "known"
}

func legacyProcessProvenAbsent(expected, observed LegacyRuntimeCandidate) bool {
	return observed.ProcessStatus == "absent" && expected.CandidateID == observed.CandidateID &&
		expected.Kind == observed.Kind && expected.RuntimeIdentity == observed.RuntimeIdentity &&
		expected.PID == observed.PID && expected.ProcStart == observed.ProcStart &&
		expected.StrongStart == observed.StrongStart
}

func sameLegacyEndpointAuthority(expected, observed LegacyRuntimeCandidate) bool {
	return legacyEndpointObservationAffirmative(expected, observed) &&
		expected.CandidateID == observed.CandidateID && expected.EndpointPath == observed.EndpointPath &&
		expected.EndpointType == observed.EndpointType && expected.EndpointOwnerUID == observed.EndpointOwnerUID &&
		expected.EndpointOwnerPID == observed.EndpointOwnerPID && expected.EndpointOwnerStart == observed.EndpointOwnerStart &&
		expected.EndpointRuntimeIdentity == observed.EndpointRuntimeIdentity
}

func legacyEndpointObservationAffirmative(expected, observed LegacyRuntimeCandidate) bool {
	if expected.EndpointPath == "" {
		return expected.ServiceManager != "" && observed.ServiceStatus == "loaded"
	}
	return observed.EndpointStatus == "responsive"
}

func legacyEndpointProvenAbsent(expected, observed LegacyRuntimeCandidate) bool {
	return observed.EndpointStatus == "absent" && expected.CandidateID == observed.CandidateID &&
		expected.EndpointPath != "" && expected.EndpointPath == observed.EndpointPath
}

func legacyRetirementArtifactsProvenAbsent(expected, observed LegacyRuntimeCandidate) bool {
	if expected.ServiceManager != "" {
		return legacyEndpointProvenAbsent(expected, observed) && observed.ServiceStatus == "absent"
	}
	if expected.EndpointPath == "" {
		return observed.ProcessStatus == "absent" && observed.EndpointStatus == "absent"
	}
	return legacyEndpointProvenAbsent(expected, observed)
}

func legacyRetirementArtifactObservationAffirmative(expected, observed LegacyRuntimeCandidate) bool {
	if expected.ServiceManager != "" {
		return observed.ServiceStatus == "loaded" &&
			(legacyEndpointProvenAbsent(expected, observed) || legacyEndpointObservationAffirmative(expected, observed))
	}
	return legacyEndpointObservationAffirmative(expected, observed)
}

func sameLegacyRetirementArtifactAuthority(expected, observed LegacyRuntimeCandidate) bool {
	if expected.ServiceManager == "" {
		return sameLegacyEndpointAuthority(expected, observed)
	}
	return sameLegacyCandidateStaticAuthority(expected, observed) && observed.ServiceStatus == "loaded" &&
		(legacyEndpointProvenAbsent(expected, observed) || sameLegacyEndpointAuthority(expected, observed))
}

func sameLegacyCandidateStaticAuthority(expected, observed LegacyRuntimeCandidate) bool { //nolint:gocyclo // Every static authority field must match exactly.
	return expected.CandidateID == observed.CandidateID && expected.Kind == observed.Kind &&
		expected.SourcePath == observed.SourcePath && expected.SourceRevision == observed.SourceRevision &&
		expected.ArtifactRevision == observed.ArtifactRevision &&
		expected.ArtifactIdentity == observed.ArtifactIdentity &&
		expected.ReportedVersion == observed.ReportedVersion && expected.RuntimeIdentity == observed.RuntimeIdentity &&
		expected.ProcessExecutable == observed.ProcessExecutable && expected.ProcessArgvRole == observed.ProcessArgvRole &&
		slices.Equal(expected.ProcessArguments, observed.ProcessArguments) &&
		maps.Equal(expected.ProcessEnvironment, observed.ProcessEnvironment) &&
		expected.EndpointPath == observed.EndpointPath && expected.EndpointType == observed.EndpointType &&
		expected.EndpointOwnerUID == observed.EndpointOwnerUID &&
		expected.EndpointRuntimeIdentity == observed.EndpointRuntimeIdentity &&
		expected.ServiceManager == observed.ServiceManager && expected.ServiceUnit == observed.ServiceUnit &&
		expected.ServiceExecutable == observed.ServiceExecutable && expected.ServiceArgvRole == observed.ServiceArgvRole &&
		expected.Classification == observed.Classification && expected.EvidenceRevision == observed.EvidenceRevision
}

func retirementDebt(candidate LegacyRuntimeCandidate, code string, at int64) LegacyMigrationDebt {
	return LegacyMigrationDebt{
		SchemaVersion: MigrationSchemaVersion, Revision: 1,
		DebtID:      quiescenceRecordID("legacy-retirement-debt", candidate.CandidateID, code),
		CandidateID: candidate.CandidateID, Code: code, Retryable: true,
		ExpectedIdentity: legacyRetirementDebtExpectedIdentity(candidate),
		ObservedIdentity: code, RetryPredicate: legacyRetirementDebtRetryPredicate,
		ProhibitedScope:  "stop_or_retire:" + candidate.CandidateID,
		EvidenceRevision: candidate.EvidenceRevision, CreatedAt: at, UpdatedAt: at,
	}
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

// ReadMigration reads and validates one exact migration transaction.
func (store *StateStore) ReadMigration(
	ctx context.Context,
	migrationID string,
) (MigrationTransaction, statestore.Revision, error) {
	if !durableRecordID.MatchString(migrationID) {
		return MigrationTransaction{}, 0, errors.New("migration id is invalid")
	}
	var record MigrationTransaction
	revision, err := store.records.Read(ctx, "migration/"+migrationID, &record)
	if err == nil {
		err = record.Validate()
	}
	if err == nil {
		err = store.validateMigrationAuthorityBlockers(ctx, record)
	}
	return record, revision, err
}

// CompareAndSwapMigration durably commits one validated migration revision.
func (store *StateStore) CompareAndSwapMigration(
	ctx context.Context,
	expected statestore.Revision,
	record MigrationTransaction,
) (statestore.Revision, error) {
	if err := record.Validate(); err != nil {
		return 0, err
	}
	if err := store.validateMigrationAuthorityBlockers(ctx, record); err != nil {
		return 0, err
	}
	return store.records.CompareAndSwap(ctx, "migration/"+record.MigrationID, expected, record)
}

func (store *StateStore) validateMigrationAuthorityBlockers(
	ctx context.Context,
	record MigrationTransaction,
) error {
	for _, blocker := range record.LiveAuthorityBlockers {
		candidate, _, err := store.readMigrationCandidate(ctx, record.MigrationID, blocker.CandidateID)
		if err != nil {
			return fmt.Errorf("read live-authority blocker candidate %q: %w", blocker.CandidateID, err)
		}
		if blocker.Kind != candidate.Kind || blocker.EvidenceRevision != candidate.EvidenceRevision ||
			blocker.LastObservedAt != candidate.LastObservedAt {
			return fmt.Errorf("live-authority blocker %q does not match exact candidate evidence", blocker.BlockerID)
		}
	}
	return nil
}

func (store *StateStore) putMigrationCandidate(
	ctx context.Context,
	migrationID string,
	candidate LegacyRuntimeCandidate,
) error {
	key := "migration/" + migrationID + "/candidates/" + candidate.CandidateID
	if _, err := store.records.CompareAndSwap(ctx, key, 0, candidate); err == nil {
		return nil
	} else if !errors.Is(err, statestore.ErrRevisionConflict) {
		return err
	}
	var existing LegacyRuntimeCandidate
	if _, err := store.records.Read(ctx, key, &existing); err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, candidate) {
		return statestore.ErrRevisionConflict
	}
	return nil
}

func (store *StateStore) readMigrationCandidate(
	ctx context.Context,
	migrationID, candidateID string,
) (LegacyRuntimeCandidate, statestore.Revision, error) {
	var candidate LegacyRuntimeCandidate
	revision, err := store.records.Read(ctx, "migration/"+migrationID+"/candidates/"+candidateID, &candidate)
	if err == nil {
		err = candidate.Validate()
	}
	return candidate, revision, err
}

func (store *StateStore) compareAndSwapMigrationDebt(
	ctx context.Context,
	expected statestore.Revision,
	debt LegacyMigrationDebt,
) error {
	if err := debt.Validate(); err != nil {
		return err
	}
	_, err := store.records.CompareAndSwap(ctx, "migration-debt/"+debt.DebtID, expected, debt)
	return err
}

func (store *StateStore) readMigrationDebt(
	ctx context.Context,
	debtID string,
) (LegacyMigrationDebt, error) {
	var debt LegacyMigrationDebt
	_, err := store.records.Read(ctx, "migration-debt/"+debtID, &debt)
	if err == nil {
		err = debt.Validate()
	}
	return debt, err
}
