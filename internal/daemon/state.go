// Package daemon contains the durable shared state and lifecycle owned by the
// single Agent Sessions user daemon.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const catalogRecord = "catalog"

// HostRuntime is the one user-host authority identity and its catalog revisions.
type HostRuntime struct {
	User                string            `json:"user"`
	Host                string            `json:"host"`
	Release             string            `json:"release,omitempty"`
	Generation          uint64            `json:"generation"`
	Endpoint            string            `json:"endpoint,omitempty"`
	ServiceState        string            `json:"service_state,omitempty"`
	ProductReadiness    map[string]string `json:"product_readiness,omitempty"`
	AttachmentRevision  uint64            `json:"attachment_revision,omitempty"`
	DeliveryRevision    uint64            `json:"delivery_revision,omitempty"`
	LaneRevision        uint64            `json:"lane_revision,omitempty"`
	CleanupDebtRevision uint64            `json:"cleanup_debt_revision,omitempty"`
	FederationRevision  uint64            `json:"federation_revision,omitempty"`
}

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
	ID                string         `json:"id"`
	CapabilityHash    string         `json:"capability_hash,omitempty"`
	Product           string         `json:"product"`
	ProfileIdentity   string         `json:"profile_identity,omitempty"`
	LaunchIntent      string         `json:"launch_intent,omitempty"`
	NativeSessionID   string         `json:"native_session_id,omitempty"`
	Name              string         `json:"name,omitempty"`
	NativeName        string         `json:"native_name,omitempty"`
	NativeNameSet     bool           `json:"native_name_set,omitempty"`
	NativeProfileRoot string         `json:"native_profile_root,omitempty"`
	Cwd               string         `json:"cwd,omitempty"`
	Groups            []string       `json:"groups,omitempty"`
	PermissionMode    string         `json:"permission_mode,omitempty"`
	ExpectedEvidence  NativeEvidence `json:"expected_evidence,omitempty"`
	Evidence          NativeEvidence `json:"evidence,omitempty"`
	DaemonGeneration  uint64         `json:"daemon_generation,omitempty"`
	CatalogRevision   uint64         `json:"catalog_revision,omitempty"`
	State             string         `json:"state"`
}

// Delivery is one neutral durable local or remote message acceptance record.
type Delivery struct {
	ID             string   `json:"id"`
	CorrelationID  string   `json:"correlation_id,omitempty"`
	Sender         string   `json:"sender"`
	Destinations   []string `json:"destinations"`
	Groups         []string `json:"groups,omitempty"`
	HostSuffix     string   `json:"host_suffix,omitempty"`
	SentAt         int64    `json:"sent_at,omitempty"`
	State          string   `json:"state"`
	Acknowledgment string   `json:"acknowledgment,omitempty"`
	RetryCause     string   `json:"retry_cause,omitempty"`
}

// Lane is one durable parent-owned native worker lane.
type Lane struct {
	ID                 string   `json:"id"`
	CapabilityHash     string   `json:"capability_hash,omitempty"`
	ParentAttachmentID string   `json:"parent_attachment_id"`
	Product            string   `json:"product"`
	Name               string   `json:"name,omitempty"`
	ProfileIdentity    string   `json:"profile_identity,omitempty"`
	NativeSessionID    string   `json:"native_session_id,omitempty"`
	Cwd                string   `json:"cwd,omitempty"`
	Groups             []string `json:"groups,omitempty"`
	ExplicitGroups     []string `json:"explicit_groups,omitempty"`
	InheritGroups      bool     `json:"inherit_groups,omitempty"`
	PermissionMode     string   `json:"permission_mode,omitempty"`
	ApprovalPolicy     string   `json:"approval_policy,omitempty"`
	Sandbox            string   `json:"sandbox,omitempty"`
	Effort             string   `json:"effort,omitempty"`
	Schema             string   `json:"schema,omitempty"`
	Arguments          []string `json:"arguments,omitempty"`
	Persistent         bool     `json:"persistent,omitempty"`
	AutoArchive        bool     `json:"auto_archive,omitempty"`
	AutoArchiveDelayMS int64    `json:"auto_archive_delay_ms,omitempty"`
	AutoArchiveAt      int64    `json:"auto_archive_at,omitempty"`
	State              string   `json:"state"`
	ArchiveRevision    uint64   `json:"archive_revision,omitempty"`
}

// Turn is one ordered accepted native dispatch within a lane.
type Turn struct {
	ID                 string `json:"id"`
	LaneID             string `json:"lane_id"`
	Sequence           uint64 `json:"sequence"`
	State              string `json:"state"`
	NativeDispatchID   string `json:"native_dispatch_id,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
	Result             string `json:"result,omitempty"`
	Diagnostic         string `json:"diagnostic,omitempty"`
	ExitCode           int    `json:"exit_code,omitempty"`
	StartedAt          int64  `json:"started_at,omitempty"`
	DeadlineAt         int64  `json:"deadline_at,omitempty"`
	CompletedAt        int64  `json:"completed_at,omitempty"`
	TerminalRevision   uint64 `json:"terminal_revision,omitempty"`
	CollectionRevision uint64 `json:"collection_revision,omitempty"`
}

// CleanupDebt retains one exact owned resource whose safe terminal state is not yet proven.
type CleanupDebt struct {
	ID                string `json:"id"`
	Resource          string `json:"resource"`
	BaselineIdentity  string `json:"baseline_identity"`
	IntendedState     string `json:"intended_state"`
	LastVerifiedState string `json:"last_verified_state"`
	Cause             string `json:"cause,omitempty"`
	RetryRevision     uint64 `json:"retry_revision"`
	Operation         string `json:"operation"`
}

// Catalog is the complete durable host state committed as one revision.
type Catalog struct {
	Host         HostRuntime                  `json:"host"`
	Attachments  map[string]ManagedAttachment `json:"attachments"`
	Deliveries   map[string]Delivery          `json:"deliveries"`
	Lanes        map[string]Lane              `json:"lanes"`
	Turns        map[string]Turn              `json:"turns"`
	CleanupDebts map[string]CleanupDebt       `json:"cleanup_debts"`
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
	return StateSnapshot{Revision: committed.Revision, Catalog: isolated}, nil
}

// ValidLifecycleTransition checks only the documented shared lifecycle shape;
// product-native operations remain adapter-owned.
func ValidLifecycleTransition(kind, from, to string) bool {
	if from == to {
		return true
	}
	transitions := map[string]map[string][]string{
		"attachment": {
			"preparing": {"prepared", "detached", "debt"},
			"prepared":  {"selecting", "attached", "detaching", "debt"},
			"selecting": {"attached", "detaching", "debt"},
			"attached":  {"detaching", "debt"},
			"detaching": {"detached", "debt"},
			"debt":      {"detaching", "detached"},
			"detached":  {"preparing"},
		},
		"delivery": {
			"prepared":  {"accepted", "rejected"},
			"accepted":  {"presented", "retryable", "rejected"},
			"presented": {"acknowledged", "retryable", "rejected"},
			"retryable": {"accepted", "rejected"},
		},
		"lane": {
			"preparing":    {"idle", "running", "retiring", "terminal", "cleanup-debt"},
			"idle":         {"preparing", "running", "terminal", "archived", "cleanup-debt"},
			"running":      {"interrupting", "retiring", "terminal", "cleanup-debt"},
			"interrupting": {"retiring", "terminal", "cleanup-debt"},
			"retiring":     {"terminal", "archived", "cleanup-debt"},
			"terminal":     {"idle", "archived", "cleanup-debt"},
			"archived":     {"idle", "cleanup-debt"},
			"cleanup-debt": {"idle", "archived"},
		},
		"turn": {
			"accepted":   {"dispatched", "terminal"},
			"dispatched": {"terminal"},
			"terminal":   {"collected"},
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
		Attachments: map[string]ManagedAttachment{}, Deliveries: map[string]Delivery{},
		Lanes: map[string]Lane{}, Turns: map[string]Turn{}, CleanupDebts: map[string]CleanupDebt{},
	}
}

func normalizedCatalog(catalog Catalog) Catalog {
	if catalog.Attachments == nil {
		catalog.Attachments = map[string]ManagedAttachment{}
	}
	if catalog.Deliveries == nil {
		catalog.Deliveries = map[string]Delivery{}
	}
	if catalog.Lanes == nil {
		catalog.Lanes = map[string]Lane{}
	}
	if catalog.Turns == nil {
		catalog.Turns = map[string]Turn{}
	}
	if catalog.CleanupDebts == nil {
		catalog.CleanupDebts = map[string]CleanupDebt{}
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
	for id, delivery := range catalog.Deliveries {
		if id == "" || delivery.ID != id || !knownState("delivery", delivery.State) {
			return fmt.Errorf("invalid delivery %q", id)
		}
	}
	for id, lane := range catalog.Lanes {
		if id == "" || lane.ID != id || !knownState("lane", lane.State) {
			return fmt.Errorf("invalid lane %q", id)
		}
		if _, ok := productcatalog.ByID(lane.Product); !ok {
			return fmt.Errorf("lane %s has unknown product %q", id, lane.Product)
		}
	}
	for id, turn := range catalog.Turns {
		if id == "" || turn.ID != id || !knownState("turn", turn.State) {
			return fmt.Errorf("invalid turn %q", id)
		}
	}
	for id, debt := range catalog.CleanupDebts {
		if id == "" || debt.ID != id || strings.TrimSpace(debt.Resource) == "" || strings.TrimSpace(debt.Operation) == "" {
			return fmt.Errorf("invalid cleanup debt %q", id)
		}
	}
	return nil
}

//nolint:gocyclo // The closed catalog checks each family's distinct terminal-state rule explicitly.
func validateCatalogTransitions(current, next Catalog) error {
	for id, attachment := range current.Attachments {
		if candidate, ok := next.Attachments[id]; ok {
			if !ValidLifecycleTransition("attachment", attachment.State, candidate.State) {
				return fmt.Errorf("attachment %s cannot transition from %s to %s", id, attachment.State, candidate.State)
			}
		} else if attachment.State != "detached" {
			return fmt.Errorf("attachment %s cannot be removed from state %s", id, attachment.State)
		}
	}
	for id, delivery := range current.Deliveries {
		if candidate, ok := next.Deliveries[id]; ok {
			if !ValidLifecycleTransition("delivery", delivery.State, candidate.State) {
				return fmt.Errorf("delivery %s cannot transition from %s to %s", id, delivery.State, candidate.State)
			}
		} else if delivery.State != "acknowledged" && delivery.State != "rejected" {
			return fmt.Errorf("delivery %s cannot be removed from state %s", id, delivery.State)
		}
	}
	for id, lane := range current.Lanes {
		if candidate, ok := next.Lanes[id]; ok {
			if !ValidLifecycleTransition("lane", lane.State, candidate.State) {
				return fmt.Errorf("lane %s cannot transition from %s to %s", id, lane.State, candidate.State)
			}
		} else if lane.State != "archived" {
			return fmt.Errorf("lane %s cannot be removed from state %s", id, lane.State)
		}
	}
	for id, turn := range current.Turns {
		if candidate, ok := next.Turns[id]; ok {
			if !ValidLifecycleTransition("turn", turn.State, candidate.State) {
				return fmt.Errorf("turn %s cannot transition from %s to %s", id, turn.State, candidate.State)
			}
		} else if turn.State != "collected" {
			return fmt.Errorf("turn %s cannot be removed from state %s", id, turn.State)
		}
	}
	return nil
}

func knownState(kind, state string) bool {
	states := map[string]map[string]bool{
		"attachment": {"preparing": true, "prepared": true, "selecting": true, "attached": true, "detaching": true, "detached": true, "debt": true},
		"delivery":   {"prepared": true, "accepted": true, "presented": true, "acknowledged": true, "retryable": true, "rejected": true},
		"lane":       {"preparing": true, "idle": true, "running": true, "interrupting": true, "retiring": true, "terminal": true, "archived": true, "cleanup-debt": true},
		"turn":       {"accepted": true, "dispatched": true, "terminal": true, "collected": true},
	}
	return states[kind][state]
}
