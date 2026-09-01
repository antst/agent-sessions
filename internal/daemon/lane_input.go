package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/statestore"
)

const laneInputMutationAttempts = 32

var (
	ErrLaneInputTooLarge    = errors.New("lane input exceeds the per-input bound")
	ErrLaneInputQuota       = errors.New("lane input quota exceeded")
	ErrLaneInputConflict    = errors.New("lane input idempotency key conflicts with prior acceptance")
	ErrLaneInputUnavailable = errors.New("lane is not a live input recipient")
	ErrLaneInputCleanupDebt = errors.New("lane input cleanup debt")
)

// LaneInputLimits bounds accepted private spool content by lane and host.
type LaneInputLimits struct {
	MaxInputBytes  int64
	MaxLaneBytes   int64
	MaxLaneObjects int
	MaxHostBytes   int64
	MaxHostObjects int
}

// DefaultLaneInputLimits keeps individual inputs aligned with the frozen
// receipt schema while bounding aggregate private disk use.
func DefaultLaneInputLimits() LaneInputLimits {
	return LaneInputLimits{
		MaxInputBytes: maxLaneInputReceiptBytes, MaxLaneBytes: 32 << 20, MaxLaneObjects: 128,
		MaxHostBytes: 256 << 20, MaxHostObjects: 2048,
	}
}

// VerifiedLaneInputMetadata is the body-free evidence returned with an
// already-open, identity-verified receipt object.
type VerifiedLaneInputMetadata struct {
	ReceiptID string
	Bytes     int64
	Digest    [sha256.Size]byte
}

// LaneInputRecoveryReport never changes a dispatching receipt implicitly.
// The coordinator must prove native acceptance, prove non-acceptance, or mark
// it ambiguous before any later dispatch.
type LaneInputRecoveryReport struct {
	Queued         int
	Dispatching    []LaneInputReceipt
	OrphansRemoved int
	ObjectsRetired int
	CleanupDebtIDs []string
}

// LaneInputEngine owns durable receipt ordering and private spool mechanics;
// it deliberately has no product-native dispatch callback.
type LaneInputEngine struct {
	store          *StateStore
	spool          *laneInputSpool
	limits         LaneInputLimits
	now            func() time.Time
	randomID       func() (string, error)
	afterSpoolSync func() error
	mu             sync.Mutex
}

func NewLaneInputEngine(store *StateStore, spoolRoot string, limits LaneInputLimits) (*LaneInputEngine, error) {
	if store == nil {
		return nil, errors.New("lane input engine requires state")
	}
	if limits.MaxInputBytes <= 0 || limits.MaxInputBytes > maxLaneInputReceiptBytes || limits.MaxLaneBytes <= 0 ||
		limits.MaxLaneObjects <= 0 || limits.MaxHostBytes <= 0 || limits.MaxHostObjects <= 0 {
		return nil, errors.New("lane input limits are invalid")
	}
	spool, err := openLaneInputSpool(spoolRoot)
	if err != nil {
		return nil, err
	}
	return &LaneInputEngine{store: store, spool: spool, limits: limits, now: time.Now, randomID: newLaneInputOpaqueID}, nil
}

// Admit durably spools, verifies, commits, and only then returns caller-visible
// acceptance. A failed state commit leaves an owned orphan for Recover.
func (e *LaneInputEngine) Admit(laneID string, body []byte) (LaneInputReceipt, error) {
	receiptID, err := newLaneInputOpaqueID()
	if err != nil {
		return LaneInputReceipt{}, err
	}
	return e.AdmitWithID(receiptID, laneID, body)
}

// AdmitWithID gives the coordinator a durable idempotency boundary. Repeating
// the same bounded key, lane, size, and digest returns the original receipt in
// any later lifecycle state; reusing a key for different input fails closed.
func (e *LaneInputEngine) AdmitWithID(receiptID, laneID string, body []byte) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !validDurableOpaqueID(receiptID) || !validDurableOpaqueID(laneID) {
		return LaneInputReceipt{}, errors.New("lane input identifier is invalid")
	}
	if int64(len(body)) > e.limits.MaxInputBytes {
		return LaneInputReceipt{}, ErrLaneInputTooLarge
	}
	bodyDigest := sha256.Sum256(body)
	snapshot, err := e.store.Read()
	if err != nil {
		return LaneInputReceipt{}, err
	}
	if existing, ok := snapshot.Catalog.LaneInputs[receiptID]; ok {
		return e.existingAdmission(existing, laneID, int64(len(body)), bodyDigest)
	}
	if err := validateLaneInputRecipient(snapshot.Catalog, laneID); err != nil {
		return LaneInputReceipt{}, err
	}
	if err := e.checkQuota(snapshot.Catalog, laneID, int64(len(body))); err != nil {
		return LaneInputReceipt{}, err
	}
	randomID, err := e.randomID()
	if err != nil {
		return LaneInputReceipt{}, err
	}
	objectID, digest, err := e.spool.create(randomID, body)
	if err != nil {
		return LaneInputReceipt{}, err
	}
	if e.afterSpoolSync != nil {
		if err := e.afterSpoolSync(); err != nil {
			return LaneInputReceipt{}, err
		}
	}
	acceptedAt := e.now().Unix()
	if acceptedAt <= 0 {
		acceptedAt = 1
	}
	var committed LaneInputReceipt
	duplicate := false
	err = e.mutate(func(catalog *Catalog) error {
		if existing, ok := catalog.LaneInputs[receiptID]; ok {
			if existing.LaneID != laneID || existing.Bytes != int64(len(body)) || existing.Digest != bodyDigest {
				return ErrLaneInputConflict
			}
			committed, duplicate = existing, true
			return nil
		}
		if err := validateLaneInputRecipient(*catalog, laneID); err != nil {
			return err
		}
		if err := e.checkQuota(*catalog, laneID, int64(len(body))); err != nil {
			return err
		}
		lane, ok := catalog.Lanes[laneID]
		if !ok {
			return errors.New("lane input lane is missing")
		}
		lane.InputSequence++
		committed = LaneInputReceipt{
			Schema: LaneInputReceiptRecordSchema, ReceiptID: receiptID, LaneID: laneID, Sequence: lane.InputSequence,
			Digest: digest, Bytes: int64(len(body)), SpoolObjectID: objectID, State: ReceiptQueued,
			Revision: 1, AcceptedAt: acceptedAt, UpdatedAt: acceptedAt,
		}
		catalog.Lanes[laneID] = lane
		catalog.LaneInputs[receiptID] = committed
		catalog.Host.LaneRevision++
		return nil
	})
	if err != nil {
		return LaneInputReceipt{}, err
	}
	if duplicate {
		orphan := LaneInputReceipt{SpoolObjectID: objectID, Digest: digest, Bytes: int64(len(body))}
		_ = e.spool.removeVerified(orphan)
	}
	return committed, nil
}

func (e *LaneInputEngine) EarliestQueued(laneID string) (LaneInputReceipt, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return LaneInputReceipt{}, false, err
	}
	if err := validateLaneInputRecipient(snapshot.Catalog, laneID); err != nil {
		return LaneInputReceipt{}, false, err
	}
	var earliest LaneInputReceipt
	found := false
	for _, receipt := range snapshot.Catalog.LaneInputs {
		if receipt.LaneID == laneID && receipt.State == ReceiptQueued && (!found || receipt.Sequence < earliest.Sequence) {
			earliest, found = receipt, true
		}
	}
	return earliest, found, nil
}

func (e *LaneInputEngine) MarkDispatching(receiptID, targetTurnID, attemptID string) (LaneInputReceipt, error) {
	if !validDurableOpaqueID(attemptID) || strings.TrimSpace(targetTurnID) == "" {
		return LaneInputReceipt{}, errors.New("lane input dispatch intent is incomplete")
	}
	return e.transitionReceipt(receiptID, func(receipt *LaneInputReceipt) error {
		if receipt.State != ReceiptQueued {
			return fmt.Errorf("lane input receipt is not queued: %s", receipt.State)
		}
		receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt = ReceiptDispatching, targetTurnID, attemptID
		return nil
	})
}

// RequeueUnsupportedSteer preserves the exact receipt and ordering authority.
func (e *LaneInputEngine) RequeueUnsupportedSteer(receiptID string) (LaneInputReceipt, error) {
	return e.requeueDispatching(receiptID)
}

// RequeueProvenNotInjected is the recovery counterpart to unsupported steer.
// The caller must first obtain product-native authoritative proof that the
// committed dispatch attempt was not accepted. The engine preserves the same
// receipt ID and lane-local sequence.
func (e *LaneInputEngine) RequeueProvenNotInjected(receiptID string) (LaneInputReceipt, error) {
	return e.requeueDispatching(receiptID)
}

func (e *LaneInputEngine) requeueDispatching(receiptID string) (LaneInputReceipt, error) {
	return e.transitionReceipt(receiptID, func(receipt *LaneInputReceipt) error {
		if receipt.State != ReceiptDispatching {
			return fmt.Errorf("lane input receipt is not dispatching: %s", receipt.State)
		}
		receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt = ReceiptQueued, "", ""
		return nil
	})
}

func (e *LaneInputEngine) MarkInjected(receiptID string, acceptance NativeAcceptanceRef) (LaneInputReceipt, error) {
	return e.transitionReceipt(receiptID, func(receipt *LaneInputReceipt) error {
		if receipt.State != ReceiptDispatching && receipt.State != ReceiptAmbiguous {
			return fmt.Errorf("lane input receipt cannot prove injection from %s", receipt.State)
		}
		if strings.TrimSpace(acceptance.NativeSessionID) == "" || acceptance.AcceptedAt <= 0 {
			return errors.New("native acceptance is incomplete")
		}
		receipt.State, receipt.NativeAcceptance, receipt.AmbiguityCause = ReceiptInjected, &acceptance, ""
		return nil
	})
}

func (e *LaneInputEngine) MarkAmbiguous(receiptID string, category AmbiguityCategory) (LaneInputReceipt, error) {
	if !knownAmbiguityCategory(category) {
		return LaneInputReceipt{}, errors.New("lane input ambiguity category is unknown")
	}
	return e.transitionReceipt(receiptID, func(receipt *LaneInputReceipt) error {
		if receipt.State != ReceiptDispatching {
			return fmt.Errorf("lane input receipt is not dispatching: %s", receipt.State)
		}
		receipt.State, receipt.AmbiguityCause = ReceiptAmbiguous, category
		return nil
	})
}

// OpenVerified returns an already-open descriptor after exact device/inode,
// type, owner, mode, size, and digest verification. No spool path is exposed.
func (e *LaneInputEngine) OpenVerified(receiptID string) (io.ReadCloser, VerifiedLaneInputMetadata, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return nil, VerifiedLaneInputMetadata{}, err
	}
	receipt, ok := snapshot.Catalog.LaneInputs[receiptID]
	if !ok || receipt.State == ReceiptRetired {
		return nil, VerifiedLaneInputMetadata{}, errors.New("lane input receipt is unavailable")
	}
	verified, err := e.spool.openVerified(receipt)
	if err != nil {
		return nil, VerifiedLaneInputMetadata{}, err
	}
	return verified.file, VerifiedLaneInputMetadata{ReceiptID: receiptID, Bytes: verified.size, Digest: verified.digest}, nil
}

func (e *LaneInputEngine) Retire(receiptID string) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return LaneInputReceipt{}, err
	}
	receipt, ok := snapshot.Catalog.LaneInputs[receiptID]
	if !ok {
		return LaneInputReceipt{}, errors.New("lane input receipt is missing")
	}
	if receipt.State != ReceiptInjected && receipt.State != ReceiptQueued && receipt.State != ReceiptAmbiguous {
		return LaneInputReceipt{}, fmt.Errorf("lane input receipt cannot retire from %s", receipt.State)
	}
	if err := e.spool.removeVerified(receipt); err != nil {
		if debtErr := e.recordCleanupDebtLocked(receipt, err); debtErr != nil {
			return LaneInputReceipt{}, errors.Join(ErrLaneInputCleanupDebt, debtErr)
		}
		return receipt, fmt.Errorf("%w: %v", ErrLaneInputCleanupDebt, err)
	}
	receipt.State, receipt.Revision = ReceiptRetired, receipt.Revision+1
	receipt.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
	if receipt.NativeAcceptance != nil && receipt.UpdatedAt < receipt.NativeAcceptance.AcceptedAt {
		receipt.UpdatedAt = receipt.NativeAcceptance.AcceptedAt
	}
	catalog := snapshot.Catalog
	catalog.LaneInputs[receiptID] = receipt
	catalog.Host.LaneRevision++
	if clearLaneInputDebt(&catalog, receiptID) {
		catalog.Host.CleanupDebtRevision++
	}
	if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
		return LaneInputReceipt{}, err
	}
	return receipt, nil
}

func (e *LaneInputEngine) Recover() (LaneInputRecoveryReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	report := LaneInputRecoveryReport{}
	if err := e.spool.validateRoot(); err != nil {
		return report, err
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return report, err
	}
	catalog := snapshot.Catalog
	referenced := make(map[string]LaneInputReceipt, len(catalog.LaneInputs))
	changed := false
	for id, receipt := range catalog.LaneInputs {
		referenced[receipt.SpoolObjectID] = receipt
		switch receipt.State {
		case ReceiptQueued, ReceiptDispatching, ReceiptAmbiguous:
			verified, verifyErr := e.spool.openVerified(receipt)
			verifiedOK := verifyErr == nil
			if verifyErr != nil {
				e.addCleanupDebt(&catalog, receipt, verifyErr)
				report.CleanupDebtIDs = append(report.CleanupDebtIDs, laneInputDebtID(receipt.ReceiptID))
				changed = true
			} else if closeErr := verified.file.Close(); closeErr != nil {
				e.addCleanupDebt(&catalog, receipt, closeErr)
				report.CleanupDebtIDs = append(report.CleanupDebtIDs, laneInputDebtID(receipt.ReceiptID))
				changed = true
				verifiedOK = false
			} else if clearLaneInputDebt(&catalog, receipt.ReceiptID) {
				catalog.Host.CleanupDebtRevision++
				changed = true
			}
			if receipt.State == ReceiptQueued {
				report.Queued++
			}
			if receipt.State == ReceiptDispatching && verifiedOK {
				report.Dispatching = append(report.Dispatching, receipt)
			}
		case ReceiptPrepared, ReceiptInjected, ReceiptRetired:
			if err := e.spool.removeVerified(receipt); err != nil {
				e.addCleanupDebt(&catalog, receipt, err)
				report.CleanupDebtIDs = append(report.CleanupDebtIDs, laneInputDebtID(receipt.ReceiptID))
				changed = true
				continue
			}
			if receipt.State == ReceiptPrepared || receipt.State == ReceiptInjected {
				receipt.State, receipt.Revision = ReceiptRetired, receipt.Revision+1
				receipt.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
				catalog.LaneInputs[id] = receipt
				changed = true
			}
			if clearLaneInputDebt(&catalog, receipt.ReceiptID) {
				catalog.Host.CleanupDebtRevision++
				changed = true
			}
			report.ObjectsRetired++
		}
	}
	entries, err := os.ReadDir(e.spool.root)
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := referenced[name]; ok || (!strings.HasPrefix(name, laneInputSpoolPrefix) && !strings.HasPrefix(name, laneInputTempPrefix)) {
			continue
		}
		removed, removeErr := e.spool.removeExactOwnedOrphan(name)
		if removeErr != nil || !removed {
			debtID := e.addOrphanCleanupDebt(&catalog, name)
			report.CleanupDebtIDs = append(report.CleanupDebtIDs, debtID)
			changed = true
			continue
		}
		report.OrphansRemoved++
	}
	if report.OrphansRemoved > 0 {
		if err := e.spool.syncRoot(); err != nil {
			return report, err
		}
	}
	if changed {
		catalog.Host.LaneRevision++
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			return report, err
		}
	}
	sort.Slice(report.Dispatching, func(i, j int) bool {
		if report.Dispatching[i].LaneID == report.Dispatching[j].LaneID {
			return report.Dispatching[i].Sequence < report.Dispatching[j].Sequence
		}
		return report.Dispatching[i].LaneID < report.Dispatching[j].LaneID
	})
	sort.Strings(report.CleanupDebtIDs)
	return report, nil
}

func (e *LaneInputEngine) transitionReceipt(receiptID string, apply func(*LaneInputReceipt) error) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var updated LaneInputReceipt
	err := e.mutate(func(catalog *Catalog) error {
		receipt, ok := catalog.LaneInputs[receiptID]
		if !ok {
			return errors.New("lane input receipt is missing")
		}
		if err := apply(&receipt); err != nil {
			return err
		}
		receipt.Revision++
		receipt.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
		if receipt.NativeAcceptance != nil && receipt.UpdatedAt < receipt.NativeAcceptance.AcceptedAt {
			receipt.UpdatedAt = receipt.NativeAcceptance.AcceptedAt
		}
		catalog.LaneInputs[receiptID] = receipt
		catalog.Host.LaneRevision++
		updated = receipt
		return nil
	})
	return updated, err
}

func (e *LaneInputEngine) checkQuota(catalog Catalog, laneID string, incoming int64) error {
	if _, ok := catalog.Lanes[laneID]; !ok {
		return errors.New("lane input lane is missing")
	}
	var laneBytes, hostBytes int64
	var laneObjects, hostObjects int
	for _, receipt := range catalog.LaneInputs {
		if receipt.State == ReceiptRetired {
			continue
		}
		if receipt.Bytes > e.limits.MaxHostBytes-hostBytes {
			return ErrLaneInputQuota
		}
		hostBytes += receipt.Bytes
		hostObjects++
		if receipt.LaneID == laneID {
			if receipt.Bytes > e.limits.MaxLaneBytes-laneBytes {
				return ErrLaneInputQuota
			}
			laneBytes += receipt.Bytes
			laneObjects++
		}
	}
	if incoming > e.limits.MaxLaneBytes-laneBytes || laneObjects >= e.limits.MaxLaneObjects ||
		incoming > e.limits.MaxHostBytes-hostBytes || hostObjects >= e.limits.MaxHostObjects {
		return ErrLaneInputQuota
	}
	return nil
}

func (e *LaneInputEngine) existingAdmission(existing LaneInputReceipt, laneID string, size int64, digest [sha256.Size]byte) (LaneInputReceipt, error) {
	if existing.LaneID != laneID || existing.Bytes != size || existing.Digest != digest {
		return LaneInputReceipt{}, ErrLaneInputConflict
	}
	return existing, nil
}

func validateLaneInputRecipient(catalog Catalog, laneID string) error {
	lane, ok := catalog.Lanes[laneID]
	if !ok {
		return errors.New("lane input lane is missing")
	}
	switch lane.State {
	case "preparing", "idle", "running", "interrupting", "terminal":
	default:
		return fmt.Errorf("%w: state=%s", ErrLaneInputUnavailable, lane.State)
	}
	for debtID := range catalog.CleanupDebts {
		if receipt, ok := catalog.LaneInputs[strings.TrimPrefix(debtID, "lane-input-")]; ok && receipt.LaneID == laneID && strings.HasPrefix(debtID, "lane-input-") {
			return fmt.Errorf("%w: unresolved lane input cleanup debt", ErrLaneInputCleanupDebt)
		}
	}
	return nil
}

func (e *LaneInputEngine) mutate(apply func(*Catalog) error) error {
	for attempt := 0; attempt < laneInputMutationAttempts; attempt++ {
		snapshot, err := e.store.Read()
		if err != nil {
			return err
		}
		catalog := snapshot.Catalog
		if err := apply(&catalog); err != nil {
			return err
		}
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("lane input state changed too frequently")
}

func (e *LaneInputEngine) recordCleanupDebtLocked(receipt LaneInputReceipt, cause error) error {
	return e.mutate(func(catalog *Catalog) error {
		e.addCleanupDebt(catalog, receipt, cause)
		return nil
	})
}

func (e *LaneInputEngine) addCleanupDebt(catalog *Catalog, receipt LaneInputReceipt, cause error) {
	id := laneInputDebtID(receipt.ReceiptID)
	prior := catalog.CleanupDebts[id]
	prior.ID = id
	prior.Resource = "lane-input:" + receipt.ReceiptID
	prior.BaselineIdentity = receipt.SpoolObjectID
	prior.IntendedState = "absent"
	prior.LastVerifiedState = "changed-or-unverifiable"
	prior.Cause = "lane-input-object-verification-failed"
	if cause == nil {
		prior.Cause = "lane-input-object-unresolved"
	}
	prior.Operation = "retire-lane-input"
	prior.RetryRevision++
	catalog.CleanupDebts[id] = prior
	catalog.Host.CleanupDebtRevision++
}

func (e *LaneInputEngine) addOrphanCleanupDebt(catalog *Catalog, objectName string) string {
	digest := sha256.Sum256([]byte(objectName))
	id := "lane-input-orphan-" + hex.EncodeToString(digest[:8])
	prior := catalog.CleanupDebts[id]
	prior.ID = id
	prior.Resource = "lane-input-orphan:" + hex.EncodeToString(digest[:8])
	prior.BaselineIdentity = "unverified-owned-prefix"
	prior.IntendedState = "absent"
	prior.LastVerifiedState = "changed-or-unverifiable"
	prior.Cause = "lane-input-orphan-verification-failed"
	prior.Operation = "retire-lane-input-orphan"
	prior.RetryRevision++
	catalog.CleanupDebts[id] = prior
	catalog.Host.CleanupDebtRevision++
	return id
}

func laneInputDebtID(receiptID string) string { return "lane-input-" + receiptID }

func clearLaneInputDebt(catalog *Catalog, receiptID string) bool {
	id := laneInputDebtID(receiptID)
	if _, ok := catalog.CleanupDebts[id]; !ok {
		return false
	}
	delete(catalog.CleanupDebts, id)
	return true
}

func newLaneInputOpaqueID() (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", fmt.Errorf("generate lane input identifier: %w", err)
	}
	return hex.EncodeToString(body), nil
}

func positiveUnix(now time.Time) int64 {
	value := now.Unix()
	if value <= 0 {
		return 1
	}
	return value
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
