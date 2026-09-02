package daemon

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/statestore"
)

const laneMutationAttempts = 32

// LaneEngine owns the one durable lane-candidate table used only to discover
// product UUIDs that an owning parent may ask the product about.
type LaneEngine struct {
	store *StateStore
	mu    sync.Mutex
}

// Candidates returns only the UUID questions owned by one parent for one
// product. Callers must still ask the product whether each UUID exists.
func (e *LaneEngine) Candidates(parent, product string) ([]LaneCandidate, error) {
	if strings.TrimSpace(parent) == "" || strings.TrimSpace(product) == "" {
		return nil, errors.New("lane candidate selector is incomplete")
	}
	snapshot, err := e.store.Read()
	if err != nil {
		return nil, err
	}
	result := make([]LaneCandidate, 0)
	for _, candidate := range snapshot.Catalog.Lanes {
		if candidate.Parent != parent || candidate.Product != product {
			continue
		}
		candidate.SecondaryGroups = slices.Clone(candidate.SecondaryGroups)
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NativeSessionID < result[j].NativeSessionID })
	return result, nil
}

func NewLaneEngine(store *StateStore) (*LaneEngine, error) {
	if store == nil {
		return nil, errors.New("lane engine requires state")
	}
	return &LaneEngine{store: store}, nil
}

// Remember writes one immutable product UUID and its parent-group ownership.
func (e *LaneEngine) Remember(candidate LaneCandidate) error {
	if strings.TrimSpace(candidate.NativeSessionID) == "" || strings.TrimSpace(candidate.Product) == "" ||
		strings.TrimSpace(candidate.Parent) == "" || strings.TrimSpace(candidate.PrimaryGroup) == "" {
		return errors.New("lane candidate is incomplete")
	}
	candidate.SecondaryGroups = slices.Clone(candidate.SecondaryGroups)
	return e.mutate(func(catalog *Catalog) error {
		if existing, ok := catalog.Lanes[candidate.NativeSessionID]; ok {
			if existing.Product == candidate.Product && existing.Parent == candidate.Parent && existing.PrimaryGroup == candidate.PrimaryGroup &&
				existing.Host == candidate.Host && slices.Equal(existing.SecondaryGroups, candidate.SecondaryGroups) {
				return nil
			}
			return errors.New("lane candidate already exists with different ownership")
		}
		catalog.Lanes[candidate.NativeSessionID] = candidate
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
	return errors.New("lane candidate table remained contended")
}
