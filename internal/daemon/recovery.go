package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

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
