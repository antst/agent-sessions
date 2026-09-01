package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

var (
	// ErrAttachmentPreparation identifies a product preparation failure.
	ErrAttachmentPreparation = errors.New("attachment preparation failed")
	// ErrAttachmentAdoption identifies rejected native evidence.
	ErrAttachmentAdoption = errors.New("attachment adoption failed")
	// ErrAttachmentRefresh identifies failed live-evidence reconciliation.
	ErrAttachmentRefresh = errors.New("attachment refresh failed")
	// ErrAttachmentDetach identifies failed exact native cleanup.
	ErrAttachmentDetach = errors.New("attachment detach failed")
	// ErrAttachmentRollback identifies failed exact preparation rollback.
	ErrAttachmentRollback = errors.New("attachment rollback failed")
	// ErrAttachmentConflict identifies changed reuse or an invalid lifecycle call.
	ErrAttachmentConflict = errors.New("attachment transaction conflict")
)

// AttachmentAdapter retains product-native behavior at the edge of the shared
// durable transaction. Each product supplies its own evidence checks and exact
// cleanup operations; the shared engine never infers one product from another.
type AttachmentAdapter struct {
	Prepare   func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Adopt     func(context.Context, ManagedAttachment, NativeEvidence) (NativeEvidence, error)
	Refresh   func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Authorize func(context.Context, ManagedAttachment, NativeEvidence) error
	Detach    func(context.Context, ManagedAttachment) error
	Rollback  func(context.Context, ManagedAttachment) error
}

// AttachmentEngine serializes the shared durable attachment lifecycle for one
// daemon generation.
type AttachmentEngine struct {
	mu         sync.Mutex
	store      *StateStore
	generation uint64
	adapters   map[string]AttachmentAdapter
	titles     map[string]liveNativeTitle
}

type liveNativeTitle struct {
	nativeSessionID string
	value           string
}

// NewAttachmentEngine creates an attachment authority over one durable store.
func NewAttachmentEngine(store *StateStore, generation uint64, adapters map[string]AttachmentAdapter) (*AttachmentEngine, error) {
	if store == nil {
		return nil, errors.New("attachment state store is nil")
	}
	if generation == 0 {
		return nil, errors.New("attachment daemon generation must be positive")
	}
	copyAdapters := make(map[string]AttachmentAdapter, len(adapters))
	for product, adapter := range adapters {
		if _, ok := productcatalog.ByID(product); !ok {
			return nil, fmt.Errorf("attachment adapter has unknown product %q", product)
		}
		copyAdapters[product] = adapter
	}
	return &AttachmentEngine{
		store: store, generation: generation, adapters: copyAdapters,
		titles: make(map[string]liveNativeTitle),
	}, nil
}

// SetAdapter replaces one product callback set. Runtime composition uses this
// when a product coordinator reconnects; durable ownership remains unchanged.
func (e *AttachmentEngine) SetAdapter(product string, adapter AttachmentAdapter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := productcatalog.ByID(product); ok {
		e.adapters[product] = adapter
	}
}

// Prepare durably records intent before invoking the product preparation.
// Prepared attachments are not discoverable or addressable.
func (e *AttachmentEngine) Prepare(ctx context.Context, requested ManagedAttachment) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := validateAttachmentRequest(requested); err != nil {
		return ManagedAttachment{}, err
	}
	adapter, ok := e.adapters[requested.Product]
	if !ok || adapter.Prepare == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s prepare callback is unavailable", ErrAttachmentPreparation, requested.Product)
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return ManagedAttachment{}, err
	}
	if existing, exists := snapshot.Catalog.Attachments[requested.ID]; exists && existing.State != "detached" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s already exists in state %s", ErrAttachmentConflict, requested.ID, existing.State)
	}
	requested.State = "preparing"
	requested.ExpectedEvidence = NativeEvidence{}
	requested.Evidence = NativeEvidence{}
	prepared, err := e.commitAttachment(snapshot, requested, nil, false)
	if err != nil {
		return ManagedAttachment{}, err
	}
	expected, prepareErr := adapter.Prepare(ctx, cloneAttachment(prepared))
	if prepareErr != nil {
		_, rollbackErr := e.rollbackLocked(ctx, requested.ID, "prepare-failed", adapter)
		if rollbackErr != nil {
			return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentPreparation, errors.Join(prepareErr, rollbackErr))
		}
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentPreparation, prepareErr)
	}
	snapshot, prepared, err = e.currentAttachment(requested.ID)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if prepared.State != "preparing" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s changed during preparation", ErrAttachmentConflict, requested.ID)
	}
	prepared.ExpectedEvidence = cloneEvidence(expected)
	prepared.State = "prepared"
	return e.commitAttachment(snapshot, prepared, nil, false)
}

// Adopt corroborates product-native evidence and grants authority only after
// the attached state is durably committed.
func (e *AttachmentEngine) Adopt(ctx context.Context, id string, observed NativeEvidence) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State == "attached" {
		if reflect.DeepEqual(attachment.Evidence, observed) {
			return cloneAttachment(attachment), nil
		}
		return ManagedAttachment{}, fmt.Errorf("%w: attached evidence changed", ErrAttachmentConflict)
	}
	if attachment.State != "prepared" && attachment.State != "selecting" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s cannot adopt from %s", ErrAttachmentConflict, id, attachment.State)
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Adopt == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s adopt callback is unavailable", ErrAttachmentAdoption, attachment.Product)
	}
	if attachment.State == "prepared" {
		attachment.State = "selecting"
		attachment, err = e.commitAttachment(snapshot, attachment, nil, false)
		if err != nil {
			return ManagedAttachment{}, err
		}
	}
	corroborated, adoptionErr := adapter.Adopt(ctx, cloneAttachment(attachment), cloneEvidence(observed))
	if adoptionErr != nil {
		_, rollbackErr := e.rollbackLocked(ctx, id, "adopt-failed", adapter)
		if rollbackErr != nil {
			return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentAdoption, errors.Join(adoptionErr, rollbackErr))
		}
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentAdoption, adoptionErr)
	}
	snapshot, attachment, err = e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State != "selecting" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s changed during adoption", ErrAttachmentConflict, id)
	}
	attachment.Evidence = cloneEvidence(corroborated)
	attachment.State = "attached"
	return e.commitAttachment(snapshot, attachment, nil, true)
}

// SelectNative binds a late native selection (for example Claude's interactive
// resume-by-name picker) to an already prepared daemon attachment. It never
// makes the attachment addressable; Adopt must still corroborate the complete
// product-native evidence afterward.
func (e *AttachmentEngine) SelectNative(id, nativeSessionID, cwd, permissionMode string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(nativeSessionID) == "" {
		return ManagedAttachment{}, fmt.Errorf("%w: selected native session is empty", ErrAttachmentConflict)
	}
	snapshot, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State != "prepared" && attachment.State != "selecting" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s cannot select from %s", ErrAttachmentConflict, id, attachment.State)
	}
	if attachment.NativeSessionID != "" && attachment.NativeSessionID != nativeSessionID {
		return ManagedAttachment{}, fmt.Errorf("%w: selected native session changed", ErrAttachmentConflict)
	}
	attachment.NativeSessionID = nativeSessionID
	if strings.TrimSpace(cwd) != "" {
		attachment.Cwd = cwd
	}
	if strings.TrimSpace(permissionMode) != "" {
		attachment.PermissionMode = permissionMode
	}
	attachment.State = "selecting"
	return e.commitAttachment(snapshot, attachment, nil, false)
}

// Refresh asks the product adapter to recorroborate one active attachment.
func (e *AttachmentEngine) Refresh(ctx context.Context, id string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State != "attached" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s is not active", ErrAttachmentConflict, id)
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Refresh == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s refresh callback is unavailable", ErrAttachmentRefresh, attachment.Product)
	}
	evidence, err := adapter.Refresh(ctx, cloneAttachment(attachment))
	if err != nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentRefresh, err)
	}
	attachment.Evidence = cloneEvidence(evidence)
	return e.commitAttachment(snapshot, attachment, nil, true)
}

// Detach withdraws authority durably before exact product cleanup. A cleanup
// ambiguity leaves retryable debt and never keeps the attachment addressable.
func (e *AttachmentEngine) Detach(ctx context.Context, id, cause string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State == "detached" {
		return cloneAttachment(attachment), nil
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Detach == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s detach callback is unavailable", ErrAttachmentDetach, attachment.Product)
	}
	return e.detachLocked(ctx, id, cause, adapter.Detach, "detach-"+attachment.Product, ErrAttachmentDetach)
}

// Rollback retries product-specific preparation rollback and clears exact debt
// only after the adapter proves cleanup complete.
func (e *AttachmentEngine) Rollback(ctx context.Context, id, cause string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State == "detached" {
		return cloneAttachment(attachment), nil
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok {
		return ManagedAttachment{}, fmt.Errorf("%w: %s rollback adapter is unavailable", ErrAttachmentRollback, attachment.Product)
	}
	return e.rollbackLocked(ctx, id, cause, adapter)
}

// ListActive returns stable isolated copies of only currently addressable peers.
func (e *AttachmentEngine) ListActive() ([]ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return nil, err
	}
	result := make([]ManagedAttachment, 0, len(snapshot.Catalog.Attachments))
	for _, attachment := range snapshot.Catalog.Attachments {
		if attachment.State == "attached" && attachment.DaemonGeneration == e.generation {
			result = append(result, cloneAttachment(attachment))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// ActiveAttachment returns one current-generation addressable attachment.
func (e *AttachmentEngine) ActiveAttachment(id string) (ManagedAttachment, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return ManagedAttachment{}, false, err
	}
	attachment, ok := snapshot.Catalog.Attachments[id]
	if !ok || attachment.State != "attached" || attachment.DaemonGeneration != e.generation {
		return ManagedAttachment{}, false, nil
	}
	return cloneAttachment(attachment), true, nil
}

// ObserveNativeTitle follows one exact live product session's title in memory.
// The observation is generation-local, never enters the durable catalog, and
// is discarded whenever the attachment stops being active.
func (e *AttachmentEngine) ObserveNativeTitle(id, nativeSessionID, title string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !validNativeTitleObservation(title) {
		return fmt.Errorf("%w: native title observation is unsafe", ErrAttachmentConflict)
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return err
	}
	attachment, ok := snapshot.Catalog.Attachments[id]
	if !ok || attachment.State != "attached" || attachment.DaemonGeneration != e.generation ||
		attachment.NativeSessionID == "" || attachment.NativeSessionID != nativeSessionID {
		return fmt.Errorf("%w: attachment %s is not the exact live native session", ErrAttachmentConflict, id)
	}
	e.titles[id] = liveNativeTitle{nativeSessionID: nativeSessionID, value: title}
	return nil
}

// LiveNativeTitle returns a title only while its exact attachment/session is
// active in this daemon generation. The boolean distinguishes an observed
// empty native title from a connection that has not announced one.
func (e *AttachmentEngine) LiveNativeTitle(id string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return "", false, err
	}
	attachment, ok := snapshot.Catalog.Attachments[id]
	observation, observed := e.titles[id]
	if !ok || attachment.State != "attached" || attachment.DaemonGeneration != e.generation ||
		!observed || observation.nativeSessionID != attachment.NativeSessionID {
		return "", false, nil
	}
	return observation.value, true, nil
}

// Authorize corroborates one hook or connector against the active attachment.
// Product-native evidence is authoritative; a launch capability is an optional
// additional gate for products whose established protocol supplies one.
func (e *AttachmentEngine) Authorize(
	ctx context.Context,
	id, capability, product string,
	evidence NativeEvidence,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, attachment, err := e.currentAttachment(id)
	if err != nil || attachment.State != "attached" || attachment.DaemonGeneration != e.generation {
		return InactiveControlError()
	}
	if product != "" && product != attachment.Product {
		return InactiveControlError()
	}
	if capability != "" && !capabilityMatches(attachment.CapabilityHash, capability) {
		return InactiveControlError()
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Authorize == nil {
		// Compatibility for internal callers that already present the exact
		// daemon-minted capability. Native relays must provide an Authorize
		// callback when no capability exists at their product boundary.
		if capability != "" && capabilityMatches(attachment.CapabilityHash, capability) {
			return nil
		}
		return InactiveControlError()
	}
	if err := adapter.Authorize(ctx, cloneAttachment(attachment), cloneEvidence(evidence)); err != nil {
		return InactiveControlError()
	}
	return nil
}

func (e *AttachmentEngine) rollbackLocked(ctx context.Context, id, cause string, adapter AttachmentAdapter) (ManagedAttachment, error) {
	if adapter.Rollback == nil {
		return e.recordAttachmentDebt(id, cause, "rollback-callback-unavailable", "rollback", ErrAttachmentRollback)
	}
	return e.detachLocked(ctx, id, cause, adapter.Rollback, "rollback", ErrAttachmentRollback)
}

func (e *AttachmentEngine) detachLocked(
	ctx context.Context,
	id, cause string,
	cleanup func(context.Context, ManagedAttachment) error,
	operation string,
	rootError error,
) (ManagedAttachment, error) {
	snapshot, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State == "detached" {
		return cloneAttachment(attachment), nil
	}
	if attachment.State != "preparing" && attachment.State != "detaching" {
		if !ValidLifecycleTransition("attachment", attachment.State, "detaching") {
			return ManagedAttachment{}, fmt.Errorf("%w: attachment %s cannot clean up from %s", ErrAttachmentConflict, id, attachment.State)
		}
		attachment.State = "detaching"
		attachment, err = e.commitAttachment(snapshot, attachment, nil, false)
		if err != nil {
			return ManagedAttachment{}, err
		}
	}
	if err := cleanup(ctx, cloneAttachment(attachment)); err != nil {
		return e.recordAttachmentDebt(id, cause, err.Error(), operationForAttachment(operation, attachment.Product), rootError)
	}
	snapshot, attachment, err = e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	attachment.State = "detached"
	return e.commitAttachment(snapshot, attachment, nil, true)
}

func (e *AttachmentEngine) recordAttachmentDebt(id, cause, lastState, operation string, rootError error) (ManagedAttachment, error) {
	snapshot, attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	attachment.State = "debt"
	debt := CleanupDebt{
		ID: attachmentDebtID(id), Resource: "attachment:" + id,
		BaselineIdentity: attachmentEvidenceIdentity(attachment), IntendedState: "detached",
		LastVerifiedState: "unknown", Cause: strings.TrimSpace(cause + ": " + lastState),
		RetryRevision: attachment.CatalogRevision + 1, Operation: operationForAttachment(operation, attachment.Product),
	}
	committed, commitErr := e.commitAttachment(snapshot, attachment, &debt, false)
	if commitErr != nil {
		return ManagedAttachment{}, errors.Join(rootError, commitErr)
	}
	return committed, fmt.Errorf("%w: %s", rootError, lastState)
}

func (e *AttachmentEngine) currentAttachment(id string) (StateSnapshot, ManagedAttachment, error) {
	if strings.TrimSpace(id) == "" {
		return StateSnapshot{}, ManagedAttachment{}, fmt.Errorf("%w: attachment id is empty", ErrAttachmentConflict)
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return StateSnapshot{}, ManagedAttachment{}, err
	}
	attachment, ok := snapshot.Catalog.Attachments[id]
	if !ok {
		return StateSnapshot{}, ManagedAttachment{}, fmt.Errorf("%w: attachment %s does not exist", ErrAttachmentConflict, id)
	}
	return snapshot, cloneAttachment(attachment), nil
}

func (e *AttachmentEngine) commitAttachment(
	snapshot StateSnapshot,
	attachment ManagedAttachment,
	debt *CleanupDebt,
	clearDebt bool,
) (ManagedAttachment, error) {
	if attachment.State != "attached" {
		delete(e.titles, attachment.ID)
	}
	catalog := snapshot.Catalog
	catalog.Host.Generation = e.generation
	catalog.Host.AttachmentRevision++
	attachment.DaemonGeneration = e.generation
	attachment.CatalogRevision = catalog.Host.AttachmentRevision
	catalog.Attachments[attachment.ID] = cloneAttachment(attachment)
	if debt != nil {
		debt.RetryRevision = attachment.CatalogRevision
		catalog.CleanupDebts[debt.ID] = *debt
	}
	if clearDebt {
		delete(catalog.CleanupDebts, attachmentDebtID(attachment.ID))
	}
	committed, err := e.store.Commit(snapshot.Revision, catalog)
	if err != nil {
		return ManagedAttachment{}, err
	}
	return cloneAttachment(committed.Catalog.Attachments[attachment.ID]), nil
}

func validNativeTitleObservation(value string) bool {
	if len([]byte(value)) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validateAttachmentRequest(attachment ManagedAttachment) error {
	if strings.TrimSpace(attachment.ID) == "" || strings.TrimSpace(attachment.CapabilityHash) == "" ||
		strings.TrimSpace(attachment.Product) == "" || strings.TrimSpace(attachment.ProfileIdentity) == "" {
		return fmt.Errorf("%w: attachment identity, capability, product, and profile are required", ErrAttachmentConflict)
	}
	if attachment.State != "" {
		return fmt.Errorf("%w: caller cannot select attachment lifecycle state", ErrAttachmentConflict)
	}
	if _, ok := productcatalog.ByID(attachment.Product); !ok {
		return fmt.Errorf("%w: unknown product %q", ErrAttachmentConflict, attachment.Product)
	}
	return nil
}

func operationForAttachment(operation, product string) string {
	if strings.Contains(operation, "-") {
		return operation
	}
	return operation + "-" + product
}

func attachmentDebtID(id string) string { return "attachment-cleanup:" + id }

func attachmentEvidenceIdentity(attachment ManagedAttachment) string {
	evidence := attachment.Evidence
	if evidence.Process.PID == 0 {
		evidence = attachment.ExpectedEvidence
	}
	return fmt.Sprintf("product=%s,pid=%d,start=%s,artifact=%s,revision=%s",
		attachment.Product, evidence.Process.PID, evidence.Process.StrongStart, evidence.ArtifactPath, evidence.ArtifactRevision)
}

func cloneAttachment(attachment ManagedAttachment) ManagedAttachment {
	attachment.Groups = append([]string(nil), attachment.Groups...)
	attachment.ExpectedEvidence = cloneEvidence(attachment.ExpectedEvidence)
	attachment.Evidence = cloneEvidence(attachment.Evidence)
	return attachment
}

func cloneEvidence(evidence NativeEvidence) NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
