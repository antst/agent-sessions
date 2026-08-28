package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/releaseinstall"
)

func TestProductionLegacyMigrationDebtRetryFreshProcessPathAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name             string
		installFreshPath bool
		wantResolved     bool
	}{
		{name: "old process and path safely absent resolve", wantResolved: true},
		{name: "fresh process owns recorded path and remains blocked", installFreshPath: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateBlockedUnknownIdentity)
			candidate.PID = 1 << 30
			candidate.ProcStart = "absent-process-start"
			candidate.StrongStart = "absent-process-strong-start"
			candidate.EndpointOwnerPID = candidate.PID
			candidate.EndpointOwnerStart = candidate.ProcStart
			candidate.EndpointOwnerUID = os.Getuid()
			candidate.EndpointPath = filepath.Join(t.TempDir(), "legacy.sock")
			if err := replaceSelectedDebtCandidate(t, state, candidate, debt); err != nil {
				t.Fatal(err)
			}
			var listener net.Listener
			if test.installFreshPath {
				var err error
				listener, err = net.Listen("unix", candidate.EndpointPath)
				if err != nil {
					t.Fatal(err)
				}
				listener.(*net.UnixListener).SetUnlinkOnClose(false)
				defer func() { _ = listener.Close() }()
			}
			observer := &productionLegacyRetirementLifecycle{}
			result, err := RetrySelectedLegacyMigrationDebt(context.Background(), state, observer, 450)
			if err != nil {
				t.Fatal(err)
			}
			if result.Resolved != test.wantResolved {
				t.Fatalf("production fresh-process retry = %+v, want resolved=%t", result, test.wantResolved)
			}
			if !test.wantResolved && (len(result.Debt) != 1 ||
				!strings.Contains(result.Debt[0].ObservedIdentity, "process=absent;path=changed")) {
				t.Fatalf("fresh replacement process/path evidence = %+v", result)
			}
		})
	}
}

func TestOfflineMigrationInspectProjectsSelectedDebtWithoutRetryOrMutation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	runtimeRoot, err := os.MkdirTemp("/tmp", "as-migration-retry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	state, err := OpenStateStore(paths.StateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate, debt := seedSelectedMigrationDebtInState(t, state, MigrationStateBlockedUnknownIdentity)
	candidate.PID = 1 << 30
	candidate.ProcStart = "absent-process-start"
	candidate.StrongStart = "absent-process-strong-start"
	candidate.EndpointOwnerPID = candidate.PID
	candidate.EndpointOwnerStart = candidate.ProcStart
	candidate.EndpointOwnerUID = os.Getuid()
	candidate.EndpointPath = filepath.Join(root, "legacy-runtime", "absent.sock")
	if err := replaceSelectedDebtCandidate(t, state, candidate, debt); err != nil {
		t.Fatal(err)
	}
	selectedDebt, err := state.readMigrationDebt(context.Background(), debt.DebtID)
	if err != nil {
		t.Fatal(err)
	}
	selectedJournal, _, err := state.ReadMigration(context.Background(), "migration-debt-retry")
	if err != nil {
		t.Fatal(err)
	}

	projection, err := RunHostMigrationInspectCLI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Debt) != 1 || !reflect.DeepEqual(projection.Debt[0], selectedDebt) {
		t.Fatalf("read-only inspection debt = %+v, want durable selected debt %+v", projection, selectedDebt)
	}
	journal, _, err := state.ReadMigration(context.Background(), "migration-debt-retry")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(journal, selectedJournal) {
		t.Fatalf("offline inspection mutated selected debt authority: %+v", journal)
	}
	unchanged, err := state.readMigrationDebt(context.Background(), debt.DebtID)
	if err != nil || !reflect.DeepEqual(unchanged, selectedDebt) {
		t.Fatalf("offline inspection mutated durable debt: %+v, %v", unchanged, err)
	}
}

func TestProductionInstallPreflightRetriesSelectedDebtBeforeFreshInventory(t *testing.T) {
	root := t.TempDir()
	state, err := OpenStateStore(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := seedSelectedMigrationDebtInState(t, state, MigrationStateBlockedUnknownIdentity)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ProcessStatus = "absent"
	endpoint := candidate
	endpoint.EndpointStatus = "absent"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = endpoint
	inspectedFresh := false
	gate := &productionHostMigrationGate{
		stateRoot: root,
		retryDebt: func(ctx context.Context, selected *StateStore, observedAt int64) (LegacyMigrationDebtRetryResult, error) {
			return RetrySelectedLegacyMigrationDebt(ctx, selected, lifecycle, observedAt)
		},
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			inspectedFresh = true
			return FirstMigrationInspection{}, nil
		},
	}
	request := releaseinstall.InstallRequest{
		Version: "retry", ContentIdentity: "sha256:retry", SourceRoot: "/release", Executable: "agent-sessions",
	}
	if err := gate.Preflight(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !inspectedFresh || !gate.ready || gate.resume {
		t.Fatalf("resolved install debt did not authorize fresh preflight: %+v", gate)
	}
	journal, _, err := state.ReadMigration(context.Background(), "migration-debt-retry")
	if err != nil || !journal.FreshInventoryRequired {
		t.Fatalf("install debt retry journal = %+v, %v", journal, err)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestProductionInstallPreflightKeepsChangedDebtBlockedWithoutFreshInventory(t *testing.T) {
	root := t.TempDir()
	state, err := OpenStateStore(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate, debt := seedSelectedMigrationDebtInState(t, state, MigrationStateBlockedUnknownIdentity)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ProcessStatus = "absent"
	replacement := candidate
	replacement.EndpointOwnerPID++
	replacement.EndpointOwnerStart = "replacement-start"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = replacement
	inspectedFresh := false
	gate := &productionHostMigrationGate{
		stateRoot: root,
		retryDebt: func(ctx context.Context, selected *StateStore, observedAt int64) (LegacyMigrationDebtRetryResult, error) {
			return RetrySelectedLegacyMigrationDebt(ctx, selected, lifecycle, observedAt)
		},
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			inspectedFresh = true
			return FirstMigrationInspection{}, nil
		},
	}
	err = gate.Preflight(context.Background(), releaseinstall.InstallRequest{Executable: "agent-sessions"})
	if err == nil || !strings.Contains(err.Error(), string(MigrationStateBlockedUnknownIdentity)) {
		t.Fatalf("changed install debt preflight = %v", err)
	}
	if inspectedFresh || gate.ready {
		t.Fatal("changed install debt authorized fresh inventory or install mutation")
	}
	updated, err := state.readMigrationDebt(context.Background(), debt.DebtID)
	if err != nil || !strings.Contains(updated.ObservedIdentity, "path=changed;resolution=blocked") {
		t.Fatalf("changed install debt evidence = %+v, %v", updated, err)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestLegacyMigrationDebtRetryFreshProcessExitAndAbsentPathResolvesForFreshInventory(t *testing.T) {
	ctx := context.Background()
	state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateBlockedUnknownIdentity)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ProcessStatus = "absent"
	endpoint := candidate
	endpoint.EndpointStatus = "absent"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = endpoint

	result, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || !result.Resolved || !result.FreshInventoryRequired ||
		result.State != MigrationStateRetryRequired || len(result.Debt) != 0 {
		t.Fatalf("resolved process/path retry = %+v", result)
	}
	journal, _, err := state.ReadMigration(ctx, "migration-debt-retry")
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != MigrationStateRetryRequired || !journal.FreshInventoryRequired ||
		len(journal.CleanupDebtIDs) != 0 || journal.RollbackCompleted {
		t.Fatalf("resolved process/path journal = %+v", journal)
	}
	resolved, err := state.readMigrationDebt(ctx, debt.DebtID)
	if err != nil || resolved.ResolvedAt == 0 || resolved.Revision != debt.Revision+1 ||
		!strings.Contains(resolved.ObservedIdentity, "process=absent;path=absent;resolution=resolved") {
		t.Fatalf("resolved durable debt = %+v, %v", resolved, err)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestLegacyMigrationDebtRetryFreshProcessAtRecordedPathRemainsBlockedWithoutLifecycleMutation(t *testing.T) {
	ctx := context.Background()
	state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateBlockedUnknownIdentity)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ProcessStatus = "absent"
	freshProcess := candidate
	freshProcess.EndpointStatus = "responsive"
	freshProcess.EndpointOwnerPID = candidate.EndpointOwnerPID + 1000
	freshProcess.EndpointOwnerStart = "fresh-process-start"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = freshProcess

	result, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 600)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || result.Resolved || result.State != MigrationStateBlockedUnknownIdentity ||
		len(result.Debt) != 1 || result.Debt[0].ResolvedAt != 0 {
		t.Fatalf("changed process/path retry = %+v", result)
	}
	journal, _, err := state.ReadMigration(ctx, "migration-debt-retry")
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != MigrationStateBlockedUnknownIdentity || journal.FreshInventoryRequired ||
		!reflect.DeepEqual(journal.CleanupDebtIDs, []string{debt.DebtID}) {
		t.Fatalf("changed process/path journal = %+v", journal)
	}
	updated, err := state.readMigrationDebt(ctx, debt.DebtID)
	if err != nil || updated.Revision != debt.Revision+1 || updated.EvidenceRevision != debt.EvidenceRevision+1 ||
		updated.UpdatedAt <= debt.UpdatedAt || updated.ResolvedAt != 0 ||
		!strings.Contains(updated.ObservedIdentity, "process=absent;path=changed;resolution=blocked") {
		t.Fatalf("updated non-resolving debt = %+v, %v", updated, err)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestLegacyMigrationDebtRetryAbsentServiceWithUnobservedPIDRemainsBlocked(t *testing.T) {
	ctx := context.Background()
	state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateBlockedUnknownIdentity)
	candidate.ServiceManager = "systemd-user"
	candidate.ServiceUnit = "agent-sessions-legacy.service"
	candidate.ServiceStatus = "loaded"
	if err := replaceSelectedDebtCandidate(t, state, candidate, debt); err != nil {
		t.Fatal(err)
	}
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ServiceStatus = "absent"
	endpoint := candidate
	endpoint.ServiceStatus = "absent"
	endpoint.EndpointStatus = "absent"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = endpoint

	result, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 650)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved || len(result.Debt) != 1 ||
		!strings.Contains(result.Debt[0].ObservedIdentity, "process=changed;path=absent;resolution=blocked") {
		t.Fatalf("service-absent retry without PID reattestation = %+v", result)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestLegacyMigrationPostCommitDebtRetryResumesRetirementOnlyAfterExactAbsence(t *testing.T) {
	ctx := context.Background()
	state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateDebt)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	process := candidate
	process.ProcessStatus = "absent"
	endpoint := candidate
	endpoint.EndpointStatus = "absent"
	lifecycle.observed[candidate.CandidateID] = process
	lifecycle.endpointObserved[candidate.CandidateID] = endpoint

	result, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 700)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || result.FreshInventoryRequired || result.State != MigrationStateRetiringLegacyArtifacts {
		t.Fatalf("post-commit debt retry = %+v", result)
	}
	journal, _, err := state.ReadMigration(ctx, "migration-debt-retry")
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != MigrationStateRetiringLegacyArtifacts || !journal.SuccessorStateDurable ||
		journal.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified || journal.AuthorityGeneration != 9 ||
		!reflect.DeepEqual(journal.VerifiedAbsentAuthorities, []string{candidate.CandidateID}) ||
		len(journal.CleanupDebtIDs) != 0 {
		t.Fatalf("post-commit resolved journal = %+v", journal)
	}
	resolved, err := state.readMigrationDebt(ctx, debt.DebtID)
	if err != nil || resolved.ResolvedAt == 0 {
		t.Fatalf("post-commit resolved debt = %+v, %v", resolved, err)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestLegacyMigrationDebtRetryChangedDurablePredicateFailsClosedWithFreshEvidence(t *testing.T) {
	ctx := context.Background()
	state, candidate, debt := seedSelectedMigrationDebt(t, MigrationStateBlockedUnknownIdentity)
	debt.RetryPredicate = "operator_guessed_cleanup"
	if _, revision, err := state.readMigrationDebtRevision(ctx, debt.DebtID); err != nil {
		t.Fatal(err)
	} else if err := state.compareAndSwapMigrationDebt(ctx, revision, debt); err != nil {
		t.Fatal(err)
	}
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)

	result, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 800)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved || len(result.Debt) != 1 ||
		!strings.Contains(result.Debt[0].ObservedIdentity, "retry_contract=changed") {
		t.Fatalf("changed retry predicate result = %+v", result)
	}
	assertDebtRetryPerformedNoLifecycleMutation(t, lifecycle)
}

func TestProductionLegacyRecordDebtRetryRequiresExactRawArtifactOrAbsence(t *testing.T) {
	for _, resolution := range []string{"restore", "remove"} {
		t.Run(resolution, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			recordPath := filepath.Join(root, "legacy", "state.json")
			if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
				t.Fatal(err)
			}
			original := []byte("{\n  \"sessionId\": \"debt-session\",\n  \"prompt\": \"original\"\n}\n")
			if err := os.WriteFile(recordPath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			identity, present, err := observeProductionLegacyRegular(recordPath)
			if err != nil || !present {
				t.Fatalf("observe original record: present=%v err=%v", present, err)
			}
			candidate := LegacyRuntimeCandidate{
				SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-record-debt", Kind: "shim",
				SourcePath: recordPath, SourceRevision: "sha256:" + strings.Repeat("a", 64),
				ArtifactRevision: "sha256:" + productionDigest(original),
				ArtifactIdentity: productionLegacyArtifactIdentityRevision(identity),
				ProcessStatus:    "absent", EndpointStatus: "absent", RelatedSessionIDs: []string{"debt-session"},
				Classification: LegacyClassificationStale, EvidenceRevision: 1, LastObservedAt: 100,
			}
			debt := retirementDebt(candidate, "ambiguous_endpoint_retirement", 110)
			if err := state.putMigrationCandidate(ctx, "migration-record-debt", candidate); err != nil {
				t.Fatal(err)
			}
			if err := state.compareAndSwapMigrationDebt(ctx, 0, debt); err != nil {
				t.Fatal(err)
			}
			journal := MigrationTransaction{
				SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-record-debt",
				FromVersions: []string{"legacy"}, TargetRuntimeIdentity: "sha256:successor",
				State: MigrationStateDebt, Candidates: []string{candidate.CandidateID},
				CleanupDebtIDs: []string{debt.DebtID}, SuccessorStateDurable: true, AuthorityGeneration: 7,
				MaintenanceWindowState:    MaintenanceWindowLegacyAbsenceVerified,
				VerifiedAbsentAuthorities: []string{candidate.CandidateID}, Revision: 2, StartedAt: 90, UpdatedAt: 110,
			}
			if _, err := state.CompareAndSwapMigration(ctx, 0, journal); err != nil {
				t.Fatal(err)
			}
			if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
				SchemaVersion: MigrationSchemaVersion, MigrationID: journal.MigrationID,
			}); err != nil {
				t.Fatal(err)
			}
			lifecycle, err := newProductionLegacyRetirementLifecycle(ProductionPaths{
				StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"),
				ControlEndpoint: filepath.Join(root, "runtime", "agent-sessions.sock"),
			}, state)
			if err != nil {
				t.Fatal(err)
			}
			mutated := []byte(`{"sessionId":"debt-session","prompt":"mutated"}`)
			if err := os.WriteFile(recordPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			blocked, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 120)
			if err != nil || blocked.Resolved || len(blocked.Debt) != 1 ||
				!strings.Contains(blocked.Debt[0].ObservedIdentity, "path=changed") {
				t.Fatalf("mutated artifact retry = %+v, %v", blocked, err)
			}
			if resolution == "restore" {
				if err := os.WriteFile(recordPath, original, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(recordPath); err != nil {
				t.Fatal(err)
			}
			resolved, err := RetrySelectedLegacyMigrationDebt(ctx, state, lifecycle, 130)
			if err != nil || !resolved.Resolved || resolved.State != MigrationStateRetiringLegacyArtifacts {
				t.Fatalf("exact artifact retry = %+v, %v", resolved, err)
			}
			engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.Recover(ctx, journal.MigrationID)
			if err != nil || !result.Complete {
				t.Fatalf("resolved retirement = %+v, %v", result, err)
			}
			if _, err := os.Lstat(recordPath); !os.IsNotExist(err) {
				t.Fatalf("resolved record remained: %v", err)
			}
		})
	}
}

func seedSelectedMigrationDebt(
	t *testing.T,
	stateValue MigrationState,
) (*StateStore, LegacyRuntimeCandidate, LegacyMigrationDebt) {
	t.Helper()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate, debt := seedSelectedMigrationDebtInState(t, state, stateValue)
	return state, candidate, debt
}

func seedSelectedMigrationDebtInState(
	t *testing.T,
	state *StateStore,
	stateValue MigrationState,
) (LegacyRuntimeCandidate, LegacyMigrationDebt) {
	t.Helper()
	ctx := context.Background()
	candidate := debtRetryTestCandidate()
	debtCode := "unobservable_process_identity"
	if stateValue == MigrationStateDebt {
		debtCode = "unobservable_endpoint_identity"
	}
	debt := retirementDebt(candidate, debtCode, 100)
	if err := state.putMigrationCandidate(ctx, "migration-debt-retry", candidate); err != nil {
		t.Fatal(err)
	}
	if err := state.compareAndSwapMigrationDebt(ctx, 0, debt); err != nil {
		t.Fatal(err)
	}
	prior := retirementTestPriorAuthority(candidate)
	journal := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-debt-retry",
		FromVersions: []string{"legacy"}, TargetRuntimeIdentity: "sha256:successor",
		State: stateValue, Candidates: []string{candidate.CandidateID}, CleanupDebtIDs: []string{debt.DebtID},
		MaintenanceWindowState: MaintenanceWindowBlocked,
		PriorAuthority:         &prior, Revision: 2, StartedAt: 90, UpdatedAt: 110,
	}
	if stateValue == MigrationStateDebt {
		journal.SuccessorStateDurable = true
		journal.MaintenanceWindowState = MaintenanceWindowLegacyAbsenceVerified
		journal.AuthorityGeneration = 9
		journal.VerifiedAbsentAuthorities = []string{candidate.CandidateID}
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: journal.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}
	return candidate, debt
}

func debtRetryTestCandidate() LegacyRuntimeCandidate {
	return LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-supervisor", Kind: "supervisor",
		SourcePath: "/state/legacy/supervisor.json", RuntimeIdentity: "sha256:legacy-supervisor",
		PID: 7201, ProcStart: "proc-start-7201", StrongStart: "strong-start-7201", ProcessStatus: "known",
		ProcessExecutable: "/opt/agent-sessions/agent-session-runtime", ProcessArgvRole: "supervisor",
		EndpointPath: "/runtime/legacy/supervisor.sock", EndpointStatus: "responsive", EndpointType: "unix",
		EndpointOwnerUID: 501, EndpointOwnerPID: 7201, EndpointOwnerStart: "proc-start-7201",
		EndpointRuntimeIdentity: "sha256:legacy-supervisor", Classification: LegacyClassificationUnknown,
		EvidenceRevision: 1, LastObservedAt: 1,
	}
}

func replaceSelectedDebtCandidate(
	t *testing.T,
	state *StateStore,
	candidate LegacyRuntimeCandidate,
	priorDebt LegacyMigrationDebt,
) error {
	t.Helper()
	ctx := context.Background()
	journal, journalRevision, err := state.ReadMigration(ctx, "migration-debt-retry")
	if err != nil {
		return err
	}
	_, candidateRevision, err := state.readMigrationCandidate(ctx, journal.MigrationID, candidate.CandidateID)
	if err != nil {
		return err
	}
	if _, err := state.records.CompareAndSwap(
		ctx, "migration/"+journal.MigrationID+"/candidates/"+candidate.CandidateID, candidateRevision, candidate,
	); err != nil {
		return err
	}
	debt, debtRevision, err := state.readMigrationDebtRevision(ctx, priorDebt.DebtID)
	if err != nil {
		return err
	}
	debt.ExpectedIdentity = legacyRetirementDebtExpectedIdentity(candidate)
	debt.EvidenceRevision = candidate.EvidenceRevision
	if err := state.compareAndSwapMigrationDebt(ctx, debtRevision, debt); err != nil {
		return err
	}
	prior := retirementTestPriorAuthority(candidate)
	journal.PriorAuthority = &prior
	journal.Revision++
	journal.UpdatedAt++
	if _, err := state.CompareAndSwapMigration(ctx, journalRevision, journal); err != nil {
		return err
	}
	return nil
}

func assertDebtRetryPerformedNoLifecycleMutation(
	t *testing.T,
	lifecycle *recordingLegacyRetirementLifecycle,
) {
	t.Helper()
	for candidateID, count := range lifecycle.retireCount {
		if count != 0 {
			t.Fatalf("debt retry retired candidate %q %d time(s)", candidateID, count)
		}
	}
	for _, call := range lifecycle.calls {
		if !strings.HasPrefix(call, "reattest-process:") && !strings.HasPrefix(call, "reattest-endpoint:") {
			t.Fatalf("debt retry performed lifecycle mutation %q", call)
		}
	}
}
