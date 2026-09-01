package daemon

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/statestore"
)

var (
	ErrNativeLeaseConflict         = errors.New("native session lease conflicts with an exact owner")
	ErrNativeLeaseLive             = errors.New("native session lease process is still live")
	ErrNativeLeaseCleanupDebt      = errors.New("native session lease process identity is unprovable")
	ErrNativeLeaseStale            = errors.New("native session lease process is proven stale")
	ErrNativeLeaseRecoveryRequired = errors.New("native session lease belongs to an earlier generation")
	ErrNativeLeaseReleaseRequired  = errors.New("native session lease release must be completed")
	errNativeLeaseNoMutation       = errors.New("native session lease observation requires no mutation")
)

// NativeLeaseRequest names the exact composite native-session authority. The
// generation is required for acquisition and corroborates ordinary mutations;
// Recover moves an old exact owner to the engine's current generation.
type NativeLeaseRequest struct {
	ProductID       string
	ProfileIdentity string
	NativeSessionID string
	OwnerLaneID     string
	Generation      uint64
}

// NativeLeaseEngine serializes ownership of a product/profile/native-session
// tuple and refuses reassignment until exact process death is proven.
type NativeLeaseEngine struct {
	store      *StateStore
	generation uint64
	now        func() time.Time
	observe    func(procinfo.Identity) procinfo.IdentityObservation
	mu         sync.Mutex
}

func NewNativeLeaseEngine(store *StateStore, generation uint64) (*NativeLeaseEngine, error) {
	if store == nil || generation == 0 {
		return nil, errors.New("native lease engine requires state and a generation")
	}
	return &NativeLeaseEngine{store: store, generation: generation, now: time.Now, observe: procinfo.ObserveIdentity}, nil
}

func (e *NativeLeaseEngine) Acquire(request NativeLeaseRequest) (NativeSessionLease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key, err := validateNativeLeaseRequest(request)
	if err != nil {
		return NativeSessionLease{}, err
	}
	if request.Generation != e.generation {
		return NativeSessionLease{}, errors.New("native lease acquisition generation is stale")
	}

	snapshot, err := e.store.Read()
	if err != nil {
		return NativeSessionLease{}, err
	}
	if existing, ok := snapshot.Catalog.NativeLeases[key]; ok {
		if existing.State != LeaseReleased && existing.OwnerLaneID != request.OwnerLaneID {
			return existing, ErrNativeLeaseConflict
		}
		if existing.State != LeaseReleased {
			if existing.Generation != e.generation {
				return existing, ErrNativeLeaseRecoveryRequired
			}
			switch existing.State {
			case LeasePrepared, LeaseHeld:
				return existing, nil
			case LeaseReleasing:
				return existing, ErrNativeLeaseReleaseRequired
			case LeaseCleanupDebt:
				return existing, ErrNativeLeaseCleanupDebt
			default:
				return existing, errors.New("native session lease has an invalid acquisition state")
			}
		}
		if existing.ProcessGroup != (procinfo.Identity{}) {
			switch e.observe(existing.ProcessGroup).Status {
			case procinfo.IdentityMatches:
				return existing, ErrNativeLeaseLive
			case procinfo.IdentityUnknown:
				return existing, ErrNativeLeaseCleanupDebt
			case procinfo.IdentityStale:
			}
		}
		catalog := snapshot.Catalog
		delete(catalog.NativeLeases, key)
		catalog.Host.LaneRevision++
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			return NativeSessionLease{}, err
		}
	}

	createdAt := positiveUnix(e.now())
	lease := NativeSessionLease{
		Schema: NativeSessionLeaseRecordSchema, ProductID: request.ProductID, ProfileIdentity: request.ProfileIdentity,
		NativeSessionID: request.NativeSessionID, OwnerLaneID: request.OwnerLaneID, Generation: e.generation,
		State: LeasePrepared, Revision: 1, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	err = e.mutate(func(catalog *Catalog) error {
		if existing, ok := catalog.NativeLeases[key]; ok {
			if existing.OwnerLaneID != request.OwnerLaneID {
				return ErrNativeLeaseConflict
			}
			if existing.Generation != e.generation {
				return ErrNativeLeaseRecoveryRequired
			}
			switch existing.State {
			case LeasePrepared, LeaseHeld:
				lease = existing
				return nil
			case LeaseReleasing:
				return ErrNativeLeaseReleaseRequired
			case LeaseCleanupDebt:
				return ErrNativeLeaseCleanupDebt
			}
			return errors.New("native session lease already exists and requires recovery")
		}
		catalog.NativeLeases[key] = lease
		catalog.Host.LaneRevision++
		return nil
	})
	return lease, err
}

func (e *NativeLeaseEngine) Hold(request NativeLeaseRequest, process procinfo.Identity) (NativeSessionLease, error) {
	if !validOptionalProcessIdentity(process) || process == (procinfo.Identity{}) {
		return NativeSessionLease{}, errors.New("native lease hold requires exact process identity")
	}
	return e.transition(request, func(lease *NativeSessionLease) error {
		if lease.State != LeasePrepared {
			return fmt.Errorf("native session lease cannot be held from %s", lease.State)
		}
		if lease.Generation != request.Generation || request.Generation != e.generation {
			return errors.New("native session lease hold generation is stale")
		}
		lease.State, lease.ProcessGroup = LeaseHeld, process
		return nil
	})
}

func (e *NativeLeaseEngine) BeginRelease(request NativeLeaseRequest) (NativeSessionLease, error) {
	return e.transition(request, func(lease *NativeSessionLease) error {
		if lease.State != LeasePrepared && lease.State != LeaseHeld {
			return fmt.Errorf("native session lease cannot begin release from %s", lease.State)
		}
		if lease.Generation != request.Generation {
			return ErrNativeLeaseRecoveryRequired
		}
		lease.State = LeaseReleasing
		return nil
	})
}

func (e *NativeLeaseEngine) CompleteRelease(request NativeLeaseRequest) (NativeSessionLease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key, err := validateNativeLeaseRequest(request)
	if err != nil {
		return NativeSessionLease{}, err
	}
	var outcome error
	var released NativeSessionLease
	err = e.mutate(func(catalog *Catalog) error {
		lease, ok := catalog.NativeLeases[key]
		if !ok {
			return errors.New("native session lease is missing")
		}
		if lease.OwnerLaneID != request.OwnerLaneID {
			return ErrNativeLeaseConflict
		}
		if lease.Generation != request.Generation {
			return ErrNativeLeaseRecoveryRequired
		}
		if lease.State != LeaseReleasing && lease.State != LeaseCleanupDebt {
			return fmt.Errorf("native session lease cannot complete release from %s", lease.State)
		}
		status := procinfo.IdentityStale
		if lease.ProcessGroup != (procinfo.Identity{}) {
			status = e.observe(lease.ProcessGroup).Status
		}
		switch status {
		case procinfo.IdentityMatches:
			released = lease
			outcome = ErrNativeLeaseLive
			return errNativeLeaseNoMutation
		case procinfo.IdentityUnknown:
			if lease.State != LeaseCleanupDebt {
				lease.State, lease.Revision = LeaseCleanupDebt, lease.Revision+1
				lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
				catalog.NativeLeases[key] = lease
				catalog.Host.CleanupDebtRevision++
			} else {
				released = lease
				outcome = ErrNativeLeaseCleanupDebt
				return errNativeLeaseNoMutation
			}
			released = lease
			outcome = ErrNativeLeaseCleanupDebt
			return nil
		case procinfo.IdentityStale:
			lease.State, lease.Revision = LeaseReleased, lease.Revision+1
			lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
			catalog.NativeLeases[key] = lease
			catalog.Host.LaneRevision++
			released = lease
			return nil
		default:
			return errors.New("unknown native process observation")
		}
	})
	if errors.Is(err, errNativeLeaseNoMutation) {
		return released, outcome
	}
	if err != nil {
		return NativeSessionLease{}, err
	}
	return released, outcome
}

// Recover either reattaches the exact live owner into the current generation,
// or proves the old process dead and releases it. It never substitutes a new
// owner or native session.
func (e *NativeLeaseEngine) Recover(request NativeLeaseRequest, expectedProcess procinfo.Identity) (NativeSessionLease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key, err := validateNativeLeaseRequest(request)
	if err != nil {
		return NativeSessionLease{}, err
	}
	if !validOptionalProcessIdentity(expectedProcess) || expectedProcess == (procinfo.Identity{}) {
		return NativeSessionLease{}, errors.New("native session lease recovery requires exact process identity")
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return NativeSessionLease{}, err
	}
	lease, ok := snapshot.Catalog.NativeLeases[key]
	if !ok {
		return NativeSessionLease{}, errors.New("native session lease is missing")
	}
	if lease.OwnerLaneID != request.OwnerLaneID || lease.ProcessGroup != expectedProcess {
		return lease, ErrNativeLeaseConflict
	}
	if lease.State != LeaseHeld {
		return lease, fmt.Errorf("native session lease cannot recover from %s", lease.State)
	}
	switch e.observe(expectedProcess).Status {
	case procinfo.IdentityMatches:
		lease.Generation, lease.Revision = e.generation, lease.Revision+1
		lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
		catalog := snapshot.Catalog
		catalog.NativeLeases[key] = lease
		catalog.Host.LaneRevision++
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			return NativeSessionLease{}, err
		}
		return lease, nil
	case procinfo.IdentityUnknown:
		lease.State, lease.Revision = LeaseCleanupDebt, lease.Revision+1
		lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
		catalog := snapshot.Catalog
		catalog.NativeLeases[key] = lease
		catalog.Host.CleanupDebtRevision++
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			return NativeSessionLease{}, err
		}
		return lease, ErrNativeLeaseCleanupDebt
	case procinfo.IdentityStale:
		// The frozen lifecycle requires Held -> Releasing -> Released. Both
		// commits occur only after this exact stale observation.
		lease.State, lease.Revision = LeaseReleasing, lease.Revision+1
		lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
		catalog := snapshot.Catalog
		catalog.NativeLeases[key] = lease
		catalog.Host.LaneRevision++
		committed, err := e.store.Commit(snapshot.Revision, catalog)
		if err != nil {
			return NativeSessionLease{}, err
		}
		lease.State, lease.Revision = LeaseReleased, lease.Revision+1
		lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
		catalog = committed.Catalog
		catalog.NativeLeases[key] = lease
		catalog.Host.LaneRevision++
		if _, err := e.store.Commit(committed.Revision, catalog); err != nil {
			return NativeSessionLease{}, err
		}
		return lease, ErrNativeLeaseStale
	default:
		return NativeSessionLease{}, errors.New("unknown native process observation")
	}
}

func (e *NativeLeaseEngine) transition(request NativeLeaseRequest, apply func(*NativeSessionLease) error) (NativeSessionLease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key, err := validateNativeLeaseRequest(request)
	if err != nil {
		return NativeSessionLease{}, err
	}
	var updated NativeSessionLease
	err = e.mutate(func(catalog *Catalog) error {
		lease, ok := catalog.NativeLeases[key]
		if !ok {
			return errors.New("native session lease is missing")
		}
		if lease.OwnerLaneID != request.OwnerLaneID {
			return ErrNativeLeaseConflict
		}
		if err := apply(&lease); err != nil {
			return err
		}
		lease.Revision++
		lease.UpdatedAt = maxInt64(positiveUnix(e.now()), maxInt64(lease.CreatedAt, lease.UpdatedAt))
		catalog.NativeLeases[key] = lease
		catalog.Host.LaneRevision++
		updated = lease
		return nil
	})
	return updated, err
}

func (e *NativeLeaseEngine) mutate(apply func(*Catalog) error) error {
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
	return errors.New("native session lease state changed too frequently")
}

func validateNativeLeaseRequest(request NativeLeaseRequest) (NativeSessionLeaseKey, error) {
	if !validDurableOpaqueID(request.OwnerLaneID) || request.Generation == 0 {
		return "", errors.New("native session lease request is incomplete")
	}
	return NewNativeSessionLeaseKey(request.ProductID, request.ProfileIdentity, request.NativeSessionID)
}
