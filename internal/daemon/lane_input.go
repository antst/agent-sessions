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

// LaneCommandReceiptPrefix reserves the product-neutral opaque ID namespace
// used by the two-commit start/resume caller acceptance boundary.
const LaneCommandReceiptPrefix = "command-"

var (
	ErrLaneInputTooLarge      = errors.New("lane input exceeds the per-input bound")
	ErrLaneInputQuota         = errors.New("lane input quota exceeded")
	ErrLaneInputConflict      = errors.New("lane input idempotency key conflicts with prior acceptance")
	ErrLaneInputUnavailable   = errors.New("lane is not a live input recipient")
	ErrLaneInputCleanupDebt   = errors.New("lane input cleanup debt")
	ErrLaneInputEarlierQueued = errors.New("lane has earlier queued input")
	errLaneInputAlreadyExact  = errors.New("lane input native acceptance is already exact")
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
	store             *StateStore
	spool             *laneInputSpool
	limits            LaneInputLimits
	now               func() time.Time
	randomID          func() (string, error)
	afterSpoolSync    func() error
	afterQueuedCommit func() error
	mu                sync.Mutex
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

// CreateLaneAdmitAndMarkDispatching durably stages a new lane and queued input,
// then atomically creates its accepted daemon turn and commits the receipt's
// dispatch intent. Callers must not acknowledge acceptance until this method
// returns the Dispatching receipt from the second commit.
func (e *LaneInputEngine) CreateLaneAdmitAndMarkDispatching(
	receiptID string, lane Lane, turn Turn, attemptID string, body []byte,
) (LaneInputReceipt, error) {
	return e.admitLaneMutationWithID(receiptID, lane, turn, attemptID, body, true)
}

// UpdateLaneAdmitAndMarkDispatching durably stages a validated lane resume and
// queued input, then atomically creates its accepted daemon turn and commits
// the receipt's dispatch intent. Active, retiring, and cleanup-debt lanes
// remain ineligible.
func (e *LaneInputEngine) UpdateLaneAdmitAndMarkDispatching(
	receiptID string, lane Lane, turn Turn, attemptID string, body []byte,
) (LaneInputReceipt, error) {
	return e.admitLaneMutationWithID(receiptID, lane, turn, attemptID, body, false)
}

func (e *LaneInputEngine) admitLaneMutationWithID(
	receiptID string,
	lane Lane,
	turn Turn,
	attemptID string,
	body []byte,
	create bool,
) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !validDurableOpaqueID(receiptID) || !strings.HasPrefix(receiptID, LaneCommandReceiptPrefix) ||
		!validDurableOpaqueID(lane.ID) || lane.Product == "" {
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
	var committed LaneInputReceipt
	existingQueued := false
	if existing, ok := snapshot.Catalog.LaneInputs[receiptID]; ok {
		committed, err = e.existingAdmission(existing, lane.ID, int64(len(body)), bodyDigest)
		if err != nil {
			return LaneInputReceipt{}, err
		}
		existingQueued = committed.State == ReceiptQueued
	} else {
		if turn.ID == "" || turn.LaneID != lane.ID || !validDurableOpaqueID(attemptID) {
			return LaneInputReceipt{}, errors.New("lane input identifier is invalid")
		}
		if create {
			if _, exists := snapshot.Catalog.Lanes[lane.ID]; exists {
				return LaneInputReceipt{}, errors.New("lane input start lane already exists without its receipt")
			}
		} else {
			current, exists := snapshot.Catalog.Lanes[lane.ID]
			if !exists || current.Product != lane.Product {
				return LaneInputReceipt{}, errors.New("lane input resume lane is missing or changed product")
			}
			for _, candidate := range snapshot.Catalog.LaneInputs {
				if candidate.LaneID == lane.ID && candidate.State == ReceiptQueued {
					return LaneInputReceipt{}, ErrLaneInputEarlierQueued
				}
			}
			switch current.State {
			case "idle", "terminal", "archived":
			default:
				return LaneInputReceipt{}, fmt.Errorf("%w: state=%s", ErrLaneInputUnavailable, current.State)
			}
		}
		quotaCatalog := snapshot.Catalog
		if create {
			quotaCatalog.Lanes[lane.ID] = lane
		}
		if err := e.checkQuota(quotaCatalog, lane.ID, int64(len(body))); err != nil {
			return LaneInputReceipt{}, err
		}
		randomID, randomErr := e.randomID()
		if randomErr != nil {
			return LaneInputReceipt{}, randomErr
		}
		objectID, digest, spoolErr := e.spool.create(randomID, body)
		if spoolErr != nil {
			return LaneInputReceipt{}, spoolErr
		}
		if e.afterSpoolSync != nil {
			if syncErr := e.afterSpoolSync(); syncErr != nil {
				return LaneInputReceipt{}, syncErr
			}
		}
		acceptedAt := positiveUnix(e.now())
		duplicate := false
		err = e.mutate(func(catalog *Catalog) error {
			if existing, ok := catalog.LaneInputs[receiptID]; ok {
				if existing.LaneID != lane.ID || existing.Bytes != int64(len(body)) || existing.Digest != bodyDigest {
					return ErrLaneInputConflict
				}
				committed, duplicate = existing, true
				return nil
			}
			current, exists := catalog.Lanes[lane.ID]
			if create {
				if exists {
					return errors.New("lane input start lane already exists without its receipt")
				}
				lane.InputSequence, lane.ArchiveRevision = 0, 0
			} else {
				if !exists || current.Product != lane.Product {
					return errors.New("lane input resume lane is missing or changed product")
				}
				for _, candidate := range catalog.LaneInputs {
					if candidate.LaneID == lane.ID && candidate.State == ReceiptQueued {
						return ErrLaneInputEarlierQueued
					}
				}
				switch current.State {
				case "idle", "terminal", "archived":
				default:
					return fmt.Errorf("%w: state=%s", ErrLaneInputUnavailable, current.State)
				}
				lane.InputSequence, lane.ArchiveRevision = current.InputSequence, current.ArchiveRevision
				if lane.NativeSessionID == "" {
					lane.NativeSessionID = current.NativeSessionID
				}
			}
			quotaCatalog := *catalog
			if create {
				quotaCatalog.Lanes[lane.ID] = lane
			}
			if err := e.checkQuota(quotaCatalog, lane.ID, int64(len(body))); err != nil {
				return err
			}
			// The coordinator-owned receipt ID namespace distinguishes this
			// unacknowledged queued phase without extending the frozen schema.
			// Preserve an existing resume lifecycle until the atomic turn/dispatch
			// commit; a newly created lane begins idle.
			lane.State, lane.CapabilityHash, lane.AutoArchiveAt = "idle", "", 0
			if !create {
				lane.State = current.State
			}
			lane.InputSequence++
			committed = LaneInputReceipt{
				Schema: LaneInputReceiptRecordSchema, ReceiptID: receiptID, LaneID: lane.ID, Sequence: lane.InputSequence,
				Digest: digest, Bytes: int64(len(body)), SpoolObjectID: objectID, State: ReceiptQueued,
				Revision: 1, AcceptedAt: acceptedAt, UpdatedAt: acceptedAt,
			}
			catalog.Lanes[lane.ID] = cloneLane(lane)
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
		if e.afterQueuedCommit != nil {
			if queuedErr := e.afterQueuedCommit(); queuedErr != nil {
				return committed, queuedErr
			}
		}
	}
	if existingQueued {
		// A replay never reuses the caller-supplied process-local Turn. The
		// coordinator must select this receipt through the ordered queue and
		// allocate a fresh Turn/attempt for every dispatch.
		return committed, nil
	}
	if committed.State != ReceiptQueued {
		return committed, nil
	}
	var dispatching LaneInputReceipt
	err = e.mutate(func(catalog *Catalog) error {
		receipt, ok := catalog.LaneInputs[receiptID]
		if !ok || receipt.State != ReceiptQueued || receipt.LaneID != lane.ID {
			return errors.New("lane input receipt is not queued for initial dispatch")
		}
		current, ok := catalog.Lanes[lane.ID]
		if !ok || current.Product != lane.Product {
			return errors.New("lane input target lane is missing or changed product")
		}
		switch current.State {
		case "preparing", "idle", "terminal", "archived":
		default:
			return fmt.Errorf("lane cannot accept queued input from state %s", current.State)
		}
		if _, exists := catalog.Turns[turn.ID]; exists {
			return errors.New("lane input target turn identity already exists")
		}
		turnSequence := uint64(1)
		for _, candidate := range catalog.Turns {
			if candidate.LaneID == lane.ID && candidate.Sequence >= turnSequence {
				turnSequence = candidate.Sequence + 1
			}
		}
		lane.InputSequence, lane.ArchiveRevision = current.InputSequence, current.ArchiveRevision
		lane.State, lane.CapabilityHash, lane.AutoArchiveAt = "idle", "", 0
		turn.State, turn.Sequence = "accepted", turnSequence
		now := maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
		receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt = ReceiptDispatching, turn.ID, attemptID
		receipt.Revision, receipt.UpdatedAt = receipt.Revision+1, now
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Turns[turn.ID] = turn
		catalog.LaneInputs[receiptID] = receipt
		catalog.Host.LaneRevision++
		dispatching = receipt
		return nil
	})
	if err != nil {
		return committed, err
	}
	return dispatching, nil
}

// RetireStagedLane atomically makes one never-acknowledged command receipt and
// its lane non-addressable after the verified spool object has been removed.
// Revision one in the command namespace is the sole staging proof.
func (e *LaneInputEngine) RetireStagedLane(receiptID string) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.store.Read()
	if err != nil {
		return LaneInputReceipt{}, err
	}
	receipt, ok := snapshot.Catalog.LaneInputs[receiptID]
	if !ok || receipt.State != ReceiptQueued || receipt.Revision != 1 ||
		!strings.HasPrefix(receipt.ReceiptID, LaneCommandReceiptPrefix) {
		return LaneInputReceipt{}, errors.New("lane input receipt is not staged")
	}
	if err := e.spool.removeVerified(receipt); err != nil {
		if debtErr := e.recordCleanupDebtLocked(receipt, err); debtErr != nil {
			return LaneInputReceipt{}, errors.Join(ErrLaneInputCleanupDebt, debtErr)
		}
		return receipt, fmt.Errorf("%w: %v", ErrLaneInputCleanupDebt, err)
	}
	var retired LaneInputReceipt
	err = e.mutate(func(catalog *Catalog) error {
		current, exists := catalog.LaneInputs[receiptID]
		if !exists || current.State != ReceiptQueued || current.Revision != 1 || current.LaneID != receipt.LaneID {
			return errors.New("lane input staging identity changed")
		}
		lane, exists := catalog.Lanes[current.LaneID]
		if !exists {
			return errors.New("staged lane disappeared")
		}
		switch lane.State {
		case "idle", "terminal", "archived":
		default:
			return fmt.Errorf("staged lane cannot retire from %s", lane.State)
		}
		now := maxInt64(positiveUnix(e.now()), current.UpdatedAt)
		current.State, current.Revision, current.UpdatedAt = ReceiptRetired, current.Revision+1, now
		lane.State, lane.CapabilityHash, lane.AutoArchiveAt = "archived", "", 0
		catalog.LaneInputs[receiptID], catalog.Lanes[lane.ID] = current, lane
		catalog.Host.LaneRevision++
		retired = current
		return nil
	})
	return retired, err
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

// AcceptTurnAndMarkDispatching atomically creates the daemon turn targeted by
// one queued receipt and commits its dispatch intent. There is no crash window
// in which an accepted turn exists without the receipt that owns its input.
func (e *LaneInputEngine) AcceptTurnAndMarkDispatching(
	receiptID string,
	lane Lane,
	turn Turn,
	attemptID string,
) (LaneInputReceipt, error) {
	return e.acceptTurnAndMarkDispatching(receiptID, lane, turn, attemptID, false)
}

// AcceptStagedTurnAndMarkDispatching is the same-key retry boundary for a
// command whose first commit survived without caller acknowledgement. It is
// the only public claim that may advance a revision-one command receipt.
func (e *LaneInputEngine) AcceptStagedTurnAndMarkDispatching(
	receiptID string,
	lane Lane,
	turn Turn,
	attemptID string,
) (LaneInputReceipt, error) {
	return e.acceptTurnAndMarkDispatching(receiptID, lane, turn, attemptID, true)
}

func (e *LaneInputEngine) acceptTurnAndMarkDispatching(
	receiptID string,
	lane Lane,
	turn Turn,
	attemptID string,
	allowStaged bool,
) (LaneInputReceipt, error) {
	if lane.ID == "" || turn.ID == "" || turn.LaneID != lane.ID || !validDurableOpaqueID(attemptID) {
		return LaneInputReceipt{}, errors.New("lane input atomic dispatch intent is incomplete")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var updated LaneInputReceipt
	err := e.mutate(func(catalog *Catalog) error {
		receipt, ok := catalog.LaneInputs[receiptID]
		if !ok || receipt.LaneID != lane.ID || receipt.State != ReceiptQueued {
			return errors.New("lane input receipt is not queued for this lane")
		}
		staged := receipt.Revision == 1 && strings.HasPrefix(receipt.ReceiptID, LaneCommandReceiptPrefix)
		if staged && !allowStaged {
			return errors.New("unacknowledged staged lane input is not dispatch eligible")
		}
		for _, candidate := range catalog.LaneInputs {
			if candidate.LaneID == lane.ID && candidate.State == ReceiptQueued && candidate.Sequence < receipt.Sequence {
				return ErrLaneInputEarlierQueued
			}
		}
		current, ok := catalog.Lanes[lane.ID]
		if !ok || current.Product != lane.Product {
			return errors.New("lane input target lane is missing or changed product")
		}
		if _, exists := catalog.Turns[turn.ID]; exists {
			return errors.New("lane input target turn identity already exists")
		}
		switch current.State {
		case "idle", "terminal":
		default:
			return fmt.Errorf("lane cannot accept queued input from state %s", current.State)
		}
		sequence := uint64(1)
		for _, candidate := range catalog.Turns {
			if candidate.LaneID == lane.ID && candidate.Sequence >= sequence {
				sequence = candidate.Sequence + 1
			}
		}
		lane.State, lane.CapabilityHash, lane.AutoArchiveAt = "idle", "", 0
		lane.ArchiveRevision, lane.InputSequence = current.ArchiveRevision, current.InputSequence
		turn.State, turn.Sequence = "accepted", sequence
		now := maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
		receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt = ReceiptDispatching, turn.ID, attemptID
		receipt.Revision, receipt.UpdatedAt = receipt.Revision+1, now
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Turns[turn.ID] = turn
		catalog.LaneInputs[receiptID] = receipt
		catalog.Host.LaneRevision++
		updated = receipt
		return nil
	})
	return updated, err
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

// RecoverAcceptedTurnAndRequeue closes the sole proven-pre-native crash window:
// the receipt and accepted daemon turn were committed atomically, but the lane
// never left idle and therefore no native authorization or I/O could occur.
// The orphan accepted turn is terminalized in the same revision that restores
// the exact receipt to its ordered queue.
func (e *LaneInputEngine) RecoverAcceptedTurnAndRequeue(receiptID, diagnostic string) (LaneInputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var updated LaneInputReceipt
	err := e.mutate(func(catalog *Catalog) error {
		receipt, ok := catalog.LaneInputs[receiptID]
		if !ok || receipt.State != ReceiptDispatching || receipt.TargetTurnID == "" {
			return errors.New("lane input receipt has no recoverable accepted turn")
		}
		lane, laneOK := catalog.Lanes[receipt.LaneID]
		turn, turnOK := catalog.Turns[receipt.TargetTurnID]
		if !laneOK || !turnOK || turn.LaneID != lane.ID || lane.State != "idle" || turn.State != "accepted" {
			return errors.New("lane input accepted turn is not proven pre-native")
		}
		now := maxInt64(positiveUnix(e.now()), maxInt64(receipt.AcceptedAt, receipt.UpdatedAt))
		catalog.Host.LaneRevision++
		turn.State, turn.Outcome, turn.Diagnostic = "terminal", "interrupted", diagnostic
		turn.CompletedAt, turn.TerminalRevision = e.now().UnixMilli(), catalog.Host.LaneRevision
		lane.State, lane.CapabilityHash = "terminal", ""
		receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt = ReceiptQueued, "", ""
		receipt.Revision, receipt.UpdatedAt = receipt.Revision+1, now
		catalog.Lanes[lane.ID] = lane
		catalog.Turns[turn.ID] = turn
		catalog.LaneInputs[receiptID] = receipt
		updated = receipt
		return nil
	})
	return updated, err
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

// MarkInjectedAndSetNativeDispatch atomically commits exact native acceptance
// evidence and the daemon turn reattachment anchor returned by the same native
// acknowledgement. A crash can therefore observe neither fact or both facts,
// but never an injected receipt whose target turn cannot be reattached.
func (e *LaneInputEngine) MarkInjectedAndSetNativeDispatch(
	receiptID string,
	acceptance NativeAcceptanceRef,
) (LaneInputReceipt, error) {
	if strings.TrimSpace(acceptance.NativeSessionID) == "" ||
		strings.TrimSpace(acceptance.NativeMessageID) == "" || acceptance.AcceptedAt <= 0 {
		return LaneInputReceipt{}, errors.New("exact native acceptance is incomplete")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var updated LaneInputReceipt
	alreadyExact := false
	err := e.mutate(func(catalog *Catalog) error {
		receipt, ok := catalog.LaneInputs[receiptID]
		if !ok || receipt.TargetTurnID == "" {
			return errors.New("lane input receipt has no dispatching target")
		}
		turn, ok := catalog.Turns[receipt.TargetTurnID]
		if !ok || turn.LaneID != receipt.LaneID {
			return errors.New("lane input target turn is missing or changed lane")
		}
		if receipt.State == ReceiptInjected && receipt.NativeAcceptance != nil &&
			receipt.NativeAcceptance.NativeSessionID == acceptance.NativeSessionID &&
			receipt.NativeAcceptance.NativeMessageID == acceptance.NativeMessageID &&
			turn.NativeDispatchID == acceptance.NativeMessageID {
			updated, alreadyExact = receipt, true
			return errLaneInputAlreadyExact
		}
		if receipt.State != ReceiptDispatching {
			return errors.New("lane input receipt has no dispatching target")
		}
		if turn.NativeDispatchID != "" && turn.NativeDispatchID != acceptance.NativeMessageID {
			return errors.New("lane input target turn native identity changed")
		}
		now := maxInt64(positiveUnix(e.now()), maxInt64(receipt.UpdatedAt, acceptance.AcceptedAt))
		receipt.State, receipt.NativeAcceptance, receipt.AmbiguityCause = ReceiptInjected, &acceptance, ""
		receipt.Revision, receipt.UpdatedAt = receipt.Revision+1, now
		turn.State, turn.NativeDispatchID = "dispatched", acceptance.NativeMessageID
		catalog.LaneInputs[receiptID] = receipt
		catalog.Turns[turn.ID] = turn
		catalog.Host.LaneRevision++
		updated = receipt
		return nil
	})
	if errors.Is(err, errLaneInputAlreadyExact) && alreadyExact {
		return updated, nil
	}
	return updated, err
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
