package releaseinstall

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type TransactionRequest struct {
	TransactionID string
	ReleaseID     string
}

type Installer struct {
	Registry *Registry
	Store    *OwnershipStore
}

type ProductInstallState string

const (
	ProductInstalled     ProductInstallState = "installed"
	ProductSkippedAbsent ProductInstallState = "skipped-absent"
)

type ProductInstallResult struct {
	ProductID string              `json:"product_id"`
	State     ProductInstallState `json:"state"`
}

type ApplyReport struct {
	Products []ProductInstallResult `json:"products"`
}

type preparedInstall struct {
	descriptor productDescriptor
	strategy   Strategy
	baseline   Baseline
	change     Change
	planned    *NativeIdentity
	installed  *NativeIdentity
	staged     bool
}

// productDescriptor keeps installer internals independent of projection
// serialization while retaining the complete catalog descriptor.
type productDescriptor = productcatalog.Descriptor

// Apply stages and validates the whole inventory before the first native
// mutation, journals every mutation intent, and compensates in reverse order.
func (installer Installer) Apply(ctx context.Context, inventory []productcatalog.Descriptor, request TransactionRequest) (ApplyReport, error) {
	if err := installer.validate(inventory); err != nil {
		return ApplyReport{}, err
	}
	if err := validateBounded(request.TransactionID, 128); err != nil {
		return ApplyReport{}, fmt.Errorf("transaction id: %w", err)
	}
	if err := validateBounded(request.ReleaseID, 128); err != nil {
		return ApplyReport{}, fmt.Errorf("release id: %w", err)
	}
	var report ApplyReport
	err := installer.Store.withTransaction(func() error {
		var err error
		report, err = installer.applyLocked(ctx, inventory, request)
		return err
	})
	return report, err
}

func (installer Installer) applyLocked(ctx context.Context, inventory []productcatalog.Descriptor, request TransactionRequest) (ApplyReport, error) {
	var report ApplyReport
	journal, err := installer.Store.ReadJournal()
	if err != nil {
		return report, err
	}
	if journal.Schema != "" {
		return report, ErrInstallInProgress
	}
	ledger, err := installer.Store.ReadLedger()
	if err != nil {
		return report, err
	}
	prepared := make([]preparedInstall, 0, len(inventory))
	for _, descriptor := range sortedDescriptors(inventory) {
		strategy, _ := installer.Registry.Resolve(descriptor.NativeRegistration.Strategy)
		discovery, err := strategy.Discover(ctx, descriptor)
		if err != nil {
			return report, fmt.Errorf("discover %s integration: %w", descriptor.ID, err)
		}
		owned := findReceipt(ledger.Receipts, descriptor.ID, strategy.Key())
		if owned != nil && (!discovery.Present || discovery.Identity == nil || !discovery.Identity.Equal(owned.Installed)) {
			markReceiptDebtComposite(&ledger, descriptor.ID, strategy.Key(), owned.Installed, "native-drift")
			if ledger.Schema != "" {
				ledger.Revision++
				if err := installer.Store.WriteLedger(ledger); err != nil {
					return report, errors.Join(ErrInstallDrift, fmt.Errorf("record %s native drift: %w", descriptor.ID, err))
				}
			}
			return report, ErrInstallDrift
		}
		if !discovery.Present && !descriptor.NativeRegistration.AssetOnly {
			report.Products = append(report.Products, ProductInstallResult{ProductID: descriptor.ID, State: ProductSkippedAbsent})
			continue
		}
		captured, err := strategy.Capture(ctx, descriptor, discovery)
		if err != nil {
			return report, fmt.Errorf("capture %s integration: %w", descriptor.ID, err)
		}
		if captured.Change == nil {
			return report, fmt.Errorf("capture %s integration returned nil change", descriptor.ID)
		}
		prepared = append(prepared, preparedInstall{descriptor: descriptor, strategy: strategy, baseline: captured.Baseline, change: captured.Change})
	}
	if len(prepared) == 0 {
		sortInstallReport(&report)
		return report, nil
	}
	for index := range prepared {
		planned, err := prepared[index].change.PlannedIdentity(ctx)
		if err != nil {
			return report, fmt.Errorf("plan %s integration: %w", prepared[index].descriptor.ID, err)
		}
		if err := validateIdentity(planned); err != nil {
			return report, fmt.Errorf("plan %s integration identity: %w", prepared[index].descriptor.ID, err)
		}
		confirmation, err := prepared[index].change.PlannedIdentity(ctx)
		if err != nil {
			return report, fmt.Errorf("confirm %s integration plan: %w", prepared[index].descriptor.ID, err)
		}
		if err := validateIdentity(confirmation); err != nil {
			return report, fmt.Errorf("confirm %s integration identity: %w", prepared[index].descriptor.ID, err)
		}
		if !confirmation.Equal(planned) {
			return report, fmt.Errorf("plan %s integration is not deterministic", prepared[index].descriptor.ID)
		}
		if owned := findReceipt(ledger.Receipts, prepared[index].descriptor.ID, prepared[index].strategy.Key()); owned != nil && owned.Installed.ResourceKey != planned.ResourceKey {
			return report, errors.Join(fmt.Errorf("owned resource identity changed for %s", prepared[index].descriptor.ID), abortChanges(ctx, prepared))
		}
		prepared[index].planned = cloneNativeIdentity(&planned)
	}
	for index := range prepared {
		discovery, err := prepared[index].strategy.Discover(ctx, prepared[index].descriptor)
		if err != nil || !discoveryMatchesBaseline(discovery, prepared[index].baseline) {
			return report, errors.Join(ErrInstallDrift, err, abortChanges(ctx, prepared))
		}
	}
	journal = CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: request.TransactionID, Phase: JournalApplying}
	for _, item := range prepared {
		journal.Entries = append(journal.Entries, JournalEntry{
			ProductID: item.descriptor.ID, Strategy: item.strategy.Key(), State: JournalEntryPrepared,
			Prior: cloneNativeIdentity(item.baseline.Current), Planned: cloneNativeIdentity(item.planned),
		})
	}
	if err := installer.Store.BeginJournal(journal); err != nil {
		return report, fmt.Errorf("write install crash journal: %w", err)
	}
	for index := range prepared {
		// The cleanup obligation is durable before Stage can create assets.
		journal.Entries[index].CleanupRequired = true
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return report, installer.abortBeforeNativeMutation(ctx, prepared, journal, err)
		}
		prepared[index].staged = true
		if err := prepared[index].change.Stage(ctx); err != nil {
			return report, installer.abortBeforeNativeMutation(ctx, prepared, journal, fmt.Errorf("stage %s integration: %w", prepared[index].descriptor.ID, err))
		}
	}
	for index := range prepared {
		if err := prepared[index].change.Validate(ctx); err != nil {
			return report, installer.abortBeforeNativeMutation(ctx, prepared, journal, fmt.Errorf("validate %s integration: %w", prepared[index].descriptor.ID, err))
		}
	}
	for index := range prepared {
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return report, installer.rollback(ctx, prepared, index-1, journal, err)
		}
		installed, err := prepared[index].change.Register(ctx)
		if err != nil {
			return report, installer.rollback(ctx, prepared, index, journal, fmt.Errorf("register %s integration: %w", prepared[index].descriptor.ID, err))
		}
		if err := validateIdentity(installed); err != nil {
			return report, installer.rollback(ctx, prepared, index, journal, fmt.Errorf("register %s returned invalid identity: %w", prepared[index].descriptor.ID, err))
		}
		prepared[index].installed = cloneNativeIdentity(&installed)
		if !installed.Equal(*prepared[index].planned) {
			return report, installer.rollback(ctx, prepared, index, journal, fmt.Errorf("register %s returned identity different from durable plan", prepared[index].descriptor.ID))
		}
		journal.Entries[index].Installed = cloneNativeIdentity(&installed)
		journal.Entries[index].State = JournalEntryRegistered
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return report, installer.rollback(ctx, prepared, index, journal, err)
		}
		if err := prepared[index].change.Verify(ctx, installed); err != nil {
			return report, installer.rollback(ctx, prepared, index, journal, fmt.Errorf("verify %s integration: %w", prepared[index].descriptor.ID, err))
		}
		journal.Entries[index].State = JournalEntryVerified
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return report, installer.rollback(ctx, prepared, index, journal, err)
		}
	}
	if err := abortChanges(ctx, prepared); err != nil {
		return report, installer.rollback(ctx, prepared, len(prepared)-1, journal, fmt.Errorf("clean staged integrations: %w", err))
	}
	for index := range journal.Entries {
		journal.Entries[index].CleanupRequired = false
	}
	journal.Revision++
	if err := installer.Store.WriteJournal(journal); err != nil {
		return report, installer.rollback(ctx, prepared, len(prepared)-1, journal, err)
	}
	priorLedger := cloneLedger(ledger)
	ledger, err = mergeOwnershipLedger(ledger, prepared, request)
	if err != nil {
		return report, installer.rollback(ctx, prepared, len(prepared)-1, journal, err)
	}
	if err := installer.Store.WriteLedger(ledger); err != nil {
		return report, installer.rollbackAfterLedgerWrite(ctx, prepared, len(prepared)-1, journal, priorLedger, ledger, err)
	}
	if err := installer.Store.ClearJournal(); err != nil {
		return report, fmt.Errorf("clear committed install journal: %w", err)
	}
	for _, item := range prepared {
		report.Products = append(report.Products, ProductInstallResult{ProductID: item.descriptor.ID, State: ProductInstalled})
	}
	sortInstallReport(&report)
	return report, nil
}

func (installer Installer) abortBeforeNativeMutation(ctx context.Context, prepared []preparedInstall, journal CrashJournal, cause error) error {
	abortErr := abortChanges(ctx, prepared)
	for index := range journal.Entries {
		journal.Entries[index].CleanupRequired = prepared[index].staged
		if prepared[index].staged {
			journal.Entries[index].State = JournalEntryDebt
			journal.Entries[index].Debt = "staging-cleanup-failed"
		}
	}
	if abortErr == nil {
		return errors.Join(cause, installer.Store.ClearJournal())
	}
	journal.Revision++
	return errors.Join(cause, abortErr, installer.Store.WriteJournal(journal))
}

func sortInstallReport(report *ApplyReport) {
	sort.Slice(report.Products, func(i, j int) bool { return report.Products[i].ProductID < report.Products[j].ProductID })
}

func (installer Installer) rollback(ctx context.Context, prepared []preparedInstall, through int, journal CrashJournal, cause error) error {
	return installer.rollbackWithLedgerReconcile(ctx, prepared, through, journal, cause, nil)
}

func (installer Installer) rollbackAfterLedgerWrite(ctx context.Context, prepared []preparedInstall, through int, journal CrashJournal, prior, candidate OwnershipLedger, cause error) error {
	return installer.rollbackWithLedgerReconcile(ctx, prepared, through, journal, cause, func() error {
		return installer.restoreAndVerifyPriorLedger(prior, candidate)
	})
}

func (installer Installer) rollbackWithLedgerReconcile(ctx context.Context, prepared []preparedInstall, through int, journal CrashJournal, cause error, reconcileLedger func() error) error {
	journal.Phase = JournalRollingBack
	journal.Revision++
	_ = installer.Store.WriteJournal(journal)
	var rollbackErr error
	for index := through; index >= 0; index-- {
		if err := prepared[index].change.Rollback(ctx, prepared[index].baseline, prepared[index].installed); err != nil {
			journal.Entries[index].State = JournalEntryDebt
			journal.Entries[index].Debt = "rollback-failed"
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback %s integration: %w", prepared[index].descriptor.ID, err))
			continue
		}
		discovery, err := prepared[index].strategy.Discover(ctx, prepared[index].descriptor)
		if err != nil || !discoveryMatchesBaseline(discovery, prepared[index].baseline) {
			journal.Entries[index].State = JournalEntryDebt
			journal.Entries[index].Debt = "rollback-unverified"
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback %s did not restore exact baseline: %w", prepared[index].descriptor.ID, err))
		}
	}
	if err := abortChanges(ctx, prepared); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("abort staged integrations: %w", err))
	}
	for index := range journal.Entries {
		journal.Entries[index].CleanupRequired = prepared[index].staged
		if prepared[index].staged {
			journal.Entries[index].State = JournalEntryDebt
			journal.Entries[index].Debt = "staging-cleanup-failed"
		}
	}
	if reconcileLedger != nil {
		if err := reconcileLedger(); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore ownership ledger: %w", err))
			for index := range journal.Entries {
				if journal.Entries[index].Debt == "" {
					journal.Entries[index].State = JournalEntryDebt
					journal.Entries[index].Debt = "ledger-restore-failed"
				}
			}
		}
	}
	if rollbackErr == nil {
		rollbackErr = installer.Store.ClearJournal()
	} else {
		journal.Revision++
		rollbackErr = errors.Join(rollbackErr, installer.Store.WriteJournal(journal))
	}
	return errors.Join(cause, rollbackErr)
}

func (installer Installer) restoreAndVerifyPriorLedger(prior, candidate OwnershipLedger) error {
	if err := installer.Store.RestoreLedgerAfterAmbiguousWrite(prior, candidate); err != nil {
		return err
	}
	live, err := installer.Store.ReadLedger()
	if err != nil {
		return err
	}
	if prior.Schema == "" {
		if live.Schema != "" {
			return ErrInstallDebt
		}
		return nil
	}
	if !ledgerSameContents(live, prior) {
		return ErrInstallDebt
	}
	return nil
}

func abortChanges(ctx context.Context, prepared []preparedInstall) error {
	var result error
	for index := len(prepared) - 1; index >= 0; index-- {
		if prepared[index].change != nil && prepared[index].staged {
			if err := prepared[index].change.Abort(ctx); err != nil {
				result = errors.Join(result, err)
			} else {
				prepared[index].staged = false
			}
		}
	}
	return result
}

func discoveryMatchesBaseline(discovery Discovery, baseline Baseline) bool {
	if baseline.Current == nil {
		return !discovery.Present && discovery.Identity == nil
	}
	return discovery.Present && discovery.Identity != nil && discovery.Identity.Equal(*baseline.Current)
}

// Remove removes only an exact installed identity. Drift remains untouched and
// is recorded as explicit debt in the ownership ledger.
func (installer Installer) Remove(ctx context.Context, inventory []productcatalog.Descriptor, transactionID string) error {
	if err := installer.validate(inventory); err != nil {
		return err
	}
	if err := validateBounded(transactionID, 128); err != nil {
		return err
	}
	return installer.Store.withTransaction(func() error {
		return installer.removeLocked(ctx, inventory, transactionID)
	})
}

func (installer Installer) removeLocked(ctx context.Context, inventory []productcatalog.Descriptor, transactionID string) error {
	existingJournal, err := installer.Store.ReadJournal()
	if err != nil {
		return err
	}
	if existingJournal.Schema != "" {
		return ErrInstallInProgress
	}
	ledger, err := installer.Store.ReadLedger()
	if err != nil || ledger.Schema == "" || len(ledger.Receipts) == 0 {
		return err
	}
	priorLedger := cloneLedger(ledger)
	descriptors := descriptorIndex(inventory)
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: transactionID, Phase: JournalRemoving}
	for _, receipt := range ledger.Receipts {
		journal.Entries = append(journal.Entries, JournalEntry{ProductID: receipt.ProductID, Strategy: receipt.Strategy, State: JournalEntryPrepared, Prior: cloneNativeIdentity(receipt.Prior), Installed: cloneNativeIdentity(&receipt.Installed)})
	}
	type removalItem struct {
		index      int
		receipt    OwnershipReceipt
		descriptor productcatalog.Descriptor
		strategy   Strategy
		captured   CapturedChange
		planned    *NativeIdentity
		staged     bool
	}
	items := make([]removalItem, 0, len(ledger.Receipts))
	ledgerDirty := false
	persistDebt := func(cause error) error {
		if !ledgerDirty {
			return cause
		}
		ledger.Revision++
		if err := installer.Store.WriteLedger(ledger); err != nil {
			return errors.Join(cause, fmt.Errorf("record removal debt: %w", err))
		}
		ledgerDirty = false
		return cause
	}
	abortItems := func() error {
		var err error
		for index := len(items) - 1; index >= 0; index-- {
			if !items[index].staged {
				continue
			}
			if abortErr := items[index].captured.Change.Abort(ctx); abortErr != nil {
				err = errors.Join(err, abortErr)
			} else {
				items[index].staged = false
			}
		}
		return err
	}
	var result error
	for index := range ledger.Receipts {
		receipt := ledger.Receipts[index]
		descriptor, ok := descriptors[receipt.ProductID]
		strategy, strategyOK := installer.Registry.Resolve(receipt.Strategy)
		if !ok || !strategyOK {
			result = errors.Join(result, ErrInstallDebt)
			ledger.Receipts[index].Debt = "strategy-unavailable"
			journal.Entries[index].State, journal.Entries[index].Debt = JournalEntryDebt, "strategy-unavailable"
			ledgerDirty = true
			continue
		}
		discovery, discoverErr := strategy.Discover(ctx, descriptor)
		if discoverErr != nil || !discovery.Present || discovery.Identity == nil || !discovery.Identity.Equal(receipt.Installed) {
			ledger.Receipts[index].Debt = "native-drift"
			journal.Entries[index].State, journal.Entries[index].Debt = JournalEntryDebt, "native-drift"
			ledgerDirty = true
			result = errors.Join(result, ErrInstallDrift, discoverErr)
			continue
		}
		captured, captureErr := strategy.Capture(ctx, descriptor, discovery)
		if captureErr != nil {
			return persistDebt(errors.Join(result, fmt.Errorf("capture %s removal baseline: %w", receipt.ProductID, captureErr)))
		}
		if captured.Change == nil {
			return persistDebt(errors.Join(result, fmt.Errorf("capture %s removal baseline returned nil change", receipt.ProductID)))
		}
		items = append(items, removalItem{index: index, receipt: receipt, descriptor: descriptor, strategy: strategy, captured: captured})
	}
	for index := range items {
		planned, err := items[index].captured.Change.PlannedIdentity(ctx)
		if err != nil {
			return persistDebt(fmt.Errorf("plan %s removal compensation: %w", items[index].receipt.ProductID, err))
		}
		confirmation, err := items[index].captured.Change.PlannedIdentity(ctx)
		if err != nil || validateIdentity(planned) != nil || validateIdentity(confirmation) != nil || !planned.Equal(confirmation) {
			return persistDebt(errors.Join(fmt.Errorf("plan %s removal compensation is invalid or nondeterministic", items[index].receipt.ProductID), err))
		}
		items[index].planned = cloneNativeIdentity(&planned)
		journal.Entries[items[index].index].Planned = cloneNativeIdentity(&planned)
	}
	if err := installer.Store.BeginJournal(journal); err != nil {
		return persistDebt(errors.Join(result, err))
	}
	abortRemovalBeforeMutation := func(cause error) error {
		abortErr := abortItems()
		for _, item := range items {
			entry := &journal.Entries[item.index]
			entry.CleanupRequired = item.staged
			if item.staged {
				entry.State, entry.Debt = JournalEntryDebt, "staging-cleanup-failed"
			}
		}
		cause = persistDebt(errors.Join(result, cause, abortErr))
		if abortErr == nil {
			return errors.Join(cause, installer.Store.ClearJournal())
		}
		journal.Revision++
		return errors.Join(cause, installer.Store.WriteJournal(journal))
	}
	for index := range items {
		journal.Entries[items[index].index].CleanupRequired = true
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return abortRemovalBeforeMutation(err)
		}
		items[index].staged = true
		if err := items[index].captured.Change.Stage(ctx); err != nil {
			return abortRemovalBeforeMutation(fmt.Errorf("stage %s removal compensation: %w", items[index].receipt.ProductID, err))
		}
	}
	for index := range items {
		if err := items[index].captured.Change.Validate(ctx); err != nil {
			return abortRemovalBeforeMutation(fmt.Errorf("validate %s removal compensation: %w", items[index].receipt.ProductID, err))
		}
	}
	removed := make([]removalItem, 0, len(items))
	compensate := func(failed *removalItem, cause error, reconcileLedger func() error) error {
		toRestore := append([]removalItem(nil), removed...)
		if failed != nil {
			toRestore = append(toRestore, *failed)
		}
		var compensationErr error
		for index := len(toRestore) - 1; index >= 0; index-- {
			item := toRestore[index]
			if err := item.captured.Change.Rollback(ctx, Baseline{Current: cloneNativeIdentity(&item.receipt.Installed)}, nil); err != nil {
				journal.Entries[item.index].State, journal.Entries[item.index].Debt = JournalEntryDebt, "rollback-failed"
				compensationErr = errors.Join(compensationErr, err)
				continue
			}
			discovery, err := item.strategy.Discover(ctx, item.descriptor)
			if err != nil || !discovery.Present || discovery.Identity == nil || !discovery.Identity.Equal(item.receipt.Installed) {
				journal.Entries[item.index].State, journal.Entries[item.index].Debt = JournalEntryDebt, "rollback-unverified"
				compensationErr = errors.Join(compensationErr, fmt.Errorf("restore %s removal baseline: %w", item.receipt.ProductID, err))
			}
		}
		compensationErr = errors.Join(compensationErr, abortItems())
		for _, item := range items {
			entry := &journal.Entries[item.index]
			entry.CleanupRequired = item.staged
			if item.staged {
				entry.State, entry.Debt = JournalEntryDebt, "staging-cleanup-failed"
			}
		}
		if ledgerDirty {
			ledger.Revision++
			if err := installer.Store.WriteLedger(ledger); err != nil {
				compensationErr = errors.Join(compensationErr, fmt.Errorf("record removal debt during compensation: %w", err))
			} else {
				ledgerDirty = false
			}
		}
		if reconcileLedger != nil {
			if err := reconcileLedger(); err != nil {
				compensationErr = errors.Join(compensationErr, fmt.Errorf("restore ownership ledger: %w", err))
				for index := range journal.Entries {
					if journal.Entries[index].Debt == "" {
						journal.Entries[index].State = JournalEntryDebt
						journal.Entries[index].Debt = "ledger-restore-failed"
					}
				}
			}
		}
		journal.Revision++
		if compensationErr == nil {
			compensationErr = installer.Store.ClearJournal()
		} else {
			compensationErr = errors.Join(compensationErr, installer.Store.WriteJournal(journal))
		}
		return errors.Join(result, cause, compensationErr)
	}
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		journal.Revision++
		if err := installer.Store.WriteJournal(journal); err != nil {
			return compensate(nil, err, nil)
		}
		if err := item.strategy.Remove(ctx, item.descriptor, RemovalRequest{Installed: item.receipt.Installed, Prior: cloneNativeIdentity(item.receipt.Prior)}); err != nil {
			return compensate(&item, fmt.Errorf("remove %s integration: %w", item.receipt.ProductID, err), nil)
		}
		post, err := item.strategy.Discover(ctx, item.descriptor)
		if err != nil || !removalReachedPrior(post, item.receipt.Prior) {
			return compensate(&item, errors.Join(fmt.Errorf("verify %s removal did not reach prior identity", item.receipt.ProductID), err), nil)
		}
		journal.Entries[item.index].State = JournalEntryVerified
		removed = append(removed, item)
	}
	removedIndexes := map[int]bool{}
	for _, item := range removed {
		removedIndexes[item.index] = true
	}
	if err := abortItems(); err != nil {
		return compensate(nil, fmt.Errorf("clean removal staging: %w", err), nil)
	}
	for _, item := range items {
		journal.Entries[item.index].CleanupRequired = false
	}
	journal.Revision++
	if err := installer.Store.WriteJournal(journal); err != nil {
		return compensate(nil, err, nil)
	}
	remaining := make([]OwnershipReceipt, 0, len(ledger.Receipts)-len(removed))
	for index, receipt := range ledger.Receipts {
		if !removedIndexes[index] {
			remaining = append(remaining, receipt)
		}
	}
	ledger.Revision++
	ledger.Receipts = remaining
	candidateLedger := cloneLedger(ledger)
	if err := installer.Store.WriteLedger(ledger); err != nil {
		return compensate(nil, err, func() error { return installer.restoreAndVerifyPriorLedger(priorLedger, candidateLedger) })
	}
	if err := installer.Store.ClearJournal(); err != nil {
		return errors.Join(result, err)
	}
	return result
}

// Recover reconciles a crash journal against live native identity. It removes
// only an exact recorded install and reports drift as durable debt.
func (installer Installer) Recover(ctx context.Context, inventory []productcatalog.Descriptor) error {
	if err := installer.validate(inventory); err != nil {
		return err
	}
	return installer.Store.withTransaction(func() error {
		return installer.recoverLocked(ctx, inventory)
	})
}

func (installer Installer) recoverLocked(ctx context.Context, inventory []productcatalog.Descriptor) error {
	journal, err := installer.Store.ReadJournal()
	if err != nil || journal.Schema == "" {
		return err
	}
	ledger, err := installer.Store.ReadLedger()
	if err != nil {
		return err
	}
	if err := installer.reconcileCleanupObligations(ctx, inventory, &journal); err != nil {
		journal.Revision++
		return errors.Join(err, installer.Store.WriteJournal(journal))
	}
	if journal.Phase == JournalRemoving {
		return installer.recoverRemoval(ctx, inventory, ledger, journal)
	}
	if journalAlreadyCommitted(journal, ledger) {
		return installer.Store.ClearJournal()
	}
	descriptors := descriptorIndex(inventory)
	var result error
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := &journal.Entries[index]
		if entry.Planned == nil {
			entry.State, entry.Debt = JournalEntryDebt, "missing-plan"
			result = errors.Join(result, ErrInstallDebt)
			continue
		}
		descriptor, descriptorOK := descriptors[entry.ProductID]
		strategy, strategyOK := installer.Registry.Resolve(entry.Strategy)
		if !descriptorOK || !strategyOK {
			entry.State, entry.Debt = JournalEntryDebt, "strategy-unavailable"
			result = errors.Join(result, ErrInstallDebt)
			continue
		}
		discovery, discoverErr := strategy.Discover(ctx, descriptor)
		if discoverErr != nil {
			entry.State, entry.Debt = JournalEntryDebt, "discover-failed"
			result = errors.Join(result, discoverErr)
			continue
		}
		exactMutation := discovery.Present && discovery.Identity != nil && (discovery.Identity.Equal(*entry.Planned) || (entry.Installed != nil && discovery.Identity.Equal(*entry.Installed)))
		if exactMutation {
			if err := strategy.Remove(ctx, descriptor, RemovalRequest{Installed: *discovery.Identity, Prior: cloneNativeIdentity(entry.Prior)}); err != nil {
				entry.State, entry.Debt = JournalEntryDebt, "remove-failed"
				result = errors.Join(result, err)
				continue
			}
			post, err := strategy.Discover(ctx, descriptor)
			if err != nil || !removalReachedPrior(post, entry.Prior) {
				entry.State, entry.Debt = JournalEntryDebt, "rollback-unverified"
				result = errors.Join(result, fmt.Errorf("recover %s did not restore exact prior: %w", entry.ProductID, err))
			}
			continue
		}
		if identityMatches(discovery.Identity, entry.Prior) || (!discovery.Present && entry.Prior == nil) {
			continue
		}
		entry.State, entry.Debt = JournalEntryDebt, "native-drift"
		result = errors.Join(result, ErrInstallDrift)
	}
	if result == nil {
		return installer.Store.ClearJournal()
	}
	journal.Revision++
	return errors.Join(result, installer.Store.WriteJournal(journal))
}

func (installer Installer) reconcileCleanupObligations(ctx context.Context, inventory []productcatalog.Descriptor, journal *CrashJournal) error {
	descriptors := descriptorIndex(inventory)
	var result error
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		if !entry.CleanupRequired {
			continue
		}
		descriptor, descriptorOK := descriptors[entry.ProductID]
		strategy, strategyOK := installer.Registry.Resolve(entry.Strategy)
		if !descriptorOK || !strategyOK || entry.Planned == nil {
			entry.State, entry.Debt = JournalEntryDebt, "cleanup-unavailable"
			result = errors.Join(result, ErrInstallDebt)
			continue
		}
		if err := strategy.ReconcileCleanup(ctx, descriptor, CleanupRequest{TransactionID: journal.TransactionID, Planned: *entry.Planned}); err != nil {
			entry.State, entry.Debt = JournalEntryDebt, "staging-cleanup-failed"
			result = errors.Join(result, fmt.Errorf("reconcile %s staged cleanup: %w", entry.ProductID, err))
			continue
		}
		entry.CleanupRequired = false
		if entry.Debt == "staging-cleanup-failed" || entry.Debt == "cleanup-unavailable" {
			entry.Debt = ""
			entry.State = JournalEntryPrepared
			if entry.Installed != nil && journal.Phase != JournalRemoving {
				entry.State = JournalEntryRegistered
			}
		}
	}
	return result
}

func (installer Installer) recoverRemoval(ctx context.Context, inventory []productcatalog.Descriptor, ledger OwnershipLedger, journal CrashJournal) error {
	descriptors := descriptorIndex(inventory)
	var result error
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := &journal.Entries[index]
		if entry.Installed == nil {
			continue
		}
		descriptor, descriptorOK := descriptors[entry.ProductID]
		strategy, strategyOK := installer.Registry.Resolve(entry.Strategy)
		if !descriptorOK || !strategyOK {
			entry.State, entry.Debt = JournalEntryDebt, "strategy-unavailable"
			markReceiptDebtComposite(&ledger, entry.ProductID, entry.Strategy, *entry.Installed, "strategy-unavailable")
			result = errors.Join(result, ErrInstallDebt)
			continue
		}
		discovery, discoverErr := strategy.Discover(ctx, descriptor)
		if discoverErr != nil {
			entry.State, entry.Debt = JournalEntryDebt, "discover-failed"
			markReceiptDebtComposite(&ledger, entry.ProductID, entry.Strategy, *entry.Installed, "discover-failed")
			result = errors.Join(result, discoverErr)
			continue
		}
		if discovery.Present && discovery.Identity != nil && discovery.Identity.Equal(*entry.Installed) {
			if err := strategy.Remove(ctx, descriptor, RemovalRequest{Installed: *entry.Installed, Prior: cloneNativeIdentity(entry.Prior)}); err != nil {
				entry.State, entry.Debt = JournalEntryDebt, "remove-failed"
				markReceiptDebtComposite(&ledger, entry.ProductID, entry.Strategy, *entry.Installed, "remove-failed")
				result = errors.Join(result, err)
				continue
			}
			discovery, discoverErr = strategy.Discover(ctx, descriptor)
		}
		if discoverErr == nil && removalReachedPrior(discovery, entry.Prior) {
			ledger.Receipts = removeReceipt(ledger.Receipts, entry.ProductID, entry.Strategy, *entry.Installed)
			continue
		}
		entry.State, entry.Debt = JournalEntryDebt, "native-drift"
		markReceiptDebtComposite(&ledger, entry.ProductID, entry.Strategy, *entry.Installed, "native-drift")
		result = errors.Join(result, ErrInstallDrift, discoverErr)
	}
	if ledger.Schema != "" {
		ledger.Revision++
		if err := installer.Store.WriteLedger(ledger); err != nil {
			return errors.Join(result, err)
		}
	}
	if result == nil {
		return installer.Store.ClearJournal()
	}
	journal.Revision++
	return errors.Join(result, installer.Store.WriteJournal(journal))
}

func (installer Installer) validate(inventory []productcatalog.Descriptor) error {
	if installer.Registry == nil || installer.Store == nil {
		return errors.New("release installer requires registry and ownership store")
	}
	if err := productcatalog.ValidateInventory(inventory); err != nil {
		return err
	}
	if err := installer.Registry.validateInventory(inventory); err != nil {
		return err
	}
	for _, descriptor := range inventory {
		if _, ok := installer.Registry.Resolve(descriptor.NativeRegistration.Strategy); !ok {
			return fmt.Errorf("missing install strategy %q", descriptor.NativeRegistration.Strategy)
		}
	}
	return nil
}

func mergeOwnershipLedger(ledger OwnershipLedger, prepared []preparedInstall, request TransactionRequest) (OwnershipLedger, error) {
	if ledger.Schema == "" {
		ledger.Schema = OwnershipLedgerSchemaV1
		ledger.Revision = 1
	} else {
		ledger.Revision++
	}
	for _, item := range prepared {
		if item.installed == nil {
			continue
		}
		receipt := OwnershipReceipt{ProductID: item.descriptor.ID, Strategy: item.strategy.Key(), TransactionID: request.TransactionID, ReleaseID: request.ReleaseID, Prior: cloneNativeIdentity(item.baseline.Current), Installed: *item.installed}
		replaced := false
		for index := range ledger.Receipts {
			existing := &ledger.Receipts[index]
			if existing.ProductID == receipt.ProductID && existing.Strategy == receipt.Strategy {
				if existing.Installed.ResourceKey != receipt.Installed.ResourceKey {
					return OwnershipLedger{}, fmt.Errorf("owned resource identity changed for %s strategy %s", receipt.ProductID, receipt.Strategy)
				}
				receipt.Prior = cloneNativeIdentity(existing.Prior)
				*existing = receipt
				replaced = true
				break
			}
		}
		if !replaced {
			ledger.Receipts = append(ledger.Receipts, receipt)
		}
	}
	return ledger, nil
}

func findReceipt(receipts []OwnershipReceipt, productID, strategy string) *OwnershipReceipt {
	for index := range receipts {
		if receipts[index].ProductID == productID && receipts[index].Strategy == strategy {
			return &receipts[index]
		}
	}
	return nil
}

func descriptorIndex(inventory []productcatalog.Descriptor) map[string]productcatalog.Descriptor {
	result := make(map[string]productcatalog.Descriptor, len(inventory))
	for _, descriptor := range inventory {
		result[descriptor.ID] = descriptor
	}
	return result
}

func journalAlreadyCommitted(journal CrashJournal, ledger OwnershipLedger) bool {
	if ledger.Schema == "" {
		return false
	}
	for _, entry := range journal.Entries {
		if entry.CleanupRequired || entry.Installed == nil {
			// A prepared entry may represent a crash inside Register. It is
			// never evidence that the transaction reached the ledger commit.
			return false
		}
		found := false
		for _, receipt := range ledger.Receipts {
			if receipt.TransactionID == journal.TransactionID && receipt.ProductID == entry.ProductID && receipt.Strategy == entry.Strategy && receipt.Installed.Equal(*entry.Installed) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func identityMatches(left, right *NativeIdentity) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func removalReachedPrior(discovery Discovery, prior *NativeIdentity) bool {
	if prior == nil {
		return !discovery.Present && discovery.Identity == nil
	}
	return discovery.Present && discovery.Identity != nil && discovery.Identity.Equal(*prior)
}

func removeReceipt(receipts []OwnershipReceipt, productID, strategy string, installed NativeIdentity) []OwnershipReceipt {
	result := receipts[:0]
	for _, receipt := range receipts {
		if receipt.ProductID == productID && receipt.Strategy == strategy && receipt.Installed.Equal(installed) {
			continue
		}
		result = append(result, receipt)
	}
	return result
}

func markReceiptDebtComposite(ledger *OwnershipLedger, productID, strategy string, installed NativeIdentity, debt string) {
	for index := range ledger.Receipts {
		if ledger.Receipts[index].ProductID == productID && ledger.Receipts[index].Strategy == strategy && ledger.Receipts[index].Installed.Equal(installed) {
			ledger.Receipts[index].Debt = debt
		}
	}
}
