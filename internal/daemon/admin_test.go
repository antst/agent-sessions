package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestHostStatusSchemaCarriesExactAuthorityAndIsolatesOptionalAdapters(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("start host runtime with an unavailable optional adapter: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	projection := runtime.StatusProjection()
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	assertExactStringSet(t, "status JSON fields", mapKeys(status), clihelp.Contract().JSONResultFields["status"])

	wantIdentity := map[string]any{
		"runtime_version":  "0.3.0-test",
		"runtime_identity": "sha256:t023-runtime",
		"generation":       float64(1),
		"pid":              float64(4242),
		"proc_start":       "t023-process-start",
		"endpoint":         runtime.options.Paths.ControlEndpoint,
	}
	for field, want := range wantIdentity {
		if got := status[field]; !reflect.DeepEqual(got, want) {
			t.Errorf("status %s = %#v, want exact identity %#v", field, got, want)
		}
	}
	service, serviceOK := status["service"].(map[string]any)
	if !serviceOK || service["manager"] != "systemd-user" || service["unit"] != "agent-sessions.service" {
		t.Errorf("status service identity = %#v, want exact systemd user unit", status["service"])
	}

	products, ok := status["products"].(map[string]any)
	if !ok {
		t.Fatalf("status products = %#v, want one independently reported object per product", status["products"])
	}
	wantProducts := make([]string, 0, len(productcatalog.Catalog().Products))
	for _, product := range productcatalog.Catalog().Products {
		wantProducts = append(wantProducts, product.ID)
	}
	assertExactStringSet(t, "status products", mapKeys(products), wantProducts)

	allowedStates := []string{"not_installed", "installed_unready", "ready", "degraded"}
	for _, product := range wantProducts {
		entry, entryOK := products[product].(map[string]any)
		if !entryOK {
			t.Errorf("status product %s = %#v, want independent adapter projection", product, products[product])
			continue
		}
		state, _ := entry["state"].(string)
		if !slices.Contains(allowedStates, state) {
			t.Errorf("status product %s state = %q, want one of %q", product, state, allowedStates)
		}
		if product == "qwen" && state != "not_installed" {
			t.Errorf("missing explicit Qwen executable state = %q, want not_installed", state)
		}
		if product != "qwen" && state == "not_installed" {
			t.Errorf("present %s executable was coupled to unavailable Qwen adapter", product)
		}
	}
	if runtime.Admission() != AdmissionReady {
		t.Fatalf("optional product availability prevented daemon admission: %s", runtime.Admission())
	}
}

func TestHostDoctorSchemaIsCompleteAndReadOnly(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	projection := runtime.DoctorProjection()
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var doctor map[string]any
	if err := json.Unmarshal(body, &doctor); err != nil {
		t.Fatal(err)
	}
	assertExactStringSet(t, "doctor JSON fields", mapKeys(doctor), clihelp.Contract().JSONResultFields["doctor"])
	if healthy, ok := doctor["healthy"].(bool); !ok || !healthy {
		t.Errorf("doctor healthy = %#v; absent optional products must not make the daemon unhealthy", doctor["healthy"])
	}

	checks, ok := doctor["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatalf("doctor checks = %#v, want complete read-only checks", doctor["checks"])
	}
	checkIDs := make([]string, 0, len(checks))
	for _, raw := range checks {
		check, checkOK := raw.(map[string]any)
		if !checkOK {
			t.Fatalf("doctor check = %#v, want object", raw)
		}
		id, _ := check["id"].(string)
		checkIDs = append(checkIDs, id)
	}
	for _, category := range []string{"service", "identity", "state", "product", "federation", "debt"} {
		if !containsSubstring(checkIDs, category) {
			t.Errorf("doctor checks %q omit required %s diagnosis", checkIDs, category)
		}
	}
}

func TestAdministrativeOperationsAreCompleteAndAbsentFromMCPConnectorRole(t *testing.T) {
	wantAdmin := []string{"migration.inspect", "migration.status", "remove.inspect", "runtime.doctor", "runtime.status"}
	assertExactStringSet(t, "admin operations", setKeys(controlRoleOperations[controlRoleAdmin]), wantAdmin)

	wantConnector := []string{
		"attachment.context",
		"lane.archive", "lane.collect", "lane.followup", "lane.interrupt", "lane.list",
		"lane.command", "lane.resume", "lane.start", "lane.status",
		"mcp.forward",
		"peer.broadcast", "peer.discover", "peer.identity", "peer.inbox", "peer.rename", "peer.send",
	}
	// Vendor MCP tools are admitted to the daemon as the connector role. Keep
	// this inventory exact so adding an admin operation to tools/list cannot be
	// hidden behind a broader connector permission set.
	gotConnector := setKeys(controlRoleOperations[controlRoleConnector])
	assertExactStringSet(t, "MCP connector operations", gotConnector, wantConnector)
	for _, operation := range gotConnector {
		if strings.HasPrefix(operation, "runtime.") || strings.HasPrefix(operation, "migration.") ||
			strings.HasPrefix(operation, "remove.") || strings.HasPrefix(operation, "purge.") {
			t.Errorf("model-facing MCP connector exposes administration operation %q", operation)
		}
	}
}

func TestMigrationAdministrationProjectsOnlyTheAuthoritativeSelectedTransaction(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	ctx := context.Background()

	emptyInspect, err := runtime.MigrationInspectProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if emptyInspect.Revision != 0 || emptyInspect.Candidates == nil || emptyInspect.Blockers == nil || emptyInspect.Debt == nil ||
		len(emptyInspect.Candidates)+len(emptyInspect.Blockers)+len(emptyInspect.Debt) != 0 {
		t.Fatalf("empty migration inspection = %+v, want explicit empty metadata collections", emptyInspect)
	}
	emptyStatus, err := runtime.MigrationStatusProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if emptyStatus.Transaction != nil || emptyStatus.State != "none" || emptyStatus.NextAction != "none" {
		t.Fatalf("empty migration status = %+v, want exact no-transaction projection", emptyStatus)
	}

	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-active-supervisor",
		Kind: "supervisor", SourcePath: "/state/legacy/supervisor.json",
		RelatedSessionIDs: []string{"session-owner-work"},
		Classification:    LegacyClassificationActiveManagedBlocker,
		EvidenceRevision:  7, LastObservedAt: 100,
	}
	if err := runtime.options.State.putMigrationCandidate(ctx, "migration-admin-blocked", candidate); err != nil {
		t.Fatal(err)
	}
	blocker := LegacyMigrationBlocker{
		SchemaVersion: MigrationSchemaVersion, Revision: 1, BlockerID: "blocker-owner-work",
		CandidateID: candidate.CandidateID, Kind: candidate.Kind, ResourceType: "peer",
		ResourceID: "session-owner-work", RequiredAction: "close_before_retry",
		EvidenceRevision: candidate.EvidenceRevision, LastObservedAt: 100,
	}
	transaction := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-admin-blocked",
		FromVersions: []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:unified-runtime",
		State: MigrationStateBlockedActivePeerOrLane, Candidates: []string{candidate.CandidateID},
		ActiveManagedBlockers: []LegacyMigrationBlocker{blocker}, Revision: 3, StartedAt: 90, UpdatedAt: 100,
		MaintenanceWindowState: MaintenanceWindowBlocked,
	}
	if _, err := runtime.options.State.CompareAndSwapMigration(ctx, 0, transaction); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.options.State.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: transaction.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}

	inspect, err := runtime.MigrationInspectProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inspect.Revision != transaction.Revision || !reflect.DeepEqual(inspect.Candidates, []LegacyRuntimeCandidate{candidate}) ||
		!reflect.DeepEqual(inspect.Blockers, []LegacyMigrationBlocker{blocker}) || inspect.Debt == nil || len(inspect.Debt) != 0 {
		t.Fatalf("selected migration inspection = %+v", inspect)
	}
	status, err := runtime.MigrationStatusProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Transaction == nil || !reflect.DeepEqual(*status.Transaction, transaction) ||
		status.State != string(MigrationStateBlockedActivePeerOrLane) ||
		status.NextAction != "close peer session-owner-work; then retry the same supported install or upgrade command" {
		t.Fatalf("selected migration status = %+v", status)
	}
	offlineInspect, err := MigrationInspectProjectionFromInspection(ctx, FirstMigrationInspection{
		Required: true, MigrationID: transaction.MigrationID, ExpectedRevision: 13,
		Candidates: []LegacyRuntimeCandidate{candidate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if offlineInspect.Revision != 13 || !reflect.DeepEqual(offlineInspect.Candidates, []LegacyRuntimeCandidate{candidate}) ||
		len(offlineInspect.Blockers) != 1 || offlineInspect.Blockers[0].CandidateID != candidate.CandidateID ||
		offlineInspect.Blockers[0].ResourceType != "peer" || offlineInspect.Blockers[0].ResourceID != "session-owner-work" ||
		len(offlineInspect.Debt) != 0 {
		t.Fatalf("offline migration inspection = %+v, want the shared T080/T081 evidence", offlineInspect)
	}

	for name, value := range map[string]any{"migrate.inspect": inspect, "migrate.status": status} {
		body, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var object map[string]any
		if unmarshalErr := json.Unmarshal(body, &object); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		assertExactStringSet(t, name+" JSON fields", mapKeys(object), clihelp.Contract().JSONResultFields[name])
	}
}

func TestMigrationAdministrationNamesLiveLegacyAuthorityBlockers(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-live-supervisor",
		Kind: "supervisor", SourcePath: "/state/legacy/supervisor.json",
		Classification:   LegacyClassificationLiveLegacyAuthority,
		EvidenceRevision: 9, LastObservedAt: 101,
	}
	if err := state.putMigrationCandidate(ctx, "migration-live-authority", candidate); err != nil {
		t.Fatal(err)
	}
	blocker := legacyMigrationBlocker(candidate, "authority", candidate.CandidateID)
	transaction := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-live-authority",
		FromVersions: []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:unified-runtime",
		State: MigrationStateBlockedLiveAuthority, Candidates: []string{candidate.CandidateID},
		LiveAuthorityBlockers:  []LegacyMigrationBlocker{blocker},
		MaintenanceWindowState: MaintenanceWindowBlocked,
		Revision:               1, StartedAt: 100, UpdatedAt: 101,
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, transaction); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: transaction.MigrationID,
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{options: RuntimeOptions{State: state}}
	inspect, err := runtime.MigrationInspectProjection(ctx)
	if err != nil || !reflect.DeepEqual(inspect.Blockers, []LegacyMigrationBlocker{blocker}) {
		t.Fatalf("live-authority inspect = %+v, %v", inspect, err)
	}
	status, err := runtime.MigrationStatusProjection(ctx)
	if err != nil || !strings.Contains(status.NextAction, candidate.CandidateID) ||
		!strings.Contains(status.NextAction, "old supported lifecycle") ||
		!strings.Contains(status.NextAction, "keep every legacy launch path held") {
		t.Fatalf("live-authority status = %+v, %v", status, err)
	}
}

func TestOfflineMigrationStatusReadsExistingStateWithoutCreatingAHostRoot(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "ams-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	stateBase := filepath.Join(base, "state-home")
	runtimeBase := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config-home"))
	t.Setenv("XDG_STATE_HOME", stateBase)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	missingRoot := filepath.Join(stateBase, stateDirectoryName)
	status, err := RunHostMigrationStatusCLI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Transaction != nil || status.State != "none" || status.NextAction != "none" {
		t.Fatalf("status without unified state = %+v", status)
	}
	if _, statErr := os.Lstat(missingRoot); !os.IsNotExist(statErr) {
		t.Fatalf("offline status created or changed absent state root: %v", statErr)
	}
}

func TestOfflineMigrationInspectUsesProductionInventoryWithoutCreatingUnifiedState(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "ami-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	home := filepath.Join(base, "home")
	stateBase := filepath.Join(base, "state-home")
	runtimeBase := filepath.Join(base, "runtime")
	tempBase := filepath.Join(base, "tmp")
	for _, path := range []string{home, runtimeBase, tempBase} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", stateBase)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	t.Setenv("TMPDIR", tempBase)

	projection, err := RunHostMigrationInspectCLI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if projection.Revision != 0 || projection.Candidates == nil || projection.Blockers == nil || projection.Debt == nil ||
		len(projection.Candidates)+len(projection.Blockers)+len(projection.Debt) != 0 {
		t.Fatalf("clean production migration inspection = %+v", projection)
	}
	if _, statErr := os.Lstat(filepath.Join(stateBase, stateDirectoryName)); !os.IsNotExist(statErr) {
		t.Fatalf("offline migration inspection created unified state: %v", statErr)
	}
}

func TestMigrationInspectionDebtCarriesExactRetryPredicateAndStatusGuidance(t *testing.T) {
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-unknown-endpoint",
		Kind: "supervisor", SourcePath: "/state/legacy/unknown.sock",
		Classification: LegacyClassificationUnknown, EvidenceRevision: 9, LastObservedAt: 200,
	}
	projection, err := MigrationInspectProjectionFromInspection(context.Background(), FirstMigrationInspection{
		Required: true, MigrationID: "migration-unknown", Candidates: []LegacyRuntimeCandidate{candidate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Blockers) != 0 || len(projection.Debt) != 1 ||
		projection.Debt[0].CandidateID != candidate.CandidateID || projection.Debt[0].Code != "unknown_identity" ||
		projection.Debt[0].RetryPredicate != "reinventory_exact_candidate" ||
		projection.Debt[0].ProhibitedScope != "stop_or_retire:"+candidate.CandidateID {
		t.Fatalf("unknown-identity migration inspection = %+v", projection)
	}
	transaction := MigrationTransaction{State: MigrationStateDebt}
	if action := migrationNextAction(transaction, projection.Debt); !strings.Contains(action, "without mutation") ||
		!strings.Contains(action, "reobserve only the recorded process and path") ||
		!strings.Contains(action, "do not delete, replace, or signal") {
		t.Fatalf("debt next action = %q", action)
	}
}

func TestMigrationAdministrationPreservesSpecificIncompleteAndIncompatibleFailures(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	ctx := context.Background()
	dispatch := runtimeAdminDispatch(runtime)

	if _, err := runtime.options.State.records.CompareAndSwap(ctx, migrationCurrentRecordKey, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-missing-journal",
	}); err != nil {
		t.Fatal(err)
	}
	_, failure := dispatch(ctx, controlPrincipal{Role: controlRoleAdmin}, controlRequest{Operation: "migration.status"})
	if failure == nil || failure.Code != "migration_state_incomplete" || !failure.Retryable ||
		!strings.Contains(failure.NextAction, "exact selected records") {
		t.Fatalf("missing selected migration failure = %+v", failure)
	}

	other := newAdminTestRuntime(t)
	if _, err := other.options.State.records.CompareAndSwap(ctx, migrationCurrentRecordKey, 0, map[string]any{
		"schema_version": MigrationSchemaVersion + 1, "migration_id": "migration-incompatible-selector",
	}); err != nil {
		t.Fatal(err)
	}
	_, failure = runtimeAdminDispatch(other)(ctx, controlPrincipal{Role: controlRoleAdmin}, controlRequest{Operation: "migration.inspect"})
	if failure == nil || failure.Code != "migration_state_incompatible" || failure.Retryable ||
		!strings.Contains(failure.NextAction, "compatible Agent Sessions release") {
		t.Fatalf("incompatible selected migration failure = %+v", failure)
	}
}

func newAdminTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"),
		ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot:         stateRoot,
		RuntimeRoot:       filepath.Join(root, "run"),
		ControlEndpoint:   filepath.Join(root, "run", "daemon.sock"),
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	overrides := make(map[string]ProductOverride)
	for _, product := range productcatalog.Catalog().Products {
		executable := filepath.Join(bin, product.ID)
		if product.ID != "qwen" {
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		overrides[product.ID] = ProductOverride{Executable: executable}
	}
	configuration := DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: "t023-host", HostName: "t023-builder",
		ProductOverrides: overrides, StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot,
		Revision: 1, UpdatedAt: 1,
	}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, Configuration: configuration, State: state,
		RuntimeVersion: "0.3.0-test", RuntimeIdentity: "sha256:t023-runtime", PID: 4242,
		ProcStart: "t023-process-start", StrongStart: "t023-strong-start",
		ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: func() time.Time { return time.UnixMilli(1000) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func setKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func assertExactStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want exactly %q", label, got, want)
	}
}

func containsSubstring(values []string, substring string) bool {
	for _, value := range values {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
