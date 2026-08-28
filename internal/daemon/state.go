package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/antst/agent-sessions/internal/statestore"
)

// HostRuntimeSchemaVersion is the accepted daemon-owned record schema.
const HostRuntimeSchemaVersion = 1

// HostRuntimeState is the durable lifecycle of one daemon generation.
type HostRuntimeState string

const (
	// HostRuntimeStarting identifies an authority being durably established.
	HostRuntimeStarting HostRuntimeState = "starting"
	// HostRuntimeRecovering identifies an authority reconstructing durable work.
	HostRuntimeRecovering HostRuntimeState = "recovering"
	// HostRuntimeReady identifies a committed authority accepting work.
	HostRuntimeReady HostRuntimeState = "ready"
	// HostRuntimeStopping identifies a committed draining authority.
	HostRuntimeStopping HostRuntimeState = "stopping"
	// HostRuntimeDebt identifies an authority blocked on retryable exact work.
	HostRuntimeDebt HostRuntimeState = "debt"
)

// HostRuntimeRecord is the exact durable identity of one host generation.
type HostRuntimeRecord struct {
	SchemaVersion   int              `json:"schema_version"`
	Generation      uint64           `json:"generation"`
	RuntimeVersion  string           `json:"runtime_version"`
	RuntimeIdentity string           `json:"runtime_identity"`
	HostID          string           `json:"host_id"`
	HostName        string           `json:"host_name"`
	PID             int              `json:"pid"`
	ProcStart       string           `json:"proc_start"`
	StrongStart     string           `json:"strong_start"`
	ControlEndpoint string           `json:"control_endpoint"`
	ServiceManager  string           `json:"service_manager"`
	ServiceUnit     string           `json:"service_unit"`
	StartedAt       int64            `json:"started_at"`
	CommittedAt     int64            `json:"committed_at"`
	State           HostRuntimeState `json:"state"`
	StateRevision   uint64           `json:"state_revision"`
}

// Validate checks exact authority, service, revision, and lifecycle fields.
//
//nolint:gocyclo // Exact authority fields and lifecycle states fail independently for actionable diagnostics.
func (record HostRuntimeRecord) Validate() error {
	if record.SchemaVersion != HostRuntimeSchemaVersion || record.Generation == 0 || record.StateRevision == 0 {
		return errors.New("host runtime record has an unsupported schema or zero authority revision")
	}
	if strings.TrimSpace(record.RuntimeVersion) == "" || strings.TrimSpace(record.RuntimeIdentity) == "" ||
		strings.TrimSpace(record.HostID) == "" || record.PID <= 0 || strings.TrimSpace(record.ProcStart) == "" ||
		strings.TrimSpace(record.StrongStart) == "" || record.StartedAt <= 0 {
		return errors.New("host runtime record has incomplete exact authority")
	}
	if !filepath.IsAbs(record.ControlEndpoint) || filepath.Clean(record.ControlEndpoint) != record.ControlEndpoint {
		return errors.New("host runtime record has a noncanonical control endpoint")
	}
	wantUnit := map[string]string{
		"systemd-user": "agent-sessions.service",
		"launchd-user": "net.antst.agent-sessions",
	}[record.ServiceManager]
	if wantUnit == "" || record.ServiceUnit != wantUnit {
		return errors.New("host runtime record has an unsupported service manager or unit")
	}
	switch record.State {
	case HostRuntimeStarting, HostRuntimeRecovering:
	case HostRuntimeReady, HostRuntimeStopping, HostRuntimeDebt:
		if record.CommittedAt <= 0 {
			return errors.New("committed host runtime state lacks committed_at")
		}
	default:
		return fmt.Errorf("unsupported host runtime state %q", record.State)
	}
	return nil
}

// RecordHeader is embedded by daemon-owned entities that participate in
// generation and revision validation.
type RecordHeader struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	Generation    uint64 `json:"generation,omitempty"`
	CreatedAt     int64  `json:"created_at,omitempty"`
	UpdatedAt     int64  `json:"updated_at"`
}

// AttachmentRecord is one daemon-owned native peer attachment.
type AttachmentRecord struct {
	RecordHeader
	AttachmentID         string         `json:"attachment_id"`
	SessionID            string         `json:"session_id,omitempty"`
	Kind                 string         `json:"kind"`
	Product              string         `json:"product"`
	ProfileIdentity      map[string]any `json:"profile_identity,omitempty"`
	Cwd                  string         `json:"cwd"`
	Name                 string         `json:"name,omitempty"`
	NameSource           string         `json:"name_source,omitempty"`
	HostID               string         `json:"host_id"`
	Groups               []string       `json:"groups"`
	PermissionMode       string         `json:"permission_mode"`
	NativeActor          map[string]any `json:"native_actor,omitempty"`
	ConnectorIdentity    map[string]any `json:"connector_identity,omitempty"`
	LaunchCapabilityHash string         `json:"launch_capability_hash"`
	State                string         `json:"state"`
	DeliveryCursor       string         `json:"delivery_cursor,omitempty"`
	CleanupDebtIDs       []string       `json:"cleanup_debt_ids,omitempty"`
}

// DeliveryRecord is one durable accepted peer-message operation.
type DeliveryRecord struct {
	RecordHeader
	MessageID                string            `json:"message_id"`
	SourceAttachmentID       string            `json:"source_attachment_id"`
	SourceHostID             string            `json:"source_host_id"`
	SourceSessionID          string            `json:"source_session_id"`
	SourceAttachmentRevision uint64            `json:"source_attachment_revision"`
	Operation                string            `json:"operation"`
	RequestedTargets         []string          `json:"requested_targets,omitempty"`
	Group                    string            `json:"group,omitempty"`
	ResolvedDestinations     []string          `json:"resolved_destinations"`
	Content                  string            `json:"content"`
	Summary                  string            `json:"summary,omitempty"`
	SentAt                   string            `json:"sent_at,omitempty"`
	State                    string            `json:"state"`
	DestinationResults       map[string]string `json:"destination_results,omitempty"`
	AcceptedRevision         uint64            `json:"accepted_revision"`
	AcceptedAt               int64             `json:"accepted_at"`
	RequestDigest            string            `json:"request_digest"`
}

// LaneRecord is one daemon-owned durable vendor lane.
type LaneRecord struct {
	RecordHeader
	LaneSessionID       string         `json:"lane_session_id"`
	Name                string         `json:"name"`
	Product             string         `json:"product"`
	ParentHostID        string         `json:"parent_host_id"`
	ParentSessionID     string         `json:"parent_session_id"`
	ParentAttachmentID  string         `json:"parent_attachment_id,omitempty"`
	ParentProduct       string         `json:"parent_product,omitempty"`
	ParentGroups        []string       `json:"parent_groups,omitempty"`
	InheritParentGroups bool           `json:"inherit_parent_groups"`
	Groups              []string       `json:"groups"`
	PermissionMode      string         `json:"permission_mode"`
	Cwd                 string         `json:"cwd"`
	RemoteHostID        string         `json:"remote_host_id,omitempty"`
	State               string         `json:"state"`
	ActiveTurnID        string         `json:"active_turn_id,omitempty"`
	NativeActor         map[string]any `json:"native_actor,omitempty"`
	CollectionCursor    string         `json:"collection_cursor,omitempty"`
	ArchiveRevision     uint64         `json:"archive_revision,omitempty"`
	CleanupDebtIDs      []string       `json:"cleanup_debt_ids,omitempty"`
}

// LaneTurnRecord is one accepted native turn within a lane.
type LaneTurnRecord struct {
	RecordHeader
	TurnID                     string         `json:"turn_id"`
	LaneSessionID              string         `json:"lane_session_id"`
	ParentContextRevision      uint64         `json:"parent_context_revision"`
	RequestDigest              string         `json:"request_digest"`
	RemoteRequestID            string         `json:"remote_request_id,omitempty"`
	RemoteFingerprint          string         `json:"remote_fingerprint,omitempty"`
	RemoteEnvelope             map[string]any `json:"remote_envelope,omitempty"`
	RemoteCancellationState    string         `json:"remote_cancellation_state,omitempty"`
	RemoteCancellationError    string         `json:"remote_cancellation_error,omitempty"`
	RemoteResultOutcome        string         `json:"remote_result_outcome,omitempty"`
	RemoteResultReference      map[string]any `json:"remote_result_reference,omitempty"`
	RemoteNoticeAcknowledgedAt int64          `json:"remote_notice_acknowledged_at,omitempty"`
	InputReference             map[string]any `json:"input_reference,omitempty"`
	DispatchState              string         `json:"dispatch_state"`
	NativeTurnIdentity         map[string]any `json:"native_turn_identity,omitempty"`
	TerminalOutcome            string         `json:"terminal_outcome,omitempty"`
	ResultReference            map[string]any `json:"result_reference,omitempty"`
	TerminalNoticeID           string         `json:"terminal_notice_id,omitempty"`
	CollectionRevision         uint64         `json:"collection_revision,omitempty"`
	CollectedAt                int64          `json:"collected_at,omitempty"`
}

// FederationStateRecord is the host's durable central-hub connection state.
type FederationStateRecord struct {
	RecordHeader
	HostID                    string   `json:"host_id"`
	HostName                  string   `json:"host_name"`
	HubAddress                string   `json:"hub_address,omitempty"`
	ConnectionGeneration      uint64   `json:"connection_generation"`
	ProtocolVersion           int      `json:"protocol_version"`
	AdvertisedCapabilities    []string `json:"advertised_capabilities,omitempty"`
	State                     string   `json:"state"`
	AdvertisedRuntimeVersion  string   `json:"advertised_runtime_version,omitempty"`
	AdvertisedRuntimeIdentity string   `json:"advertised_runtime_identity,omitempty"`
	AdvertisedProducts        []string `json:"advertised_products,omitempty"`
	RemoteRosterRevision      string   `json:"remote_roster_revision,omitempty"`
	LastConnectedAt           int64    `json:"last_connected_at,omitempty"`
	LastErrorCode             string   `json:"last_error_code,omitempty"`
}

// TransactionRecord is one install, removal, or purge journal.
type TransactionRecord struct {
	RecordHeader
	TransactionID string         `json:"transaction_id"`
	Kind          string         `json:"kind"`
	Operation     string         `json:"operation,omitempty"`
	State         string         `json:"state"`
	Details       map[string]any `json:"details,omitempty"`
}

// DebtRecord is exact retryable lifecycle work that cannot yet finish safely.
type DebtRecord struct {
	RecordHeader
	DebtID           string `json:"debt_id"`
	Operation        string `json:"operation"`
	ResourceKind     string `json:"resource_kind"`
	ResourceIdentity string `json:"resource_identity"`
	ExpectedRevision string `json:"expected_revision,omitempty"`
	ObservedRevision string `json:"observed_revision,omitempty"`
	CauseCode        string `json:"cause_code"`
	CauseDetail      string `json:"cause_detail,omitempty"`
	RetryPredicate   string `json:"retry_predicate"`
	ProhibitedScope  string `json:"prohibited_scope"`
	ResolvedAt       int64  `json:"resolved_at,omitempty"`
}

var durableRecordID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Validate checks bounded debt identity and retry-policy metadata.
func (record DebtRecord) Validate() error {
	if record.SchemaVersion != HostRuntimeSchemaVersion || record.Revision == 0 || record.UpdatedAt <= 0 {
		return errors.New("lifecycle debt has an unsupported schema or zero revision")
	}
	if !durableRecordID.MatchString(record.DebtID) || strings.TrimSpace(record.Operation) == "" ||
		strings.TrimSpace(record.ResourceKind) == "" || strings.TrimSpace(record.ResourceIdentity) == "" ||
		strings.TrimSpace(record.CauseCode) == "" || strings.TrimSpace(record.RetryPredicate) == "" ||
		strings.TrimSpace(record.ProhibitedScope) == "" {
		return errors.New("lifecycle debt has incomplete exact identity or retry policy")
	}
	if record.CreatedAt <= 0 || record.ResolvedAt < 0 {
		return errors.New("lifecycle debt has invalid timestamps")
	}
	return nil
}

// StateStore composes host schemas over the role-neutral atomic store.
type StateStore struct{ records *statestore.Store }

// OpenStateStore opens the one canonical host durable root.
func OpenStateStore(root string, maxRecordBytes int64) (*StateStore, error) {
	records, err := statestore.Open(statestore.Options{Root: root, MaxRecordBytes: maxRecordBytes})
	if err != nil {
		return nil, err
	}
	return &StateStore{records: records}, nil
}

// OpenStateStoreExisting opens an already-created host state root without
// creating directories or changing permissions. Offline inspection uses this
// boundary so a read-only command cannot become the first state mutation.
func OpenStateStoreExisting(root string, maxRecordBytes int64) (*StateStore, error) {
	records, err := statestore.OpenExisting(statestore.Options{Root: root, MaxRecordBytes: maxRecordBytes})
	if err != nil {
		return nil, err
	}
	return &StateStore{records: records}, nil
}

// ReadRuntime reads and validates the exact host authority record.
func (store *StateStore) ReadRuntime(ctx context.Context) (HostRuntimeRecord, statestore.Revision, error) {
	var record HostRuntimeRecord
	revision, err := store.records.Read(ctx, "runtime", &record)
	if err == nil {
		err = record.Validate()
	}
	return record, revision, err
}

// CompareAndSwapRuntime commits one validated authority revision.
func (store *StateStore) CompareAndSwapRuntime(ctx context.Context, expected statestore.Revision, record HostRuntimeRecord) (statestore.Revision, error) {
	if err := record.Validate(); err != nil {
		return 0, err
	}
	return store.records.CompareAndSwap(ctx, "runtime", expected, record)
}

// Recover removes only validated abandoned same-root temporary records.
func (store *StateStore) Recover(ctx context.Context) error { return store.records.Recover(ctx) }

func (store *StateStore) readAttachmentCatalog(ctx context.Context) (attachmentCatalog, statestore.Revision, error) {
	var catalog attachmentCatalog
	revision, err := store.records.Read(ctx, "attachments", &catalog)
	return catalog, revision, err
}

func (store *StateStore) compareAndSwapAttachmentCatalog(
	ctx context.Context,
	expected statestore.Revision,
	catalog attachmentCatalog,
) (statestore.Revision, error) {
	return store.records.CompareAndSwap(ctx, "attachments", expected, catalog)
}

func (store *StateStore) readDeliveryCatalog(ctx context.Context) (deliveryCatalog, statestore.Revision, error) {
	var catalog deliveryCatalog
	revision, err := store.records.Read(ctx, "deliveries", &catalog)
	return catalog, revision, err
}

func (store *StateStore) compareAndSwapDeliveryCatalog(
	ctx context.Context,
	expected statestore.Revision,
	catalog deliveryCatalog,
) (statestore.Revision, error) {
	return store.records.CompareAndSwap(ctx, "deliveries", expected, catalog)
}

func (store *StateStore) readLaneCatalog(ctx context.Context) (laneCatalog, statestore.Revision, error) {
	var catalog laneCatalog
	revision, err := store.records.Read(ctx, "lanes", &catalog)
	return catalog, revision, err
}

func (store *StateStore) compareAndSwapLaneCatalog(
	ctx context.Context,
	expected statestore.Revision,
	catalog laneCatalog,
) (statestore.Revision, error) {
	return store.records.CompareAndSwap(ctx, "lanes", expected, catalog)
}

// ReadFederation reads the daemon-owned hub connection state.
func (store *StateStore) ReadFederation(ctx context.Context) (FederationStateRecord, statestore.Revision, error) {
	var record FederationStateRecord
	revision, err := store.records.Read(ctx, "federation/state", &record)
	return record, revision, err
}

// ReadDebt reads and validates one exact lifecycle-debt record.
func (store *StateStore) ReadDebt(ctx context.Context, debtID string) (DebtRecord, statestore.Revision, error) {
	if !durableRecordID.MatchString(debtID) {
		return DebtRecord{}, 0, errors.New("lifecycle debt id is invalid")
	}
	var record DebtRecord
	revision, err := store.records.Read(ctx, "debt/"+debtID, &record)
	if err == nil {
		err = record.Validate()
		return record, revision, err
	}
	if !os.IsNotExist(err) {
		return DebtRecord{}, 0, err
	}
	return DebtRecord{}, 0, os.ErrNotExist
}

// CompareAndSwapDebt commits one validated lifecycle-debt revision.
func (store *StateStore) CompareAndSwapDebt(ctx context.Context, expected statestore.Revision, record DebtRecord) (statestore.Revision, error) {
	if err := record.Validate(); err != nil {
		return 0, err
	}
	return store.records.CompareAndSwap(ctx, "debt/"+record.DebtID, expected, record)
}
