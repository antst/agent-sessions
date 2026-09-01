package daemon

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/statestore"
)

const laneMutationAttempts = 32

// LaneEngine owns the durable product-neutral lane lifecycle. Native launch,
// resume, interrupt, and archive operations remain adapter callbacks in the
// host coordinator; their observed results enter this engine as state facts.
type LaneEngine struct {
	store *StateStore
	now   func() time.Time
	mu    sync.Mutex
}

// NewLaneEngine constructs a durable lane engine over the daemon catalog.
func NewLaneEngine(store *StateStore) (*LaneEngine, error) {
	if store == nil {
		return nil, errors.New("lane engine requires state")
	}
	return &LaneEngine{store: store, now: time.Now}, nil
}

// Create publishes one product-owned lane address. Turn execution and results
// stay with the product and are never written to daemon state.
func (e *LaneEngine) Create(lane Lane) error {
	if lane.ID == "" {
		return errors.New("new lane identity is incomplete")
	}
	return e.mutate(func(catalog *Catalog) error {
		if _, exists := catalog.Lanes[lane.ID]; exists {
			return errors.New("lane identity already exists")
		}
		lane.State, lane.AutoArchiveAt = "idle", 0
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Host.LaneRevision++
		return nil
	})
}

// Update refreshes the daemon-owned routing projection for an existing lane.
// It does not accept, queue, or record a turn.
func (e *LaneEngine) Update(lane Lane) error {
	if lane.ID == "" {
		return errors.New("lane identity is incomplete")
	}
	return e.mutate(func(catalog *Catalog) error {
		current, ok := catalog.Lanes[lane.ID]
		if !ok {
			return errors.New("lane state is missing")
		}
		if current.Product != lane.Product {
			return errors.New("lane product cannot change")
		}
		if err := preserveExistingLaneNativeSession(current, &lane); err != nil {
			return err
		}
		lane.State, lane.CapabilityHash, lane.AutoArchiveAt = "idle", "", 0
		lane.ArchiveRevision = current.ArchiveRevision
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Host.LaneRevision++
		return nil
	})
}

// preserveExistingLaneNativeSession treats the durable lane as the only
// authority after creation. Existing-lane lifecycle mutations may omit that
// projection, but they cannot bind an unbound lane or replace a bound one.
// Binding is reserved for SetNativeSessionID after a validated exact Open, or
// LaneInputEngine.MarkInjectedAndSetNativeDispatch at exact first acceptance.
func preserveExistingLaneNativeSession(current Lane, candidate *Lane) error {
	if candidate == nil {
		return errors.New("lane mutation candidate is missing")
	}
	if current.NativeSessionID == "" {
		if candidate.NativeSessionID != "" {
			return errors.New("native lane session identity requires an explicit binding boundary")
		}
		return nil
	}
	if candidate.NativeSessionID != "" && candidate.NativeSessionID != current.NativeSessionID {
		return errors.New("native lane session identity cannot change")
	}
	candidate.NativeSessionID = current.NativeSessionID
	return nil
}

// TransitionLane commits a product-neutral lane state transition. Capability
// authorization is retained only while a lane can accept native input.
func (e *LaneEngine) TransitionLane(laneID, state, capabilityHash string) error {
	return e.mutate(func(catalog *Catalog) error {
		lane, ok := catalog.Lanes[laneID]
		if !ok {
			return errors.New("lane state is missing")
		}
		lane.State = state
		if state == "preparing" || state == "running" {
			lane.CapabilityHash = capabilityHash
		} else {
			lane.CapabilityHash = ""
		}
		if state == "archived" {
			lane.AutoArchiveAt = 0
			lane.ArchiveRevision++
		}
		catalog.Lanes[laneID] = lane
		catalog.Host.LaneRevision++
		return nil
	})
}

// SetNativeSessionID records a product adapter's immutable native session
// selection without changing lifecycle state.
func (e *LaneEngine) SetNativeSessionID(laneID, nativeID string) error {
	if strings.TrimSpace(nativeID) == "" {
		return errors.New("native lane session identity is empty")
	}
	return e.mutate(func(catalog *Catalog) error {
		lane, ok := catalog.Lanes[laneID]
		if !ok {
			return errors.New("lane state is missing")
		}
		if lane.NativeSessionID != "" && lane.NativeSessionID != nativeID {
			return errors.New("native lane session identity cannot change")
		}
		lane.NativeSessionID = nativeID
		catalog.Lanes[laneID] = lane
		catalog.Host.LaneRevision++
		return nil
	})
}

// RecordCleanupDebt preserves an exact owned resource and prevents the lane
// from being reported as cleanly archived.
func (e *LaneEngine) RecordCleanupDebt(laneID string, debt CleanupDebt) error {
	if debt.ID == "" || debt.Resource == "" || debt.Operation == "" {
		return errors.New("lane cleanup debt is incomplete")
	}
	return e.mutate(func(catalog *Catalog) error {
		lane, ok := catalog.Lanes[laneID]
		if !ok {
			return errors.New("lane state is missing")
		}
		lane.State, lane.CapabilityHash = "cleanup-debt", ""
		catalog.Lanes[laneID] = lane
		catalog.CleanupDebts[debt.ID] = debt
		catalog.Host.LaneRevision++
		catalog.Host.CleanupDebtRevision++
		return nil
	})
}

// ResolveCleanupDebt removes one verified debt and transitions its lane to the
// caller's proven terminal state.
func (e *LaneEngine) ResolveCleanupDebt(laneID, debtID, state string) error {
	return e.mutate(func(catalog *Catalog) error {
		lane, ok := catalog.Lanes[laneID]
		if !ok || lane.State != "cleanup-debt" {
			return errors.New("lane has no cleanup debt")
		}
		if _, ok := catalog.CleanupDebts[debtID]; !ok {
			return errors.New("cleanup debt is missing")
		}
		lane.State = state
		if state == "archived" {
			lane.AutoArchiveAt = 0
			lane.ArchiveRevision++
		}
		catalog.Lanes[laneID] = lane
		delete(catalog.CleanupDebts, debtID)
		catalog.Host.LaneRevision++
		catalog.Host.CleanupDebtRevision++
		return nil
	})
}

func (e *LaneEngine) mutate(apply func(*Catalog) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for attempt := 0; attempt < laneMutationAttempts; attempt++ {
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
	return errors.New("lane state remained contended")
}

func cloneLane(lane Lane) Lane {
	lane.Groups = append([]string(nil), lane.Groups...)
	lane.ExplicitGroups = append([]string(nil), lane.ExplicitGroups...)
	lane.Arguments = append([]string(nil), lane.Arguments...)
	return lane
}
