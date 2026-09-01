// Package daemon contains the durable shared state and lifecycle owned by the
// single Agent Sessions user daemon.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	catalogRecord = "catalog"

	NativeSessionLeaseRecordSchema RecordSchema = "agent-sessions.native-session-lease.v1"
	ComponentBindingRecordSchema   RecordSchema = "agent-sessions.component-binding.v1"
	ComponentSessionRecordSchema   RecordSchema = "agent-sessions.component-session.v1"

	maxDurableOpaqueIDBytes = 128
)

// RecordSchema identifies one independently versioned durable daemon domain.
// A missing map is legacy-compatible; a present record always carries an exact
// known schema and unknown versions fail closed.
type RecordSchema string

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

type LeaseState string

const (
	LeasePrepared    LeaseState = "prepared"
	LeaseHeld        LeaseState = "held"
	LeaseReleasing   LeaseState = "releasing"
	LeaseReleased    LeaseState = "released"
	LeaseCleanupDebt LeaseState = "cleanup-debt"
)

// NativeSessionLeaseKey is a JSON-compatible canonical composite map key.
type NativeSessionLeaseKey string

// NewNativeSessionLeaseKey encodes the exact product/profile/native identity
// without delimiter ambiguity.
func NewNativeSessionLeaseKey(productID, profileIdentity, nativeSessionID string) (NativeSessionLeaseKey, error) {
	parts := [3]string{productID, profileIdentity, nativeSessionID}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", errors.New("native session lease key fields are required")
		}
	}
	body, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return NativeSessionLeaseKey(body), nil
}

type NativeSessionLease struct {
	Schema          RecordSchema      `json:"schema"`
	ProductID       string            `json:"product_id"`
	ProfileIdentity string            `json:"profile_identity"`
	NativeSessionID string            `json:"native_session_id"`
	OwnerLaneID     string            `json:"owner_lane_id"`
	Generation      uint64            `json:"generation"`
	ProcessGroup    procinfo.Identity `json:"process_group,omitempty"`
	State           LeaseState        `json:"state"`
	Revision        uint64            `json:"revision"`
	CreatedAt       int64             `json:"created_at"`
	UpdatedAt       int64             `json:"updated_at"`
}

type BindingState string

const (
	BindingBinding  BindingState = "binding"
	BindingReady    BindingState = "ready"
	BindingRetiring BindingState = "retiring"
	BindingClosed   BindingState = "closed"
)

type ComponentBinding struct {
	Schema            RecordSchema      `json:"schema"`
	BindingID         string            `json:"binding_id"`
	AttachmentID      string            `json:"attachment_id"`
	ProcessIdentity   procinfo.Identity `json:"process_identity"`
	BootstrapRevision uint64            `json:"bootstrap_revision"`
	Generation        uint64            `json:"generation"`
	State             BindingState      `json:"state"`
	LastInboundSeq    uint64            `json:"last_inbound_seq,omitempty"`
	LastOutboundSeq   uint64            `json:"last_outbound_seq,omitempty"`
}

type ComponentSessionState string

const (
	ComponentSessionAnnounced ComponentSessionState = "announced"
	ComponentSessionIdle      ComponentSessionState = "idle"
	ComponentSessionBusy      ComponentSessionState = "busy"
	ComponentSessionClosing   ComponentSessionState = "closing"
	ComponentSessionClosed    ComponentSessionState = "closed"
)

type ComponentSession struct {
	Schema          RecordSchema          `json:"schema"`
	AttachmentID    string                `json:"attachment_id"`
	BindingID       string                `json:"binding_id"`
	NativeSessionID string                `json:"native_session_id"`
	State           ComponentSessionState `json:"state"`
	LastEventSeq    uint64                `json:"last_event_seq,omitempty"`
}

// Catalog is the complete durable host state committed as one revision.
type Catalog struct {
	Host              HostRuntime                                  `json:"host"`
	Attachments       map[string]ManagedAttachment                 `json:"attachments"`
	Lanes             map[string]Lane                              `json:"lanes"`
	CleanupDebts      map[string]CleanupDebt                       `json:"cleanup_debts"`
	NativeLeases      map[NativeSessionLeaseKey]NativeSessionLease `json:"native_leases"`
	ComponentBindings map[string]ComponentBinding                  `json:"component_bindings"`
	ComponentSessions map[string]ComponentSession                  `json:"component_sessions"`
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
			"preparing": {"prepared", "detached", "debt"},
			"prepared":  {"selecting", "attached", "detaching", "debt"},
			"selecting": {"attached", "detaching", "debt"},
			"attached":  {"detaching", "debt"},
			"detaching": {"detached", "debt"},
			"debt":      {"detaching", "detached"},
			"detached":  {"preparing"},
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
		"lease": {
			"prepared": {"held", "releasing", "cleanup-debt"}, "held": {"releasing", "cleanup-debt"},
			"releasing": {"released", "cleanup-debt"}, "cleanup-debt": {"released"},
		},
		"component-binding": {
			"binding": {"ready", "retiring", "closed"}, "ready": {"retiring", "closed"},
			"retiring": {"closed"},
		},
		"component-session": {
			"announced": {"idle", "busy", "closing", "closed"}, "idle": {"busy", "closing", "closed"},
			"busy": {"idle", "closing", "closed"}, "closing": {"closed"},
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
		Lanes:       map[string]Lane{}, CleanupDebts: map[string]CleanupDebt{},
		NativeLeases:      map[NativeSessionLeaseKey]NativeSessionLease{},
		ComponentBindings: map[string]ComponentBinding{}, ComponentSessions: map[string]ComponentSession{},
	}
}

func normalizedCatalog(catalog Catalog) Catalog {
	if catalog.Attachments == nil {
		catalog.Attachments = map[string]ManagedAttachment{}
	}
	if catalog.Lanes == nil {
		catalog.Lanes = map[string]Lane{}
	}
	if catalog.CleanupDebts == nil {
		catalog.CleanupDebts = map[string]CleanupDebt{}
	}
	if catalog.NativeLeases == nil {
		catalog.NativeLeases = map[NativeSessionLeaseKey]NativeSessionLease{}
	}
	if catalog.ComponentBindings == nil {
		catalog.ComponentBindings = map[string]ComponentBinding{}
	}
	if catalog.ComponentSessions == nil {
		catalog.ComponentSessions = map[string]ComponentSession{}
	}
	return catalog
}

func validDurableOpaqueID(value string) bool {
	if len(value) == 0 || len(value) > maxDurableOpaqueIDBytes {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validOptionalProcessIdentity(identity procinfo.Identity) bool {
	if identity == (procinfo.Identity{}) {
		return true
	}
	return identity.PID > 1 && identity.Start != "" && identity.StrongStart != ""
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
		if id == "" || lane.ID != id || !knownState("lane", lane.State) {
			return fmt.Errorf("invalid lane %q", id)
		}
		if lane.NativeSessionID != "" && strings.TrimSpace(lane.NativeSessionID) == "" {
			return fmt.Errorf("lane %s has a blank native session identity", id)
		}
		if _, ok := productcatalog.ByID(lane.Product); !ok {
			return fmt.Errorf("lane %s has unknown product %q", id, lane.Product)
		}
	}
	for id, debt := range catalog.CleanupDebts {
		if id == "" || debt.ID != id || strings.TrimSpace(debt.Resource) == "" || strings.TrimSpace(debt.Operation) == "" {
			return fmt.Errorf("invalid cleanup debt %q", id)
		}
	}
	for key, lease := range catalog.NativeLeases {
		want, err := NewNativeSessionLeaseKey(lease.ProductID, lease.ProfileIdentity, lease.NativeSessionID)
		if lease.Schema != NativeSessionLeaseRecordSchema || err != nil || key != want || !knownState("lease", string(lease.State)) ||
			lease.Generation == 0 || (catalog.Host.Generation > 0 && lease.Generation > catalog.Host.Generation) ||
			!validDurableOpaqueID(lease.OwnerLaneID) || lease.Revision == 0 || lease.CreatedAt <= 0 ||
			lease.UpdatedAt < lease.CreatedAt || !validOptionalProcessIdentity(lease.ProcessGroup) {
			return fmt.Errorf("invalid native session lease %q", key)
		}
		if _, ok := productcatalog.ByID(lease.ProductID); !ok {
			return fmt.Errorf("native session lease %q has unknown product", key)
		}
		lane, ownerExists := catalog.Lanes[lease.OwnerLaneID]
		if lease.State != LeaseReleased {
			if !ownerExists || lane.Product != lease.ProductID || lane.ProfileIdentity != lease.ProfileIdentity || lane.NativeSessionID != lease.NativeSessionID {
				return fmt.Errorf("native session lease %q has invalid owner lane", key)
			}
		} else if ownerExists && (lane.Product != lease.ProductID || lane.ProfileIdentity != lease.ProfileIdentity || lane.NativeSessionID != lease.NativeSessionID) {
			return fmt.Errorf("released native session lease %q has conflicting historical owner", key)
		}
	}
	activeBindings := map[string]bool{}
	readyBindings := map[string]bool{}
	for id, binding := range catalog.ComponentBindings {
		if binding.Schema != ComponentBindingRecordSchema || !validDurableOpaqueID(id) || binding.BindingID != id ||
			!knownState("component-binding", string(binding.State)) || binding.Generation == 0 ||
			(catalog.Host.Generation > 0 && binding.Generation > catalog.Host.Generation) ||
			!validOptionalProcessIdentity(binding.ProcessIdentity) || binding.ProcessIdentity == (procinfo.Identity{}) ||
			binding.BootstrapRevision == 0 {
			return fmt.Errorf("invalid component binding %q", id)
		}
		attachment, ok := catalog.Attachments[binding.AttachmentID]
		if !ok {
			return fmt.Errorf("component binding %s references unknown attachment", id)
		}
		if binding.State == BindingBinding || binding.State == BindingReady {
			if catalog.Host.Generation > 0 && binding.Generation != catalog.Host.Generation {
				return fmt.Errorf("active component binding %s belongs to stale generation %d", id, binding.Generation)
			}
			if attachment.State != "prepared" && attachment.State != "selecting" && attachment.State != "attached" {
				return fmt.Errorf("active component binding %s references attachment in state %s", id, attachment.State)
			}
			activeKey := fmt.Sprintf("%s\x00%d", binding.AttachmentID, binding.Generation)
			if activeBindings[activeKey] {
				return fmt.Errorf("attachment %s has multiple active component bindings in generation %d", binding.AttachmentID, binding.Generation)
			}
			activeBindings[activeKey] = true
		}
		if binding.State == BindingReady {
			if readyBindings[binding.AttachmentID] {
				return fmt.Errorf("attachment %s has multiple ready component bindings", binding.AttachmentID)
			}
			readyBindings[binding.AttachmentID] = true
		}
	}
	for id, session := range catalog.ComponentSessions {
		if session.Schema != ComponentSessionRecordSchema || !validDurableOpaqueID(id) || session.AttachmentID != id ||
			!validDurableOpaqueID(session.BindingID) || session.NativeSessionID == "" ||
			!knownState("component-session", string(session.State)) || (session.State != ComponentSessionClosed && session.LastEventSeq == 0) {
			return fmt.Errorf("invalid component session %q", id)
		}
		attachment, ok := catalog.Attachments[id]
		if !ok || (attachment.NativeSessionID != "" && attachment.NativeSessionID != session.NativeSessionID) {
			return fmt.Errorf("component session %s does not match attachment", id)
		}
		if session.State != ComponentSessionClosed {
			binding, ok := catalog.ComponentBindings[session.BindingID]
			if !ok || binding.AttachmentID != id || (binding.State != BindingBinding && binding.State != BindingReady) {
				return fmt.Errorf("component session %s has invalid binding", id)
			}
			if (session.State == ComponentSessionIdle || session.State == ComponentSessionBusy) &&
				(attachment.State != "attached" || attachment.NativeSessionID == "") {
				return fmt.Errorf("active component session %s lacks an exact attached native session", id)
			}
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
			if lane.NativeSessionID != candidate.NativeSessionID &&
				(lane.NativeSessionID != "" || candidate.NativeSessionID == "") {
				return fmt.Errorf("lane %s native session identity changed after binding", id)
			}
			if !ValidLifecycleTransition("lane", lane.State, candidate.State) {
				return fmt.Errorf("lane %s cannot transition from %s to %s", id, lane.State, candidate.State)
			}
		} else if lane.State != "archived" {
			return fmt.Errorf("lane %s cannot be removed from state %s", id, lane.State)
		}
	}
	for key, lease := range current.NativeLeases {
		if candidate, ok := next.NativeLeases[key]; ok {
			if lease.Schema != candidate.Schema || lease.ProductID != candidate.ProductID || lease.ProfileIdentity != candidate.ProfileIdentity ||
				lease.NativeSessionID != candidate.NativeSessionID || lease.OwnerLaneID != candidate.OwnerLaneID || lease.CreatedAt != candidate.CreatedAt {
				return fmt.Errorf("native session lease %s changed immutable identity", key)
			}
			if candidate.Generation < lease.Generation || candidate.Revision < lease.Revision || candidate.UpdatedAt < lease.UpdatedAt ||
				(!reflect.DeepEqual(lease, candidate) && candidate.Revision <= lease.Revision) {
				return fmt.Errorf("native session lease %s regressed mutation evidence", key)
			}
			if lease.ProcessGroup != candidate.ProcessGroup {
				initialHold := lease.State == LeasePrepared && lease.ProcessGroup == (procinfo.Identity{}) && candidate.State == LeaseHeld
				recoveredGeneration := candidate.Generation > lease.Generation
				if !initialHold && !recoveredGeneration {
					return fmt.Errorf("native session lease %s changed process identity without a new generation", key)
				}
			}
			if !ValidLifecycleTransition("lease", string(lease.State), string(candidate.State)) {
				return fmt.Errorf("native session lease %s cannot transition from %s to %s", key, lease.State, candidate.State)
			}
		} else if lease.State != LeaseReleased {
			return fmt.Errorf("native session lease %s cannot be removed from state %s", key, lease.State)
		}
	}
	for id, binding := range current.ComponentBindings {
		if candidate, ok := next.ComponentBindings[id]; ok {
			if binding.Schema != candidate.Schema || binding.AttachmentID != candidate.AttachmentID ||
				binding.ProcessIdentity != candidate.ProcessIdentity || binding.Generation != candidate.Generation ||
				binding.BootstrapRevision != candidate.BootstrapRevision {
				return fmt.Errorf("component binding %s changed immutable identity", id)
			}
			if candidate.LastInboundSeq < binding.LastInboundSeq || candidate.LastOutboundSeq < binding.LastOutboundSeq {
				return fmt.Errorf("component binding %s sequence regressed", id)
			}
			if !ValidLifecycleTransition("component-binding", string(binding.State), string(candidate.State)) {
				return fmt.Errorf("component binding %s cannot transition from %s to %s", id, binding.State, candidate.State)
			}
		} else if binding.State != BindingClosed {
			return fmt.Errorf("component binding %s cannot be removed from state %s", id, binding.State)
		}
	}
	for id, session := range current.ComponentSessions {
		if candidate, ok := next.ComponentSessions[id]; ok {
			if session.Schema != candidate.Schema || session.AttachmentID != candidate.AttachmentID || session.NativeSessionID != candidate.NativeSessionID {
				return fmt.Errorf("component session %s changed immutable identity", id)
			}
			if candidate.LastEventSeq < session.LastEventSeq {
				return fmt.Errorf("component session %s event sequence regressed", id)
			}
			if session.BindingID != candidate.BindingID {
				prior, priorOK := current.ComponentBindings[session.BindingID]
				rebound, reboundOK := next.ComponentBindings[candidate.BindingID]
				retiredPrior, stillPresent := next.ComponentBindings[session.BindingID]
				if !priorOK || !reboundOK || rebound.Generation <= prior.Generation ||
					(stillPresent && retiredPrior.State != BindingRetiring && retiredPrior.State != BindingClosed) {
					return fmt.Errorf("component session %s changed binding without exact generation retirement", id)
				}
			}
			if !ValidLifecycleTransition("component-session", string(session.State), string(candidate.State)) {
				return fmt.Errorf("component session %s cannot transition from %s to %s", id, session.State, candidate.State)
			}
		} else if session.State != ComponentSessionClosed {
			return fmt.Errorf("component session %s cannot be removed from state %s", id, session.State)
		}
	}

	for key, lease := range next.NativeLeases {
		if _, exists := current.NativeLeases[key]; !exists {
			if lease.State != LeasePrepared {
				return fmt.Errorf("native session lease %s starts in non-initial state %s", key, lease.State)
			}
			if next.Host.Generation > 0 && lease.Generation != next.Host.Generation {
				return fmt.Errorf("native session lease %s starts in stale generation %d", key, lease.Generation)
			}
		}
	}
	for id, binding := range next.ComponentBindings {
		if _, exists := current.ComponentBindings[id]; !exists && binding.State != BindingBinding {
			return fmt.Errorf("component binding %s starts in non-initial state %s", id, binding.State)
		}
	}
	for id, session := range next.ComponentSessions {
		if _, exists := current.ComponentSessions[id]; !exists && session.State != ComponentSessionAnnounced {
			return fmt.Errorf("component session %s starts in non-initial state %s", id, session.State)
		}
	}
	return nil
}

func knownState(kind, state string) bool {
	states := map[string]map[string]bool{
		"attachment":        {"preparing": true, "prepared": true, "selecting": true, "attached": true, "detaching": true, "detached": true, "debt": true},
		"lane":              {"preparing": true, "idle": true, "running": true, "interrupting": true, "retiring": true, "terminal": true, "archived": true, "cleanup-debt": true},
		"lease":             {"prepared": true, "held": true, "releasing": true, "released": true, "cleanup-debt": true},
		"component-binding": {"binding": true, "ready": true, "retiring": true, "closed": true},
		"component-session": {"announced": true, "idle": true, "busy": true, "closing": true, "closed": true},
	}
	return states[kind][state]
}
