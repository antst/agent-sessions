package daemon

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Dispatch atomically records native acceptance of one previously accepted
// turn and the lane policy/evidence used for that dispatch.
func (e *LaneEngine) Dispatch(lane Lane, turn Turn) error {
	if lane.ID == "" || turn.ID == "" || turn.LaneID != lane.ID {
		return errors.New("dispatched lane and turn identities are incomplete")
	}
	return e.mutate(func(catalog *Catalog) error {
		currentLane, ok := catalog.Lanes[lane.ID]
		if !ok || currentLane.Product != lane.Product {
			return errors.New("dispatched lane state is missing or changed product")
		}
		currentTurn, ok := catalog.Turns[turn.ID]
		if !ok || currentTurn.LaneID != lane.ID {
			return errors.New("accepted lane turn is missing")
		}
		if currentTurn.State != "accepted" && currentTurn.State != "dispatched" {
			return fmt.Errorf("lane turn cannot dispatch from state %s", currentTurn.State)
		}
		lane.State = "running"
		lane.ArchiveRevision = currentLane.ArchiveRevision
		turn.State, turn.Sequence = "dispatched", currentTurn.Sequence
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Turns[turn.ID] = turn
		catalog.Host.LaneRevision++
		return nil
	})
}

// SetNativeDispatchID records a native turn identity after a product has
// accepted it. The lane can recover using its durable native session even if
// this best-effort strengthening races a daemon restart.
func (e *LaneEngine) SetNativeDispatchID(turnID, nativeID string) error {
	if nativeID == "" {
		return errors.New("native dispatch identity is empty")
	}
	return e.mutate(func(catalog *Catalog) error {
		turn, ok := catalog.Turns[turnID]
		if !ok {
			return errors.New("lane turn disappeared before native dispatch was committed")
		}
		if turn.NativeDispatchID != "" && turn.NativeDispatchID != nativeID {
			return errors.New("native dispatch identity cannot change")
		}
		turn.NativeDispatchID = nativeID
		catalog.Turns[turnID] = turn
		catalog.Host.LaneRevision++
		return nil
	})
}

// Complete records terminal evidence exactly once. Replaying identical
// evidence is idempotent; conflicting terminal evidence is rejected.
func (e *LaneEngine) Complete(lane Lane, turn Turn) (bool, error) {
	alreadyTerminal := false
	err := e.mutate(func(catalog *Catalog) error {
		currentLane, ok := catalog.Lanes[lane.ID]
		if !ok || currentLane.Product != lane.Product {
			return errors.New("terminal lane state is missing or changed product")
		}
		currentTurn, ok := catalog.Turns[turn.ID]
		if !ok || currentTurn.LaneID != lane.ID {
			return errors.New("terminal lane turn is missing")
		}
		if currentTurn.State == "terminal" {
			if !sameTerminalTurn(currentTurn, turn) {
				return errors.New("terminal lane turn evidence conflicts with committed evidence")
			}
			alreadyTerminal = true
			return nil
		}
		if currentTurn.State != "accepted" && currentTurn.State != "dispatched" {
			return fmt.Errorf("lane turn cannot complete from state %s", currentTurn.State)
		}
		catalog.Host.LaneRevision++
		lane.State, lane.CapabilityHash = "terminal", ""
		lane.ArchiveRevision = currentLane.ArchiveRevision
		turn.State, turn.Sequence = "terminal", currentTurn.Sequence
		turn.TerminalRevision = catalog.Host.LaneRevision
		catalog.Lanes[lane.ID] = cloneLane(lane)
		catalog.Turns[turn.ID] = turn
		return nil
	})
	return alreadyTerminal, err
}

// Collect acknowledges one terminal turn exactly once and advances a terminal
// lane to idle only after all older terminal turns have been collected.
func (e *LaneEngine) Collect(laneID, turnID string, defaultAutoArchiveDelay time.Duration) (LaneCollection, error) {
	result := LaneCollection{}
	err := e.mutate(func(catalog *Catalog) error {
		turn, ok := catalog.Turns[turnID]
		if !ok || turn.LaneID != laneID || turn.State != "terminal" {
			return errors.New("lane turn is not collectable")
		}
		catalog.Host.LaneRevision++
		turn.State = "collected"
		turn.CollectionRevision = catalog.Host.LaneRevision
		catalog.Turns[turnID] = turn
		result.RemainingDebt = catalogHasCollectionDebt(*catalog, laneID)
		lane, ok := catalog.Lanes[laneID]
		if !ok {
			return errors.New("lane state is missing")
		}
		if lane.State == "terminal" && !result.RemainingDebt {
			lane.State = "idle"
			if lane.AutoArchive {
				delay := time.Duration(lane.AutoArchiveDelayMS) * time.Millisecond
				if delay <= 0 {
					delay = defaultAutoArchiveDelay
				}
				if delay > 0 {
					lane.AutoArchiveAt = e.now().UnixMilli() + delay.Milliseconds()
					result.AutoArchiveAt = lane.AutoArchiveAt
				}
			}
			catalog.Lanes[laneID] = lane
		}
		return nil
	})
	return result, err
}

// OldestCollectable returns the earliest terminal, uncollected turn.
func (e *LaneEngine) OldestCollectable(laneID string) (Turn, bool, error) {
	snapshot, err := e.store.Read()
	if err != nil {
		return Turn{}, false, err
	}
	turns := make([]Turn, 0)
	for _, turn := range snapshot.Catalog.Turns {
		if turn.LaneID == laneID && turn.State == "terminal" {
			turns = append(turns, turn)
		}
	}
	if len(turns) == 0 {
		return Turn{}, false, nil
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Sequence < turns[j].Sequence })
	return turns[0], true, nil
}

// HasCollectionDebt reports whether a lane owns any terminal uncollected turn.
func (e *LaneEngine) HasCollectionDebt(laneID string) (bool, error) {
	snapshot, err := e.store.Read()
	if err != nil {
		return false, err
	}
	return catalogHasCollectionDebt(snapshot.Catalog, laneID), nil
}

// ReconcileRestart terminalizes active turns that a product adapter cannot
// reattach. The callback is the only product-specific recovery decision.
func (e *LaneEngine) ReconcileRestart(canRecover func(Lane, Turn) bool, diagnostic string) error {
	if diagnostic == "" {
		diagnostic = "Agent Sessions daemon restarted during the accepted turn"
	}
	return e.mutate(func(catalog *Catalog) error {
		changed := ReconcileRestartedLaneCatalog(catalog, e.now().UnixMilli(), canRecover, diagnostic)
		if changed {
			catalog.Host.LaneRevision++
			for id, turn := range catalog.Turns {
				if turn.State == "terminal" && turn.TerminalRevision == 0 && turn.Diagnostic == diagnostic {
					turn.TerminalRevision = catalog.Host.LaneRevision
					catalog.Turns[id] = turn
				}
			}
		}
		return nil
	})
}

// ReconcileRestartedLaneCatalog applies the product-neutral restart rule to
// one in-memory catalog. It is exported so recovery parity tests can exercise
// the exact state transformation without launching native products.
func ReconcileRestartedLaneCatalog(
	catalog *Catalog,
	now int64,
	canRecover func(Lane, Turn) bool,
	diagnostic string,
) bool {
	latest := make(map[string]Turn)
	for _, turn := range catalog.Turns {
		if current, ok := latest[turn.LaneID]; !ok || turn.Sequence > current.Sequence {
			latest[turn.LaneID] = turn
		}
	}
	changed := false
	for id, lane := range catalog.Lanes {
		if lane.State != "preparing" && lane.State != "running" && lane.State != "interrupting" && lane.State != "retiring" {
			continue
		}
		turn := latest[id]
		if canRecover != nil && canRecover(lane, turn) {
			continue
		}
		lane.State, lane.CapabilityHash = "terminal", ""
		catalog.Lanes[id] = lane
		if turn.ID != "" && (turn.State == "accepted" || turn.State == "dispatched") {
			turn.State, turn.Outcome, turn.ExitCode = "terminal", "interrupted", 1
			turn.Diagnostic, turn.CompletedAt = diagnostic, now
			catalog.Turns[turn.ID] = turn
		}
		changed = true
	}
	return changed
}

// PrepareTerminalNotice creates or reopens one metadata-only, stable terminal
// notice delivery. Acknowledged notices are never emitted again.
func (e *LaneEngine) PrepareTerminalNotice(notice Delivery) (bool, error) {
	acknowledged := false
	err := e.mutate(func(catalog *Catalog) error {
		current, exists := catalog.Deliveries[notice.ID]
		if exists && current.State == "acknowledged" {
			acknowledged = true
			return nil
		}
		if notice.ID == "" || notice.CorrelationID == "" || notice.Sender == "" || len(notice.Destinations) != 1 {
			return errors.New("terminal notice metadata is incomplete")
		}
		notice.State = "accepted"
		if exists && current.State == "presented" {
			notice.State = "presented"
		}
		catalog.Deliveries[notice.ID] = notice
		catalog.Host.DeliveryRevision++
		return nil
	})
	return acknowledged, err
}

// TransitionTerminalNotice commits retry/presentation/acknowledgement state
// without persisting message content or a vendor error.
func (e *LaneEngine) TransitionTerminalNotice(noticeID, state, retryCause string) error {
	return e.mutate(func(catalog *Catalog) error {
		delivery, ok := catalog.Deliveries[noticeID]
		if !ok {
			return errors.New("terminal delivery state disappeared")
		}
		if delivery.State != state && !ValidLifecycleTransition("delivery", delivery.State, state) {
			return fmt.Errorf("invalid terminal notice transition %s -> %s", delivery.State, state)
		}
		delivery.State, delivery.RetryCause = state, retryCause
		if state == "acknowledged" {
			delivery.Acknowledgment = "destination-accepted"
		}
		catalog.Deliveries[noticeID] = delivery
		catalog.Host.DeliveryRevision++
		return nil
	})
}

func catalogHasCollectionDebt(catalog Catalog, laneID string) bool {
	for _, turn := range catalog.Turns {
		if turn.LaneID == laneID && turn.State == "terminal" {
			return true
		}
	}
	return false
}

func sameTerminalTurn(left, right Turn) bool {
	return left.Outcome == right.Outcome && left.Result == right.Result &&
		left.Diagnostic == right.Diagnostic && left.ExitCode == right.ExitCode &&
		left.CompletedAt == right.CompletedAt
}
