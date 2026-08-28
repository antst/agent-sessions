package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRetirementNeverStopsAndRetiresExactOperatorStoppedArtifacts(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := retirementTestCandidate("supervisor", 5101)
	federationAgent := retirementTestCandidate("host_federation_agent", 5102)
	lifecycle := newRecordingLegacyRetirementLifecycle(supervisor, federationAgent)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: lifecycle,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
		MigrationID: "migration-exact-stop", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate",
		Candidates:            []LegacyRuntimeCandidate{supervisor, federationAgent},
		PriorAuthority:        retirementTestPriorAuthority(supervisor),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AuthoritiesStopped || result.ArtifactsRetired || result.Complete || result.Ready || len(result.Debt) != 0 {
		t.Fatalf("stop result = %+v, want stopped authorities without pre-commit endpoint retirement", result)
	}

	if len(lifecycle.calls) != 0 {
		t.Fatalf("maintenance-window acceptance signalled legacy authority: %q", lifecycle.calls)
	}
	commitRetirementAuthority(t, state, "migration-exact-stop")
	lifecycle.calls = nil
	result, err = engine.RetireArtifacts(ctx, "migration-exact-stop")
	if err != nil || !result.Complete || !result.ArtifactsRetired || result.Ready {
		t.Fatalf("retire result = %+v, %v", result, err)
	}
	wantCalls := []string{
		"reattest-endpoint:candidate-supervisor", "retire-endpoint:candidate-supervisor",
		"reattest-endpoint:candidate-host_federation_agent", "retire-endpoint:candidate-host_federation_agent",
	}
	if !reflect.DeepEqual(lifecycle.calls, wantCalls) {
		t.Fatalf("legacy endpoint retirement calls = %q, want %q", lifecycle.calls, wantCalls)
	}
	for _, candidate := range []LegacyRuntimeCandidate{supervisor, federationAgent} {
		assertRetirementLifecycleSawExactCandidate(t, lifecycle, candidate)
	}

	journal, _, err := state.ReadMigration(ctx, "migration-exact-stop")
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != MigrationStateComplete ||
		!reflect.DeepEqual(journal.VerifiedAbsentAuthorities, []string{supervisor.CandidateID, federationAgent.CandidateID}) ||
		!reflect.DeepEqual(journal.RetiredCandidateIDs, []string{supervisor.CandidateID, federationAgent.CandidateID}) {
		t.Fatalf("durable exact retirement journal = %+v", journal)
	}
}

func TestLegacyRetirementPreservesExcludedAndUnrelatedProcesses(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target := retirementTestCandidate("supervisor", 5201)
	vendor := retirementTestCandidate("native_codex", 5202)
	vendor.Classification = "excluded"
	unrelated := retirementTestCandidate("unrelated_user_process", 5203)
	unrelated.Classification = "excluded"
	lifecycle := newRecordingLegacyRetirementLifecycle(target, vendor, unrelated)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
		MigrationID: "migration-preserve-unrelated", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate",
		Candidates:            []LegacyRuntimeCandidate{target, vendor, unrelated},
		PriorAuthority:        retirementTestPriorAuthority(target),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AuthoritiesStopped || result.Complete {
		t.Fatalf("legacy stop did not stop only exact authorities: %+v", result)
	}
	commitRetirementAuthority(t, state, "migration-preserve-unrelated")
	if _, err := engine.RetireArtifacts(ctx, "migration-preserve-unrelated"); err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []LegacyRuntimeCandidate{vendor, unrelated} {
		for _, call := range lifecycle.calls {
			if strings.HasSuffix(call, ":"+excluded.CandidateID) {
				t.Fatalf("excluded or unrelated process was selected for mutation: %q", call)
			}
		}
		if lifecycle.retireCount[excluded.CandidateID] != 0 {
			t.Fatalf("excluded candidate was mutated: candidate=%+v lifecycle=%+v", excluded, lifecycle)
		}
	}
}

func TestLegacyRetirementChangedEndpointBecomesDebtWithoutUnlink(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target := retirementTestCandidate("qwen_host", 5301)
	lifecycle := newRecordingLegacyRetirementLifecycle(target)
	changed := lifecycle.endpointObserved[target.CandidateID]
	changed.EndpointStatus = "responsive"
	changed.EndpointOwnerPID++
	lifecycle.endpointObserved[target.CandidateID] = changed
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
		MigrationID: "migration-changed-endpoint", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate", Candidates: []LegacyRuntimeCandidate{target},
		PriorAuthority: retirementTestPriorAuthority(target),
	})
	if err != nil || !result.AuthoritiesStopped {
		t.Fatalf("stop before changed-endpoint discriminator = %+v, %v", result, err)
	}
	commitRetirementAuthority(t, state, "migration-changed-endpoint")
	result, err = engine.RetireArtifacts(ctx, "migration-changed-endpoint")
	if !errors.Is(err, ErrLegacyRetirementDebt) {
		t.Fatalf("changed endpoint error = %v, want ErrLegacyRetirementDebt", err)
	}
	if result.Complete || result.Ready || len(result.Debt) != 1 ||
		result.Debt[0].CandidateID != target.CandidateID || result.Debt[0].Code != "changed_endpoint_identity" ||
		!result.Debt[0].Retryable {
		t.Fatalf("changed endpoint result = %+v", result)
	}
	if lifecycle.retireCount[target.CandidateID] != 0 {
		t.Fatalf("changed endpoint was unlinked: lifecycle=%+v", lifecycle)
	}
	journal, _, readErr := state.ReadMigration(ctx, "migration-changed-endpoint")
	if readErr != nil || journal.State != MigrationStateDebt ||
		!reflect.DeepEqual(journal.RetiredCandidateIDs, []string(nil)) {
		t.Fatalf("changed endpoint journal = %+v, %v", journal, readErr)
	}
}

func TestLegacyRetirementNeverConsultsOrSignalsProcessLifecycle(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("supervisor", 5700)
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
		MigrationID: "migration-no-process-actions", TargetRuntimeIdentity: "sha256:unified-candidate",
		Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
	})
	if err != nil || !result.AuthoritiesStopped {
		t.Fatalf("verified-absence acceptance = %+v, %v", result, err)
	}
	for _, call := range lifecycle.calls {
		if strings.Contains(call, "process") || strings.Contains(call, "stop") || strings.Contains(call, "exit") {
			t.Fatalf("installer consulted legacy process lifecycle: %q", lifecycle.calls)
		}
	}
}

func TestLegacyRetirementEndpointAmbiguityAlwaysBecomesDebtWithoutCleanup(t *testing.T) {
	tests := []struct {
		name, wantCode string
		configure      func(*recordingLegacyRetirementLifecycle, LegacyRuntimeCandidate)
	}{
		{name: "unknown status with copied identity", wantCode: "unobservable_endpoint_identity", configure: func(lifecycle *recordingLegacyRetirementLifecycle, candidate LegacyRuntimeCandidate) {
			observed := lifecycle.endpointObserved[candidate.CandidateID]
			observed.EndpointStatus = "unknown"
			lifecycle.endpointObserved[candidate.CandidateID] = observed
		}},
		{name: "reattest error", wantCode: "unobservable_endpoint_identity", configure: func(lifecycle *recordingLegacyRetirementLifecycle, candidate LegacyRuntimeCandidate) {
			lifecycle.endpointErr[candidate.CandidateID] = errors.New("endpoint observation unavailable")
		}},
		{name: "retirement ambiguity", wantCode: "ambiguous_endpoint_retirement", configure: func(lifecycle *recordingLegacyRetirementLifecycle, candidate LegacyRuntimeCandidate) {
			lifecycle.retireErr[candidate.CandidateID] = errors.New("endpoint retirement result unavailable")
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			state, err := OpenStateStore(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			candidate := retirementTestCandidate("qwen_host", 5800+index)
			lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
			engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
			if err != nil {
				t.Fatal(err)
			}
			migrationID := fmt.Sprintf("migration-endpoint-debt-%d", index)
			if _, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
				MigrationID: migrationID, TargetRuntimeIdentity: "sha256:unified-candidate",
				Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
			}); err != nil {
				t.Fatal(err)
			}
			commitRetirementAuthority(t, state, migrationID)
			test.configure(lifecycle, candidate)
			result, err := engine.RetireArtifacts(ctx, migrationID)
			if !errors.Is(err, ErrLegacyRetirementDebt) || len(result.Debt) != 1 ||
				result.Debt[0].Code != test.wantCode || !result.Debt[0].Retryable {
				t.Fatalf("endpoint ambiguity result = %+v, %v; want debt %q", result, err, test.wantCode)
			}
			journal, _, readErr := state.ReadMigration(ctx, migrationID)
			if readErr != nil || journal.State != MigrationStateDebt || len(journal.CleanupDebtIDs) != 1 ||
				len(journal.RetiredCandidateIDs) != 0 {
				t.Fatalf("endpoint ambiguity journal = %+v, %v", journal, readErr)
			}
			if test.wantCode == "unobservable_endpoint_identity" && lifecycle.retireCount[candidate.CandidateID] != 0 {
				t.Fatalf("unknown endpoint observation authorized cleanup: %+v", lifecycle.retireCount)
			}
		})
	}
}

func TestLegacyRetirementTreatsNotExistAsIdempotentOnlyAfterExactAbsence(t *testing.T) {
	for _, test := range []struct {
		name         string
		proveAbsent  bool
		wantComplete bool
	}{
		{name: "proven absent is idempotent", proveAbsent: true, wantComplete: true},
		{name: "responsive disappearance remains debt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			state, err := OpenStateStore(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			candidate := retirementTestCandidate("supervisor", 5850)
			lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
			if test.proveAbsent {
				observed := lifecycle.endpointObserved[candidate.CandidateID]
				observed.EndpointStatus = "absent"
				lifecycle.endpointObserved[candidate.CandidateID] = observed
			} else {
				observed := lifecycle.endpointObserved[candidate.CandidateID]
				observed.EndpointStatus = "responsive"
				lifecycle.endpointObserved[candidate.CandidateID] = observed
			}
			lifecycle.retireErr[candidate.CandidateID] = &os.PathError{
				Op: "remove", Path: candidate.EndpointPath, Err: os.ErrNotExist,
			}
			engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
			if err != nil {
				t.Fatal(err)
			}
			migrationID := "migration-idempotent-absence-" + fmt.Sprintf("%t", test.proveAbsent)
			if _, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, LegacyRetirementRequest{
				MigrationID: migrationID, TargetRuntimeIdentity: "sha256:unified-candidate",
				Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
			}); err != nil {
				t.Fatal(err)
			}
			commitRetirementAuthority(t, state, migrationID)
			result, retireErr := engine.RetireArtifacts(ctx, migrationID)
			if test.wantComplete {
				if retireErr != nil || !result.Complete || !result.ArtifactsRetired || len(result.Debt) != 0 {
					t.Fatalf("proven-absent retirement = %+v, %v", result, retireErr)
				}
				journal, _, readErr := state.ReadMigration(ctx, migrationID)
				if readErr != nil || !reflect.DeepEqual(journal.RetiredCandidateIDs, []string{candidate.CandidateID}) {
					t.Fatalf("proven-absent journal = %+v, %v", journal, readErr)
				}
				return
			}
			if !errors.Is(retireErr, ErrLegacyRetirementDebt) || result.Complete || len(result.Debt) != 1 ||
				result.Debt[0].Code != "ambiguous_endpoint_retirement" {
				t.Fatalf("unproven disappearance = %+v, %v", result, retireErr)
			}
		})
	}
}

func TestLegacyRetirementCrashJournalRecoversWithoutAnyLegacyStop(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first := retirementTestCandidate("supervisor", 5401)
	second := retirementTestCandidate("grok_lane_manager", 5402)
	lifecycle := newRecordingLegacyRetirementLifecycle(first, second)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(MigrationStateAdopting)

	request := LegacyRetirementRequest{
		MigrationID: "migration-crash-recovery", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate",
		Candidates:            []LegacyRuntimeCandidate{first, second},
		PriorAuthority:        retirementTestPriorAuthority(first),
	}
	if _, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, request); !errors.Is(err, ErrInjectedMigrationCrash) {
		t.Fatalf("retirement crash = %v, want ErrInjectedMigrationCrash", err)
	}
	journal, revision, err := state.ReadMigration(ctx, request.MigrationID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != MigrationStateAdopting ||
		!reflect.DeepEqual(journal.VerifiedAbsentAuthorities, []string{first.CandidateID, second.CandidateID}) ||
		len(journal.RetiredCandidateIDs) != 0 || revision == 0 {
		t.Fatalf("crash journal = %+v revision=%d", journal, revision)
	}

	recovered, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovered.Recover(ctx, request.MigrationID)
	if err != nil || !result.AuthoritiesStopped || result.Complete || result.ArtifactsRetired {
		t.Fatalf("recover retirement = %+v, %v", result, err)
	}
	if lifecycle.retireCount[first.CandidateID] != 0 || lifecycle.retireCount[second.CandidateID] != 0 {
		t.Fatalf("pre-commit recovery retired endpoints: %+v", lifecycle.retireCount)
	}
	journal, _, err = state.ReadMigration(ctx, request.MigrationID)
	if err != nil || journal.State != MigrationStateAdopting {
		t.Fatalf("recovered journal = %+v, %v", journal, err)
	}
	commitRetirementAuthority(t, state, request.MigrationID)
	result, err = recovered.Recover(ctx, request.MigrationID)
	if err != nil || !result.Complete || !result.ArtifactsRetired {
		t.Fatalf("post-commit recovery = %+v, %v", result, err)
	}
	if lifecycle.retireCount[first.CandidateID] != 1 || lifecycle.retireCount[second.CandidateID] != 1 {
		t.Fatalf("post-commit recovery endpoint counts = %+v", lifecycle.retireCount)
	}
}

func TestLegacyRollbackBeforeReadyLeavesOperatorStoppedAuthorityStopped(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	priorCandidate := retirementTestCandidate("supervisor", 5501)
	prior := retirementTestPriorAuthority(priorCandidate)
	lifecycle := newRecordingLegacyRetirementLifecycle(priorCandidate)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(MigrationStateAdopting)
	request := LegacyRetirementRequest{
		MigrationID: "migration-prior-rollback", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate", Candidates: []LegacyRuntimeCandidate{priorCandidate},
		PriorAuthority: prior,
	}
	if _, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, request); !errors.Is(err, ErrInjectedMigrationCrash) {
		t.Fatalf("prepare rollback journal = %v", err)
	}
	lifecycle.calls = nil
	result, err := engine.RollbackBeforeReady(ctx, request.MigrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Restored || len(result.Debt) != 0 {
		t.Fatalf("rollback result = %+v", result)
	}
	if len(lifecycle.calls) != 0 {
		t.Fatalf("rollback calls = %q, want none", lifecycle.calls)
	}
	journal, _, err := state.ReadMigration(ctx, request.MigrationID)
	if err != nil || journal.State != MigrationStateRetryRequired ||
		journal.MaintenanceWindowState != MaintenanceWindowUnverified ||
		journal.PriorAuthority.JournalRevision != prior.JournalRevision {
		t.Fatalf("rollback journal = %+v, %v", journal, err)
	}
}

func TestLegacyCombinedHostProcessEndpointAndServiceAreOneLifecycleCandidate(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("host_federation_agent", 5551)
	candidate.SourcePath = "/home/test/.config/systemd/user/peer-federator-agent.service"
	candidate.SourceRevision = "sha256:service-definition"
	candidate.ServiceManager = "systemd-user"
	candidate.ServiceUnit = "peer-federator-agent.service"
	candidate.ServiceStatus = "loaded"
	candidate.ServiceExecutable = candidate.ProcessExecutable
	candidate.ServiceArgvRole = "agent"
	lifecycle := newRecordingLegacyRetirementLifecycle(candidate)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	request := LegacyRetirementRequest{
		MigrationID: "migration-combined-host-authority", TargetRuntimeIdentity: "sha256:unified-candidate",
		Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
	}
	stopped, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, request)
	if err != nil || !stopped.AuthoritiesStopped {
		t.Fatalf("combined absence acceptance = %+v, err=%v", stopped, err)
	}
	commitRetirementAuthority(t, state, request.MigrationID)
	retired, err := engine.RetireArtifacts(ctx, request.MigrationID)
	if err != nil || !retired.Complete || lifecycle.retireCount[candidate.CandidateID] != 1 {
		t.Fatalf("combined authority retirement = %+v, count=%d, err=%v", retired, lifecycle.retireCount[candidate.CandidateID], err)
	}
	journal, _, err := state.ReadMigration(ctx, request.MigrationID)
	if err != nil || len(journal.Candidates) != 1 || len(journal.VerifiedAbsentAuthorities) != 1 ||
		len(journal.RetiredCandidateIDs) != 1 {
		t.Fatalf("combined authority journal = %+v, %v", journal, err)
	}
}

func TestMaintenanceRollbackNeverRestartsPriorMetadata(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	priorCandidate := retirementTestCandidate("supervisor", 5601)
	lifecycle := newRecordingLegacyRetirementLifecycle(priorCandidate)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(MigrationStateAdopting)
	request := LegacyRetirementRequest{
		MigrationID: "migration-changed-prior", ExpectedRevision: 0,
		TargetRuntimeIdentity: "sha256:unified-candidate", Candidates: []LegacyRuntimeCandidate{priorCandidate},
		PriorAuthority: retirementTestPriorAuthority(priorCandidate),
	}
	if _, err := engine.PrepareAndAcceptVerifiedLegacyAbsence(ctx, request); !errors.Is(err, ErrInjectedMigrationCrash) {
		t.Fatalf("prepare rollback journal = %v", err)
	}
	lifecycle.calls = nil

	result, err := engine.RollbackMaintenanceWindowBeforeReady(ctx, request.MigrationID)
	if err != nil || !result.Restored || len(result.Debt) != 0 {
		t.Fatalf("maintenance rollback = %+v, %v", result, err)
	}
	if len(lifecycle.calls) != 0 {
		t.Fatalf("maintenance rollback invoked lifecycle operations: calls=%q", lifecycle.calls)
	}
}

func retirementTestCandidate(kind string, _ int) LegacyRuntimeCandidate {
	return LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-" + kind,
		Kind: kind, Classification: LegacyClassificationStale,
		SourcePath: "/state/legacy/" + kind + ".json", RuntimeIdentity: "sha256:legacy-" + kind,
		ProcessStatus: "absent", EndpointPath: "/runtime/legacy/" + kind + ".sock",
		EndpointStatus: "absent", EndpointType: "unix", EndpointOwnerUID: 501,
		EndpointRuntimeIdentity: "sha256:legacy-" + kind, EvidenceRevision: 1, LastObservedAt: 1,
	}
}

func commitRetirementAuthority(t *testing.T, state *StateStore, migrationID string) {
	t.Helper()
	journal, revision, err := state.ReadMigration(context.Background(), migrationID)
	if err != nil {
		t.Fatal(err)
	}
	journal.State = MigrationStateAuthorityCommitted
	journal.SuccessorStateDurable = true
	journal.MaintenanceWindowState = MaintenanceWindowLegacyAbsenceVerified
	journal.AuthorityGeneration = 2
	journal.Revision++
	journal.UpdatedAt++
	if _, err := state.CompareAndSwapMigration(context.Background(), revision, journal); err != nil {
		t.Fatal(err)
	}
}

func retirementTestPriorAuthority(candidate LegacyRuntimeCandidate) LegacyPriorAuthority {
	return LegacyPriorAuthority{
		Candidate: candidate, JournalRevision: 17,
		ReleaseSelection: "/releases/legacy/current", StateSelection: "/state/legacy/current",
		ConnectorRevision: "connectors-revision-17", ServiceManager: "systemd-user",
		ServiceUnit: "peer-federator-agent.service",
	}
}

type recordingLegacyRetirementLifecycle struct {
	observed         map[string]LegacyRuntimeCandidate
	endpointObserved map[string]LegacyRuntimeCandidate
	processErr       map[string]error
	endpointErr      map[string]error
	retireErr        map[string]error
	calls            []string
	retireCount      map[string]int
	exactInputs      map[string][]LegacyRuntimeCandidate
}

func newRecordingLegacyRetirementLifecycle(candidates ...LegacyRuntimeCandidate) *recordingLegacyRetirementLifecycle {
	observed := make(map[string]LegacyRuntimeCandidate, len(candidates))
	endpointObserved := make(map[string]LegacyRuntimeCandidate, len(candidates))
	for _, candidate := range candidates {
		observed[candidate.CandidateID] = candidate
		endpointObserved[candidate.CandidateID] = candidate
	}
	return &recordingLegacyRetirementLifecycle{
		observed: observed, endpointObserved: endpointObserved,
		processErr:  make(map[string]error),
		endpointErr: make(map[string]error), retireErr: make(map[string]error),
		retireCount: make(map[string]int),
		exactInputs: make(map[string][]LegacyRuntimeCandidate),
	}
}

func (lifecycle *recordingLegacyRetirementLifecycle) record(operation string, candidate LegacyRuntimeCandidate) {
	lifecycle.calls = append(lifecycle.calls, operation+":"+candidate.CandidateID)
	lifecycle.exactInputs[operation] = append(lifecycle.exactInputs[operation], candidate)
}

func (lifecycle *recordingLegacyRetirementLifecycle) ReattestProcess(
	_ context.Context,
	candidate LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, error) {
	lifecycle.record("reattest-process", candidate)
	if err := lifecycle.processErr[candidate.CandidateID]; err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	observed, ok := lifecycle.observed[candidate.CandidateID]
	if !ok {
		return LegacyRuntimeCandidate{}, errors.New("candidate not observed")
	}
	return observed, nil
}

func (lifecycle *recordingLegacyRetirementLifecycle) ReattestEndpoint(
	_ context.Context,
	candidate LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, error) {
	lifecycle.record("reattest-endpoint", candidate)
	if err := lifecycle.endpointErr[candidate.CandidateID]; err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	return lifecycle.endpointObserved[candidate.CandidateID], nil
}

func (lifecycle *recordingLegacyRetirementLifecycle) RetireEndpoint(
	_ context.Context,
	candidate LegacyRuntimeCandidate,
) error {
	lifecycle.record("retire-endpoint", candidate)
	lifecycle.retireCount[candidate.CandidateID]++
	return lifecycle.retireErr[candidate.CandidateID]
}

func assertRetirementLifecycleSawExactCandidate(
	t *testing.T,
	lifecycle *recordingLegacyRetirementLifecycle,
	want LegacyRuntimeCandidate,
) {
	t.Helper()
	for _, operation := range []string{"reattest-endpoint", "retire-endpoint"} {
		inputs := lifecycle.exactInputs[operation]
		found := false
		for _, input := range inputs {
			if input.CandidateID == want.CandidateID {
				found = true
				if !reflect.DeepEqual(input, want) {
					t.Fatalf("%s input = %+v, want exact candidate %+v", operation, input, want)
				}
			}
		}
		if !found {
			t.Fatalf("%s did not receive candidate %q", operation, want.CandidateID)
		}
	}
}
