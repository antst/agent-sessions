package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/statestore"
)

const migrationCurrentRecordKey = "migration/current"

func preparedFirstMigrationRecordKey(migrationID string) string {
	return "migration/" + migrationID + "/prepared-adoption"
}

type preparedFirstMigration struct {
	SchemaVersion int                `json:"schema_version"`
	MigrationID   string             `json:"migration_id"`
	Adoption      LegacyAdoptionPlan `json:"adoption"`
}

// Validate rejects an incomplete durable prepared-adoption shard.
func (prepared preparedFirstMigration) Validate() error {
	if prepared.SchemaVersion != MigrationSchemaVersion || !durableRecordID.MatchString(prepared.MigrationID) {
		return errors.New("prepared first migration has an incompatible identity")
	}
	return validateLegacyAdoptionPlan(prepared.Adoption)
}

func (store *StateStore) savePreparedFirstMigration(
	ctx context.Context,
	migrationID string,
	plan LegacyAdoptionPlan,
) error {
	prepared := preparedFirstMigration{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID, Adoption: cloneLegacyAdoptionPlan(plan),
	}
	if err := prepared.Validate(); err != nil {
		return err
	}
	if _, err := store.records.CompareAndSwap(ctx, preparedFirstMigrationRecordKey(migrationID), 0, prepared); err == nil {
		return nil
	} else if !errors.Is(err, statestore.ErrRevisionConflict) {
		return err
	}
	current, readErr := store.readPreparedFirstMigration(ctx, migrationID)
	if readErr != nil {
		return readErr
	}
	if !reflect.DeepEqual(current.Adoption, plan) {
		return ErrMigrationAdoptionConflict
	}
	return nil
}

func (store *StateStore) readPreparedFirstMigration(
	ctx context.Context,
	migrationID string,
) (preparedFirstMigration, error) {
	var prepared preparedFirstMigration
	_, err := store.records.Read(ctx, preparedFirstMigrationRecordKey(migrationID), &prepared)
	if err == nil {
		err = prepared.Validate()
	}
	if err == nil && prepared.MigrationID != migrationID {
		err = errors.New("prepared first migration does not match its journal shard")
	}
	return prepared, err
}

func (store *StateStore) commitPreparedFirstMigration(ctx context.Context, migrationID string) error {
	prepared, err := store.readPreparedFirstMigration(ctx, migrationID)
	if err != nil {
		return err
	}
	_, err = CommitLegacyAdoption(ctx, store, migrationID, prepared.Adoption)
	return err
}

// rollbackPreparedFirstMigration durably tombstones only the exact adoption
// shard owned by a terminally rolled-back, pre-authority migration. Keeping a
// revisioned tombstone makes crash retries idempotent without making a stale
// plan visible or allowing the same migration ID to be reused.
func (store *StateStore) rollbackPreparedFirstMigration( //nolint:gocyclo // Exact binding, terminal-state, CAS, and retry checks stay at one rollback boundary.
	ctx context.Context,
	migrationID string,
	planRevision string,
) error {
	if store == nil || store.records == nil || !durableRecordID.MatchString(migrationID) ||
		strings.TrimSpace(planRevision) == "" {
		return errors.New("legacy adoption rollback requires exact migration and plan revisions")
	}
	prepared, err := store.readPreparedFirstMigration(ctx, migrationID)
	if err != nil {
		return err
	}
	if prepared.Adoption.Revision != planRevision {
		return ErrMigrationAdoptionConflict
	}
	journal, _, err := store.ReadMigration(ctx, migrationID)
	if err != nil {
		return err
	}
	if !journal.FreshInventoryRequired || !journal.RollbackCompleted ||
		journal.State != MigrationStateRetryRequired || journal.SuccessorStateDurable ||
		journal.AuthorityGeneration != 0 {
		return errors.New("legacy adoption rollback requires a terminal pre-authority journal")
	}
	var adoption committedLegacyAdoption
	revision, err := store.records.Read(ctx, legacyAdoptionRecordKey(migrationID), &adoption)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if adoption.SchemaVersion != MigrationSchemaVersion || adoption.MigrationID != migrationID ||
		adoption.Plan.Revision != planRevision {
		return ErrMigrationAdoptionConflict
	}
	if err := validateLegacyAdoptionPlan(adoption.Plan); err != nil {
		return err
	}
	if !reflect.DeepEqual(adoption.Plan, prepared.Adoption) {
		return ErrMigrationAdoptionConflict
	}
	if adoption.RolledBack {
		return nil
	}
	adoption.RolledBack = true
	_, err = store.records.CompareAndSwap(ctx, legacyAdoptionRecordKey(migrationID), revision, adoption)
	if !errors.Is(err, statestore.ErrRevisionConflict) {
		return err
	}
	var current committedLegacyAdoption
	_, readErr := store.records.Read(ctx, legacyAdoptionRecordKey(migrationID), &current)
	if readErr == nil && current.SchemaVersion == MigrationSchemaVersion &&
		current.MigrationID == migrationID && current.Plan.Revision == planRevision && current.RolledBack {
		return nil
	}
	return err
}

// rollbackSelectedPreparedFirstMigration binds the install rollback seam to
// the exact durable prepared-plan revision instead of accepting caller data.
func (store *StateStore) rollbackSelectedPreparedFirstMigration(ctx context.Context, migrationID string) error {
	prepared, err := store.readPreparedFirstMigration(ctx, migrationID)
	if err != nil {
		return err
	}
	return store.rollbackPreparedFirstMigration(ctx, migrationID, prepared.Adoption.Revision)
}

// MigrationCurrent is the sole durable selector for the active or most
// recently completed first-migration journal. Recovery and administration
// resolve this exact ID and never scan per-migration shards or guess newest.
type MigrationCurrent struct {
	SchemaVersion int    `json:"schema_version"`
	MigrationID   string `json:"migration_id"`
}

// Validate rejects an unsupported selector before it can authorize recovery.
func (current MigrationCurrent) Validate() error {
	if current.SchemaVersion != MigrationSchemaVersion || !durableRecordID.MatchString(current.MigrationID) {
		return errors.New("current migration selector has an incompatible identity")
	}
	return nil
}

// ReadCurrentMigration reads the authoritative selector. os.IsNotExist is
// preserved so status can distinguish a clean host from malformed state.
func (store *StateStore) ReadCurrentMigration(
	ctx context.Context,
) (MigrationCurrent, statestore.Revision, error) {
	if store == nil || store.records == nil {
		return MigrationCurrent{}, 0, errors.New("migration state store is unavailable")
	}
	var current MigrationCurrent
	revision, err := store.records.Read(ctx, migrationCurrentRecordKey, &current)
	if err == nil {
		err = current.Validate()
	}
	return current, revision, err
}

// SelectCurrentMigration CAS-selects one already-created, validated journal.
// Requiring the journal first lets install order selection before any legacy
// lifecycle mutation without permitting a dangling or scan-derived pointer.
func (store *StateStore) SelectCurrentMigration(
	ctx context.Context,
	expected statestore.Revision,
	current MigrationCurrent,
) (statestore.Revision, error) {
	if store == nil || store.records == nil {
		return 0, errors.New("migration state store is unavailable")
	}
	if err := current.Validate(); err != nil {
		return 0, err
	}
	if _, _, err := store.ReadMigration(ctx, current.MigrationID); err != nil {
		if os.IsNotExist(err) {
			return 0, errors.New("current migration selector requires an existing journal")
		}
		return 0, fmt.Errorf("validate selected migration journal: %w", err)
	}
	return store.records.CompareAndSwap(ctx, migrationCurrentRecordKey, expected, current)
}

// CommitFirstMigrationAuthority advances only the authoritative selected
// adopting journal after the candidate runtime has durably reported its exact
// identity and generation. Artifact retirement remains T083's next phase.
func CommitFirstMigrationAuthority( //nolint:gocyclo // Authority gates and idempotent committed states remain explicit.
	ctx context.Context,
	store *StateStore,
	migrationID string,
	targetRuntimeIdentity string,
	authorityGeneration uint64,
	observedAt int64,
) error {
	if store == nil || strings.TrimSpace(targetRuntimeIdentity) == "" || authorityGeneration == 0 || observedAt <= 0 {
		return errors.New("first migration authority commit has incomplete successor evidence")
	}
	current, _, err := store.ReadCurrentMigration(ctx)
	if err != nil {
		return err
	}
	if current.MigrationID != migrationID {
		return errors.New("first migration authority commit does not match the current selector")
	}
	journal, revision, err := store.ReadMigration(ctx, migrationID)
	if err != nil {
		return err
	}
	if journal.State == MigrationStateAuthorityCommitted ||
		journal.State == MigrationStateRetiringLegacyArtifacts ||
		journal.State == MigrationStateComplete || journal.State == MigrationStateDebt {
		if journal.TargetRuntimeIdentity != targetRuntimeIdentity ||
			journal.AuthorityGeneration != authorityGeneration || !journal.SuccessorStateDurable ||
			journal.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified {
			return errors.New("committed first migration authority conflicts with candidate evidence")
		}
		_, _, err := store.readCommittedLegacyAdoption(ctx, migrationID)
		return err
	}
	if journal.State != MigrationStateAdopting ||
		journal.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified {
		return fmt.Errorf("first migration cannot commit successor authority from %q", journal.State)
	}
	if journal.TargetRuntimeIdentity != targetRuntimeIdentity {
		return errors.New("first migration target runtime identity changed before authority commit")
	}
	if _, _, err := store.readCommittedLegacyAdoption(ctx, migrationID); err != nil {
		return fmt.Errorf("validate durable adopted successor state: %w", err)
	}
	journal.State = MigrationStateAuthorityCommitted
	journal.SuccessorStateDurable = true
	journal.AuthorityGeneration = authorityGeneration
	journal.Revision++
	if observedAt <= journal.UpdatedAt {
		observedAt = journal.UpdatedAt + 1
	}
	journal.UpdatedAt = observedAt
	_, err = store.CompareAndSwapMigration(ctx, revision, journal)
	return err
}

// FirstMigrationRecoveryOptions binds the selected migration journal to the
// exact retirement engine used by the candidate daemon before admission.
type FirstMigrationRecoveryOptions struct {
	State      *StateStore
	Retirement *LegacyRetirementEngine
}

// FirstMigrationRecovery serializes recovery of the sole selected journal so
// concurrent startup retries cannot redispatch a supported lifecycle action.
type FirstMigrationRecovery struct {
	mu         sync.Mutex
	state      *StateStore
	retirement *LegacyRetirementEngine
}

// NewFirstMigrationRecovery validates the pre-admission recovery composition.
func NewFirstMigrationRecovery(options FirstMigrationRecoveryOptions) (*FirstMigrationRecovery, error) {
	if options.State == nil || options.Retirement == nil {
		return nil, errors.New("first migration recovery requires state and retirement")
	}
	return &FirstMigrationRecovery{state: options.State, retirement: options.Retirement}, nil
}

// Recover resolves only migration/current. Once exact legacy stops and the
// aggregate adopted state are durable, it commits this runtime generation as
// successor authority and retires exact legacy artifacts before admission.
func (recovery *FirstMigrationRecovery) Recover( //nolint:gocyclo // Exact selected-state recovery remains auditable in one boundary.
	ctx context.Context,
	targetRuntimeIdentity string,
	authorityGeneration uint64,
	observedAt int64,
) error {
	if recovery == nil {
		return errors.New("first migration recovery is unavailable")
	}
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	current, _, err := recovery.state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	journal, _, err := recovery.state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	if journal.FreshInventoryRequired {
		// The release transaction restored the prior authority and terminated
		// this migration attempt. Only a new lock-held inspection may select a
		// successor journal; daemon recovery must not replay stale stops. Finish
		// an adoption tombstone if rollback crashed after its terminal journal CAS.
		return recovery.state.rollbackSelectedPreparedFirstMigration(ctx, current.MigrationID)
	}
	if journal.State == MigrationStateComplete {
		return nil
	}
	if journal.State == MigrationStateLegacyAbsenceVerified {
		if _, err := recovery.retirement.AcceptVerifiedLegacyAbsence(ctx, current.MigrationID); err != nil {
			return fmt.Errorf("accept operator-stopped legacy estate: %w", err)
		}
		journal, _, err = recovery.state.ReadMigration(ctx, current.MigrationID)
		if err != nil {
			return err
		}
	}
	if journal.State == MigrationStateRetryRequired {
		return nil
	}
	if journal.State == MigrationStateAdopting {
		if err := recovery.state.commitPreparedFirstMigration(ctx, current.MigrationID); err != nil {
			return fmt.Errorf("commit prepared successor adoption: %w", err)
		}
		if err := CommitFirstMigrationAuthority(
			ctx, recovery.state, current.MigrationID, targetRuntimeIdentity, authorityGeneration, observedAt,
		); err != nil {
			return fmt.Errorf("commit candidate migration authority: %w", err)
		}
	}
	if _, err := LoadAdoptedState(ctx, recovery.state); err != nil {
		return fmt.Errorf("validate durable adopted successor state before retirement: %w", err)
	}
	result, err := recovery.retirement.Recover(ctx, current.MigrationID)
	if err != nil {
		return fmt.Errorf("recover exact legacy artifact retirement: %w", err)
	}
	if !result.Complete || !result.ArtifactsRetired {
		return errors.New("selected first migration did not complete before daemon admission")
	}
	return nil
}

// RecoveryStep binds one component callback to the canonical recovery order.
// Runtime.Start, rather than a caller-provided slice, remains the ordering
// authority.
type RecoveryStep struct {
	Stage RecoveryStage
	Run   RecoveryHook
}

// ComposeRecoveryHooks validates component hooks against the canonical order.
func ComposeRecoveryHooks(steps ...RecoveryStep) (map[RecoveryStage]RecoveryHook, error) {
	order := make(map[RecoveryStage]int, len(orderedRecoveryStages))
	for index, stage := range orderedRecoveryStages {
		order[stage] = index
	}
	hooks := make(map[RecoveryStage]RecoveryHook, len(steps))
	last := -1
	for _, step := range steps {
		position, known := order[step.Stage]
		if !known || step.Run == nil {
			return nil, fmt.Errorf("invalid recovery step %q", step.Stage)
		}
		if _, duplicate := hooks[step.Stage]; duplicate {
			return nil, fmt.Errorf("duplicate recovery step %q", step.Stage)
		}
		if position <= last {
			return nil, errors.New("recovery steps are not in canonical order")
		}
		hooks[step.Stage] = step.Run
		last = position
	}
	return hooks, nil
}

// LifecycleDebtInput supplies bounded metadata for one exact unresolved action.
type LifecycleDebtInput struct {
	DebtID           string
	Operation        string
	ResourceKind     string
	ResourceIdentity string
	ExpectedRevision string
	ObservedRevision string
	CauseCode        string
	CauseDetail      string
	RetryPredicate   string
	ProhibitedScope  string
}

// RecordLifecycleDebt creates one exact retryable record. It confers no
// authority to mutate the named resource.
func (runtime *Runtime) RecordLifecycleDebt(ctx context.Context, input LifecycleDebtInput) (DebtRecord, error) {
	now := runtime.options.Now().UnixMilli()
	record := DebtRecord{
		RecordHeader: RecordHeader{
			SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: runtime.Generation(),
			CreatedAt: now, UpdatedAt: now,
		},
		DebtID: input.DebtID, Operation: input.Operation, ResourceKind: input.ResourceKind,
		ResourceIdentity: input.ResourceIdentity, ExpectedRevision: input.ExpectedRevision,
		ObservedRevision: input.ObservedRevision, CauseCode: input.CauseCode,
		CauseDetail: diagnostics.BoundedCauseDetail(input.CauseDetail), RetryPredicate: input.RetryPredicate,
		ProhibitedScope: input.ProhibitedScope,
	}
	if _, err := runtime.options.State.CompareAndSwapDebt(ctx, 0, record); err != nil {
		return DebtRecord{}, fmt.Errorf("record lifecycle debt %q: %w", input.DebtID, err)
	}
	return record, nil
}

// RetryLifecycleDebt always re-reads the exact record and commits through its
// observed revision. A failed observation updates bounded diagnostic metadata;
// it never widens the prohibited scope or guesses a new resource identity.
func (runtime *Runtime) RetryLifecycleDebt(ctx context.Context, debtID string, observe func(context.Context, DebtRecord) error) (DebtRecord, error) {
	if observe == nil {
		return DebtRecord{}, errors.New("lifecycle debt retry requires an observer")
	}
	record, revision, err := runtime.options.State.ReadDebt(ctx, debtID)
	if err != nil {
		return DebtRecord{}, err
	}
	if record.ResolvedAt != 0 {
		return record, nil
	}
	record.Revision++
	record.UpdatedAt = runtime.options.Now().UnixMilli()
	if observeErr := observe(ctx, record); observeErr != nil {
		record.CauseDetail = diagnostics.BoundedCauseDetail(observeErr.Error())
		if _, err := runtime.options.State.CompareAndSwapDebt(ctx, revision, record); err != nil {
			return DebtRecord{}, fmt.Errorf("update lifecycle debt %q: %w", debtID, err)
		}
		return record, observeErr
	}
	record.ResolvedAt = record.UpdatedAt
	if _, err := runtime.options.State.CompareAndSwapDebt(ctx, revision, record); err != nil {
		return DebtRecord{}, fmt.Errorf("resolve lifecycle debt %q: %w", debtID, err)
	}
	return record, nil
}

// RecoverDurableState is the first storage action used by recovery hooks. It
// removes only validated same-root temporary records.
func (runtime *Runtime) RecoverDurableState(ctx context.Context) error {
	return runtime.options.State.Recover(ctx)
}
