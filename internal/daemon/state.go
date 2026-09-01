// Package daemon contains the durable shared state and lifecycle owned by the
// single Agent Sessions user daemon.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	catalogRecord = "catalog"
)

// LaneCandidate remembers only which product UUID an owning parent may ask
// the product about while the lane is offline.
type LaneCandidate struct {
	NativeSessionID string   `json:"uuid"`
	Product         string   `json:"product"`
	Parent          string   `json:"parent"`
	PrimaryGroup    string   `json:"primary_group"`
	SecondaryGroups []string `json:"secondary_groups,omitempty"`
	Host            string   `json:"host,omitempty"`
}

// Catalog is the complete durable host state committed as one revision.
type Catalog struct {
	Lanes map[string]LaneCandidate `json:"lanes"`
}

// StateSnapshot is one isolated committed daemon catalog revision.
type StateSnapshot struct {
	Revision uint64
	Catalog  Catalog
}

// StateStore validates lifecycle transitions over one atomic statestore.
type StateStore struct {
	store *statestore.Store
}

// OpenState opens one bounded daemon catalog store.
func OpenState(root string, maxBytes int64) (*StateStore, error) {
	store, err := statestore.Open(root, maxBytes)
	if err != nil {
		return nil, err
	}
	return &StateStore{store: store}, nil
}

// Read returns one isolated daemon catalog snapshot.
func (s *StateStore) Read() (StateSnapshot, error) {
	snapshot, err := s.store.Read()
	if err != nil {
		return StateSnapshot{}, err
	}
	if snapshot.Revision == 0 {
		return StateSnapshot{Catalog: emptyCatalog()}, nil
	}
	body, ok := snapshot.Records[catalogRecord]
	if !ok || len(snapshot.Records) != 1 {
		return StateSnapshot{}, errors.New("daemon catalog record is missing or ambiguous")
	}
	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return StateSnapshot{}, fmt.Errorf("decode daemon catalog: %w", err)
	}
	catalog = normalizedCatalog(catalog)
	if err := validateCatalog(catalog); err != nil {
		return StateSnapshot{}, err
	}
	return StateSnapshot{Revision: snapshot.Revision, Catalog: catalog}, nil
}

// Commit validates and atomically commits a complete catalog revision.
func (s *StateStore) Commit(expectedRevision uint64, catalog Catalog) (StateSnapshot, error) {
	catalog = normalizedCatalog(catalog)
	if err := validateCatalog(catalog); err != nil {
		return StateSnapshot{}, err
	}
	current, err := s.Read()
	if err != nil {
		return StateSnapshot{}, err
	}
	if current.Revision != expectedRevision {
		return StateSnapshot{}, fmt.Errorf("%w: current=%d expected=%d", statestore.ErrConflict, current.Revision, expectedRevision)
	}
	if err := validateCatalogTransitions(current.Catalog, catalog); err != nil {
		return StateSnapshot{}, err
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		return StateSnapshot{}, fmt.Errorf("encode daemon catalog: %w", err)
	}
	committed, err := s.store.Commit(expectedRevision, map[string]json.RawMessage{catalogRecord: body})
	if err != nil {
		return StateSnapshot{}, err
	}
	var isolated Catalog
	if err := json.Unmarshal(committed.Records[catalogRecord], &isolated); err != nil {
		return StateSnapshot{}, fmt.Errorf("decode committed daemon catalog: %w", err)
	}
	return StateSnapshot{Revision: committed.Revision, Catalog: normalizedCatalog(isolated)}, nil
}

func emptyCatalog() Catalog {
	return Catalog{Lanes: map[string]LaneCandidate{}}
}

func normalizedCatalog(catalog Catalog) Catalog {
	if catalog.Lanes == nil {
		catalog.Lanes = map[string]LaneCandidate{}
	}
	return catalog
}

func validateCatalog(catalog Catalog) error {
	for id, lane := range catalog.Lanes {
		if strings.TrimSpace(id) == "" || id != lane.NativeSessionID || strings.TrimSpace(lane.Parent) == "" || strings.TrimSpace(lane.PrimaryGroup) == "" {
			return fmt.Errorf("invalid lane candidate %q", id)
		}
		if _, ok := productcatalog.ByID(lane.Product); !ok {
			return fmt.Errorf("lane %s has unknown product %q", id, lane.Product)
		}
	}
	return nil
}

func validateCatalogTransitions(current, next Catalog) error {
	for id, lane := range current.Lanes {
		if candidate, ok := next.Lanes[id]; ok {
			if lane.NativeSessionID != candidate.NativeSessionID || lane.Product != candidate.Product || lane.Parent != candidate.Parent ||
				lane.PrimaryGroup != candidate.PrimaryGroup || !slices.Equal(lane.SecondaryGroups, candidate.SecondaryGroups) || lane.Host != candidate.Host {
				return fmt.Errorf("lane candidate %s cannot change", id)
			}
		} else {
			return fmt.Errorf("lane candidate %s cannot be removed", id)
		}
	}
	return nil
}
