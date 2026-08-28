package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/antst/agent-sessions/internal/statestore"
)

const legacyRetirementDebtRetryPredicate = "reinventory_and_reattest_exact_candidate"

// LegacyMigrationDebtObserver is the read-only subset of the legacy lifecycle
// used by an install-locked debt retry. Retry never calls a stop, retire,
// restart, or service mutation method.
type LegacyMigrationDebtObserver interface {
	// ReattestProcess reads the candidate's current process/service identity.
	ReattestProcess(context.Context, LegacyRuntimeCandidate) (LegacyRuntimeCandidate, error)
	// ReattestEndpoint reads the candidate's current path/service-artifact identity.
	ReattestEndpoint(context.Context, LegacyRuntimeCandidate) (LegacyRuntimeCandidate, error)
}

// LegacyMigrationDebtRetryResult reports whether one selected blocked journal
// was re-observed and whether its durable debt gate was resolved.
type LegacyMigrationDebtRetryResult struct {
	Attempted              bool                  `json:"attempted"`
	Resolved               bool                  `json:"resolved"`
	MigrationID            string                `json:"migration_id,omitempty"`
	State                  MigrationState        `json:"state,omitempty"`
	FreshInventoryRequired bool                  `json:"fresh_inventory_required,omitempty"`
	Debt                   []LegacyMigrationDebt `json:"debt"`
}

type legacyMigrationDebtRetryItem struct {
	record    LegacyMigrationDebt
	revision  statestore.Revision
	candidate LegacyRuntimeCandidate
}

// RetrySelectedLegacyMigrationDebt freshly observes only the debt and
// candidate records referenced by migration/current. Its caller must hold the
// host release-install lock. A failed or changed observation updates bounded
// evidence but performs no legacy lifecycle mutation. A safe pre-commit
// observation terminates stale migration authority and requires a full fresh
// inventory; a safe post-commit observation resumes exact artifact retirement.
func RetrySelectedLegacyMigrationDebt( //nolint:gocyclo // The fail-closed durable retry boundary is intentionally explicit.
	ctx context.Context,
	state *StateStore,
	observer LegacyMigrationDebtObserver,
	observedAt int64,
) (LegacyMigrationDebtRetryResult, error) {
	result := LegacyMigrationDebtRetryResult{Debt: []LegacyMigrationDebt{}}
	if state == nil || state.records == nil || observer == nil || observedAt <= 0 {
		return result, errors.New("legacy migration debt retry requires state, observer, and positive observation time")
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read selected migration for debt retry: %w", err)
	}
	journal, journalRevision, err := state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return result, fmt.Errorf("read selected migration journal for debt retry: %w", err)
	}
	result.MigrationID, result.State = journal.MigrationID, journal.State
	if journal.State != MigrationStateBlockedUnknownIdentity && journal.State != MigrationStateDebt {
		return result, nil
	}
	result.Attempted = true
	if len(journal.CleanupDebtIDs) == 0 {
		return result, errors.New("selected blocked migration has no referenced debt")
	}

	items, err := loadLegacyMigrationDebtRetryItems(ctx, state, journal)
	if err != nil {
		return result, err
	}
	allSafe := true
	latestObservation := journal.UpdatedAt
	for index := range items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		item := &items[index]
		process, processErr := observer.ReattestProcess(ctx, item.candidate)
		if errors.Is(processErr, context.Canceled) || errors.Is(processErr, context.DeadlineExceeded) {
			return result, processErr
		}
		endpoint, endpointErr := observer.ReattestEndpoint(ctx, item.candidate)
		if errors.Is(endpointErr, context.Canceled) || errors.Is(endpointErr, context.DeadlineExceeded) {
			return result, endpointErr
		}

		contractExact := legacyMigrationDebtRetryContractExact(journal.State, item.record, item.candidate)
		processState := legacyMigrationDebtProcessState(item.candidate, process, processErr)
		pathState := legacyMigrationDebtPathState(item.candidate, endpoint, endpointErr)
		safe := contractExact && legacyMigrationDebtObservationSafe(journal.State, processState, pathState)
		allSafe = allSafe && safe

		item.record.Revision++
		item.record.EvidenceRevision++
		item.record.UpdatedAt = nextMigrationObservationTime(observedAt, item.record.UpdatedAt)
		if item.record.UpdatedAt > latestObservation {
			latestObservation = item.record.UpdatedAt
		}
		item.record.ObservedIdentity = legacyMigrationDebtObservationLabel(contractExact, processState, pathState, safe)
		if safe {
			item.record.ResolvedAt = item.record.UpdatedAt
		} else {
			item.record.ResolvedAt = 0
		}
		if err := state.compareAndSwapMigrationDebt(ctx, item.revision, item.record); err != nil {
			return result, fmt.Errorf("update migration debt %q after fresh observation: %w", item.record.DebtID, err)
		}
		result.Debt = append(result.Debt, item.record)
	}
	if !allSafe {
		return result, nil
	}

	journal.CleanupDebtIDs = nil
	journal.Revision++
	journal.UpdatedAt = nextMigrationObservationTime(observedAt, latestObservation)
	switch journal.State { //nolint:exhaustive // The selected-state guard above admits only these two debt states.
	case MigrationStateBlockedUnknownIdentity:
		journal.State = MigrationStateRetryRequired
		journal.FreshInventoryRequired = true
		journal.SuccessorStateDurable = false
		journal.MaintenanceWindowState = MaintenanceWindowUnverified
		journal.AuthorityGeneration = 0
		journal.VerifiedAbsentAuthorities = nil
		journal.RetiredCandidateIDs = nil
		journal.RollbackCompleted = false
		journal.RollbackCause = "migration_debt_resolved_for_fresh_inventory"
	case MigrationStateDebt:
		journal.State = MigrationStateRetiringLegacyArtifacts
		journal.FreshInventoryRequired = false
	}
	if _, err := state.CompareAndSwapMigration(ctx, journalRevision, journal); err != nil {
		return result, fmt.Errorf("commit resolved migration debt transition: %w", err)
	}
	result.Resolved = true
	result.State = journal.State
	result.FreshInventoryRequired = journal.FreshInventoryRequired
	result.Debt = []LegacyMigrationDebt{}
	return result, nil
}

func loadLegacyMigrationDebtRetryItems(
	ctx context.Context,
	state *StateStore,
	journal MigrationTransaction,
) ([]legacyMigrationDebtRetryItem, error) {
	items := make([]legacyMigrationDebtRetryItem, 0, len(journal.CleanupDebtIDs))
	for _, debtID := range journal.CleanupDebtIDs {
		debt, revision, err := state.readMigrationDebtRevision(ctx, debtID)
		if err != nil {
			return nil, fmt.Errorf("read selected migration debt %q: %w", debtID, err)
		}
		if !slices.Contains(journal.Candidates, debt.CandidateID) {
			return nil, fmt.Errorf("selected migration debt %q references an unjournaled candidate", debtID)
		}
		candidate, _, err := state.readMigrationCandidate(ctx, journal.MigrationID, debt.CandidateID)
		if err != nil {
			return nil, fmt.Errorf("read debt candidate %q: %w", debt.CandidateID, err)
		}
		items = append(items, legacyMigrationDebtRetryItem{record: debt, revision: revision, candidate: candidate})
	}
	return items, nil
}

func (store *StateStore) readMigrationDebtRevision(
	ctx context.Context,
	debtID string,
) (LegacyMigrationDebt, statestore.Revision, error) {
	var debt LegacyMigrationDebt
	revision, err := store.records.Read(ctx, "migration-debt/"+debtID, &debt)
	if err == nil {
		err = debt.Validate()
	}
	return debt, revision, err
}

func legacyMigrationDebtRetryContractExact(
	state MigrationState,
	debt LegacyMigrationDebt,
	candidate LegacyRuntimeCandidate,
) bool {
	if debt.RetryPredicate != legacyRetirementDebtRetryPredicate ||
		debt.ProhibitedScope != "stop_or_retire:"+candidate.CandidateID ||
		debt.ExpectedIdentity != legacyRetirementDebtExpectedIdentity(candidate) ||
		debt.EvidenceRevision < candidate.EvidenceRevision {
		return false
	}
	if state == MigrationStateBlockedUnknownIdentity {
		switch debt.Code {
		case "unobservable_process_identity", "changed_process_identity", "ambiguous_supported_stop",
			"unverified_process_exit", "changed_prior_authority", "changed_restarted_authority":
			return true
		default:
			return false
		}
	}
	if state == MigrationStateDebt {
		switch debt.Code {
		case "unobservable_endpoint_identity", "changed_endpoint_identity", "ambiguous_endpoint_retirement":
			return true
		default:
			return false
		}
	}
	return false
}

func legacyMigrationDebtProcessState(
	expected, observed LegacyRuntimeCandidate,
	observeErr error,
) string {
	if observeErr != nil {
		return "unknown"
	}
	if expected.ServiceManager != "" && expected.PID <= 1 && observed.ServiceStatus == "absent" ||
		legacyProcessProvenAbsent(expected, observed) {
		return "absent"
	}
	if sameLegacyProcessAuthority(expected, observed) {
		return "exact"
	}
	if observed.ProcessStatus == "known" || observed.ServiceStatus == "loaded" {
		return "changed"
	}
	return "unknown"
}

func legacyMigrationDebtPathState(
	expected, observed LegacyRuntimeCandidate,
	observeErr error,
) string {
	if observeErr != nil {
		return "unknown"
	}
	if expected.ArtifactRevision != "" || expected.ArtifactIdentity != "" {
		if observed.ArtifactRevision == "" && observed.ArtifactIdentity == "" {
			return "absent"
		}
		if observed.ArtifactRevision != expected.ArtifactRevision || observed.ArtifactIdentity != expected.ArtifactIdentity {
			return "changed"
		}
		return "exact"
	}
	if expected.ServiceManager != "" && expected.EndpointPath == "" && observed.ServiceStatus == "absent" {
		return "absent"
	}
	if legacyRetirementArtifactsProvenAbsent(expected, observed) {
		return "absent"
	}
	if sameLegacyRetirementArtifactAuthority(expected, observed) {
		return "exact"
	}
	if legacyRetirementArtifactObservationAffirmative(expected, observed) {
		return "changed"
	}
	return "unknown"
}

func legacyMigrationDebtObservationSafe(state MigrationState, processState, pathState string) bool {
	switch state { //nolint:exhaustive // Every non-debt state is deliberately unsafe at this bounded retry boundary.
	case MigrationStateBlockedUnknownIdentity:
		return processState == "absent" && pathState == "absent" ||
			processState == "exact" && pathState == "exact"
	case MigrationStateDebt:
		return processState == "absent" && (pathState == "absent" || pathState == "exact")
	default:
		return false
	}
}

func legacyMigrationDebtObservationLabel(contractExact bool, processState, pathState string, safe bool) string {
	contractState, resolution := "exact", "blocked"
	if !contractExact {
		contractState = "changed"
	}
	if safe {
		resolution = "resolved"
	}
	return strings.Join([]string{
		"retry_contract=" + contractState,
		"process=" + processState,
		"path=" + pathState,
		"resolution=" + resolution,
	}, ";")
}

func legacyRetirementDebtExpectedIdentity(candidate LegacyRuntimeCandidate) string {
	return candidate.RuntimeIdentity + "|" + candidate.EndpointPath
}

func nextMigrationObservationTime(observedAt, after int64) int64 {
	if observedAt <= after {
		return after + 1
	}
	return observedAt
}
