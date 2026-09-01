// Package daemon contains the durable shared state and lifecycle owned by the
// single Agent Sessions user daemon.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	catalogRecord = "catalog"
)

// NativeEvidence is the shared exact identity skeleton plus distinct optional
// native evidence fields. Product adapters decide which fields are required.
type NativeEvidence struct {
	Process          procinfo.Identity   `json:"process"`
	Ancestry         []procinfo.Identity `json:"ancestry,omitempty"`
	Executable       string              `json:"executable,omitempty"`
	RegistryPath     string              `json:"registry_path,omitempty"`
	SocketPath       string              `json:"socket_path,omitempty"`
	ThreadID         string              `json:"thread_id,omitempty"`
	LaunchTokenHash  string              `json:"launch_token_hash,omitempty"`
	LeaderIdentity   string              `json:"leader_identity,omitempty"`
	RosterRevision   string              `json:"roster_revision,omitempty"`
	ArtifactPath     string              `json:"artifact_path,omitempty"`
	ArtifactType     string              `json:"artifact_type,omitempty"`
	ArtifactMode     uint32              `json:"artifact_mode,omitempty"`
	ArtifactOwner    uint32              `json:"artifact_owner,omitempty"`
	ArtifactRevision string              `json:"artifact_revision,omitempty"`
	ArtifactDevice   uint64              `json:"artifact_device,omitempty"`
	ArtifactInode    uint64              `json:"artifact_inode,omitempty"`
	ArtifactPrefix   string              `json:"artifact_prefix,omitempty"`
	ArtifactBytes    int64               `json:"artifact_bytes,omitempty"`
	RegistryDevice   uint64              `json:"registry_device,omitempty"`
	RegistryInode    uint64              `json:"registry_inode,omitempty"`
	RegistryPrefix   string              `json:"registry_prefix,omitempty"`
	RegistryBytes    int64               `json:"registry_bytes,omitempty"`
}

// ManagedAttachment is durable Agent Sessions ownership of one native peer.
type ManagedAttachment struct {
	ID                 string         `json:"id"`
	CapabilityHash     string         `json:"capability_hash,omitempty"`
	Product            string         `json:"product"`
	ProfileIdentity    string         `json:"profile_identity,omitempty"`
	LaunchIntent       string         `json:"launch_intent,omitempty"`
	NativeSessionID    string         `json:"native_session_id,omitempty"`
	NativeProfileRoot  string         `json:"native_profile_root,omitempty"`
	Cwd                string         `json:"cwd,omitempty"`
	Groups             []string       `json:"groups,omitempty"`
	PermissionMode     string         `json:"permission_mode,omitempty"`
	ExpectedEvidence   NativeEvidence `json:"expected_evidence,omitempty"`
	Evidence           NativeEvidence `json:"evidence,omitempty"`
	DaemonGeneration   uint64         `json:"daemon_generation,omitempty"`
	CatalogRevision    uint64         `json:"catalog_revision,omitempty"`
	ComponentProtocol  string         `json:"component_protocol,omitempty"`
	ComponentRevision  uint64         `json:"component_revision,omitempty"`
	IntegrationVersion string         `json:"integration_version,omitempty"`
	State              string         `json:"state"`
}

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
	Attachments map[string]ManagedAttachment `json:"attachments"`
	Lanes       map[string]LaneCandidate     `json:"lanes"`
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

// ValidLifecycleTransition checks only the documented shared lifecycle shape;
// product-native operations remain adapter-owned.
func ValidLifecycleTransition(kind, from, to string) bool {
	if from == to {
		return true
	}
	transitions := map[string]map[string][]string{
		"attachment": {
			"preparing": {"prepared", "detached"},
			"prepared":  {"selecting", "attached", "detaching", "detached"},
			"selecting": {"attached", "detaching", "detached"},
			"attached":  {"detaching", "detached"},
			"detaching": {"detached"},
			"detached":  {"preparing"},
		},
	}
	for _, candidate := range transitions[kind][from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func emptyCatalog() Catalog {
	return Catalog{
		Attachments: map[string]ManagedAttachment{},
		Lanes:       map[string]LaneCandidate{},
	}
}

func normalizedCatalog(catalog Catalog) Catalog {
	if catalog.Attachments == nil {
		catalog.Attachments = map[string]ManagedAttachment{}
	}
	if catalog.Lanes == nil {
		catalog.Lanes = map[string]LaneCandidate{}
	}
	return catalog
}

//nolint:gocyclo // The closed catalog validates each independent record family explicitly.
func validateCatalog(catalog Catalog) error {
	for id, attachment := range catalog.Attachments {
		if id == "" || attachment.ID != id || !knownState("attachment", attachment.State) {
			return fmt.Errorf("invalid managed attachment %q", id)
		}
		if _, ok := productcatalog.ByID(attachment.Product); !ok {
			return fmt.Errorf("managed attachment %s has unknown product %q", id, attachment.Product)
		}
	}
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

//nolint:gocyclo // The closed catalog checks each family's distinct terminal-state rule explicitly.
func validateCatalogTransitions(current, next Catalog) error {
	for id, attachment := range current.Attachments {
		if candidate, ok := next.Attachments[id]; ok {
			if candidate.ComponentRevision < attachment.ComponentRevision {
				return fmt.Errorf("attachment %s component revision regressed", id)
			}
			if !ValidLifecycleTransition("attachment", attachment.State, candidate.State) {
				return fmt.Errorf("attachment %s cannot transition from %s to %s", id, attachment.State, candidate.State)
			}
		} else if attachment.State != "detached" {
			return fmt.Errorf("attachment %s cannot be removed from state %s", id, attachment.State)
		}
	}
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

func knownState(kind, state string) bool {
	states := map[string]map[string]bool{
		"attachment": {"preparing": true, "prepared": true, "selecting": true, "attached": true, "detaching": true, "detached": true},
	}
	return states[kind][state]
}
