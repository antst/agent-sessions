package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

func TestFirstMigrationInstallAcceptsOperatorStoppedEstateAndAdoptsBeforeCandidate(t *testing.T) {
	ctx := context.Background()
	lifecycle, state, retirementOps, request, targetIdentity := newFirstMigrationTestLifecycle(t)
	if err := lifecycle.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if err != nil || current.MigrationID != "migration-install-first" {
		t.Fatalf("current migration = %+v, %v", current, err)
	}
	journal, _, err := state.ReadMigration(ctx, current.MigrationID)
	if err != nil || journal.State != MigrationStateAdopting ||
		journal.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified ||
		journal.TargetRuntimeIdentity != targetIdentity {
		t.Fatalf("prepared journal = %+v, %v", journal, err)
	}
	if retirementOps.retireCount["candidate-supervisor"] != 0 || len(retirementOps.calls) != 0 {
		t.Fatalf("pre-candidate preparation invoked lifecycle operations: %v", retirementOps.calls)
	}
	if _, err := LoadAdoptedState(ctx, state); !os.IsNotExist(err) {
		t.Fatalf("pre-ready adopted state became globally visible: %v", err)
	}
	if _, _, err := state.readCommittedLegacyAdoption(ctx, current.MigrationID); err != nil {
		t.Fatalf("migration-scoped adoption was not crash-durable before candidate start: %v", err)
	}
}

func TestProductionHostMigrationPreflightRejectsRacedBlockerBeforeStateCreation(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "unified-state")
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("a", 64),
		SourceRoot: "/release/source", Executable: "agent-sessions",
	}
	blocker := retirementTestCandidate("qwen_lane_manager", 4001)
	blocker.Classification = LegacyClassificationActiveManagedBlocker
	blocker.RelatedLaneIDs = []string{"raced-live-lane"}
	gate := &productionHostMigrationGate{
		stateRoot: stateRoot,
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			return FirstMigrationInspection{
				Required: true, MigrationID: "migration-raced-blocker",
				Candidates: []LegacyRuntimeCandidate{blocker},
			}, nil
		},
	}
	if err := gate.Preflight(context.Background(), request); !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		t.Fatalf("lock-held raced blocker preflight = %v", err)
	}
	if _, err := os.Lstat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("blocked lock-held preflight created unified state: %v", err)
	}
	if gate.ready {
		t.Fatal("blocked lock-held preflight retained mutation authority")
	}
}

func TestProductionHostMigrationPreflightBindsExactInstallRequest(t *testing.T) {
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("b", 64),
		SourceRoot: "/release/source", Executable: "agent-sessions",
	}
	gate := &productionHostMigrationGate{
		stateRoot: filepath.Join(t.TempDir(), "unified-state"),
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			return FirstMigrationInspection{}, nil
		},
	}
	if err := gate.Preflight(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	staged := request
	staged.SourceRoot = "/immutable/release"
	if err := gate.FinalInspect(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	if inspection, err := gate.Inspect(context.Background(), staged); err != nil || inspection.Required {
		t.Fatalf("exact staged request did not consume preflight: %+v, %v", inspection, err)
	}
	changed := staged
	changed.ContentIdentity = "sha256:" + strings.Repeat("c", 64)
	if _, err := gate.Inspect(context.Background(), changed); err == nil {
		t.Fatal("changed install request consumed a different lock-held preflight")
	}
}

func TestProductionHostMigrationFinalInspectionRejectsLateLegacyAuthorityWithoutUnifiedMutation(t *testing.T) {
	ctx := context.Background()
	stateRoot := filepath.Join(t.TempDir(), "unified-state")
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("f", 64),
		SourceRoot: "/release/source", Executable: "agent-sessions",
	}
	live := retirementTestCandidate("late-supervisor", 4011)
	live.Classification = LegacyClassificationActiveManagedBlocker
	live.RelatedSessionIDs = nil
	live.RelatedLaneIDs = nil
	inspections := 0
	gate := &productionHostMigrationGate{
		stateRoot: stateRoot,
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			inspections++
			if inspections == 1 {
				return FirstMigrationInspection{}, nil
			}
			return FirstMigrationInspection{
				Required: true, MigrationID: "migration-late-live-authority",
				Candidates: []LegacyRuntimeCandidate{live},
			}, nil
		},
	}
	initialized := false
	hooks := &hostInstallRoleHooks{
		migrationGate: gate,
		initialize: func(bool) (*HostInstallLifecycle, error) {
			initialized = true
			return nil, errors.New("must not initialize unified migration state")
		},
	}
	if err := gate.Preflight(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Prepare(ctx, request); !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		t.Fatalf("late legacy authority prepare = %v", err)
	}
	if initialized {
		t.Fatal("late legacy authority reached unified lifecycle initialization")
	}
	if _, err := os.Lstat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("late legacy authority created unified durable state: %v", err)
	}
	if inspections != 2 {
		t.Fatalf("production inspection count = %d, want preflight plus final", inspections)
	}
}

func TestProductionHostMigrationPreflightRejectsSelectedBlockedJournalBeforeReleaseMutation(t *testing.T) {
	ctx := context.Background()
	stateRoot := filepath.Join(t.TempDir(), "state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("selected-blocked-supervisor", 4041)
	candidate.Classification = LegacyClassificationActiveManagedBlocker
	candidate.RelatedSessionIDs = []string{"still-active-peer"}
	if err := state.putMigrationCandidate(ctx, "migration-selected-blocked", candidate); err != nil {
		t.Fatal(err)
	}
	blocker := LegacyMigrationBlocker{
		SchemaVersion: MigrationSchemaVersion, Revision: 1, BlockerID: "blocker-still-active-peer",
		CandidateID: candidate.CandidateID, Kind: candidate.Kind, ResourceType: "peer",
		ResourceID: "still-active-peer", RequiredAction: "close_before_retry",
		EvidenceRevision: candidate.EvidenceRevision, LastObservedAt: candidate.LastObservedAt,
	}
	journal := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-selected-blocked",
		FromVersions: []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:blocked-target",
		State: MigrationStateBlockedActivePeerOrLane, MaintenanceWindowState: MaintenanceWindowBlocked,
		Candidates: []string{candidate.CandidateID}, ActiveManagedBlockers: []LegacyMigrationBlocker{blocker},
		Revision: 2, StartedAt: 100, UpdatedAt: 101,
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: journal.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}
	inspected := false
	gate := &productionHostMigrationGate{
		stateRoot: stateRoot,
		inspect: func(context.Context) (FirstMigrationInspection, error) {
			inspected = true
			return FirstMigrationInspection{}, nil
		},
	}
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("a", 64),
		SourceRoot: "/release/source", Executable: "agent-sessions",
	}
	if err := gate.Preflight(ctx, request); err == nil || !strings.Contains(err.Error(), string(MigrationStateBlockedActivePeerOrLane)) {
		t.Fatalf("selected blocked migration preflight = %v", err)
	}
	if inspected || gate.ready {
		t.Fatal("selected blocked migration obtained new release mutation authority")
	}
}

func TestHostReleaseFailureDispositionIsBoundToCurrentMigrationAuthority(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
	stateRoot := filepath.Join(root, "state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("failure-disposition-supervisor", 4051)
	operations := newRecordingLegacyRetirementLifecycle(candidate)
	retirement, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity := "sha256:" + strings.Repeat("d", 64)
	request := LegacyRetirementRequest{
		MigrationID: "migration-release-failure-disposition", TargetRuntimeIdentity: targetIdentity,
		Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
	}
	journal, _, err := retirement.PrepareRetirement(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := StageLegacyAdoption(ctx, completeLegacyAdoptionRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.savePreparedFirstMigration(ctx, journal.MigrationID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: journal.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retirement.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID); err != nil {
		t.Fatal(err)
	}
	if err := state.commitPreparedFirstMigration(ctx, journal.MigrationID); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "prefix")
	record, err := captureHostSurfaceRollback(prefix, stateRoot, journal.MigrationID, targetIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveHostSurfaceRollback(record); err != nil {
		t.Fatal(err)
	}
	hooks := &hostInstallRoleHooks{prefix: prefix, stateRoot: stateRoot}
	if disposition, err := hooks.FailureDisposition(ctx, releaseinstall.PhaseReady); err != nil ||
		disposition != releaseinstall.FailureDispositionRollback {
		t.Fatalf("pre-commit disposition = %q, %v", disposition, err)
	}
	if err := CommitFirstMigrationAuthority(ctx, state, journal.MigrationID, targetIdentity, 7, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if disposition, err := hooks.FailureDisposition(ctx, releaseinstall.PhaseReady); err != nil ||
		disposition != releaseinstall.FailureDispositionRollForward {
		t.Fatalf("committed disposition = %q, %v", disposition, err)
	}

	// A later steady-state install must not inherit forward-only authority from
	// the historical completed migration unless its durable rollback record is
	// explicitly bound to that migration and target.
	unbound, err := captureHostSurfaceRollback(prefix, stateRoot, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveHostSurfaceRollback(unbound); err != nil {
		t.Fatal(err)
	}
	if disposition, err := hooks.FailureDisposition(ctx, releaseinstall.PhaseReady); err != nil ||
		disposition != releaseinstall.FailureDispositionRollback {
		t.Fatalf("unbound steady-state disposition = %q, %v", disposition, err)
	}
}

func TestProductionFirstMigrationInspectionIsReadOnlyAndProjectsUnknownDebt(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy-runtime")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, "identity.json")
	if err := os.WriteFile(marker, []byte(`{"pid":41}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sources := []LegacyInventorySource{{ID: "bridge-runtime", Kind: "runtime", Path: legacy, Target: true, MaxDepth: 3}}
	inspection, err := inspectProductionFirstMigrationSources(context.Background(), sources, 100)
	if err != nil || !inspection.Required || len(inspection.Candidates) != 1 ||
		inspection.Candidates[0].Classification != LegacyClassificationUnknown {
		t.Fatalf("production inspection = %+v, %v", inspection, err)
	}
	report, quiescenceErr := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{
		Candidates: inspection.Candidates,
	})
	if quiescenceErr == nil || len(report.Debt) != 1 || report.Debt[0].Code != "unknown_identity" {
		t.Fatalf("unknown production evidence did not fail closed: report=%+v err=%v", report, quiescenceErr)
	}
	after, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("read-only production inspection changed source metadata")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != `{"pid":41}` {
		t.Fatalf("read-only production inspection changed source body: %q, %v", body, err)
	}

	missing := []LegacyInventorySource{{ID: "missing", Kind: "state", Path: filepath.Join(root, "absent"), Target: true}}
	clean, err := inspectProductionFirstMigrationSources(context.Background(), missing, 100)
	if err != nil || clean.Required || !emptyFirstMigrationInspection(clean) {
		t.Fatalf("clean production inspection = %+v, %v", clean, err)
	}
	emptyRoot := filepath.Join(root, "empty-known-root")
	if err := os.Mkdir(emptyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	empty, err := inspectProductionFirstMigrationSources(context.Background(), []LegacyInventorySource{
		{ID: "bridge-state", Kind: "state", Path: emptyRoot, Target: true, MaxDepth: 5},
	}, 100)
	if err != nil || empty.Required || !emptyFirstMigrationInspection(empty) {
		t.Fatalf("empty exact legacy root was treated as an unknown estate: %+v, %v", empty, err)
	}
}

func TestProductionNoAgentEstateKeepsExactStaleChildClassification(t *testing.T) {
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "state", "claude-code-peer")
	runtimeRoot := filepath.Join(root, "run", "codex-claude-peer-"+strconv.Itoa(os.Getuid()))
	recordRoot := filepath.Join(bridgeRoot, "sessions", "stale")
	if err := os.MkdirAll(recordRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"sessionId":"closed-session","pid":1073741824,"procStart":"stale-start","entrypoint":"codex","socketPath":"` + filepath.Join(runtimeRoot, "closed.sock") + `"}`)
	if err := os.WriteFile(filepath.Join(recordRoot, "state.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := inspectProductionFirstMigrationSources(context.Background(), []LegacyInventorySource{
		{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5},
		{ID: "bridge-xdg-runtime", Kind: "runtime", Path: runtimeRoot, Target: true, MaxDepth: 3},
	}, 100)
	if err != nil || !inspection.Required || len(inspection.Candidates) != 1 ||
		inspection.Candidates[0].Classification != LegacyClassificationStale {
		t.Fatalf("no-agent exact estate = %+v, %v", inspection, err)
	}
	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: inspection.Candidates})
	if err != nil || !report.LegacyAbsenceVerified || len(report.Debt) != 0 {
		t.Fatalf("stale no-agent estate was forced to unknown debt: %+v, %v", report, err)
	}
}

func TestProductionFirstMigrationCollectorNamesLiveAuthorityAsMaintenanceBlockerConcurrently(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state", "agent-sessions", "agents")
	agentState := filepath.Join(stateRoot, "host-a")
	runtimeRoot := filepath.Join(root, "run", "peer-federator")
	serviceManager, serviceUnit := productionLegacyServiceIdentity()
	servicePath := filepath.Join(root, "home", ".config", "systemd", "user", "peer-federator-agent.service")
	if runtime.GOOS == "darwin" {
		servicePath = filepath.Join(root, "home", "Library", "LaunchAgents", "net.antst.peer-federator.agent.plist")
	}
	for _, directory := range []string{agentState, runtimeRoot, filepath.Dir(servicePath), filepath.Join(agentState, "session-names")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	preference := federation.SessionPreferences{
		SessionID: "session-a", Product: "claude", Kind: federation.SessionKindInteractive,
		ExplicitGroups: []string{"project"}, AlwaysApprove: true, UpdatedAt: 80, Revision: "catalog-r8",
	}
	catalogBody, _ := json.Marshal(map[string]any{
		"version": 2, "sessions": map[string]federation.SessionPreferences{"session-a": preference},
	})
	if err := os.WriteFile(filepath.Join(agentState, "sessions.json"), catalogBody, 0o600); err != nil {
		t.Fatal(err)
	}
	nameBody, _ := json.Marshal(map[string]any{
		"version": 1, "session_id": "session-a", "product": "claude", "kind": "interactive",
		"name": "reviewer", "updated_at": 81,
	})
	if err := os.WriteFile(filepath.Join(agentState, "session-names", "session.json"), nameBody, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(runtimeRoot, "agent.sock")
	if err := os.WriteFile(endpoint, []byte("test endpoint identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "bin", "peer-federator")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("legacy executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := hostExecutableIdentity(executable)
	if err != nil {
		t.Fatal(err)
	}
	sources := []LegacyInventorySource{
		{ID: "host-agent-state", Kind: "state", Path: stateRoot, Target: true, MaxDepth: 5},
		{ID: "federator-xdg-runtime", Kind: "runtime", Path: runtimeRoot, Target: true, MaxDepth: 3},
		{ID: "systemd-host-agent", Kind: "service", Path: servicePath, Target: true},
	}
	probe := productionLegacyInspectionProbe{
		ObserveAgent: func(context.Context, string) (productionLegacyAgentObservation, error) {
			return productionLegacyAgentObservation{
				Status: productionLegacyAgentStatus{
					RuntimeVersion: "0.2.4", ProtocolVersion: federation.ProtocolVersion,
					HostID: "host-a", HostName: "host a", Hub: "hub.example.test:7419",
					Capabilities: []string{"claude-lane"}, RuntimeDir: runtimeRoot, StateDir: agentState,
				},
				Peer:       controlPeerEvidence{UID: os.Getuid(), PID: 4401, ProcStart: "start-4401", StrongStart: "strong-4401"},
				Executable: executable, Identity: identity,
			}, nil
		},
		ServiceLoaded: func(context.Context, string, string) (bool, error) { return true, nil },
		RemoteWatches: func([]LegacyRuntimeCandidate, int64) ([]LegacyRuntimeCandidate, error) {
			return nil, nil
		},
	}
	const callers = 16
	start := make(chan struct{})
	results := make(chan FirstMigrationInspection, callers)
	errorsCh := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			inspection, err := collectProductionFirstMigrationWithProbe(context.Background(), sources, 100, probe)
			results <- inspection
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var expectedID string
	for inspection := range results {
		if !inspection.Required || len(inspection.Candidates) != 1 ||
			inspection.Adoption.HostID != "host-a" || len(inspection.Adoption.Sessions) != 1 ||
			inspection.Adoption.Sessions[0].PermissionMode != "bypassPermissions" ||
			len(inspection.Adoption.Names) != 1 || inspection.PriorAuthority.ServiceUnit != serviceUnit {
			t.Fatalf("exact production inspection = %+v", inspection)
		}
		candidate := inspection.Candidates[0]
		if candidate.Classification != LegacyClassificationActiveManagedBlocker || candidate.EndpointPath != endpoint ||
			candidate.ServiceManager != serviceManager || candidate.ServiceUnit != serviceUnit ||
			candidate.SourcePath != servicePath || !strings.HasPrefix(candidate.SourceRevision, "sha256:") {
			t.Fatalf("production candidate does not reconcile process/endpoint/service ownership: %+v", candidate)
		}
		report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: inspection.Candidates})
		if !errors.Is(err, ErrLegacyQuiescenceBlocked) || len(report.Blockers) != 1 ||
			report.Blockers[0].ResourceType != "authority" || report.Blockers[0].ResourceID != candidate.CandidateID {
			t.Fatalf("live legacy authority was not an exact maintenance blocker: %+v, %v", report, err)
		}
		if expectedID == "" {
			expectedID = inspection.MigrationID
		} else if inspection.MigrationID != expectedID {
			t.Fatalf("concurrent inspection identity changed: %q / %q", inspection.MigrationID, expectedID)
		}
	}
	baseObserve := probe.ObserveAgent
	activeProbe := probe
	activeProbe.ObserveAgent = func(ctx context.Context, endpoint string) (productionLegacyAgentObservation, error) {
		observation, observeErr := baseObserve(ctx, endpoint)
		observation.Status.LocalPeers = 2
		return observation, observeErr
	}
	active, err := collectProductionFirstMigrationWithProbe(context.Background(), sources, 101, activeProbe)
	if err != nil || len(active.Candidates) != 1 {
		t.Fatalf("active scalar production inspection = %+v, %v", active, err)
	}
	if active.Candidates[0].ReportedActiveCount != 2 ||
		active.Candidates[0].Classification != LegacyClassificationActiveManagedBlocker {
		t.Fatalf("scalar evidence changed exact authority classification: %+v", active.Candidates)
	}
	report, quiescenceErr := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: active.Candidates})
	if !errors.Is(quiescenceErr, ErrLegacyQuiescenceBlocked) || report.LegacyAbsenceVerified ||
		len(report.Blockers) != 1 || len(report.Debt) != 0 {
		t.Fatalf("active scalar production quiescence = %+v, %v", report, quiescenceErr)
	}
}

func TestProductionRecordedDarwinRuntimeRootIsBoundedAndExact(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	bridgeState := filepath.Join(stateHome, "claude-code-peer")
	profileKey := "profile-a"
	profileRoot := filepath.Join(bridgeState, "profiles", profileKey)
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "codex-claude-peer-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	misleadingExecutableRoot := filepath.Join(t.TempDir(), "ccp-"+strconv.Itoa(os.Getuid()))
	if err := os.WriteFile(
		filepath.Join(bridgeState, "native-runtime-path"),
		[]byte(filepath.Join(misleadingExecutableRoot, "agent-session-runtime")+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	stateBody, err := json.Marshal(productionLegacySupervisorState{
		ControlSocket: filepath.Join(runtimeRoot, "supervisor-"+profileKey+".sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(profileRoot, "supervisor.json")
	if err := os.WriteFile(statePath, stateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := productionRecordedRuntimeRoots(stateHome, os.Getuid())
	recordedFederatorRoot := filepath.Join(filepath.Dir(runtimeRoot), "peer-federator-"+strconv.Itoa(os.Getuid()))
	if err != nil || !reflect.DeepEqual(roots, []string{runtimeRoot, recordedFederatorRoot}) {
		t.Fatalf("recorded roots = %v, %v", roots, err)
	}
	if slices.Contains(roots, misleadingExecutableRoot) {
		t.Fatal("native-runtime-path executable directory became migration authority")
	}
	stateBody, err = json.Marshal(productionLegacySupervisorState{
		ControlSocket: filepath.Join(t.TempDir(), "unrelated", "supervisor-"+profileKey+".sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, stateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := productionRecordedRuntimeRoots(stateHome, os.Getuid()); err == nil {
		t.Fatal("accepted durable supervisor control root outside shipped Darwin roots")
	}
}

func TestProductionLegacyBridgeAdoptionPreservesCollectedMetadataWithoutContent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "claude-code-peer")
	laneDirectory := filepath.Join(bridgeRoot, "profiles", "profile-a", "qwen-lanes")
	if err := os.MkdirAll(laneDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(laneDirectory, "lane-a.json")
	write := func(prompt, result, message, cleanupError string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"name": "review", "threadId": "lane-a", "qwenSessionId": "native-qwen-a", "cwd": "/workspace",
			"status": "archived", "nativeArchiveState": "archived", "permissionMode": "default",
			"groups": []string{"child", "parent-group"}, "explicitGroups": []string{"child"},
			"parentSessionId": "parent-a", "parentHostId": "host-a", "inheritParentGroups": true,
			"createdAt": int64(80), "updatedAt": int64(100), "collectedTurnId": "turn-a",
			"turns": []map[string]any{{
				"id": "turn-a", "requestDigest": "sha256:request-a", "status": "completed", "outcome": "completed",
				"prompt": prompt, "result": result, "createdAt": int64(81), "completedAt": int64(90),
				"collected": true, "collectedAt": int64(95), "qwenSessionId": "native-qwen-a",
			}},
			"notices":     []map[string]any{{"id": "notice-a", "turnId": "turn-a", "message": message, "sentAt": int64(96)}},
			"cleanupDebt": []map[string]any{{"operation": "cleanup", "error": cleanupError, "attempts": 2, "updatedAt": int64(99)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base, err := productionSupervisorOnlyAdoption(nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	sources := []LegacyInventorySource{{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5}}
	const canary = "PRODUCTION-MIGRATION-CONTENT-CANARY"
	write(canary+"-prompt", canary+"-result", canary+"-notice", canary+"-cleanup")
	first, err := productionLegacyBridgeAdoption(ctx, base, sources, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Lanes) != 1 || first.Lanes[0].LaneSessionID != "lane-a" ||
		first.Lanes[0].State != LaneStateArchived || first.Lanes[0].CollectionCursor != "turn-a:1" ||
		len(first.Turns) != 1 || first.Turns[0].DispatchState != LaneDispatchCollected ||
		len(first.Notices) != 1 || first.Notices[0].NoticeID != "notice-a" || len(first.Debt) != 1 {
		t.Fatalf("production bridge adoption = %+v", first)
	}
	if _, err := StageLegacyAdoption(ctx, first); err != nil {
		t.Fatalf("stage production bridge adoption: %v", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("production bridge adoption retained content: %s", encoded)
	}

	write("changed-prompt", "changed-result", "changed-notice", "changed-cleanup")
	second, err := productionLegacyBridgeAdoption(ctx, base, sources, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.SourceRevision != first.SourceRevision {
		t.Fatalf("content changed metadata-only source revision: %q / %q", first.SourceRevision, second.SourceRevision)
	}
}

func TestProductionBridgeRecordInventoryNamesExactShimHostsManagersAndLanes(t *testing.T) {
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "state", "claude-code-peer")
	runtimeRoot := filepath.Join(root, "run", "codex-claude-peer-"+strconv.Itoa(os.Getuid()))
	paths := []string{
		filepath.Join(bridgeRoot, "sessions", "session-record"),
		filepath.Join(bridgeRoot, "profiles", "profile-a", "claude-lanes"),
		filepath.Join(bridgeRoot, "profiles", "profile-a", "grok-lanes"),
		filepath.Join(bridgeRoot, "profiles", "profile-a", "qwen-lanes"),
		filepath.Join(bridgeRoot, "profiles", "profile-a", "grok-launches"),
		filepath.Join(bridgeRoot, "profiles", "profile-a", "lanes"),
		runtimeRoot,
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacyJSON := func(path string, value map[string]any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacyJSON(filepath.Join(paths[0], "state.json"), map[string]any{
		"sessionId": "interactive-a", "pid": 101, "procStart": "start-101", "entrypoint": "claude",
		"socketPath": filepath.Join(runtimeRoot, "interactive.sock"),
	})
	writeLegacyJSON(filepath.Join(paths[1], "claude.json"), map[string]any{
		"sessionId": "claude-lane-a", "managerPid": 102, "managerProcStart": "start-102",
		"controlSocket": filepath.Join(runtimeRoot, "claude-lane.sock"),
	})
	writeLegacyJSON(filepath.Join(paths[2], "grok.json"), map[string]any{
		"sessionId": "grok-lane-a", "managerPid": 103, "managerProcStart": "start-103",
		"managerStrongStart": "strong-103", "controlSocket": filepath.Join(runtimeRoot, "grok-lane.sock"),
	})
	writeLegacyJSON(filepath.Join(paths[3], "qwen.json"), map[string]any{
		"threadId": "qwen-lane-a", "managerPid": 104, "managerProcStart": "start-104",
		"managerStrongStart": "strong-104", "controlSocket": filepath.Join(runtimeRoot, "qwen-lane.sock"),
	})
	writeLegacyJSON(filepath.Join(paths[4], "grok-host.json"), map[string]any{
		"sessionId": "grok-session-a", "hostPid": 105, "hostProcStart": "start-105",
		"controlSocket": filepath.Join(runtimeRoot, "grok-host.sock"),
	})
	writeLegacyJSON(filepath.Join(paths[5], "codex.json"), map[string]any{
		"sessionId": "codex-lane-a", "status": "idle",
	})

	sources := []LegacyInventorySource{
		{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5},
		{ID: "bridge-xdg-runtime", Kind: "runtime", Path: runtimeRoot, Target: true, MaxDepth: 3},
	}
	observed := make(map[string]productionLegacyRecordDescriptor)
	inventory, err := productionLegacyBridgeRecords(context.Background(), sources, 100,
		func(descriptor productionLegacyRecordDescriptor, _ []LegacyInventorySource, observedAt int64) (LegacyRuntimeCandidate, error) {
			observed[descriptor.kind] = descriptor
			evidence := exactLegacyCandidateEvidence(descriptor.kind, descriptor.pid)
			evidence.CandidateID = quiescenceRecordID("fixture", descriptor.sourcePath)
			evidence.SourcePath, evidence.SourceRevision = descriptor.sourcePath, descriptor.sourceRevision
			evidence.RelatedSessionIDs = append([]string(nil), descriptor.relatedSessions...)
			evidence.RelatedLaneIDs = append([]string(nil), descriptor.relatedLanes...)
			evidence.ObservedAt = observedAt
			return ClassifyLegacyCandidate(evidence)
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude_host", "claude_lane_manager", "grok_lane_manager", "qwen_lane_manager", "grok_host"} {
		if _, ok := observed[kind]; !ok {
			t.Fatalf("exact %s record was omitted: observed=%v", kind, observed)
		}
	}
	if len(inventory.candidates) != 6 || !reflect.DeepEqual(inventory.supervisorLanes["profile-a"], []string{"codex-lane-a"}) {
		t.Fatalf("bridge record inventory = candidates=%+v supervisor lanes=%v", inventory.candidates, inventory.supervisorLanes)
	}
	for _, candidate := range inventory.candidates {
		if candidate.Kind == "codex_lane_record" {
			if candidate.Classification != LegacyClassificationRetired || candidate.ProcessStatus != "absent" ||
				candidate.EndpointStatus != "absent" ||
				!reflect.DeepEqual(candidate.RelatedLaneIDs, []string{"codex-lane-a"}) {
				t.Fatalf("unattested active Codex lane record gained process authority: %+v", candidate)
			}
			continue
		}
		if candidate.Classification != LegacyClassificationActiveManagedBlocker ||
			len(candidate.RelatedSessionIDs)+len(candidate.RelatedLaneIDs) != 1 {
			t.Fatalf("exact record did not name one real blocker: %+v", candidate)
		}
	}
}

func TestProductionCodexLaneRecordsKeepTerminalMetadataOutOfBlockersAndDebt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "lanes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{
		"lane-idle": "idle", "lane-completed": "completed",
		"lane-failed": "failed", "lane-interrupted": "interrupted", "lane-timed-out": "timed_out",
		"lane-cancelled": "cancelled", "lane-canceled": "canceled", "lane-archived": "archived",
	}
	for laneID, status := range statuses {
		body, err := json.Marshal(map[string]any{"sessionId": laneID, "status": status})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, sessionkey.FromID(laneID)+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	liveLanes, candidates, err := productionLegacyCodexLaneRecords(context.Background(), directory, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(liveLanes, []string{"lane-idle"}) {
		t.Fatalf("possibly live lane metadata = %v", liveLanes)
	}
	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: candidates})
	if err != nil || len(report.Blockers) != 0 || len(report.Debt) != 0 {
		t.Fatalf("metadata-only lane quiescence = %+v, %v", report, err)
	}
	for _, candidate := range candidates {
		if candidate.ProcessStatus != "absent" || candidate.EndpointStatus != "absent" ||
			candidate.Classification != LegacyClassificationRetired {
			t.Fatalf("lane metadata gained cleanup authority: %+v", candidate)
		}
	}
}

func TestProductionIncompleteCodexLaneMetadataBecomesPreservedDebtNotIdentityDebt(t *testing.T) {
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "claude-code-peer")
	directory := filepath.Join(bridgeRoot, "profiles", "profile-a", "lanes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"type": "", "name": "", "threadId": "thread-lane", "sessionId": "", "cwd": "", "status": "",
		"turnId": "turn-wake", "collectedTurnId": "turn-old", "timedOutTurnId": "turn-wake",
		"terminalOutcome": "timed_out", "createdAt": int64(0), "updatedAt": int64(100),
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, sessionkey.FromID("thread-lane")+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	liveLanes, candidates, err := productionLegacyCodexLaneRecords(context.Background(), directory, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveLanes) != 0 || len(candidates) != 1 ||
		candidates[0].Kind != "codex_lane_record" ||
		candidates[0].Classification != LegacyClassificationRetired ||
		candidates[0].ProcessStatus != "absent" || candidates[0].EndpointStatus != "absent" {
		t.Fatalf("incomplete lane inventory = live=%v candidates=%+v", liveLanes, candidates)
	}
	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: candidates})
	if err != nil || len(report.Blockers) != 0 || len(report.Debt) != 0 {
		t.Fatalf("incomplete metadata became authority debt: report=%+v err=%v", report, err)
	}

	adoption, err := productionLegacyBridgeAdoption(context.Background(), LegacyAdoptionRequest{
		SourceRevision: "sha256:" + strings.Repeat("a", 64), HostID: "host-a",
	}, []LegacyInventorySource{{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true}}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Lanes) != 0 || len(adoption.Debt) != 1 ||
		adoption.Debt[0].CauseCode != "incomplete_legacy_lane_projection" ||
		adoption.Debt[0].ResourceIdentity != "codex/thread-lane" {
		t.Fatalf("incomplete lane adoption = %+v", adoption)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("preserved source lane metadata disappeared: %v", err)
	}
}

func TestProductionUnreadableCodexLaneRecordStillFailsClosedAsIdentityDebt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "lanes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveLanes, candidates, err := productionLegacyCodexLaneRecords(context.Background(), directory, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveLanes) != 0 || len(candidates) != 1 || candidates[0].Classification != LegacyClassificationUnknown {
		t.Fatalf("unreadable lane inventory = live=%v candidates=%+v", liveLanes, candidates)
	}
	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: candidates})
	if !errors.Is(err, ErrLegacyQuiescenceBlocked) || len(report.Debt) != 1 || report.Debt[0].Code != "unknown_identity" {
		t.Fatalf("unreadable lane did not fail closed: report=%+v err=%v", report, err)
	}
}

func TestProductionUnknownCodexLaneStatusIsPreservedAsProjectionDebt(t *testing.T) {
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "claude-code-peer")
	directory := filepath.Join(bridgeRoot, "profiles", "profile-a", "lanes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"threadId": "thread-a", "sessionId": "lane-a", "name": "lane-a", "cwd": root,
		"status": "future_status", "parentHostId": "host-parent", "parentSessionId": "session-parent",
		"createdAt": int64(100), "updatedAt": int64(200),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sessionkey.FromID("lane-a")+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	liveLanes, candidates, err := productionLegacyCodexLaneRecords(context.Background(), directory, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveLanes) != 0 || len(candidates) != 1 || candidates[0].Classification != LegacyClassificationRetired {
		t.Fatalf("unknown-status lane inventory = live=%v candidates=%+v", liveLanes, candidates)
	}
	adoption, err := productionLegacyBridgeAdoption(context.Background(), LegacyAdoptionRequest{
		SourceRevision: "sha256:" + strings.Repeat("b", 64), HostID: "host-a",
	}, []LegacyInventorySource{{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true}}, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Lanes) != 0 || len(adoption.Debt) != 1 ||
		adoption.Debt[0].CauseCode != "incomplete_legacy_lane_projection" ||
		adoption.Debt[0].ResourceIdentity != "codex/lane-a" {
		t.Fatalf("unknown-status lane adoption = %+v", adoption)
	}
}

func TestProductionBridgeInventoryIncludesExactInteractiveOwnersAndRetiredRecords(t *testing.T) {
	root := t.TempDir()
	bridgeRoot := filepath.Join(root, "claude-code-peer")
	profileRoot := filepath.Join(bridgeRoot, "profiles", "profile-a")
	ownerDirectory := filepath.Join(bridgeRoot, "interactive-owners")
	retiredDirectory := filepath.Join(profileRoot, "retired")
	for _, directory := range []string{ownerDirectory, retiredDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ownerID, retiredID := "interactive-owner-a", "retired-peer-a"
	identity := procinfo.Read(os.Getpid())
	if identity.Status != procinfo.Known {
		t.Fatal("current process identity is not observable")
	}
	ownerPath := filepath.Join(ownerDirectory, sessionkey.FromID(ownerID)+".json")
	writeOwner := func(procStart string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"threadId": ownerID, "requestId": "request-a", "ownerPid": os.Getpid(),
			"ownerProcStart": procStart, "updatedAt": int64(100),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ownerPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOwner(identity.Start)
	retiredBody, err := json.Marshal(map[string]any{"threadId": retiredID, "retiredAt": int64(90)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retiredDirectory, sessionkey.FromID(retiredID)+".json"), retiredBody, 0o600); err != nil {
		t.Fatal(err)
	}
	sources := []LegacyInventorySource{{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5}}
	inspect := func() productionLegacyRecordInventory {
		t.Helper()
		inventory, err := productionLegacyBridgeRecords(
			context.Background(), sources, 100, classifyProductionLegacyRecord,
		)
		if err != nil {
			t.Fatal(err)
		}
		return inventory
	}
	inventory := inspect()
	if len(inventory.candidates) != 2 {
		t.Fatalf("profile record candidates = %+v", inventory.candidates)
	}
	classifications := make(map[string]string)
	for _, candidate := range inventory.candidates {
		classifications[candidate.Kind] = candidate.Classification
	}
	if classifications["interactive_owner_record"] != LegacyClassificationActiveManagedBlocker ||
		classifications["retired_record"] != LegacyClassificationRetired {
		t.Fatalf("profile record classifications = %v", classifications)
	}
	writeOwner("reused-owner-start")
	inventory = inspect()
	for _, candidate := range inventory.candidates {
		if candidate.Kind == "interactive_owner_record" && candidate.Classification != LegacyClassificationStale {
			t.Fatalf("disproven owner did not become stale: %+v", candidate)
		}
	}
}

func TestProductionQwenHostArgvCorroboratesExactRegistrationSession(t *testing.T) {
	arguments := []string{
		"/release/agent-session-runtime", "qwen-host", "--runtime-dir", "/run/user/1000",
		"--registration-json", `{"session_id":"qwen-session-a","product":"qwen"}`, "--", "qwen",
	}
	if !productionLegacyArgumentsMatch("qwen_host", "qwen-session-a", arguments) {
		t.Fatal("exact Qwen host registration was not recognized")
	}
	if productionLegacyArgumentsMatch("qwen_host", "different-session", arguments) {
		t.Fatal("Qwen host argv authorized a different durable session")
	}
}

func TestProductionRemoteLaneWatchInventoryRequiresExactOwnerAncestryAndArgv(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "peer-federator")
	if err := os.WriteFile(executable, []byte("legacy watch executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "legacy-owner", Kind: "host_federation_agent",
		SourcePath: "/state/legacy-owner.json", SourceRevision: "sha256:owner", PID: 4001,
		ProcStart: "owner-start", StrongStart: "owner-strong", ProcessStatus: "known",
		RelatedLaneIDs: []string{"remote-lane-a"}, Classification: LegacyClassificationActiveManagedBlocker,
		EvidenceRevision: 1, LastObservedAt: 100,
	}
	processes := []procinfo.Process{
		{PID: 4001, Info: procinfo.Info{Status: procinfo.Known, Parent: 1, Start: "owner-start", StrongStart: "owner-strong"}},
		{PID: 4002, Info: procinfo.Info{Status: procinfo.Known, Parent: 4001, Start: "watch-start", StrongStart: "watch-strong"}},
		{PID: 4003, Info: procinfo.Info{Status: procinfo.Known, Parent: 1, Start: "unrelated-start", StrongStart: "unrelated-strong"}},
	}
	watches, err := productionLegacyRemoteWatchCandidatesFromProcesses(
		[]LegacyRuntimeCandidate{owner}, 101, processes,
		func(pid int) ([]string, error) {
			if pid == 4002 {
				return []string{executable, "lane-watch", "--lane", "remote-lane-a"}, nil
			}
			return []string{executable, "status"}, nil
		},
		func(int) (string, error) { return executable, nil },
	)
	if err != nil || len(watches) != 1 {
		t.Fatalf("remote watch inventory = %+v, %v", watches, err)
	}
	watch := watches[0]
	if watch.Kind != "remote_lane_watch" || watch.PID != 4002 || watch.ProcStart != "watch-start" ||
		watch.StrongStart != "watch-strong" || watch.ProcessExecutable != executable ||
		!reflect.DeepEqual(watch.RelatedLaneIDs, []string{"remote-lane-a"}) ||
		watch.Classification != LegacyClassificationUnknown {
		t.Fatalf("remote watch lost exact owner ancestry/argv identity: %+v", watch)
	}
}

func TestProductionSupervisorRetirementRemovesExactRecordAndUnlockedStartLock(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "profile", "supervisor.json")
	lockPath := filepath.Join(filepath.Dir(recordPath), "supervisor-start.lock")
	endpoint := filepath.Join(root, "runtime", "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatal(err)
	}
	state := productionLegacySupervisorState{
		ControlSocket: endpoint, PID: 777, ProcStart: "start-777",
		RuntimeIdentity: "sha256:runtime-777", PluginVersion: "0.2.4",
	}
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := exactLegacyCandidateEvidence("supervisor", 777)
	evidence.SourcePath, evidence.SourceRevision = recordPath, "sha256:"+productionDigest(body)
	evidence.RuntimeIdentity = state.RuntimeIdentity
	evidence.PID, evidence.ProcStart = state.PID, state.ProcStart
	evidence.Process.PID, evidence.Process.ProcStart = state.PID, state.ProcStart
	evidence.Endpoint.Path = endpoint
	evidence.Endpoint.OwnerPID, evidence.Endpoint.OwnerProcStart = state.PID, state.ProcStart
	evidence.Endpoint.RuntimeIdentity = state.RuntimeIdentity
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &productionLegacyRetirementLifecycle{}
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{recordPath, lockPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("retired supervisor artifact remains at %s: %v", path, err)
		}
	}

	if err := os.WriteFile(recordPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil ||
		!strings.Contains(err.Error(), "start lock is held") {
		t.Fatalf("concurrent supervisor start lock retirement error = %v", err)
	}
	if _, err := os.Lstat(recordPath); err != nil {
		t.Fatalf("held lock allowed durable supervisor record removal: %v", err)
	}
}

func TestHostInstallRecoversDurableReleaseTransactionBeforeStartingAnother(t *testing.T) {
	recoveryFailure := errors.New("pointer commit recovery failed")
	recorder := &recordingHostReleaseTransaction{recoverErr: recoveryFailure}
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("a", 64),
		SourceRoot: "/release", Executable: "agent-sessions",
	}
	if err := recoverThenInstallHost(context.Background(), recorder, request); !errors.Is(err, recoveryFailure) {
		t.Fatalf("recovery error = %v, want %v", err, recoveryFailure)
	}
	if !reflect.DeepEqual(recorder.calls, []string{"recover"}) {
		t.Fatalf("failed recovery started a replacement transaction: %v", recorder.calls)
	}
	recorder.recoverErr = nil
	if err := recoverThenInstallHost(context.Background(), recorder, request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorder.calls, []string{"recover", "recover", "install"}) {
		t.Fatalf("host transaction ordering = %v", recorder.calls)
	}
}

type recordingHostReleaseTransaction struct {
	calls      []string
	recoverErr error
}

func (transaction *recordingHostReleaseTransaction) Recover(context.Context) error {
	transaction.calls = append(transaction.calls, "recover")
	return transaction.recoverErr
}

func (transaction *recordingHostReleaseTransaction) Install(
	context.Context,
	releaseinstall.InstallRequest,
) (releaseinstall.InstallResult, error) {
	transaction.calls = append(transaction.calls, "install")
	return releaseinstall.InstallResult{}, nil
}

func TestFirstMigrationCandidateRecoveryIsConcurrentIdempotentBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	lifecycle, state, retirementOps, request, targetIdentity := newFirstMigrationTestLifecycle(t)
	if err := lifecycle.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	recovery, err := NewFirstMigrationRecovery(FirstMigrationRecoveryOptions{
		State: state, Retirement: lifecycle.retirement,
	})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 24
	start := make(chan struct{})
	errorsCh := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errorsCh <- recovery.Recover(ctx, targetIdentity, 9, 1_900_000_000_000)
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	journal, _, err := state.ReadMigration(ctx, "migration-install-first")
	if err != nil || journal.State != MigrationStateComplete || journal.AuthorityGeneration != 9 ||
		!journal.SuccessorStateDurable {
		t.Fatalf("recovered journal = %+v, %v", journal, err)
	}
	if retirementOps.retireCount["candidate-supervisor"] != 1 {
		t.Fatalf("concurrent recovery redispatched retirement: retires=%v", retirementOps.retireCount)
	}
	release := releaseinstall.InstalledRelease{
		Role: releaseinstall.RoleHost, Root: request.SourceRoot,
		Executable: filepath.Join(request.SourceRoot, "bin", "agent-sessions"),
	}
	if err := lifecycle.BeforeReady(ctx, release); err != nil {
		t.Fatalf("install readiness did not accept foreground-completed journal: %v", err)
	}
}

func TestFirstMigrationRecoveryCommitsDurablePreparedAdoptionAfterStopCrash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("supervisor", 4201)
	operations := newRecordingLegacyRetirementLifecycle(candidate)
	retirement, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := StageLegacyAdoption(ctx, completeLegacyAdoptionRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	journal, _, err := retirement.PrepareRetirement(ctx, LegacyRetirementRequest{
		MigrationID: "migration-crash-after-stops", TargetRuntimeIdentity: "sha256:candidate-runtime",
		Candidates: []LegacyRuntimeCandidate{candidate}, PriorAuthority: retirementTestPriorAuthority(candidate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.savePreparedFirstMigration(ctx, journal.MigrationID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: journal.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retirement.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdoptedState(ctx, state); !os.IsNotExist(err) {
		t.Fatalf("simulated stop crash already committed adoption: %v", err)
	}
	recovery, err := NewFirstMigrationRecovery(FirstMigrationRecoveryOptions{State: state, Retirement: retirement})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Recover(ctx, "sha256:candidate-runtime", 11, 1_900_000_000_100); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdoptedState(ctx, state); err != nil {
		t.Fatalf("recovery did not commit durable prepared adoption: %v", err)
	}
	completed, _, err := state.ReadMigration(ctx, journal.MigrationID)
	if err != nil || completed.State != MigrationStateComplete || operations.retireCount[candidate.CandidateID] != 1 {
		t.Fatalf("crash recovery = journal %+v, retires=%v, err=%v",
			completed, operations.retireCount, err)
	}
}

func TestFirstMigrationRollbackLeavesOperatorStoppedLegacyAuthorityStopped(t *testing.T) {
	ctx := context.Background()
	lifecycle, state, retirementOps, request, _ := newFirstMigrationTestLifecycle(t)
	if err := lifecycle.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	journal, _, err := state.ReadMigration(ctx, "migration-install-first")
	if err != nil || journal.State != MigrationStateRetryRequired || !journal.RollbackCompleted ||
		journal.MaintenanceWindowState != MaintenanceWindowUnverified || journal.SuccessorStateDurable {
		t.Fatalf("rolled back journal = %+v, %v", journal, err)
	}
	if len(retirementOps.calls) != 0 {
		t.Fatalf("migration rollback duplicated release-owned successor stop: calls=%v", retirementOps.calls)
	}
}

func TestTerminalReleaseRollbackForcesFreshInventoryBeforeNewInstall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retirementTestCandidate("stale-supervisor-record", 0)
	candidate.Kind = "stale_supervisor_record"
	candidate.PID, candidate.ProcStart, candidate.StrongStart = 0, "", ""
	candidate.ProcessStatus, candidate.EndpointStatus = "absent", "absent"
	candidate.ProcessExecutable, candidate.ProcessArgvRole = "", ""
	candidate.ProcessArguments, candidate.ProcessEnvironment = nil, nil
	candidate.EndpointPath, candidate.EndpointType = "", ""
	candidate.EndpointOwnerUID, candidate.EndpointOwnerPID, candidate.EndpointOwnerStart = 0, 0, ""
	candidate.EndpointRuntimeIdentity = ""
	candidate.Classification = LegacyClassificationStale
	operations := newRecordingLegacyRetirementLifecycle(candidate)
	retirement, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: operations})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(root, "release")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "bin", "agent-sessions"), []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:" + strings.Repeat("9", 64),
		SourceRoot: sourceRoot, Executable: "agent-sessions",
	}
	inspection := func(migrationID, revision string) FirstMigrationInspection {
		currentCandidate := candidate
		currentCandidate.CandidateID = "candidate-" + migrationID
		currentCandidate.SourceRevision = revision
		adoption := completeLegacyAdoptionRequest(t)
		adoption.SourceRevision = revision
		return FirstMigrationInspection{
			Required: true, MigrationID: migrationID, FromVersions: []string{"legacy-stopped-metadata"},
			Candidates: []LegacyRuntimeCandidate{currentCandidate}, Adoption: adoption,
		}
	}
	firstInspection := inspection("migration-terminal-first", "sha256:first-inspection")
	first, err := NewFirstMigrationLifecycle(FirstMigrationLifecycleOptions{
		State: state, Retirement: retirement,
		Inspect: func(context.Context, releaseinstall.InstallRequest) (FirstMigrationInspection, error) {
			return firstInspection, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := first.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rolledBack, _, err := state.ReadMigration(ctx, firstInspection.MigrationID)
	if err != nil || !rolledBack.FreshInventoryRequired || !rolledBack.RollbackCompleted {
		t.Fatalf("terminal rollback journal = %+v, %v", rolledBack, err)
	}
	secondInspection := inspection("migration-terminal-second", "sha256:changed-legacy-metadata")
	inspections := 0
	second, err := NewFirstMigrationLifecycle(FirstMigrationLifecycleOptions{
		State: state, Retirement: retirement,
		Inspect: func(context.Context, releaseinstall.InstallRequest) (FirstMigrationInspection, error) {
			inspections++
			return secondInspection, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	current, _, err := state.ReadCurrentMigration(ctx)
	if err != nil || current.MigrationID != secondInspection.MigrationID || inspections != 1 {
		t.Fatalf("fresh retry selector = %+v, inspections=%d, err=%v", current, inspections, err)
	}
	if len(operations.calls) != 0 {
		t.Fatalf("migration rollback invoked a lifecycle operation: calls=%v", operations.calls)
	}
}

func TestHostInstallReadinessFailureRollsBackConnectorsAndSelectedMigration(t *testing.T) {
	ctx := context.Background()
	migration, state, _, request, _ := newFirstMigrationTestLifecycle(t)
	connectorsRecorder := &connectorRecorder{}
	connectors, err := NewHostInstallHooks(connectorDriversForCatalog(connectorsRecorder))
	if err != nil {
		t.Fatal(err)
	}
	readinessFailure := errors.New("candidate admission unavailable")
	host, err := NewHostInstallLifecycle(connectors, migration, func(context.Context, releaseinstall.InstalledRelease) error {
		return readinessFailure
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	release := releaseinstall.InstalledRelease{
		Role: releaseinstall.RoleHost, Root: request.SourceRoot,
		Executable: filepath.Join(request.SourceRoot, "bin", "agent-sessions"),
	}
	if err := host.Ready(ctx, release); !errors.Is(err, readinessFailure) {
		t.Fatalf("readiness error = %v, want %v", err, readinessFailure)
	}
	if err := host.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	journal, _, err := state.ReadMigration(ctx, "migration-install-first")
	if err != nil || journal.State != MigrationStateRetryRequired || !journal.RollbackCompleted {
		t.Fatalf("release readiness rollback journal = %+v, %v", journal, err)
	}
	wantConnectorTail := []string{"rollback:qwen", "rollback:grok", "rollback:claude", "rollback:codex"}
	if len(connectorsRecorder.calls) < len(wantConnectorTail) {
		t.Fatalf("connector rollback calls = %v", connectorsRecorder.calls)
	}
	gotTail := connectorsRecorder.calls[len(connectorsRecorder.calls)-len(wantConnectorTail):]
	for index := range wantConnectorTail {
		if gotTail[index] != wantConnectorTail[index] {
			t.Fatalf("connector rollback tail = %v, want %v", gotTail, wantConnectorTail)
		}
	}
}

func newFirstMigrationTestLifecycle(
	t *testing.T,
) (*FirstMigrationLifecycle, *StateStore, *recordingLegacyRetirementLifecycle, releaseinstall.InstallRequest, string) {
	t.Helper()
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := operatorStoppedRetirementTestCandidate("supervisor")
	operations := newRecordingLegacyRetirementLifecycle(candidate)
	clock := int64(1_800_000_000_000)
	retirement, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: operations,
		Now: func() time.Time { clock++; return time.UnixMilli(clock) },
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(root, "release")
	executable := filepath.Join(sourceRoot, "bin", "agent-sessions")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("candidate-runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetIdentity, err := hostExecutableIdentity(executable)
	if err != nil {
		t.Fatal(err)
	}
	request := releaseinstall.InstallRequest{
		Version: "0.3.0", ContentIdentity: "sha256:release", SourceRoot: sourceRoot, Executable: "agent-sessions",
	}
	inspection := FirstMigrationInspection{
		Required: true, MigrationID: "migration-install-first", FromVersions: []string{"0.2.4"},
		Candidates: []LegacyRuntimeCandidate{candidate},
		Adoption:   completeLegacyAdoptionRequest(t),
	}
	lifecycle, err := NewFirstMigrationLifecycle(FirstMigrationLifecycleOptions{
		State: state, Retirement: retirement,
		Inspect: func(context.Context, releaseinstall.InstallRequest) (FirstMigrationInspection, error) {
			return inspection, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle, state, operations, request, targetIdentity
}

func operatorStoppedRetirementTestCandidate(kind string) LegacyRuntimeCandidate {
	candidate := retirementTestCandidate(kind, 0)
	candidate.RuntimeIdentity = ""
	candidate.PID = 0
	candidate.ProcStart = ""
	candidate.StrongStart = ""
	candidate.ProcessStatus = "absent"
	candidate.ProcessExecutable = ""
	candidate.ProcessArgvRole = ""
	candidate.ProcessArguments = nil
	candidate.ProcessEnvironment = nil
	candidate.EndpointPath = ""
	candidate.EndpointStatus = "absent"
	candidate.EndpointType = ""
	candidate.EndpointOwnerUID = 0
	candidate.EndpointOwnerPID = 0
	candidate.EndpointOwnerStart = ""
	candidate.EndpointRuntimeIdentity = ""
	candidate.Classification = LegacyClassificationStale
	return candidate
}
