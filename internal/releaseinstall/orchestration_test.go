package releaseinstall

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestInstallerStagesAndValidatesEverythingBeforeNativeMutation(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 2)
	registry, err := NewRegistry(inventory, strategies...)
	if err != nil {
		t.Fatal(err)
	}
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-1", ReleaseID: "release-1"}); err != nil {
		t.Fatal(err)
	}
	for index, state := range states {
		want := []string{"discover", "capture", "plan", "plan", "discover", "stage", "validate", "register", "verify", "abort"}
		if !reflect.DeepEqual(state.calls, want) {
			t.Fatalf("state %d calls = %v, want %v", index, state.calls, want)
		}
	}
	ledger, err := store.ReadLedger()
	if err != nil || len(ledger.Receipts) != 2 {
		t.Fatalf("ledger = %#v, %v", ledger, err)
	}
}

func TestInstallerValidationFailurePrecedesEveryNativeMutation(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 2)
	states[1].failValidate = errors.New("invalid staged integration")
	registry, _ := NewRegistry(inventory, strategies...)
	installer := Installer{Registry: registry, Store: ownershipStoreFixture(t)}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-invalid", ReleaseID: "release"}); err == nil {
		t.Fatal("invalid staged integration was registered")
	}
	for index, state := range states {
		if containsString(state.calls, "register") || state.current != nil {
			t.Fatalf("state %d mutated before all validation completed: calls=%v current=%#v", index, state.calls, state.current)
		}
		if state.aborts == 0 {
			t.Fatalf("state %d staged assets were not aborted", index)
		}
	}
}

func TestInstallerAbortsOnlyAttemptedStagesIncludingPartialFailure(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 3)
	states[0].failStage = errors.New("partial stage failure")
	registry, _ := NewRegistry(inventory, strategies...)
	if _, err := (Installer{Registry: registry, Store: ownershipStoreFixture(t)}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-stage", ReleaseID: "release"}); err == nil {
		t.Fatal("stage failure succeeded")
	}
	if states[0].aborts != 1 || states[1].aborts != 1 || states[2].aborts != 0 {
		t.Fatalf("stage aborts = %d/%d/%d, want 1/1/0", states[0].aborts, states[1].aborts, states[2].aborts)
	}
}

func TestStagingCleanupDebtSurvivesUntilRecoveryVerifiesAbsence(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	states[0].failStage = errors.New("partial stage failure")
	states[0].failAbort = errors.New("abort could not verify cleanup")
	states[0].cleanupErr = errors.New("staged assets still present")
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-cleanup", ReleaseID: "release"}); err == nil {
		t.Fatal("stage and abort failure succeeded")
	}
	journal, err := store.ReadJournal()
	if err != nil || len(journal.Entries) != 1 || !journal.Entries[0].CleanupRequired || journal.Entries[0].Debt != "staging-cleanup-failed" {
		t.Fatalf("cleanup obligation was not durable: %#v, %v", journal, err)
	}
	if err := installer.Recover(context.Background(), inventory); err == nil {
		t.Fatal("recovery cleared cleanup debt without verified absence")
	}
	journal, _ = store.ReadJournal()
	if journal.Schema == "" || !journal.Entries[0].CleanupRequired {
		t.Fatalf("failed cleanup recovery cleared journal: %#v", journal)
	}
	states[0].cleanupErr = nil
	if err := installer.Recover(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	journal, _ = store.ReadJournal()
	if journal.Schema != "" || states[0].cleanupCalls != 2 {
		t.Fatalf("verified cleanup did not converge: journal=%#v calls=%d", journal, states[0].cleanupCalls)
	}
}

func TestPostMutationAbortFailureDoesNotClearOnRestoredNativePrior(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	states[0].failAbort = errors.New("abort could not verify cleanup")
	states[0].cleanupErr = errors.New("staged assets still present")
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-post-abort", ReleaseID: "release"}); err == nil {
		t.Fatal("post-mutation abort failure succeeded")
	}
	if states[0].current != nil {
		t.Fatalf("native prior was not restored: %#v", states[0].current)
	}
	journal, err := store.ReadJournal()
	if err != nil || len(journal.Entries) != 1 || !journal.Entries[0].CleanupRequired {
		t.Fatalf("post-mutation cleanup debt was not retained: %#v, %v", journal, err)
	}
	if err := installer.Recover(context.Background(), inventory); err == nil {
		t.Fatal("native prior caused cleanup debt to be cleared")
	}
	states[0].cleanupErr = nil
	if err := installer.Recover(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	journal, _ = store.ReadJournal()
	if journal.Schema != "" {
		t.Fatalf("verified post-mutation cleanup did not clear journal: %#v", journal)
	}
}

func TestInstallerReturnsStructuredAbsentSkipWithoutMutation(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	inventory[0].NativeRegistration.AssetOnly = false
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	report, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-skip", ReleaseID: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Products, []ProductInstallResult{{ProductID: inventory[0].ID, State: ProductSkippedAbsent}}) {
		t.Fatalf("skip report = %#v", report)
	}
	if containsString(states[0].calls, "capture") || containsString(states[0].calls, "register") {
		t.Fatalf("absent native product was mutated: %v", states[0].calls)
	}
	ledger, _ := store.ReadLedger()
	journal, _ := store.ReadJournal()
	if ledger.Schema != "" || journal.Schema != "" {
		t.Fatalf("absent skip persisted transaction state: %#v %#v", ledger, journal)
	}
}

func TestInstallerReportIsCanonicalAcrossInstalledAndAbsentProducts(t *testing.T) {
	inventory, strategies, _ := syntheticInstallFixture(t, 2)
	inventory[0].NativeRegistration.AssetOnly = false
	registry, _ := NewRegistry(inventory, strategies...)
	report, err := (Installer{Registry: registry, Store: ownershipStoreFixture(t)}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-report", ReleaseID: "release"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ProductInstallResult{
		{ProductID: "claude", State: ProductInstalled},
		{ProductID: "codex", State: ProductSkippedAbsent},
	}
	if !reflect.DeepEqual(report.Products, want) {
		t.Fatalf("mixed install report = %#v, want %#v", report.Products, want)
	}
}

func TestInstallerFailureInjectionRollsBackInReverseIncludingLedgerCommit(t *testing.T) {
	for _, point := range []string{"journal", "register", "verify", "ledger"} {
		t.Run(point, func(t *testing.T) {
			inventory, strategies, states := syntheticInstallFixture(t, 2)
			registry, err := NewRegistry(inventory, strategies...)
			if err != nil {
				t.Fatal(err)
			}
			store := ownershipStoreFixture(t)
			switch point {
			case "journal":
				store.beforeReplace = func(kind string) error {
					if kind == journalFilename {
						return errors.New("journal commit failure")
					}
					return nil
				}
			case "register":
				states[1].failRegister = errors.New("register failure")
			case "verify":
				states[1].failVerify = errors.New("verify failure")
			case "ledger":
				store.beforeReplace = func(kind string) error {
					if kind == ownershipFilename {
						return errors.New("ledger commit failure")
					}
					return nil
				}
			}
			installer := Installer{Registry: registry, Store: store}
			if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-fail", ReleaseID: "release"}); err == nil {
				t.Fatal("injected failure succeeded")
			}
			if states[0].current != nil || states[1].current != nil {
				t.Fatalf("rollback did not restore baselines: %#v %#v", states[0].current, states[1].current)
			}
			if point != "journal" && (states[0].aborts == 0 || states[1].aborts == 0) {
				t.Fatalf("failure %s left staged assets: aborts=%d/%d", point, states[0].aborts, states[1].aborts)
			}
			if point == "journal" && (containsString(states[0].calls, "stage") || containsString(states[1].calls, "stage")) {
				t.Fatalf("journal failure occurred after staging: %v / %v", states[0].calls, states[1].calls)
			}
			journal, err := store.ReadJournal()
			if err != nil || journal.Schema != "" {
				t.Fatalf("successful compensation left crash journal: %#v, %v", journal, err)
			}
		})
	}
}

func TestApplyReconcilesPostRenameLedgerFsyncFailure(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	fired := false
	store.afterReplace = func(name string) error {
		if name == ownershipFilename && !fired {
			fired = true
			return errors.New("injected directory fsync failure after ledger rename")
		}
		return nil
	}
	if _, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-post-rename", ReleaseID: "release"}); err == nil {
		t.Fatal("post-rename ledger failure succeeded")
	}
	ledger, err := store.ReadLedger()
	if err != nil || ledger.Schema != "" || states[0].current != nil {
		t.Fatalf("apply did not restore exact absent ledger/native baseline: ledger=%#v current=%#v err=%v", ledger, states[0].current, err)
	}
	journal, _ := store.ReadJournal()
	if journal.Schema != "" {
		t.Fatalf("verified apply compensation left journal: %#v", journal)
	}
}

func TestApplyRetainsJournalWhenLedgerRestorationIsFsyncAmbiguous(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-prior", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	states[0].target.Revision = "updated"
	states[0].target.Digest = digestForTest("updated")
	store.afterReplace = func(name string) error {
		if name == ownershipFilename {
			return errors.New("persistent directory fsync failure")
		}
		return nil
	}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-ambiguous-ledger", ReleaseID: "release"}); err == nil {
		t.Fatal("ambiguous ledger restoration succeeded")
	}
	journal, _ := store.ReadJournal()
	if journal.Schema == "" {
		t.Fatal("ambiguous ledger restoration falsely cleared crash journal")
	}
}

func TestInstallerCarriesOriginalPriorAcrossSameVersionUpdate(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	prior := NativeIdentity{ResourceKey: "resource-codex", Kind: "plugin", Revision: "user-prior", Digest: digestForTest("user-prior")}
	states[0].current = cloneNativeIdentity(&prior)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	request := TransactionRequest{TransactionID: "txn-1", ReleaseID: "same-release"}
	if _, err := installer.Apply(context.Background(), inventory, request); err != nil {
		t.Fatal(err)
	}
	states[0].calls = nil
	request.TransactionID = "txn-2"
	if _, err := installer.Apply(context.Background(), inventory, request); err != nil {
		t.Fatal(err)
	}
	ledger, _ := store.ReadLedger()
	if len(ledger.Receipts) != 1 || ledger.Receipts[0].Prior == nil || !ledger.Receipts[0].Prior.Equal(prior) {
		t.Fatalf("original prior was not carried across update: %#v", ledger)
	}
}

func TestInstallerRejectsOwnedLiveDriftBeforeStageOrMutation(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-1", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	drift := states[0].target
	drift.Revision = "external"
	drift.Digest = digestForTest("external")
	states[0].current = &drift
	states[0].calls = nil
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-2", ReleaseID: "release"}); !errors.Is(err, ErrInstallDrift) {
		t.Fatalf("owned drift update error = %v", err)
	}
	if containsString(states[0].calls, "stage") || containsString(states[0].calls, "register") {
		t.Fatalf("owned drift reached mutation preparation: %v", states[0].calls)
	}
	ledger, _ := store.ReadLedger()
	if len(ledger.Receipts) != 1 || ledger.Receipts[0].Debt != "native-drift" || !ledger.Receipts[0].Installed.Equal(states[0].target) {
		t.Fatalf("owned drift overwrote baseline: %#v", ledger)
	}
}

func TestInstallerRejectsRegisterIdentityDifferentFromDurablePlan(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	other := states[0].target
	other.Revision = "unexpected"
	other.Digest = digestForTest("unexpected")
	states[0].returned = &other
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	if _, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-plan", ReleaseID: "release"}); err == nil {
		t.Fatal("native identity different from durable plan was accepted")
	}
	if states[0].current != nil {
		t.Fatalf("planned-identity mismatch was not rolled back: %#v", states[0].current)
	}
	journal, _ := store.ReadJournal()
	if journal.Schema != "" {
		t.Fatalf("verified rollback left journal: %#v", journal)
	}
}

func TestInstallerRejectsNondeterministicPlanBeforeNativeMutation(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	other := states[0].target
	other.Revision = "different-plan"
	other.Digest = digestForTest("different-plan")
	states[0].plannedAgain = &other
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	if _, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-plan", ReleaseID: "release"}); err == nil {
		t.Fatal("nondeterministic native plan was accepted")
	}
	if states[0].current != nil || containsString(states[0].calls, "register") {
		t.Fatalf("nondeterministic plan reached native mutation: current=%#v calls=%v", states[0].current, states[0].calls)
	}
}

func TestInstallerDurablyJournalsPlanBeforeRegister(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	states[0].beforeRegister = func() {
		journal, err := store.ReadJournal()
		if err != nil {
			t.Fatalf("read pre-register journal: %v", err)
		}
		if len(journal.Entries) != 1 || journal.Entries[0].State != JournalEntryPrepared || journal.Entries[0].Planned == nil || !journal.Entries[0].Planned.Equal(states[0].target) || journal.Entries[0].Installed != nil {
			t.Fatalf("pre-register journal does not carry exact planned identity: %#v", journal)
		}
	}
	if _, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-journal", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerRemovalIsExactAndDriftBecomesDebt(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	states[0].calls = nil
	drift := NativeIdentity{ResourceKey: states[0].target.ResourceKey, Kind: "plugin", Revision: "user-change", Digest: digestForTest("user-change")}
	states[0].current = &drift
	if err := installer.Remove(context.Background(), inventory, "remove-1"); !errors.Is(err, ErrInstallDrift) {
		t.Fatalf("drift removal error = %v", err)
	}
	if containsString(states[0].calls, "remove") {
		t.Fatal("drifted user state was removed")
	}
	ledger, _ := store.ReadLedger()
	if len(ledger.Receipts) != 1 || ledger.Receipts[0].Debt == "" {
		t.Fatalf("drift debt was not recorded: %#v", ledger)
	}

	states[0].current = cloneNativeIdentity(&ledger.Receipts[0].Installed)
	states[0].calls = nil
	if err := installer.Remove(context.Background(), inventory, "remove-2"); err != nil {
		t.Fatal(err)
	}
	ledger, _ = store.ReadLedger()
	if len(ledger.Receipts) != 0 || !containsString(states[0].calls, "remove") {
		t.Fatalf("exact removal failed: ledger=%#v calls=%v", ledger, states[0].calls)
	}
}

func TestRemoveReconcilesPostRenameLedgerFsyncFailure(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn-install", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	priorLedger, _ := store.ReadLedger()
	installed := *states[0].current
	fired := false
	store.afterReplace = func(name string) error {
		if name == ownershipFilename && !fired {
			fired = true
			return errors.New("injected directory fsync failure after removal ledger rename")
		}
		return nil
	}
	if err := installer.Remove(context.Background(), inventory, "txn-remove"); err == nil {
		t.Fatal("post-rename removal ledger failure succeeded")
	}
	liveLedger, err := store.ReadLedger()
	if err != nil || !ledgerSameContents(liveLedger, priorLedger) || states[0].current == nil || !states[0].current.Equal(installed) {
		t.Fatalf("remove did not restore ledger/native baseline: prior=%#v live=%#v current=%#v err=%v", priorLedger, liveLedger, states[0].current, err)
	}
	journal, _ := store.ReadJournal()
	if journal.Schema != "" {
		t.Fatalf("verified removal compensation left journal: %#v", journal)
	}
}

func TestInstallerRemoveFailureCompensatesEarlierExactRemoval(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 2)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	wantCodex := *states[0].current
	wantClaude := *states[1].current
	// Ledger order is claude,codex and removal is reverse, so failing Claude
	// must compensate the already removed Codex registration.
	states[1].removeErr = errors.New("injected remove failure")
	if err := installer.Remove(context.Background(), inventory, "remove-fail"); err == nil {
		t.Fatal("injected remove failure succeeded")
	}
	if states[0].current == nil || !states[0].current.Equal(wantCodex) || states[1].current == nil || !states[1].current.Equal(wantClaude) {
		t.Fatalf("remove compensation lost exact installs: %#v %#v", states[0].current, states[1].current)
	}
	ledger, _ := store.ReadLedger()
	if len(ledger.Receipts) != 2 {
		t.Fatalf("compensated removal changed ledger: %#v", ledger)
	}
}

func TestInstallerPersistsRemovalDriftDebtWhenLaterStagingFails(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 2)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn", ReleaseID: "release"}); err != nil {
		t.Fatal(err)
	}
	drift := states[0].target
	drift.Revision = "external"
	drift.Digest = digestForTest("external-removal")
	states[0].current = &drift
	states[1].failStage = errors.New("staging failed")
	if err := installer.Remove(context.Background(), inventory, "remove-with-debt"); err == nil {
		t.Fatal("removal with drift and staging failure succeeded")
	}
	ledger, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	receipt := findReceipt(ledger.Receipts, inventory[0].ID, strategies[0].Key())
	if receipt == nil || receipt.Debt != "native-drift" {
		t.Fatalf("removal drift debt was lost after staging failure: %#v", ledger)
	}
	if containsString(states[0].calls, "remove") || containsString(states[1].calls, "remove") {
		t.Fatalf("preflight failure reached native removal: %v / %v", states[0].calls, states[1].calls)
	}
}

func TestCrashRecoveryNeverBlindlyUninstallsDrift(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installed := states[0].target
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "crash", Phase: JournalApplying, Entries: []JournalEntry{{
		ProductID: inventory[0].ID, Strategy: strategies[0].Key(), State: JournalEntryRegistered, Planned: &installed, Installed: &installed,
	}}}
	if err := store.BeginJournal(journal); err != nil {
		t.Fatal(err)
	}
	drift := installed
	drift.Revision = "external"
	drift.Digest = digestForTest("external")
	states[0].current = &drift
	installer := Installer{Registry: registry, Store: store}
	if err := installer.Recover(context.Background(), inventory); !errors.Is(err, ErrInstallDrift) {
		t.Fatalf("recovery drift error = %v", err)
	}
	if containsString(states[0].calls, "remove") {
		t.Fatal("crash recovery blindly removed drifted state")
	}
	got, _ := store.ReadJournal()
	if len(got.Entries) != 1 || got.Entries[0].Debt == "" {
		t.Fatalf("recovery debt missing: %#v", got)
	}
}

func TestCrashDuringRegisterUsesDurablePlanToRecoverExactBaseline(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	planned := states[0].target
	states[0].current = cloneNativeIdentity(&planned)
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "crash-plan", Phase: JournalApplying, Entries: []JournalEntry{{ProductID: inventory[0].ID, Strategy: strategies[0].Key(), State: JournalEntryPrepared, Planned: &planned}}}
	if err := store.BeginJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := (Installer{Registry: registry, Store: store}).Recover(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	if states[0].current != nil || !containsString(states[0].calls, "remove") {
		t.Fatalf("planned crash mutation was skipped: current=%#v calls=%v", states[0].current, states[0].calls)
	}
}

func TestCrashDuringUpdateRegisterIsNotMistakenForCommittedLedger(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	installer := Installer{Registry: registry, Store: store}
	if _, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "prior-transaction", ReleaseID: "prior-release"}); err != nil {
		t.Fatal(err)
	}
	prior := states[0].target
	planned := prior
	planned.Revision = "updated"
	planned.Digest = digestForTest("updated")
	states[0].current = cloneNativeIdentity(&planned)
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "crashed-update", Phase: JournalApplying, Entries: []JournalEntry{{
		ProductID: inventory[0].ID, Strategy: strategies[0].Key(), State: JournalEntryPrepared, Prior: &prior, Planned: &planned,
	}}}
	if err := store.BeginJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := installer.Recover(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	if states[0].current == nil || !states[0].current.Equal(prior) || !containsString(states[0].calls, "remove") {
		t.Fatalf("crashed update register was skipped: current=%#v calls=%v", states[0].current, states[0].calls)
	}
}

func TestRollbackAndRecoveryRequireLiveExactPostcondition(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		inventory, strategies, states := syntheticInstallFixture(t, 1)
		states[0].failVerify = errors.New("verify failed")
		states[0].rollbackNoRestore = true
		registry, _ := NewRegistry(inventory, strategies...)
		store := ownershipStoreFixture(t)
		if _, err := (Installer{Registry: registry, Store: store}).Apply(context.Background(), inventory, TransactionRequest{TransactionID: "txn", ReleaseID: "release"}); err == nil {
			t.Fatal("unverified rollback succeeded")
		}
		journal, _ := store.ReadJournal()
		if len(journal.Entries) != 1 || journal.Entries[0].Debt != "rollback-unverified" {
			t.Fatalf("unverified rollback debt = %#v", journal)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		inventory, strategies, states := syntheticInstallFixture(t, 1)
		states[0].removeNoRestore = true
		planned := states[0].target
		states[0].current = &planned
		registry, _ := NewRegistry(inventory, strategies...)
		store := ownershipStoreFixture(t)
		journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "crash", Phase: JournalApplying, Entries: []JournalEntry{{ProductID: inventory[0].ID, Strategy: strategies[0].Key(), State: JournalEntryPrepared, Planned: &planned}}}
		if err := store.BeginJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := (Installer{Registry: registry, Store: store}).Recover(context.Background(), inventory); err == nil {
			t.Fatal("recovery cleared without exact baseline proof")
		}
		got, _ := store.ReadJournal()
		if len(got.Entries) != 1 || got.Entries[0].Debt != "rollback-unverified" {
			t.Fatalf("unverified recovery debt = %#v", got)
		}
	})
}

func TestInstallerRefusesToOverwriteUnrecoveredCrashJournal(t *testing.T) {
	inventory, strategies, states := syntheticInstallFixture(t, 1)
	registry, _ := NewRegistry(inventory, strategies...)
	store := ownershipStoreFixture(t)
	planned := states[0].target
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "prior-crash", Phase: JournalApplying, Entries: []JournalEntry{{ProductID: inventory[0].ID, Strategy: strategies[0].Key(), State: JournalEntryPrepared, Planned: &planned}}}
	if err := store.BeginJournal(journal); err != nil {
		t.Fatal(err)
	}
	installer := Installer{Registry: registry, Store: store}
	_, err := installer.Apply(context.Background(), inventory, TransactionRequest{TransactionID: "new-op", ReleaseID: "release"})
	if !errors.Is(err, ErrInstallInProgress) {
		t.Fatalf("new transaction over prior journal error = %v", err)
	}
	if states[0].current != nil || containsString(states[0].calls, "register") {
		t.Fatalf("new transaction mutated native state: %#v %v", states[0].current, states[0].calls)
	}
	if states[0].aborts != 0 || containsString(states[0].calls, "stage") {
		t.Fatalf("known unrecovered journal did not fail before staging: calls=%v", states[0].calls)
	}
	got, _ := store.ReadJournal()
	if got.TransactionID != "prior-crash" {
		t.Fatalf("prior journal was replaced: %#v", got)
	}
}

func syntheticInstallFixture(t *testing.T, count int) ([]productcatalog.Descriptor, []Strategy, []*fakeInstallState) {
	t.Helper()
	inventory := productcatalog.All()[:count]
	strategies := make([]Strategy, 0, count)
	states := make([]*fakeInstallState, 0, count)
	for index := range inventory {
		key := "synthetic-" + inventory[index].ID
		inventory[index].NativeRegistration = productcatalog.NativeRegistration{Strategy: key, Args: []string{inventory[index].ID}, AssetOnly: true}
		state := &fakeInstallState{target: NativeIdentity{
			ResourceKey: NativeToken("resource-" + inventory[index].ID), Kind: "plugin", Revision: "installed", Digest: digestForTest("installed-" + inventory[index].ID),
		}}
		states = append(states, state)
		strategies = append(strategies, &fakeInstallStrategy{key: key, wantArgs: []string{inventory[index].ID}, state: state})
	}
	return inventory, strategies, states
}

func ownershipStoreFixture(t *testing.T) *OwnershipStore {
	t.Helper()
	store, err := OpenOwnershipStore(filepath.Join(t.TempDir(), "state"), 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
