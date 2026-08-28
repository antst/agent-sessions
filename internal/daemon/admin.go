package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/statestore"
)

var inspectProductionFirstMigration = InspectProductionFirstMigration

// QueryAdmin performs one correlated read-only administrative request against
// the already-running production daemon. It never starts service lifetime.
func QueryAdmin(ctx context.Context, operation string) (json.RawMessage, error) {
	if operation != "runtime.status" && operation != "runtime.doctor" &&
		operation != "migration.inspect" && operation != "migration.status" && operation != "remove.inspect" {
		return nil, fmt.Errorf("unsupported online administrative operation %q", operation)
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return nil, &UnavailableError{Cause: err, NextAction: daemonInspectionCommand()}
	}
	connection, err := DialControlEndpoint(ctx, paths.ControlEndpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	return queryAdminConnection(ctx, connection, operation)
}

//nolint:gocyclo // One bounded exchange validates every hello and correlated response invariant together.
func queryAdminConnection(ctx context.Context, connection net.Conn, operation string) (json.RawMessage, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	helloID := "admin-hello"
	if err := writeControlFrame(connection, controlHello{
		Type: "hello", Version: localControlProtocolVersion, RequestID: helloID, Role: controlRoleAdmin,
	}); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(connection, 64*1024)
	body, err := readBoundedControlFrame(reader)
	if err != nil {
		return nil, err
	}
	var hello struct {
		Type             string `json:"type"`
		Version          int    `json:"version"`
		RequestID        string `json:"request_id"`
		DaemonGeneration uint64 `json:"daemon_generation"`
		RuntimeVersion   string `json:"runtime_version"`
		Role             string `json:"role"`
	}
	if err := decodeStrictControlFrame(body, &hello); err != nil || hello.Type != "hello.result" || hello.RequestID != helloID || hello.DaemonGeneration == 0 {
		return nil, errors.New("daemon returned an invalid administrative hello")
	}
	requestID := "admin-request"
	if err := writeControlFrame(connection, controlRequest{
		Type: "request", Version: localControlProtocolVersion, RequestID: requestID,
		Operation: operation, ExpectedGeneration: hello.DaemonGeneration, Payload: json.RawMessage(`{}`),
	}); err != nil {
		return nil, err
	}
	body, err = readBoundedControlFrame(reader)
	if err != nil {
		return nil, err
	}
	var response controlResponse
	if err := decodeStrictControlFrame(body, &response); err != nil || response.Type != "response" || response.RequestID != requestID {
		return nil, errors.New("daemon returned an invalid administrative response")
	}
	if !response.Accepted {
		if response.Error == nil {
			return nil, errors.New("daemon rejected administration without a cause")
		}
		return nil, &AdministrativeError{
			Operation: operation, Code: response.Error.Code, Message: response.Error.Message,
			Retryable: response.Error.Retryable, NextAction: response.Error.NextAction,
		}
	}
	return append(json.RawMessage(nil), response.Result...), nil
}

// AdministrativeError preserves one daemon-classified, metadata-only admin
// refusal through the local client and stable public CLI envelope.
type AdministrativeError struct {
	Operation  string
	Code       string
	Message    string
	Retryable  bool
	NextAction string
}

func (failure *AdministrativeError) Error() string {
	if failure == nil {
		return "administrative operation failed"
	}
	return fmt.Sprintf("daemon rejected %s: %s", failure.Operation, failure.Message)
}

// ExitCode maps daemon metadata to the stable public semantic classes.
func (failure *AdministrativeError) ExitCode() int {
	if failure == nil {
		return 1
	}
	switch failure.Code {
	case "migration_blocked", "migration_state_unsafe":
		return 4
	case "migration_state_incompatible":
		return 5
	case "operation_unavailable":
		return 3
	}
	if failure.Retryable {
		return 6
	}
	return 1
}

func runtimeAdminDispatch(runtime *Runtime) func(context.Context, controlPrincipal, controlRequest) (controlDispatchResult, *controlError) {
	return func(ctx context.Context, _ controlPrincipal, request controlRequest) (controlDispatchResult, *controlError) {
		var result any
		var projectionErr error
		switch request.Operation {
		case "runtime.status":
			result = runtime.StatusProjection()
		case "runtime.doctor":
			result = runtime.DoctorProjection()
		case "migration.inspect":
			result, projectionErr = runtime.MigrationInspectProjection(ctx)
		case "migration.status":
			result, projectionErr = runtime.MigrationStatusProjection(ctx)
		case "remove.inspect":
			result = runtime.RemovalInspection()
		default:
			return controlDispatchResult{}, &controlError{Code: "operation_unavailable", Message: "administrative operation is not implemented", Retryable: false}
		}
		if projectionErr != nil {
			return controlDispatchResult{}, migrationAdminControlError(projectionErr)
		}
		body, err := json.Marshal(result)
		if err != nil {
			return controlDispatchResult{}, &controlError{Code: "internal", Message: "encode administrative result", Retryable: true}
		}
		return controlDispatchResult{Result: body}, nil
	}
}

// MigrationInspectProjection is the stable metadata-only legacy inventory
// result. Every candidate, blocker and debt entry is a validated durable
// record referenced by the authoritative current transaction.
type MigrationInspectProjection struct {
	Revision   uint64                   `json:"revision"`
	Candidates []LegacyRuntimeCandidate `json:"candidates"`
	Blockers   []LegacyMigrationBlocker `json:"blockers"`
	Debt       []LegacyMigrationDebt    `json:"debt"`
}

// MigrationStatusProjection is the stable current transaction and supported
// operator action. State uses only MigrationState values or the established
// no-transaction sentinel "none".
type MigrationStatusProjection struct {
	Transaction *MigrationTransaction `json:"transaction"`
	State       string                `json:"state"`
	NextAction  string                `json:"next_action"`
}

type migrationAdminSnapshot struct {
	transaction *MigrationTransaction
	candidates  []LegacyRuntimeCandidate
	debt        []LegacyMigrationDebt
}

// MigrationInspectProjection reads only the selected durable transaction; it
// never inventories roots, scans journals, repairs state, or starts services.
func (runtime *Runtime) MigrationInspectProjection(ctx context.Context) (MigrationInspectProjection, error) {
	snapshot, err := readMigrationAdminSnapshot(ctx, runtime.options.State)
	if err != nil {
		return MigrationInspectProjection{}, err
	}
	if snapshot.transaction == nil {
		return MigrationInspectProjection{
			Candidates: []LegacyRuntimeCandidate{}, Blockers: []LegacyMigrationBlocker{}, Debt: []LegacyMigrationDebt{},
		}, nil
	}
	return MigrationInspectProjection{
		Revision: snapshot.transaction.Revision, Candidates: snapshot.candidates,
		Blockers: migrationJournalBlockers(*snapshot.transaction),
		Debt:     snapshot.debt,
	}, nil
}

// MigrationInspectProjectionFromInspection projects the same production
// T080/T081 evidence consumed by install. A blocked quiescence result is a
// successful inspection result, not an administrative transport failure.
func MigrationInspectProjectionFromInspection(
	ctx context.Context,
	inspection FirstMigrationInspection,
) (MigrationInspectProjection, error) {
	if !inspection.Required {
		if !emptyFirstMigrationInspection(inspection) {
			return MigrationInspectProjection{}, errors.New("optional first migration inspection contains migration authority")
		}
		return MigrationInspectProjection{
			Candidates: []LegacyRuntimeCandidate{}, Blockers: []LegacyMigrationBlocker{}, Debt: []LegacyMigrationDebt{},
		}, nil
	}
	if !durableRecordID.MatchString(inspection.MigrationID) || len(inspection.Candidates) == 0 {
		return MigrationInspectProjection{}, errors.New("first migration inspection has incomplete exact identity")
	}
	report, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: inspection.Candidates})
	if err != nil && !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		return MigrationInspectProjection{}, err
	}
	return MigrationInspectProjection{
		Revision:   uint64(inspection.ExpectedRevision),
		Candidates: cloneLegacyRuntimeCandidatesForAdmin(inspection.Candidates),
		Blockers:   append([]LegacyMigrationBlocker(nil), report.Blockers...),
		Debt:       append([]LegacyMigrationDebt(nil), report.Debt...),
	}, nil
}

// RunHostMigrationInspectCLI executes the same bounded production inventory
// used by first-migration install preparation. A selected debt journal is
// projected from its durable records without re-observation or mutation;
// bounded debt retry runs only under the explicit install transaction lock.
func RunHostMigrationInspectCLI(ctx context.Context) (MigrationInspectProjection, error) {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", err)
	}
	state, err := OpenStateStoreExisting(paths.StateRoot, defaultMaximumStateRecordBytes)
	if err == nil {
		current, _, currentErr := state.ReadCurrentMigration(ctx)
		if currentErr == nil {
			transaction, _, transactionErr := state.ReadMigration(ctx, current.MigrationID)
			if transactionErr != nil {
				return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", transactionErr)
			}
			if transaction.State == MigrationStateBlockedUnknownIdentity || transaction.State == MigrationStateDebt {
				snapshot, snapshotErr := readMigrationAdminSnapshot(ctx, state)
				if snapshotErr != nil {
					return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", snapshotErr)
				}
				return migrationInspectProjectionFromAdminSnapshot(snapshot), nil
			}
		} else if !os.IsNotExist(currentErr) {
			return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", currentErr)
		}
	} else if !os.IsNotExist(err) {
		return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", err)
	}
	inspection, err := inspectProductionFirstMigration(ctx)
	if err != nil {
		return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", err)
	}
	projection, err := MigrationInspectProjectionFromInspection(ctx, inspection)
	if err != nil {
		return MigrationInspectProjection{}, administrativeMigrationError("migration.inspect", err)
	}
	return projection, nil
}

func migrationInspectProjectionFromAdminSnapshot(snapshot migrationAdminSnapshot) MigrationInspectProjection {
	if snapshot.transaction == nil {
		return MigrationInspectProjection{
			Candidates: []LegacyRuntimeCandidate{}, Blockers: []LegacyMigrationBlocker{}, Debt: []LegacyMigrationDebt{},
		}
	}
	return MigrationInspectProjection{
		Revision: snapshot.transaction.Revision, Candidates: snapshot.candidates,
		Blockers: migrationJournalBlockers(*snapshot.transaction),
		Debt:     snapshot.debt,
	}
}

func migrationJournalBlockers(transaction MigrationTransaction) []LegacyMigrationBlocker {
	blockers := make([]LegacyMigrationBlocker, 0,
		len(transaction.ActiveManagedBlockers)+len(transaction.LiveAuthorityBlockers))
	blockers = append(blockers, transaction.ActiveManagedBlockers...)
	blockers = append(blockers, transaction.LiveAuthorityBlockers...)
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].BlockerID < blockers[j].BlockerID })
	return blockers
}

// MigrationStatusProjection reports one selected journal and exact retry
// guidance without performing the retry or mutating any record.
func (runtime *Runtime) MigrationStatusProjection(ctx context.Context) (MigrationStatusProjection, error) {
	return migrationStatusProjection(ctx, runtime.options.State)
}

func migrationStatusProjection(ctx context.Context, state *StateStore) (MigrationStatusProjection, error) {
	snapshot, err := readMigrationAdminSnapshot(ctx, state)
	if err != nil {
		return MigrationStatusProjection{}, err
	}
	if snapshot.transaction == nil {
		return MigrationStatusProjection{State: "none", NextAction: "none"}, nil
	}
	transaction := snapshot.transaction.clone()
	return MigrationStatusProjection{
		Transaction: &transaction, State: string(transaction.State),
		NextAction: migrationNextAction(transaction, snapshot.debt),
	}, nil
}

// RunHostMigrationStatusCLI reads the exact current selector without opening
// a control socket or changing the state root. It therefore remains useful
// while first migration is blocked, failed, or pre-ready.
func RunHostMigrationStatusCLI(ctx context.Context) (MigrationStatusProjection, error) {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return MigrationStatusProjection{}, administrativeMigrationError("migration.status", err)
	}
	state, err := OpenStateStoreExisting(paths.StateRoot, defaultMaximumStateRecordBytes)
	if os.IsNotExist(err) {
		return MigrationStatusProjection{State: "none", NextAction: "none"}, nil
	}
	if err != nil {
		return MigrationStatusProjection{}, administrativeMigrationError("migration.status", err)
	}
	projection, err := migrationStatusProjection(ctx, state)
	if err != nil {
		return MigrationStatusProjection{}, administrativeMigrationError("migration.status", err)
	}
	return projection, nil
}

func readMigrationAdminSnapshot(ctx context.Context, state *StateStore) (migrationAdminSnapshot, error) {
	if state == nil || state.records == nil {
		return migrationAdminSnapshot{}, errors.New("migration state store is unavailable")
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if os.IsNotExist(err) {
		return migrationAdminSnapshot{}, nil
	} else if err != nil {
		return migrationAdminSnapshot{}, fmt.Errorf("read current migration selector: %w", err)
	}
	transaction, _, err := state.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return migrationAdminSnapshot{}, fmt.Errorf("read selected migration transaction: %w", err)
	}
	candidates := make([]LegacyRuntimeCandidate, 0, len(transaction.Candidates))
	for _, candidateID := range transaction.Candidates {
		candidate, _, readErr := state.readMigrationCandidate(ctx, transaction.MigrationID, candidateID)
		if readErr != nil {
			return migrationAdminSnapshot{}, fmt.Errorf("read selected migration candidate %q: %w", candidateID, readErr)
		}
		candidates = append(candidates, candidate)
	}
	debt := make([]LegacyMigrationDebt, 0, len(transaction.CleanupDebtIDs))
	for _, debtID := range transaction.CleanupDebtIDs {
		record, readErr := state.readMigrationDebt(ctx, debtID)
		if readErr != nil {
			return migrationAdminSnapshot{}, fmt.Errorf("read selected migration debt %q: %w", debtID, readErr)
		}
		debt = append(debt, record)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CandidateID < candidates[j].CandidateID })
	sort.Slice(debt, func(i, j int) bool { return debt[i].DebtID < debt[j].DebtID })
	return migrationAdminSnapshot{transaction: &transaction, candidates: candidates, debt: debt}, nil
}

func migrationNextAction(transaction MigrationTransaction, debt []LegacyMigrationDebt) string {
	if transaction.FreshInventoryRequired {
		return "retry the same supported install or upgrade command; it will run a fresh bounded legacy inventory before any lifecycle mutation"
	}
	switch transaction.State {
	case MigrationStateComplete:
		return "none"
	case MigrationStateBlockedActivePeerOrLane:
		parts := make([]string, 0, len(transaction.ActiveManagedBlockers))
		for _, blocker := range transaction.ActiveManagedBlockers {
			parts = append(parts, "close "+blocker.ResourceType+" "+blocker.ResourceID)
		}
		sort.Strings(parts)
		return strings.Join(parts, "; ") + "; then retry the same supported install or upgrade command"
	case MigrationStateBlockedLiveAuthority:
		parts := make([]string, 0, len(transaction.LiveAuthorityBlockers))
		for _, blocker := range transaction.LiveAuthorityBlockers {
			parts = append(parts, "stop authority "+blocker.ResourceID+" through its old supported lifecycle")
		}
		sort.Strings(parts)
		return strings.Join(parts, "; ") + "; keep every legacy launch path held, then retry the same supported install or upgrade command"
	case MigrationStateBlockedUnknownIdentity, MigrationStateDebt:
		if len(debt) != 0 {
			return "run agent-sessions migrate inspect --json to review the recorded debt without mutation; do not delete, replace, or signal the recorded PID or path manually; retry the same supported install or upgrade command, which will reobserve only the recorded process and path"
		}
		return "run agent-sessions migrate inspect --json and restore the referenced migration debt records"
	case MigrationStateInventorying, MigrationStateLegacyAbsenceVerified, MigrationStateAdopting,
		MigrationStateAuthorityCommitted, MigrationStateRetiringLegacyArtifacts, MigrationStateRetryRequired:
		return "retry the same supported install or upgrade command to resume the durable migration transaction"
	}
	return "migration state is unsupported; do not mutate legacy authority"
}

func migrationAdminControlError(err error) *controlError {
	if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file or directory") {
		return &controlError{
			Code: "migration_state_incomplete", Message: "selected migration records are incomplete", Retryable: true,
			NextAction: "retry the same supported install or upgrade command after restoring the exact selected records",
		}
	}
	if errors.Is(err, statestore.ErrCorruptRecord) || strings.Contains(err.Error(), "incompatible") ||
		strings.Contains(err.Error(), "unsupported schema") {
		return &controlError{
			Code: "migration_state_incompatible", Message: "selected migration state is incompatible", Retryable: false,
			NextAction: "install a compatible Agent Sessions release and inspect migration status again",
		}
	}
	if errors.Is(err, statestore.ErrUnsafeRecord) {
		return &controlError{
			Code: "migration_state_unsafe", Message: "selected migration state path or ownership is unsafe", Retryable: false,
			NextAction: "restore the owner-only canonical state path, then inspect migration status again",
		}
	}
	return &controlError{
		Code: "migration_state_unavailable", Message: "selected migration state is temporarily unavailable", Retryable: true,
		NextAction: "inspect daemon service logs and retry agent-sessions migrate status",
	}
}

func administrativeMigrationError(operation string, err error) error {
	failure := migrationAdminControlError(err)
	return &AdministrativeError{
		Operation: operation, Code: failure.Code, Message: failure.Message,
		Retryable: failure.Retryable, NextAction: failure.NextAction,
	}
}

func cloneLegacyRuntimeCandidatesForAdmin(source []LegacyRuntimeCandidate) []LegacyRuntimeCandidate {
	cloned := append([]LegacyRuntimeCandidate(nil), source...)
	for index := range cloned {
		cloned[index].RelatedSessionIDs = append([]string(nil), cloned[index].RelatedSessionIDs...)
		cloned[index].RelatedLaneIDs = append([]string(nil), cloned[index].RelatedLaneIDs...)
	}
	return cloned
}
