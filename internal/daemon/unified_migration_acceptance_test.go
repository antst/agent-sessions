package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestUnifiedMigrationAcceptance(t *testing.T) {
	if os.Getenv("AGENT_SESSIONS_UNIFIED_MIGRATION_ACCEPTANCE") != "1" {
		t.Skip("set AGENT_SESSIONS_UNIFIED_MIGRATION_ACCEPTANCE=1 through scripts/test-unified-migration")
	}
	ctx := context.Background()
	root := migrationAcceptanceRoot(t)
	inventory := migrationAcceptanceInventory(t, root)
	state, err := OpenStateStore(filepath.Join(root, "unified-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newMigrationAcceptanceLifecycle(t)
	t.Cleanup(lifecycle.cleanup)

	supervisor := lifecycle.startAuthority(t, "supervisor", filepath.Join(root, "legacy-runtime", "supervisor.sock"), inventory[0].Path)
	agent := lifecycle.startAuthority(t, "host_federation_agent", filepath.Join(root, "legacy-runtime", "agent.sock"), inventory[1].Path)
	blocker := lifecycle.startAuthority(t, "qwen_lane_manager", filepath.Join(root, "legacy-runtime", "qwen-lane.sock"), inventory[0].Path)
	blockerEvidence := blocker.evidence
	blockerEvidence.RelatedLaneIDs = []string{"fixture-qwen-lane"}
	blockerCandidate, err := ClassifyLegacyCandidate(blockerEvidence)
	if err != nil {
		t.Fatal(err)
	}
	staleEvidence := migrationAcceptanceAbsentEvidence("shim", filepath.Join(root, "legacy-state", "stale-shim.json"))
	staleEvidence.ReportedActiveCount = 37
	if err := os.WriteFile(staleEvidence.SourcePath, []byte(`{"fixture":"stale-shim"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := ClassifyLegacyCandidate(staleEvidence)
	if err != nil {
		t.Fatal(err)
	}
	excluded, err := ClassifyLegacyCandidate(LegacyCandidateEvidence{
		CandidateID: "candidate-native-qwen", Kind: "native_qwen",
		SourcePath: filepath.Join(root, "vendor-profiles", ".qwen"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelated := lifecycle.startUnrelated(t)
	vendorCanaries := migrationAcceptanceVendorCanaries(t, root)

	candidates := []LegacyRuntimeCandidate{supervisor.candidate, agent.candidate, blockerCandidate, stale, excluded}
	before := migrationAcceptanceTreeSnapshot(t, root)
	report, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: candidates})
	if !errors.Is(err, ErrLegacyQuiescenceBlocked) || report.LegacyAbsenceVerified || len(report.Blockers) != 3 ||
		!migrationAcceptanceHasBlocker(report, supervisor.candidate.CandidateID, "authority", supervisor.candidate.CandidateID) ||
		!migrationAcceptanceHasBlocker(report, agent.candidate.CandidateID, "authority", agent.candidate.CandidateID) ||
		!migrationAcceptanceHasBlocker(report, blockerCandidate.CandidateID, "lane", "fixture-qwen-lane") ||
		lifecycle.installerLegacyActionCount() != 0 {
		t.Fatalf("blocked read-only quiescence = report=%+v err=%v calls=%v", report, err, lifecycle.callsSnapshot())
	}
	after := migrationAcceptanceTreeSnapshot(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("blocked migration mutated its fixture:\nbefore=%v\nafter=%v", before, after)
	}

	// The operator, not the installer, establishes the maintenance window by
	// stopping every responsive legacy authority. This fixture intentionally
	// leaves each exact Agent Sessions record for post-commit retirement.
	closedSupervisor := lifecycle.stopForOperator(t, supervisor)
	closedAgent := lifecycle.stopForOperator(t, agent)
	closedBlocker := lifecycle.stopForOperator(t, blocker)
	candidates = []LegacyRuntimeCandidate{closedSupervisor, closedAgent, closedBlocker, stale, excluded}
	report, err = EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: candidates})
	if err != nil || !report.LegacyAbsenceVerified || len(report.Blockers) != 0 || len(report.Debt) != 0 {
		t.Fatalf("quiescent migration gate = %+v, %v", report, err)
	}

	// A replacement launch during the operator-held window must make the next
	// inventory fail closed. The installer still performs no lifecycle action.
	replacement := lifecycle.startAuthority(
		t,
		"claude_lane_manager",
		filepath.Join(root, "legacy-runtime", "replacement-lane.sock"),
		inventory[0].Path,
	)
	replacementEvidence := replacement.evidence
	replacementEvidence.RelatedLaneIDs = []string{"fixture-replacement-lane"}
	replacementCandidate, err := ClassifyLegacyCandidate(replacementEvidence)
	if err != nil {
		t.Fatal(err)
	}
	replacementReport, replacementErr := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{
		Candidates: append(append([]LegacyRuntimeCandidate(nil), candidates...), replacementCandidate),
	})
	if !errors.Is(replacementErr, ErrLegacyQuiescenceBlocked) || replacementReport.LegacyAbsenceVerified ||
		!migrationAcceptanceHasBlocker(
			replacementReport,
			replacementCandidate.CandidateID,
			"lane",
			"fixture-replacement-lane",
		) || lifecycle.installerLegacyActionCount() != 0 {
		t.Fatalf("replacement launch was not refused read-only: report=%+v err=%v calls=%v",
			replacementReport, replacementErr, lifecycle.callsSnapshot())
	}
	lifecycle.stopForOperator(t, replacement)

	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: lifecycle, Now: migrationAcceptanceClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(MigrationStateAdopting)
	migrationID := "unified-migration-acceptance"
	request := LegacyRetirementRequest{
		MigrationID: migrationID, TargetRuntimeIdentity: "sha256:unified-fixture-runtime",
		FromVersions: []string{"0.2.3", "0.2.4"}, Candidates: candidates,
	}
	journal, _, err := engine.PrepareRetirement(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID); !errors.Is(err, ErrInjectedMigrationCrash) {
		t.Fatalf("expected crash after verified operator-owned absence, got %v", err)
	}
	if lifecycle.installerLegacyActionCount() != 0 {
		t.Fatalf("installer acted on legacy authority after crash: calls=%v", lifecycle.callsSnapshot())
	}
	recovered, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{
		State: state, Lifecycle: lifecycle, Now: migrationAcceptanceClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveryResult, err := recovered.Recover(ctx, migrationID)
	if err != nil || !recoveryResult.AuthoritiesStopped || lifecycle.installerLegacyActionCount() != 0 {
		t.Fatalf("absence recovery dispatched legacy lifecycle work: result=%+v err=%v calls=%v",
			recoveryResult, err, lifecycle.callsSnapshot())
	}

	adoptionRequest := migrationAcceptanceAdoptionRequest(root)
	adoptionPlan, err := StageLegacyAdoption(ctx, adoptionRequest)
	if err != nil {
		t.Fatal(err)
	}
	adoption, err := CommitLegacyAdoption(ctx, state, migrationID, adoptionPlan)
	if err != nil || adoption.AlreadyCommitted || adoption.StateRevision == "" {
		t.Fatalf("migration adoption = %+v, %v", adoption, err)
	}
	if retry, retryErr := CommitLegacyAdoption(ctx, state, migrationID, adoptionPlan); retryErr != nil || !retry.AlreadyCommitted {
		t.Fatalf("migration adoption retry = %+v, %v", retry, retryErr)
	}

	unifiedEndpoint := filepath.Join(root, "current-runtime", "agent-sessions.sock")
	unifiedListener := migrationAcceptanceUnixListener(t, unifiedEndpoint)
	t.Cleanup(func() { migrationAcceptanceCloseListener(unifiedListener, unifiedEndpoint) })
	selectAndCommitMigrationAuthorityForAcceptance(t, ctx, state, migrationID, request.TargetRuntimeIdentity, 9)
	retired, err := recovered.RetireArtifacts(ctx, migrationID)
	if err != nil || !retired.Complete || !retired.ArtifactsRetired || retired.Ready {
		t.Fatalf("legacy retirement = %+v, %v", retired, err)
	}
	journal, _, err = state.ReadMigration(ctx, migrationID)
	if err != nil || journal.State != MigrationStateComplete || journal.AuthorityGeneration != 9 ||
		len(journal.RetiredCandidateIDs) != 4 {
		t.Fatalf("completed migration journal = %+v, %v", journal, err)
	}
	if got := migrationAcceptanceSocketPaths(t, root); !reflect.DeepEqual(got, []string{unifiedEndpoint}) {
		t.Fatalf("post-migration endpoints = %q, want only %q", got, unifiedEndpoint)
	}
	if !migrationAcceptanceProcessAlive(t, unrelated.Process.Pid) {
		t.Fatal("unrelated test-owned process was stopped")
	}
	migrationAcceptanceAssertVendorCanaries(t, vendorCanaries)
	if lifecycle.operatorStopCount() != 4 || lifecycle.installerLegacyActionCount() != 0 ||
		lifecycle.retireCount(closedBlocker.CandidateID) != 1 ||
		lifecycle.wasMutated(excluded.CandidateID) {
		t.Fatalf("migration used legacy lifecycle or missed exact retirement: calls=%v", lifecycle.callsSnapshot())
	}

	migrationAcceptanceRollbackDiscriminator(t, root)
	fmt.Println(`{"type":"unified.migration.fixture.passed","blocker_zero_mutation":true,"operator_maintenance_window":true,"replacement_launch_refused":true,"live_handoff":false,"compatibility_drain":false,"installer_legacy_process_actions":0,"operator_authorities_stopped":4,"adoption_commits":1,"legacy_artifacts_retired":4,"recovery":true,"migration_rollback_process_actions":0,"rollback_retry_required":true,"current_daemon_endpoints":1,"vendor_profiles_mutated":0,"unrelated_processes_signalled":0}`)
}

type migrationAcceptanceAuthority struct {
	candidate LegacyRuntimeCandidate
	evidence  LegacyCandidateEvidence
	command   *exec.Cmd
	listener  *net.UnixListener
	waited    bool
	mu        sync.Mutex
}

func (authority *migrationAcceptanceAuthority) stopForOperator(t *testing.T) {
	t.Helper()
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.waited {
		return
	}
	if err := authority.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := authority.command.Wait(); err != nil && !migrationAcceptanceExpectedSignal(err) {
		t.Fatal(err)
	}
	authority.waited = true
	if authority.listener != nil {
		_ = authority.listener.Close()
		authority.listener = nil
	}
	if err := os.Remove(authority.candidate.EndpointPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

type migrationAcceptanceLifecycle struct {
	t             *testing.T
	mu            sync.Mutex
	authorities   map[string]*migrationAcceptanceAuthority
	calls         []string
	retires       map[string]int
	operatorStops int
	unrelated     []*exec.Cmd
}

func newMigrationAcceptanceLifecycle(t *testing.T) *migrationAcceptanceLifecycle {
	return &migrationAcceptanceLifecycle{
		t: t, authorities: make(map[string]*migrationAcceptanceAuthority),
		retires: make(map[string]int),
	}
}

func (fixture *migrationAcceptanceLifecycle) stopForOperator(
	t *testing.T,
	authority *migrationAcceptanceAuthority,
) LegacyRuntimeCandidate {
	t.Helper()
	authority.stopForOperator(t)
	fixture.mu.Lock()
	fixture.operatorStops++
	fixture.mu.Unlock()
	evidence := authority.evidence
	evidence.Process.Status = "absent"
	evidence.Endpoint.Status = "absent"
	evidence.RelatedSessionIDs = nil
	evidence.RelatedLaneIDs = nil
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil || candidate.Classification != LegacyClassificationStale {
		t.Fatalf("operator-stopped candidate = %+v, %v", candidate, err)
	}
	return candidate
}

func (fixture *migrationAcceptanceLifecycle) startAuthority(
	t *testing.T,
	kind, endpointPath, sourceRoot string,
) *migrationAcceptanceAuthority {
	t.Helper()
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceRoot, "candidate-"+kind+".json")
	if err := os.WriteFile(sourcePath, []byte(`{"fixture":"legacy-authority"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(sourceRoot, "bin", "agent-session-runtime")
	if kind == "host_federation_agent" {
		executable = filepath.Join(sourceRoot, "bin", "peer-federator")
	}
	migrationAcceptanceMaterializeExecutable(t, executable)
	command := exec.Command(executable, "60")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := migrationAcceptanceObserveProcess(t, command.Process.Pid)
	listener := migrationAcceptanceUnixListener(t, endpointPath)
	evidence := LegacyCandidateEvidence{
		CandidateID: "candidate-" + kind, Kind: kind, SourcePath: sourcePath,
		SourceRevision: "fixture-source-r1", ReportedVersion: "legacy-fixture",
		RuntimeIdentity: "sha256:fixture-" + kind, PID: process.PID,
		ProcStart: process.Start, StrongStart: process.StrongStart, EvidenceRevision: 1, ObservedAt: 1_800_000_000_000,
		Process: LegacyProcessObservation{
			Status: "known", PID: process.PID, ProcStart: process.Start, StrongStart: process.StrongStart,
			Executable: executable, ArgvRole: kind,
		},
		Endpoint: LegacyEndpointObservation{
			Status: "responsive", Path: endpointPath, Type: "unix", OwnerUID: os.Getuid(),
			OwnerPID: process.PID, OwnerProcStart: process.Start, RuntimeIdentity: "sha256:fixture-" + kind,
		},
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil || candidate.Classification != LegacyClassificationLiveLegacyAuthority {
		_ = command.Process.Kill()
		_ = command.Wait()
		migrationAcceptanceCloseListener(listener, endpointPath)
		t.Fatalf("fixture candidate %s = %+v, %v", kind, candidate, err)
	}
	authority := &migrationAcceptanceAuthority{
		candidate: candidate, evidence: evidence, command: command, listener: listener,
	}
	fixture.mu.Lock()
	fixture.authorities[candidate.CandidateID] = authority
	fixture.mu.Unlock()
	return authority
}

func (fixture *migrationAcceptanceLifecycle) startUnrelated(t *testing.T) *exec.Cmd {
	t.Helper()
	command := exec.Command("sleep", "60")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.unrelated = append(fixture.unrelated, command)
	fixture.mu.Unlock()
	return command
}

func (fixture *migrationAcceptanceLifecycle) ReattestEndpoint(
	_ context.Context,
	candidate LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, error) {
	fixture.record("reattest-endpoint", candidate.CandidateID)
	authority := fixture.authority(candidate.CandidateID)
	if authority != nil && !migrationAcceptanceSameCandidate(candidate, authority.candidate) {
		return LegacyRuntimeCandidate{}, errors.New("fixture endpoint identity changed")
	}
	info, err := os.Lstat(candidate.EndpointPath)
	if os.IsNotExist(err) {
		observed := candidate
		observed.ProcessStatus = "absent"
		observed.EndpointStatus = "absent"
		return observed, nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return LegacyRuntimeCandidate{}, errors.New("fixture endpoint is not the exact unix socket")
	}
	return candidate, nil
}

func (fixture *migrationAcceptanceLifecycle) RetireEndpoint(
	_ context.Context,
	candidate LegacyRuntimeCandidate,
) error {
	fixture.record("retire-endpoint", candidate.CandidateID)
	observed, err := fixture.ReattestEndpoint(context.Background(), candidate)
	if err != nil || observed.ProcessStatus != "absent" || observed.EndpointStatus != "absent" {
		if err == nil {
			err = errors.New("fixture retirement observed a live legacy artifact")
		}
		return err
	}
	for _, path := range []string{candidate.EndpointPath, candidate.SourcePath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fixture.mu.Lock()
	fixture.retires[candidate.CandidateID]++
	fixture.mu.Unlock()
	return nil
}

func (fixture *migrationAcceptanceLifecycle) record(operation, identity string) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.calls = append(fixture.calls, operation+":"+identity)
}

func (fixture *migrationAcceptanceLifecycle) authority(id string) *migrationAcceptanceAuthority {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.authorities[id]
}

func (fixture *migrationAcceptanceLifecycle) installerLegacyActionCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, call := range fixture.calls {
		if !strings.HasPrefix(call, "reattest-endpoint:") &&
			!strings.HasPrefix(call, "retire-endpoint:") {
			return 1
		}
	}
	return 0
}

func (fixture *migrationAcceptanceLifecycle) operatorStopCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.operatorStops
}

func (fixture *migrationAcceptanceLifecycle) retireCount(id string) int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.retires[id]
}

func (fixture *migrationAcceptanceLifecycle) wasMutated(id string) bool {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.retires[id] != 0
}

func (fixture *migrationAcceptanceLifecycle) callsSnapshot() []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]string(nil), fixture.calls...)
}

func (fixture *migrationAcceptanceLifecycle) cleanup() {
	fixture.mu.Lock()
	authorities := make([]*migrationAcceptanceAuthority, 0, len(fixture.authorities))
	for _, authority := range fixture.authorities {
		authorities = append(authorities, authority)
	}
	unrelated := append([]*exec.Cmd(nil), fixture.unrelated...)
	fixture.mu.Unlock()
	for _, authority := range authorities {
		authority.mu.Lock()
		if !authority.waited && authority.command != nil && authority.command.Process != nil {
			_ = authority.command.Process.Kill()
			_ = authority.command.Wait()
			authority.waited = true
		}
		if authority.listener != nil {
			_ = authority.listener.Close()
		}
		_ = os.Remove(authority.candidate.EndpointPath)
		authority.mu.Unlock()
	}
	for _, command := range unrelated {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}
}

func migrationAcceptanceRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("AGENT_SESSIONS_UNIFIED_MIGRATION_TEST_ROOT")
	if !migrationAbsoluteCleanPath(root) || !strings.HasPrefix(filepath.Base(filepath.Dir(root)), "asm.") {
		t.Fatalf("migration acceptance root %q is not the script-owned compact root", root)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func migrationAcceptanceInventory(t *testing.T, root string) []LegacyInventorySource {
	t.Helper()
	options := LegacyInventoryOptions{
		Platform: runtime.GOOS, UID: os.Getuid(), HomeDir: filepath.Join(root, "home"),
		StateHome: filepath.Join(root, "legacy-state"), RuntimeDir: filepath.Join(root, "run"),
		SystemTempDir: filepath.Join(root, "tmp"),
	}
	if runtime.GOOS == "darwin" {
		options.RuntimeDir = ""
		options.RecordedRuntimeRoots = []string{filepath.Join(options.SystemTempDir, fmt.Sprintf("ccp-%d", os.Getuid()))}
	}
	for _, path := range []string{options.HomeDir, options.StateHome, options.RuntimeDir, options.SystemTempDir} {
		if path != "" {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	sources, err := LegacyInventorySources(options)
	if err != nil || len(sources) < 8 {
		t.Fatalf("closed legacy inventory = %v, %v", sources, err)
	}
	for _, source := range sources {
		if source.Kind == "service" {
			if err := os.MkdirAll(filepath.Dir(source.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.Path, []byte("fixture service\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(source.Path, 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(source.Path, ".codex") || strings.Contains(source.Path, ".claude") ||
			strings.Contains(source.Path, ".grok") || strings.Contains(source.Path, ".qwen") {
			t.Fatalf("closed inventory entered vendor profile %q", source.Path)
		}
	}
	return sources
}

func migrationAcceptanceAbsentEvidence(kind, sourcePath string) LegacyCandidateEvidence {
	return LegacyCandidateEvidence{
		CandidateID: "candidate-" + kind, Kind: kind, SourcePath: sourcePath,
		RuntimeIdentity: "sha256:fixture-" + kind, PID: 99123,
		ProcStart: "absent-start", StrongStart: "absent-strong", EvidenceRevision: 1, ObservedAt: 1_800_000_000_000,
		Process:  LegacyProcessObservation{Status: "absent"},
		Endpoint: LegacyEndpointObservation{Status: "absent", Path: filepath.Join(filepath.Dir(sourcePath), kind+".sock")},
	}
}

func migrationAcceptanceAdoptionRequest(root string) LegacyAdoptionRequest {
	now := int64(1_800_000_000_000)
	header := func(revision uint64) RecordHeader {
		return RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: revision, Generation: 9, CreatedAt: now - 1_000, UpdatedAt: now}
	}
	return LegacyAdoptionRequest{
		SourceRevision: "sha256:fixture-legacy-state-r17", HostID: "fixture-host",
		Sessions: []LegacySessionRecord{
			{SessionID: "fixture-parent", Product: "claude", Kind: federation.SessionKindInteractive, ExplicitGroups: []string{"fixture-group"}, PermissionMode: "dontAsk", DeliveryCursor: "message-7", UpdatedAt: now},
			{SessionID: "fixture-lane", Product: "qwen", Kind: federation.SessionKindLane, ExplicitGroups: []string{"fixture-child"}, InheritedGroups: []string{"fixture-group"}, ParentSessionID: "fixture-parent", ParentHostID: "fixture-host", InheritParentGroups: true, PermissionMode: "default", DeliveryCursor: "message-9", UpdatedAt: now},
		},
		Names: []LegacySessionName{
			{SessionID: "fixture-parent", Product: "claude", Kind: federation.SessionKindInteractive, Name: "fixture-parent", NameSource: "explicit", UpdatedAt: now},
			{SessionID: "fixture-lane", Product: "qwen", Kind: federation.SessionKindLane, Name: "fixture-lane", NameSource: "legacy_lane", UpdatedAt: now},
		},
		Lanes: []LaneRecord{{
			RecordHeader: header(3), LaneSessionID: "fixture-lane", Name: "fixture-lane", Product: "qwen",
			ParentHostID: "fixture-host", ParentSessionID: "fixture-parent", ParentGroups: []string{"fixture-group"},
			InheritParentGroups: true, Groups: []string{"fixture-group"}, PermissionMode: "default",
			Cwd: filepath.Join(root, "workspace"), State: LaneStateArchived,
			CollectionCursor: "fixture-turn:2", ArchiveRevision: 3, CleanupDebtIDs: []string{"fixture-cleanup-debt"},
		}},
		Turns: []LaneTurnRecord{{
			RecordHeader: header(2), TurnID: "fixture-turn", LaneSessionID: "fixture-lane",
			ParentContextRevision: 1, RequestDigest: "sha256:fixture-turn", DispatchState: LaneDispatchCollected,
			NativeTurnIdentity: map[string]any{"session_id": "native-fixture"}, TerminalOutcome: LaneDispatchInterrupted,
			ResultReference: map[string]any{"native_result_id": "native-result"}, TerminalNoticeID: "fixture-notice",
			CollectionRevision: 2, CollectedAt: now,
		}},
		Notices: []LaneNotice{{
			RecordHeader: header(1), NoticeID: "fixture-notice", LaneSessionID: "fixture-lane", TurnID: "fixture-turn",
			ParentHostID: "fixture-host", ParentSessionID: "fixture-parent", Outcome: LaneDispatchInterrupted,
		}},
		Hub: FederationStateRecord{
			RecordHeader: header(4), HostID: "fixture-host", HostName: "fixture-host",
			HubAddress: "hub.fixture.test:7443", ConnectionGeneration: 4, ProtocolVersion: federation.ProtocolVersion,
			AdvertisedCapabilities: []string{"qwen-lane"}, State: "reconnecting",
			AdvertisedRuntimeVersion: "legacy-fixture", AdvertisedRuntimeIdentity: "sha256:legacy-fixture",
			AdvertisedProducts: []string{"qwen"}, RemoteRosterRevision: "fixture-roster-r4",
		},
		Debt: []DebtRecord{{
			RecordHeader: header(1), DebtID: "fixture-cleanup-debt", Operation: "cleanup",
			ResourceKind: "legacy_lane", ResourceIdentity: "fixture-lane/native-fixture",
			CauseCode: "identity_changed", RetryPredicate: "reobserve exact native identity",
			ProhibitedScope: "vendor profiles and unrelated processes",
		}},
		ExcludedPaths: []string{
			filepath.Join(root, "vendor-profiles", ".claude"), filepath.Join(root, "vendor-profiles", ".codex"),
			filepath.Join(root, "vendor-profiles", ".grok"), filepath.Join(root, "vendor-profiles", ".qwen"),
		},
	}
}

func selectAndCommitMigrationAuthorityForAcceptance(
	t *testing.T,
	ctx context.Context,
	state *StateStore,
	migrationID string,
	targetRuntimeIdentity string,
	generation uint64,
) {
	t.Helper()
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion,
		MigrationID:   migrationID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := CommitFirstMigrationAuthority(
		ctx,
		state,
		migrationID,
		targetRuntimeIdentity,
		generation,
		1_800_000_000_100,
	); err != nil {
		t.Fatal(err)
	}
}

func migrationAcceptanceRollbackDiscriminator(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	state, err := OpenStateStore(filepath.Join(root, "rollback-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newMigrationAcceptanceLifecycle(t)
	t.Cleanup(lifecycle.cleanup)
	authority := lifecycle.startAuthority(
		t,
		"supervisor",
		filepath.Join(root, "rollback-runtime", "supervisor.sock"),
		filepath.Join(root, "rollback-legacy-state"),
	)
	closedAuthority := lifecycle.stopForOperator(t, authority)
	engine, err := NewLegacyRetirementEngine(LegacyRetirementEngineOptions{State: state, Lifecycle: lifecycle, Now: migrationAcceptanceClock()})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(MigrationStateAdopting)
	request := LegacyRetirementRequest{
		MigrationID: "unified-migration-rollback", TargetRuntimeIdentity: "sha256:rollback-successor",
		FromVersions: []string{"0.2.4"}, Candidates: []LegacyRuntimeCandidate{closedAuthority},
	}
	journal, _, err := engine.PrepareRetirement(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcceptVerifiedLegacyAbsence(ctx, journal.MigrationID); !errors.Is(err, ErrInjectedMigrationCrash) {
		t.Fatalf("rollback setup absence acceptance = %v", err)
	}
	result, err := engine.RollbackMaintenanceWindowBeforeReady(ctx, request.MigrationID)
	if err != nil || !result.Restored || len(result.Debt) != 0 {
		t.Fatalf("before-ready rollback = %+v, %v", result, err)
	}
	journal, _, err = state.ReadMigration(ctx, request.MigrationID)
	if err != nil || journal.State != MigrationStateRetryRequired || !journal.RollbackCompleted ||
		journal.SuccessorStateDurable || journal.MaintenanceWindowState != MaintenanceWindowUnverified {
		t.Fatalf("rollback journal = %+v, %v", journal, err)
	}
	calls := lifecycle.callsSnapshot()
	if len(calls) != 0 || lifecycle.installerLegacyActionCount() != 0 ||
		migrationAcceptanceProcessAlive(t, closedAuthority.PID) {
		t.Fatalf("rollback did not leave unified and legacy authorities stopped: calls=%q", calls)
	}
}

func migrationAcceptanceVendorCanaries(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	for _, product := range []string{".codex", ".claude", ".grok", ".qwen"} {
		path := filepath.Join(root, "vendor-profiles", product, "credential-canary")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte("fixture-vendor-canary-" + product)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		result[path] = body
	}
	return result
}

func migrationAcceptanceAssertVendorCanaries(t *testing.T, canaries map[string][]byte) {
	t.Helper()
	for path, want := range canaries {
		got, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("vendor canary %q changed: body=%q err=%v", path, got, err)
		}
	}
}

func migrationAcceptanceTreeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	result := []string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := relative + "|" + info.Mode().String() + fmt.Sprintf("|%d", info.Size())
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(body)
			entry += "|" + hex.EncodeToString(digest[:])
		}
		result = append(result, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func migrationAcceptanceUnixListener(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	address := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	return listener
}

func migrationAcceptanceMaterializeExecutable(t *testing.T, target string) {
	t.Helper()
	if _, err := os.Stat(target); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, body, 0o700); err != nil {
		t.Fatal(err)
	}
}

func migrationAcceptanceCloseListener(listener *net.UnixListener, path string) {
	if listener != nil {
		_ = listener.Close()
	}
	_ = os.Remove(path)
}

func migrationAcceptanceSocketPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket != 0 {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func migrationAcceptanceObserveProcess(t *testing.T, pid int) procinfo.Process {
	t.Helper()
	info := migrationAcceptanceReadProcess(t, pid)
	process := procinfo.Process{PID: pid, Info: info}
	if process.Status != procinfo.Known || process.Start == "" || process.StrongStart == "" {
		t.Fatalf("test-owned process lacks exact identity: %+v", process)
	}
	return process
}

func migrationAcceptanceProcessAlive(t *testing.T, pid int) bool {
	t.Helper()
	info := migrationAcceptanceReadProcess(t, pid)
	return info.Status == procinfo.Known && info.State != "Z" && info.State != "X"
}

func migrationAcceptanceReadProcess(t *testing.T, pid int) procinfo.Info {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		info := procinfo.Read(pid)
		if info.Status == procinfo.Known || info.Status == procinfo.Absent {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("test-owned process %d remained unobservable", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func migrationAcceptanceSameCandidate(left, right LegacyRuntimeCandidate) bool {
	return left.CandidateID == right.CandidateID && left.PID == right.PID && left.ProcStart == right.ProcStart &&
		left.StrongStart == right.StrongStart && left.RuntimeIdentity == right.RuntimeIdentity &&
		left.EndpointPath == right.EndpointPath
}

func migrationAcceptanceHasBlocker(
	report LegacyQuiescenceReport,
	candidateID string,
	resourceType string,
	resourceID string,
) bool {
	for _, blocker := range report.Blockers {
		if blocker.CandidateID == candidateID && blocker.ResourceType == resourceType &&
			blocker.ResourceID == resourceID {
			return true
		}
	}
	return false
}

func migrationAcceptanceExpectedSignal(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}

func migrationAcceptanceClock() func() time.Time {
	var mu sync.Mutex
	now := time.UnixMilli(1_800_000_000_000)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Millisecond)
		return now
	}
}
