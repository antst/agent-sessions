package daemon

import (
	"context"
	"strings"
	"testing"
)

func TestMigrationTransactionMaintenanceWindowStateCoupling(t *testing.T) {
	valid := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion,
		MigrationID:   "migration-schema-coupling",
		FromVersions:  []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:unified",
		State: MigrationStateLegacyAbsenceVerified, Candidates: []string{"candidate-supervisor"},
		MaintenanceWindowState:    MaintenanceWindowLegacyAbsenceVerified,
		VerifiedAbsentAuthorities: []string{"candidate-supervisor"},
		Revision:                  1, StartedAt: 10, UpdatedAt: 10,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*MigrationTransaction)
	}{
		{name: "arbitrary maintenance value", mutate: func(record *MigrationTransaction) {
			record.MaintenanceWindowState = "invented"
		}},
		{name: "absence phase unverified", mutate: func(record *MigrationTransaction) {
			record.MaintenanceWindowState = MaintenanceWindowUnverified
		}},
		{name: "absence phase missing IDs", mutate: func(record *MigrationTransaction) {
			record.VerifiedAbsentAuthorities = nil
		}},
		{name: "absence phase wrong IDs", mutate: func(record *MigrationTransaction) {
			record.VerifiedAbsentAuthorities = []string{"candidate-other"}
		}},
		{name: "retry retains verified authority", mutate: func(record *MigrationTransaction) {
			record.State = MigrationStateRetryRequired
			record.FreshInventoryRequired = true
		}},
		{name: "blocked claims verified absence", mutate: func(record *MigrationTransaction) {
			record.State = MigrationStateBlockedUnknownIdentity
			record.CleanupDebtIDs = []string{"debt-supervisor"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := valid.clone()
			test.mutate(&record)
			if err := record.Validate(); err == nil {
				t.Fatalf("invalid coupled state validated: %+v", record)
			}
		})
	}
}

func TestMigrationTransactionLiveAuthorityBlockersAreExactAndCloned(t *testing.T) {
	blocker := LegacyMigrationBlocker{
		SchemaVersion: MigrationSchemaVersion, Revision: 1,
		BlockerID: "blocker-live-supervisor", CandidateID: "candidate-supervisor",
		Kind: "supervisor", ResourceType: "authority", ResourceID: "candidate-supervisor",
		RequiredAction: "close_before_retry", EvidenceRevision: 2, LastObservedAt: 20,
	}
	record := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-live-schema",
		FromVersions: []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:unified",
		State: MigrationStateBlockedLiveAuthority, Candidates: []string{"candidate-supervisor"},
		LiveAuthorityBlockers:  []LegacyMigrationBlocker{blocker},
		MaintenanceWindowState: MaintenanceWindowBlocked,
		Revision:               1, StartedAt: 20, UpdatedAt: 20,
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := record.clone()
	clone.LiveAuthorityBlockers[0].Kind = "changed"
	if record.LiveAuthorityBlockers[0].Kind != "supervisor" {
		t.Fatal("migration clone shared live-authority blocker storage")
	}
	orphan := record.clone()
	orphan.LiveAuthorityBlockers[0].CandidateID = "candidate-other"
	orphan.LiveAuthorityBlockers[0].ResourceID = "candidate-other"
	if err := orphan.Validate(); err == nil {
		t.Fatal("noncandidate live-authority blocker validated")
	}
}

func TestMigrationStateStoreRejectsLiveAuthorityBlockerEvidenceMismatch(t *testing.T) {
	ctx := context.Background()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "candidate-supervisor",
		Kind: "supervisor", SourcePath: "/legacy/supervisor.json",
		Classification:   LegacyClassificationLiveLegacyAuthority,
		EvidenceRevision: 2, LastObservedAt: 20,
	}
	if err := state.putMigrationCandidate(ctx, "migration-blocker-mismatch", candidate); err != nil {
		t.Fatal(err)
	}
	blocker := legacyMigrationBlocker(candidate, "authority", candidate.CandidateID)
	blocker.Kind = "different-authority"
	record := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: "migration-blocker-mismatch",
		FromVersions: []string{"0.2.4"}, TargetRuntimeIdentity: "sha256:unified",
		State: MigrationStateBlockedLiveAuthority, Candidates: []string{candidate.CandidateID},
		LiveAuthorityBlockers:  []LegacyMigrationBlocker{blocker},
		MaintenanceWindowState: MaintenanceWindowBlocked,
		Revision:               1, StartedAt: 20, UpdatedAt: 20,
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, record); err == nil ||
		!strings.Contains(err.Error(), "does not match exact candidate evidence") {
		t.Fatalf("mismatched live-authority blocker write = %v", err)
	}
}
