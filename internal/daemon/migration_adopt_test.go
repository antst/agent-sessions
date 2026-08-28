package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

const migrationVendorContentCanary = "T077_VENDOR_PROFILE_CONTENT_MUST_NOT_BE_ADOPTED_70f8d23c"
const legacyAdoptionTestMigrationID = "migration-adoption-test"

func TestLegacyAdoptionStagesThenAtomicallyCommitsCatalogGroupsNamesHubAndDebt(t *testing.T) {
	ctx := context.Background()
	stateRoot := filepath.Join(t.TempDir(), "target-state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request := completeLegacyAdoptionRequest(t)
	plan, err := StageLegacyAdoption(ctx, request)
	if err != nil {
		t.Fatalf("stage legacy adoption: %v", err)
	}
	if plan.Revision == "" || plan.SourceRevision != request.SourceRevision {
		t.Fatalf("staged adoption omitted revision provenance: %+v", plan)
	}
	wantSessions := append([]LegacySessionRecord(nil), request.Sessions...)
	sort.Slice(wantSessions, func(i, j int) bool {
		return wantSessions[i].Product+wantSessions[i].SessionID < wantSessions[j].Product+wantSessions[j].SessionID
	})
	wantNames := append([]LegacySessionName(nil), request.Names...)
	sort.Slice(wantNames, func(i, j int) bool {
		return wantNames[i].Product+wantNames[i].SessionID < wantNames[j].Product+wantNames[j].SessionID
	})
	if !reflect.DeepEqual(plan.Snapshot.Sessions, wantSessions) || !reflect.DeepEqual(plan.Snapshot.Names, wantNames) {
		t.Fatalf("staged session catalog/name projection = %#v / %#v", plan.Snapshot.Sessions, plan.Snapshot.Names)
	}
	if _, loadErr := LoadAdoptedState(ctx, state); loadErr == nil {
		t.Fatal("staging published adoption into the target state before commit")
	}
	selectLegacyAdoptionTestMigration(t, ctx, state, legacyAdoptionTestMigrationID)

	result, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, plan)
	if err != nil {
		t.Fatalf("commit legacy adoption: %v", err)
	}
	if result.PlanRevision != plan.Revision || result.StateRevision == "" || result.AlreadyCommitted {
		t.Fatalf("adoption result = %+v", result)
	}
	wantConfigurationCount := 0
	if request.Configuration != nil {
		wantConfigurationCount = 1
	}
	wantCounts := map[string]int{
		"sessions": len(request.Sessions), "names": len(request.Names), "lanes": len(request.Lanes),
		"deliveries": len(request.Deliveries), "delivery_cursors": len(request.DeliveryCursors),
		"delivery_notices": len(request.DeliveryNotices), "preparations": len(request.Preparations),
		"configuration": wantConfigurationCount,
		"turns":         len(request.Turns), "notices": len(request.Notices), "debt": len(request.Debt),
	}
	if !reflect.DeepEqual(result.AdoptedCounts, wantCounts) {
		t.Fatalf("adopted counts = %#v, want %#v", result.AdoptedCounts, wantCounts)
	}

	if _, loadErr := LoadAdoptedState(ctx, state); !os.IsNotExist(loadErr) {
		t.Fatalf("pre-authority adoption became globally visible: %v", loadErr)
	}
	commitLegacyAdoptionTestAuthority(t, ctx, state, legacyAdoptionTestMigrationID)
	loaded, err := LoadAdoptedState(ctx, state)
	if err != nil {
		t.Fatalf("load committed adoption: %v", err)
	}
	if !reflect.DeepEqual(loaded, plan.Snapshot) {
		t.Fatalf("committed adoption differs from validated stage:\n got: %#v\nwant: %#v", loaded, plan.Snapshot)
	}
	if loaded.SourceRevision != request.SourceRevision || loaded.HostID != "host-stable" ||
		loaded.Hub.HubAddress != "hub.example.test:7443" || loaded.Hub.ProtocolVersion != federation.ProtocolVersion {
		t.Fatalf("adoption lost provenance/host/hub identity: %+v", loaded)
	}
	sessionCatalog, _, err := state.ReadSessionCatalog(ctx)
	if err != nil || !reflect.DeepEqual(sessionCatalog.Sessions, plan.Snapshot.Sessions) ||
		!reflect.DeepEqual(sessionCatalog.Names, plan.Snapshot.Names) {
		t.Fatalf("durable session catalog/name projection = %+v, %v", sessionCatalog, err)
	}
	for _, session := range loaded.Sessions {
		if session.SessionID == "parent-session" {
			if !reflect.DeepEqual(session.ExplicitGroups, []string{"project-alpha", "reviewers"}) ||
				session.PermissionMode != "dontAsk" || session.DeliveryCursor != "delivery-message-41" {
				t.Fatalf("parent catalog/group/permission/cursor changed: %+v", session)
			}
		}
		if session.SessionID == "child-session" {
			if session.ParentSessionID != "parent-session" || session.ParentHostID != "host-stable" ||
				!session.InheritParentGroups || !reflect.DeepEqual(session.InheritedGroups, []string{"project-alpha"}) {
				t.Fatalf("child parent/group inheritance changed: %+v", session)
			}
		}
	}
	attachments, _, attachmentErr := state.readAttachmentCatalog(ctx)
	if attachmentErr != nil && !os.IsNotExist(attachmentErr) {
		t.Fatal(attachmentErr)
	}
	if len(attachments.Attachments) != 0 {
		t.Fatalf("durable metadata adoption created %d live attachment records", len(attachments.Attachments))
	}
	hub, _, err := state.ReadFederation(ctx)
	if err != nil || !reflect.DeepEqual(hub, plan.Snapshot.Hub) {
		t.Fatalf("durable adopted hub state = %+v, %v", hub, err)
	}
	for _, wantDebt := range request.Debt {
		gotDebt, _, readErr := state.ReadDebt(ctx, wantDebt.DebtID)
		if readErr != nil || !reflect.DeepEqual(gotDebt, wantDebt) {
			t.Fatalf("durable adopted debt %q = %+v, %v", wantDebt.DebtID, gotDebt, readErr)
		}
	}

	retried, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, plan)
	if err != nil || !retried.AlreadyCommitted || retried.StateRevision != result.StateRevision {
		t.Fatalf("idempotent adoption retry = %+v, %v", retried, err)
	}
}

func TestLegacyAdoptionPreservesCompletedLaneCursorNoticeAndArchiveWithoutRedispatch(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "target-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request := completeLegacyAdoptionRequest(t)
	plan, err := StageLegacyAdoption(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	selectLegacyAdoptionTestMigration(t, ctx, state, legacyAdoptionTestMigrationID)
	if _, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, plan); err != nil {
		t.Fatal(err)
	}
	commitLegacyAdoptionTestAuthority(t, ctx, state, legacyAdoptionTestMigrationID)
	catalog, _, err := state.readLaneCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.Lanes, plan.Snapshot.Lanes) ||
		!reflect.DeepEqual(catalog.Turns, plan.Snapshot.Turns) ||
		!reflect.DeepEqual(catalog.Notices, plan.Snapshot.Notices) {
		t.Fatalf("adopted lane catalog changed:\n got: %#v\nwant: %#v", catalog, plan.Snapshot)
	}
	if len(catalog.Lanes) != 2 || len(catalog.Turns) != 2 || len(catalog.Notices) != 2 {
		t.Fatalf("adopted lane inventory = %#v", catalog)
	}
	for _, lane := range catalog.Lanes {
		if lane.ActiveTurnID != "" || (lane.State != LaneStateIdle && lane.State != LaneStateArchived) {
			t.Fatalf("adoption created live or redispatchable lane state: %+v", lane)
		}
		if lane.LaneSessionID == "lane-completed" && (lane.CollectionCursor != "turn-completed:3" || lane.ArchiveRevision != 0) {
			t.Fatalf("completed lane cursor changed: %+v", lane)
		}
		if lane.LaneSessionID == "lane-archived" && (lane.CollectionCursor != "turn-interrupted:2" || lane.ArchiveRevision != 7) {
			t.Fatalf("archived lane cursor/revision changed: %+v", lane)
		}
	}
	for _, turn := range catalog.Turns {
		if turn.DispatchState != LaneDispatchCollected || turn.CollectionRevision == 0 || turn.CollectedAt == 0 ||
			turn.TerminalNoticeID == "" || (turn.TerminalOutcome != LaneDispatchCompleted && turn.TerminalOutcome != LaneDispatchInterrupted) {
			t.Fatalf("terminal adopted turn became ambiguous or redispatchable: %+v", turn)
		}
	}
	for _, notice := range catalog.Notices {
		if notice.NoticeID == "" || notice.ParentHostID != "host-stable" || notice.ParentSessionID != "parent-session" {
			t.Fatalf("terminal notice lost exact parent pointer: %+v", notice)
		}
	}
}

func TestLegacyAdoptionRejectsLiveOrIncompleteStateBeforeAnyMutation(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "target-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*LegacyAdoptionRequest)
	}{
		{name: "live lane", mutate: func(request *LegacyAdoptionRequest) {
			request.Lanes[0].State = LaneStateRunning
			request.Lanes[0].ActiveTurnID = request.Turns[0].TurnID
			request.Turns[0].DispatchState = LaneDispatchRunning
			request.Turns[0].TerminalOutcome = ""
		}},
		{name: "notice loses parent", mutate: func(request *LegacyAdoptionRequest) {
			request.Notices[0].ParentSessionID = ""
		}},
		{name: "hub loses stable identity", mutate: func(request *LegacyAdoptionRequest) {
			request.Hub.HostID = ""
		}},
		{name: "debt loses prohibited scope", mutate: func(request *LegacyAdoptionRequest) {
			request.Debt[0].ProhibitedScope = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := completeLegacyAdoptionRequest(t)
			test.mutate(&request)
			if _, err := StageLegacyAdoption(ctx, request); err == nil {
				t.Fatal("invalid legacy state produced an adoption plan")
			}
			if _, loadErr := LoadAdoptedState(ctx, state); loadErr == nil {
				t.Fatal("invalid stage mutated the target state")
			}
		})
	}
}

func TestLegacyAdoptionConflictingRetryRefusesWithoutChangingCommittedState(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "target-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request := completeLegacyAdoptionRequest(t)
	plan, err := StageLegacyAdoption(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	selectLegacyAdoptionTestMigration(t, ctx, state, legacyAdoptionTestMigrationID)
	if _, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, plan); err != nil {
		t.Fatal(err)
	}
	commitLegacyAdoptionTestAuthority(t, ctx, state, legacyAdoptionTestMigrationID)
	before, err := LoadAdoptedState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Names = append([]LegacySessionName(nil), request.Names...)
	changed.Names[0].Name = "changed-after-stage"
	changedPlan, err := StageLegacyAdoption(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, changedPlan); !errors.Is(err, ErrMigrationAdoptionConflict) {
		t.Fatalf("conflicting same-source adoption = %v, want revision conflict", err)
	}
	after, err := LoadAdoptedState(ctx, state)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("conflicting adoption changed committed state:\n before: %#v\n after: %#v\nerror: %v", before, after, err)
	}
}

func TestRolledBackAdoptionAllowsChangedFreshProcessMigrationPlan(t *testing.T) {
	ctx := context.Background()
	stateRoot := filepath.Join(t.TempDir(), "target-state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	firstID := "migration-adoption-rolled-back"
	firstPlan, err := StageLegacyAdoption(ctx, completeLegacyAdoptionRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	selectLegacyAdoptionTestMigration(t, ctx, state, firstID)
	if err := state.savePreparedFirstMigration(ctx, firstID, firstPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitLegacyAdoption(ctx, state, firstID, firstPlan); err != nil {
		t.Fatal(err)
	}
	terminallyRollbackLegacyAdoptionTestMigration(t, ctx, state, firstID)
	if _, err := LoadAdoptedState(ctx, state); !os.IsNotExist(err) {
		t.Fatalf("rolled-back adoption remained globally visible: %v", err)
	}

	// Simulate a crash after the terminal journal CAS but before the adoption
	// tombstone, then let a fresh daemon process finish that exact rollback.
	state, err = OpenStateStoreExisting(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewFirstMigrationRecovery(FirstMigrationRecoveryOptions{
		State: state, Retirement: &LegacyRetirementEngine{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Recover(ctx, "sha256:unused-rolled-back-runtime", 1, 1_800_000_000_100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.readCommittedLegacyAdoption(ctx, firstID); !os.IsNotExist(err) {
		t.Fatalf("fresh recovery did not tombstone rolled-back adoption: %v", err)
	}

	// The next fresh-process inspection may now select changed metadata.
	changed := completeLegacyAdoptionRequest(t)
	changed.SourceRevision = "legacy-state-revision-changed-after-rollback"
	changed.Names[0].Name = "changed-after-terminal-rollback"
	changedPlan, err := StageLegacyAdoption(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	secondID := "migration-adoption-fresh-retry"
	putLegacyAdoptionTestMigration(t, ctx, state, secondID)
	_, selectorRevision, err := state.ReadCurrentMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, selectorRevision, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: secondID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.savePreparedFirstMigration(ctx, secondID, changedPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitLegacyAdoption(ctx, state, secondID, changedPlan); err != nil {
		t.Fatalf("changed fresh migration adoption conflicted with rolled-back plan: %v", err)
	}
	commitLegacyAdoptionTestAuthority(t, ctx, state, secondID)
	loaded, err := LoadAdoptedState(ctx, state)
	if err != nil || !reflect.DeepEqual(loaded, changedPlan.Snapshot) {
		t.Fatalf("fresh migration projection = %#v, %v", loaded, err)
	}
}

func TestAdoptionRollbackWrongRevisionPreservesExactAndOtherMigration(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "target-state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	firstID, otherID := "migration-adoption-exact", "migration-adoption-other"
	firstPlan, err := StageLegacyAdoption(ctx, completeLegacyAdoptionRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	otherRequest := completeLegacyAdoptionRequest(t)
	otherRequest.SourceRevision = "other-migration-source-revision"
	otherRequest.Names[0].Name = "other-migration-name"
	otherPlan, err := StageLegacyAdoption(ctx, otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	selectLegacyAdoptionTestMigration(t, ctx, state, firstID)
	putLegacyAdoptionTestMigration(t, ctx, state, otherID)
	for _, staged := range []struct {
		id   string
		plan LegacyAdoptionPlan
	}{{firstID, firstPlan}, {otherID, otherPlan}} {
		if err := state.savePreparedFirstMigration(ctx, staged.id, staged.plan); err != nil {
			t.Fatal(err)
		}
		if _, err := CommitLegacyAdoption(ctx, state, staged.id, staged.plan); err != nil {
			t.Fatal(err)
		}
	}
	terminallyRollbackLegacyAdoptionTestMigration(t, ctx, state, firstID)
	var otherBefore committedLegacyAdoption
	otherRevision, err := state.records.Read(ctx, legacyAdoptionRecordKey(otherID), &otherBefore)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.rollbackPreparedFirstMigration(ctx, firstID, otherPlan.Revision); !errors.Is(err, ErrMigrationAdoptionConflict) {
		t.Fatalf("wrong-revision rollback = %v, want adoption conflict", err)
	}
	first, _, err := state.readCommittedLegacyAdoption(ctx, firstID)
	if err != nil || first.RolledBack {
		t.Fatalf("wrong-revision rollback changed exact adoption: %+v, %v", first, err)
	}
	if err := state.rollbackPreparedFirstMigration(ctx, firstID, firstPlan.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.readCommittedLegacyAdoption(ctx, firstID); !os.IsNotExist(err) {
		t.Fatalf("exact rolled-back adoption remains selected: %v", err)
	}
	var otherAfter committedLegacyAdoption
	afterRevision, err := state.records.Read(ctx, legacyAdoptionRecordKey(otherID), &otherAfter)
	if err != nil || afterRevision != otherRevision || !reflect.DeepEqual(otherAfter, otherBefore) {
		t.Fatalf("rollback changed other migration: before=%+v/%d after=%+v/%d err=%v",
			otherBefore, otherRevision, otherAfter, afterRevision, err)
	}
}

func TestLegacyAdoptionExcludesVendorProfilesTranscriptsCredentialsAndCaches(t *testing.T) {
	ctx := context.Background()
	fixtureRoot := t.TempDir()
	vendorRoots := []string{
		filepath.Join(fixtureRoot, ".codex"), filepath.Join(fixtureRoot, ".claude"),
		filepath.Join(fixtureRoot, ".grok"), filepath.Join(fixtureRoot, ".qwen"),
	}
	for _, root := range vendorRoots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"credentials.json", "profile.json", "transcript.jsonl", "history.db", "cache.bin"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(migrationVendorContentCanary+"-"+name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(root, 0o000); err != nil {
			t.Fatal(err)
		}
		root := root
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	}
	request := completeLegacyAdoptionRequest(t)
	request.ExcludedPaths = append([]string(nil), vendorRoots...)
	plan, err := StageLegacyAdoption(ctx, request)
	if err != nil {
		t.Fatalf("staging inspected an excluded vendor profile: %v", err)
	}
	wantExcluded := append([]string(nil), vendorRoots...)
	sort.Strings(wantExcluded)
	if !reflect.DeepEqual(plan.ExcludedPaths, wantExcluded) {
		t.Fatalf("vendor exclusions = %q, want %q", plan.ExcludedPaths, wantExcluded)
	}
	for _, target := range plan.TargetPaths {
		for _, excluded := range vendorRoots {
			if target == excluded || strings.HasPrefix(target, excluded+string(filepath.Separator)) {
				t.Fatalf("adoption plan targets vendor-owned path %q", target)
			}
		}
	}
	targetRoot := filepath.Join(fixtureRoot, "target-state")
	state, err := OpenStateStore(targetRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitLegacyAdoption(ctx, state, legacyAdoptionTestMigrationID, plan); err != nil {
		t.Fatal(err)
	}
	planBody, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planBody), migrationVendorContentCanary) {
		t.Fatal("validated adoption plan retained vendor content")
	}
	if err := filepath.Walk(targetRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(body), migrationVendorContentCanary) {
				t.Errorf("target state copied vendor content into %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func selectLegacyAdoptionTestMigration(t *testing.T, ctx context.Context, state *StateStore, migrationID string) {
	t.Helper()
	putLegacyAdoptionTestMigration(t, ctx, state, migrationID)
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
	}); err != nil {
		t.Fatal(err)
	}
}

func putLegacyAdoptionTestMigration(t *testing.T, ctx context.Context, state *StateStore, migrationID string) {
	t.Helper()
	const candidateID = "candidate-verified-absent"
	journal := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
		FromVersions: []string{"legacy-test"}, TargetRuntimeIdentity: "sha256:adoption-test-runtime",
		State: MigrationStateAdopting, MaintenanceWindowState: MaintenanceWindowLegacyAbsenceVerified,
		Candidates: []string{candidateID}, VerifiedAbsentAuthorities: []string{candidateID},
		Revision: 1, StartedAt: 1_800_000_000_000, UpdatedAt: 1_800_000_000_000,
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, journal); err != nil {
		t.Fatal(err)
	}
}

func commitLegacyAdoptionTestAuthority(t *testing.T, ctx context.Context, state *StateStore, migrationID string) {
	t.Helper()
	if err := CommitFirstMigrationAuthority(
		ctx, state, migrationID, "sha256:adoption-test-runtime", 7, 1_800_000_000_001,
	); err != nil {
		t.Fatal(err)
	}
}

func terminallyRollbackLegacyAdoptionTestMigration(
	t *testing.T,
	ctx context.Context,
	state *StateStore,
	migrationID string,
) {
	t.Helper()
	journal, revision, err := state.ReadMigration(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	journal.State = MigrationStateRetryRequired
	journal.MaintenanceWindowState = MaintenanceWindowUnverified
	journal.SuccessorStateDurable = false
	journal.AuthorityGeneration = 0
	journal.VerifiedAbsentAuthorities = nil
	journal.RetiredCandidateIDs = nil
	journal.RollbackCompleted = true
	journal.FreshInventoryRequired = true
	journal.RollbackCause = "successor_not_ready"
	journal.Revision++
	journal.UpdatedAt++
	if _, err := state.CompareAndSwapMigration(ctx, revision, journal); err != nil {
		t.Fatal(err)
	}
}

func completeLegacyAdoptionRequest(t *testing.T) LegacyAdoptionRequest {
	t.Helper()
	now := int64(1_800_000_000_000)
	header := func(revision uint64) RecordHeader {
		return RecordHeader{
			SchemaVersion: HostRuntimeSchemaVersion, Revision: revision, Generation: 4,
			CreatedAt: now - 1_000, UpdatedAt: now,
		}
	}
	sessions := []LegacySessionRecord{
		{
			SessionID: "child-session", Product: "qwen", Kind: federation.SessionKindLane,
			ExplicitGroups: []string{"child-explicit"}, InheritedGroups: []string{"project-alpha"},
			ParentSessionID: "parent-session", ParentHostID: "host-stable", InheritParentGroups: true,
			PermissionMode: "default", DeliveryCursor: "delivery-child-9", UpdatedAt: now,
		},
		{
			SessionID: "parent-session", Product: "claude", Kind: federation.SessionKindInteractive,
			ExplicitGroups: []string{"project-alpha", "reviewers"}, PermissionMode: "dontAsk",
			DeliveryCursor: "delivery-message-41", UpdatedAt: now - 100,
		},
	}
	names := []LegacySessionName{
		{SessionID: "parent-session", Product: "claude", Kind: federation.SessionKindInteractive, Name: "reviewer", NameSource: "explicit", UpdatedAt: now - 100},
		{SessionID: "child-session", Product: "qwen", Kind: federation.SessionKindLane, Name: "review-lane", NameSource: "legacy_lane", UpdatedAt: now},
	}
	lanes := []LaneRecord{
		{
			RecordHeader: header(5), LaneSessionID: "lane-completed", Name: "completed", Product: "codex",
			ParentHostID: "host-stable", ParentSessionID: "parent-session", ParentGroups: []string{"project-alpha", "reviewers"},
			InheritParentGroups: true, Groups: []string{"project-alpha"}, PermissionMode: "default", Cwd: "/workspace",
			State: LaneStateIdle, CollectionCursor: "turn-completed:3",
		},
		{
			RecordHeader: header(7), LaneSessionID: "lane-archived", Name: "archived", Product: "qwen",
			ParentHostID: "host-stable", ParentSessionID: "parent-session", ParentGroups: []string{"project-alpha"},
			Groups: []string{"archive"}, PermissionMode: "default", Cwd: "/workspace",
			State: LaneStateArchived, CollectionCursor: "turn-interrupted:2", ArchiveRevision: 7,
			CleanupDebtIDs: []string{"debt-lane-cleanup"},
		},
	}
	turns := []LaneTurnRecord{
		{
			RecordHeader: header(3), TurnID: "turn-completed", LaneSessionID: "lane-completed",
			ParentContextRevision: 2, RequestDigest: "sha256:turn-completed", DispatchState: LaneDispatchCollected,
			NativeTurnIdentity: map[string]any{"thread_id": "thread-c", "turn_id": "native-c"},
			TerminalOutcome:    LaneDispatchCompleted, ResultReference: map[string]any{"native_result_id": "result-c"},
			TerminalNoticeID: "notice-completed", CollectionRevision: 3, CollectedAt: now - 30,
		},
		{
			RecordHeader: header(2), TurnID: "turn-interrupted", LaneSessionID: "lane-archived",
			ParentContextRevision: 4, RequestDigest: "sha256:turn-interrupted", DispatchState: LaneDispatchCollected,
			NativeTurnIdentity: map[string]any{"session_id": "native-q"}, TerminalOutcome: LaneDispatchInterrupted,
			ResultReference: map[string]any{"native_result_id": "result-q"}, TerminalNoticeID: "notice-interrupted",
			CollectionRevision: 2, CollectedAt: now - 20,
		},
	}
	notices := []LaneNotice{
		{RecordHeader: header(1), NoticeID: "notice-completed", LaneSessionID: "lane-completed", TurnID: "turn-completed", ParentHostID: "host-stable", ParentSessionID: "parent-session", Outcome: LaneDispatchCompleted},
		{RecordHeader: header(1), NoticeID: "notice-interrupted", LaneSessionID: "lane-archived", TurnID: "turn-interrupted", ParentHostID: "host-stable", ParentSessionID: "parent-session", Outcome: LaneDispatchInterrupted},
	}
	hub := FederationStateRecord{
		RecordHeader: header(8), HostID: "host-stable", HostName: "workstation", HubAddress: "hub.example.test:7443",
		ConnectionGeneration: 19, ProtocolVersion: federation.ProtocolVersion,
		AdvertisedCapabilities: []string{"claude-lane", "qwen-lane"}, State: "reconnecting",
		AdvertisedRuntimeVersion: "legacy-host-release", AdvertisedRuntimeIdentity: "sha256:legacy-host",
		AdvertisedProducts: []string{"claude", "qwen"}, RemoteRosterRevision: "roster-31",
		LastConnectedAt: now - 10_000, LastErrorCode: "hub_connection_lost",
	}
	debt := []DebtRecord{
		{
			RecordHeader: header(1), DebtID: "debt-lane-cleanup", Operation: "cleanup",
			ResourceKind: "legacy_lane_runtime", ResourceIdentity: "lane-archived/native-q",
			ExpectedRevision: "sha256:expected", ObservedRevision: "sha256:changed", CauseCode: "identity_changed",
			CauseDetail: "native cleanup identity must be re-observed", RetryPredicate: "exact identity matches",
			ProhibitedScope: "do not remove vendor profile or signal an unverified process",
		},
	}
	return LegacyAdoptionRequest{
		SourceRevision: "sha256:legacy-state-revision-77", HostID: "host-stable",
		Sessions: sessions, Names: names, Lanes: lanes, Turns: turns, Notices: notices, Hub: hub, Debt: debt,
	}
}
