// Package daemon contains the durable shared state and lifecycle owned by the
// single Agent Sessions user daemon.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antst/sessionbus/internal/pathidentity"
	"github.com/antst/sessionbus/internal/productcatalog"
	"github.com/antst/sessionbus/internal/statestore"
)

// PrepareStateRoot keeps only an empty root or the current one-table state.
// When permitted, every other safe directory is removed as pre-campaign state;
// otherwise its validation error is returned without changing the root.
func PrepareStateRoot(root string, maxBytes int64, output io.Writer, resetIncompatible bool) error {
	if output == nil {
		output = io.Discard
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return errors.New("refuse to prepare an unsafe daemon state root")
	}
	identity, err := pathidentity.ExistingNoFollow(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("validate daemon state root: %w", err)
	}
	if identity.Kind != pathidentity.KindDirectory {
		return errors.New("daemon state root is not a directory")
	}
	reason := validateCurrentStateRoot(root, maxBytes)
	if reason == nil {
		return nil
	}
	if !resetIncompatible {
		return reason
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove incompatible daemon state root %s: %w", root, err)
	}
	_, err = fmt.Fprintf(output, "Removed incompatible pre-0.4.0 Agent Sessions state root %s: %s\n", root, reason)
	return err
}

func validateCurrentStateRoot(root string, maxBytes int64) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read daemon state root: %w", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "state.json":
		case "state.lock":
			info, statErr := os.Lstat(filepath.Join(root, entry.Name()))
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("daemon state lock is invalid")
			}
		case "run":
			identity, identityErr := pathidentity.ExistingNoFollow(filepath.Join(root, entry.Name()))
			if identityErr != nil || identity.Kind != pathidentity.KindDirectory {
				return errors.New("daemon runtime directory is invalid")
			}
		default:
			return fmt.Errorf("daemon state root contains unknown entry %q", entry.Name())
		}
	}
	statePath := filepath.Join(root, "state.json")
	info, err := os.Lstat(statePath)
	if errors.Is(err, os.ErrNotExist) {
		for _, entry := range entries {
			if entry.Name() != "state.lock" && entry.Name() != "run" {
				return errors.New("daemon state root is neither empty nor a current one-table store")
			}
		}
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxBytes {
		return errors.New("daemon state snapshot is invalid")
	}
	body, err := os.ReadFile(statePath)
	if err != nil {
		return fmt.Errorf("read daemon state snapshot: %w", err)
	}
	var disk struct {
		Schema   int                        `json:"schema"`
		Revision uint64                     `json:"revision"`
		Records  map[string]json.RawMessage `json:"records"`
	}
	if json.Unmarshal(body, &disk) != nil || disk.Schema != 1 || disk.Revision == 0 || len(disk.Records) != 1 {
		return errors.New("daemon state snapshot is not a current one-table store")
	}
	catalogBody, ok := disk.Records[catalogRecord]
	if !ok {
		return errors.New("daemon catalog record is missing")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(catalogBody, &fields) != nil || len(fields) != 1 || fields["lanes"] == nil {
		return errors.New("daemon catalog record is not the current one-table shape")
	}
	var catalog Catalog
	if json.Unmarshal(catalogBody, &catalog) != nil {
		return errors.New("daemon catalog record is invalid")
	}
	return validateCatalog(normalizedCatalog(catalog))
}

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
