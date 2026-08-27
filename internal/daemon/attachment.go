package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	// AttachmentStatePrepared identifies a durably admitted launch awaiting an authoritative native selection.
	AttachmentStatePrepared = "prepared"
	// AttachmentStateAttached identifies a corroborated, discoverable native peer.
	AttachmentStateAttached = "attached"
	// AttachmentStateDetaching identifies an attachment whose exact native actor is leaving.
	AttachmentStateDetaching = "detaching"
	// AttachmentStateDetached identifies a retained but no longer discoverable native peer.
	AttachmentStateDetached = "detached"
)

var (
	// ErrAttachmentNotAttested rejects bare or capability-mismatched native sessions.
	ErrAttachmentNotAttested = errors.New("attachment is not attested")
	// ErrAttachmentEvidenceChanged rejects an actor whose exact native evidence changed.
	ErrAttachmentEvidenceChanged = errors.New("attachment native evidence changed")
	// ErrAttachmentIdentityChanged rejects an attempt to replace the adopted native session.
	ErrAttachmentIdentityChanged = errors.New("attachment native identity changed")
	// ErrAttachmentSelecting identifies a prepared attachment without an authoritative native selection.
	ErrAttachmentSelecting = errors.New("attachment native selection is unresolved")
	// ErrAttachmentAmbiguous rejects a display name that resolves to multiple visible peers.
	ErrAttachmentAmbiguous = errors.New("attachment selector is ambiguous")
	// ErrAttachmentNotFound identifies an absent attachment or session preference.
	ErrAttachmentNotFound = errors.New("attachment was not found")
)

// AttachmentAdapter corroborates vendor-owned actors without taking their lifecycle or transcript authority.
type AttachmentAdapter interface {
	// Corroborate validates current vendor-owned evidence against one exact prepared or attached actor.
	Corroborate(context.Context, AttachmentRecord, map[string]any) (map[string]any, error)
	// Reconnect revalidates an attached actor while a successor daemon generation is recovering.
	Reconnect(context.Context, AttachmentRecord) (map[string]any, error)
}

// InteractiveAttachmentAdapter adds product-native launch preparation without taking vendor lifetime ownership.
type InteractiveAttachmentAdapter interface {
	AttachmentAdapter
	// PrepareInteractive validates intent and returns the exact direct vendor handoff plan.
	PrepareInteractive(context.Context, AttachmentPrepareRequest) (NativeLaunchPlan, error)
}

// ConnectorProcessEvidence is the kernel-corroborated process identity of one
// short-lived vendor connector reaching the daemon.
type ConnectorProcessEvidence struct {
	PID         int
	ProcStart   string
	StrongStart string
}

// ObservedConnectorAdapter resolves a late-bound native session from the
// connector's exact vendor process ancestry. Products without a supported
// observation contract remain durably selecting.
type ObservedConnectorAdapter interface {
	// ObserveConnector resolves one native session from exact connector process evidence.
	ObserveConnector(context.Context, AttachmentRecord, ConnectorProcessEvidence) (string, map[string]any, error)
}

// InteractiveLaunchIntent carries parsed wrapper intent without granting native authority.
type InteractiveLaunchIntent struct {
	Mode               string   `json:"mode"`
	Selector           string   `json:"selector,omitempty"`
	SelectorIsName     bool     `json:"selector_is_name,omitempty"`
	CwdExplicit        bool     `json:"cwd_explicit,omitempty"`
	NativeArguments    []string `json:"native_arguments,omitempty"`
	PermissionExplicit bool     `json:"permission_explicit,omitempty"`
}

// NativeLaunchPlan is the daemon-validated direct handoff to a vendor executable.
type NativeLaunchPlan struct {
	Executable          string            `json:"executable"`
	Arguments           []string          `json:"arguments,omitempty"`
	Environment         map[string]string `json:"environment,omitempty"`
	SessionID           string            `json:"session_id,omitempty"`
	Cwd                 string            `json:"cwd"`
	ExpectedNativeActor map[string]any    `json:"expected_native_actor,omitempty"`
}

// AttachmentRegistryOptions supplies the one daemon generation and its vendor adapters.
type AttachmentRegistryOptions struct {
	State      *StateStore
	Generation uint64
	HostID     string
	Now        func() time.Time
	Capability func() (string, error)
	Adapters   map[string]AttachmentAdapter
}

// AttachmentPrepareRequest durably reserves one managed native launch.
type AttachmentPrepareRequest struct {
	Product             string                  `json:"product"`
	Kind                string                  `json:"kind"`
	ProfileIdentity     map[string]any          `json:"profile_identity,omitempty"`
	Cwd                 string                  `json:"cwd"`
	Name                string                  `json:"name,omitempty"`
	NameSource          string                  `json:"name_source,omitempty"`
	Groups              []string                `json:"groups,omitempty"`
	PermissionMode      string                  `json:"permission_mode,omitempty"`
	ExpectedNativeActor map[string]any          `json:"expected_native_actor,omitempty"`
	Intent              InteractiveLaunchIntent `json:"intent"`
}

// AttachmentAdoptRequest binds an authoritative vendor session to a prepared attachment.
type AttachmentAdoptRequest struct {
	AttachmentID string         `json:"attachment_id"`
	Capability   string         `json:"capability"`
	SessionID    string         `json:"session_id"`
	NativeActor  map[string]any `json:"native_actor,omitempty"`
}

// AttachmentRefreshRequest refreshes exact evidence without changing functional identity.
type AttachmentRefreshRequest struct {
	AttachmentID string         `json:"attachment_id"`
	SessionID    string         `json:"session_id"`
	NativeActor  map[string]any `json:"native_actor,omitempty"`
}

// ConnectorAttestation proves one ephemeral connector belongs to an already prepared attachment.
type ConnectorAttestation struct {
	Product      string
	AttachmentID string
	SessionID    string
	Capability   string
	NativeActor  map[string]any
}

// AttachmentSelector selects one visible attachment by exact address or unambiguous display name.
type AttachmentSelector struct {
	HostID    string `json:"host_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
}

// AttachmentPrepareResult returns the durable reservation and its one-time launch capability.
type AttachmentPrepareResult struct {
	Attachment AttachmentRecord `json:"attachment"`
	Capability string           `json:"capability"`
	Launch     NativeLaunchPlan `json:"launch"`
}

// AttachmentPreferences retains existing session choices without owning vendor history.
type AttachmentPreferences struct {
	SessionID      string   `json:"session_id"`
	Product        string   `json:"product"`
	Kind           string   `json:"kind"`
	Groups         []string `json:"groups,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	Revision       uint64   `json:"revision"`
	UpdatedAt      int64    `json:"updated_at"`
}

type attachmentCatalog struct {
	Attachments []AttachmentRecord      `json:"attachments"`
	Preferences []AttachmentPreferences `json:"preferences,omitempty"`
}

// AttachmentRegistry is the daemon-owned durable catalog and live evidence authority.
type AttachmentRegistry struct {
	mu            sync.Mutex
	state         *StateStore
	storeRevision statestore.Revision
	generation    uint64
	hostID        string
	now           func() time.Time
	capability    func() (string, error)
	adapters      map[string]AttachmentAdapter
	attachments   map[string]AttachmentRecord
	preferences   map[string]AttachmentPreferences
}

// NewAttachmentRegistry loads the durable catalog without publishing any attachment before reconciliation.
func NewAttachmentRegistry(options AttachmentRegistryOptions) (*AttachmentRegistry, error) {
	if options.State == nil || options.Generation == 0 || strings.TrimSpace(options.HostID) == "" {
		return nil, errors.New("attachment registry requires state, generation, and host identity")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Capability == nil {
		options.Capability = randomAttachmentCapability
	}
	registry := &AttachmentRegistry{
		state: options.State, generation: options.Generation, hostID: options.HostID,
		now: options.Now, capability: options.Capability, adapters: make(map[string]AttachmentAdapter),
		attachments: make(map[string]AttachmentRecord), preferences: make(map[string]AttachmentPreferences),
	}
	for product, adapter := range options.Adapters {
		if adapter != nil {
			registry.adapters[product] = adapter
		}
	}
	catalog, revision, err := options.State.readAttachmentCatalog(context.Background())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load attachment catalog: %w", err)
	}
	if err == nil {
		registry.storeRevision = revision
		for _, record := range catalog.Attachments {
			registry.attachments[record.AttachmentID] = cloneAttachmentRecord(record)
		}
		for _, preference := range catalog.Preferences {
			registry.preferences[attachmentPreferenceKey(preference.Product, preference.SessionID)] = cloneAttachmentPreferences(preference)
		}
	}
	return registry, nil
}

// Prepare commits launch intent before the vendor process can become managed.
func (registry *AttachmentRegistry) Prepare(ctx context.Context, request AttachmentPrepareRequest) (AttachmentRecord, string, error) {
	record, capability, _, err := registry.PrepareInteractive(ctx, request)
	return record, capability, err
}

// PrepareInteractive validates product-native intent before committing the durable reservation.
func (registry *AttachmentRegistry) PrepareInteractive(ctx context.Context, request AttachmentPrepareRequest) (AttachmentRecord, string, NativeLaunchPlan, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if err := registry.validatePrepare(request); err != nil {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, err
	}
	adapter, ok := registry.adapters[request.Product].(InteractiveAttachmentAdapter)
	if !ok {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, fmt.Errorf("%s adapter cannot prepare interactive launches", request.Product)
	}
	launch, err := adapter.PrepareInteractive(ctx, request)
	if err != nil {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, err
	}
	if err := validateNativeLaunchPlan(request, launch); err != nil {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, err
	}
	capability, err := registry.capability()
	if err != nil || strings.TrimSpace(capability) == "" {
		if err == nil {
			err = errors.New("empty capability")
		}
		return AttachmentRecord{}, "", NativeLaunchPlan{}, fmt.Errorf("create launch capability: %w", err)
	}
	attachmentID, err := randomAttachmentID()
	if err != nil {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, err
	}
	now := registry.now().UnixMilli()
	record := AttachmentRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: registry.generation, CreatedAt: now, UpdatedAt: now},
		AttachmentID: attachmentID, Kind: request.Kind, Product: request.Product,
		ProfileIdentity: cloneAttachmentEvidence(request.ProfileIdentity), Cwd: request.Cwd,
		Name: request.Name, NameSource: request.NameSource, HostID: registry.hostID,
		Groups: normalizeAttachmentGroups(request.Groups), PermissionMode: request.PermissionMode,
		NativeActor:          cloneAttachmentEvidence(launch.ExpectedNativeActor),
		LaunchCapabilityHash: attachmentCapabilityHash(capability), State: AttachmentStatePrepared,
	}
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		attachments[attachmentID] = record
	}); err != nil {
		return AttachmentRecord{}, "", NativeLaunchPlan{}, err
	}
	return cloneAttachmentRecord(record), capability, cloneNativeLaunchPlan(launch), nil
}

// Adopt atomically binds the authoritative native identity after vendor selection.
func (registry *AttachmentRegistry) Adopt(ctx context.Context, request AttachmentAdoptRequest) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[request.AttachmentID]
	if !ok || !attachmentCapabilityMatches(record.LaunchCapabilityHash, request.Capability) {
		return AttachmentRecord{}, ErrAttachmentNotAttested
	}
	if record.State != AttachmentStatePrepared || strings.TrimSpace(request.SessionID) == "" {
		return AttachmentRecord{}, ErrAttachmentIdentityChanged
	}
	adapter := registry.adapters[record.Product]
	actor, err := adapter.Corroborate(ctx, cloneAttachmentRecord(record), cloneAttachmentEvidence(request.NativeActor))
	if err != nil {
		return AttachmentRecord{}, err
	}
	record.SessionID = request.SessionID
	record.NativeActor = actor
	record.State = AttachmentStateAttached
	registry.advanceAttachment(&record)
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		attachments[record.AttachmentID] = record
	}); err != nil {
		return AttachmentRecord{}, err
	}
	return cloneAttachmentRecord(record), nil
}

// Refresh re-attests the same native session and records current exact actor evidence.
func (registry *AttachmentRegistry) Refresh(ctx context.Context, request AttachmentRefreshRequest) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[request.AttachmentID]
	if !ok || record.State != AttachmentStateAttached {
		return AttachmentRecord{}, ErrAttachmentNotFound
	}
	if request.SessionID != record.SessionID {
		return AttachmentRecord{}, ErrAttachmentIdentityChanged
	}
	actor, err := registry.adapters[record.Product].Corroborate(ctx, cloneAttachmentRecord(record), cloneAttachmentEvidence(request.NativeActor))
	if err != nil {
		return AttachmentRecord{}, err
	}
	record.NativeActor = actor
	registry.advanceAttachment(&record)
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		attachments[record.AttachmentID] = record
	}); err != nil {
		return AttachmentRecord{}, err
	}
	return cloneAttachmentRecord(record), nil
}

// Detach removes one exact attachment from discovery while retaining session preferences.
func (registry *AttachmentRegistry) Detach(ctx context.Context, attachmentID, _ string) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attachmentID]
	if !ok {
		return AttachmentRecord{}, ErrAttachmentNotFound
	}
	record.State = AttachmentStateDetached
	registry.advanceAttachment(&record)
	preference := AttachmentPreferences{
		SessionID: record.SessionID, Product: record.Product, Kind: record.Kind,
		Groups: append([]string(nil), record.Groups...), PermissionMode: record.PermissionMode,
		Revision: record.Revision, UpdatedAt: record.UpdatedAt,
	}
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, preferences map[string]AttachmentPreferences) {
		attachments[record.AttachmentID] = record
		if record.SessionID != "" {
			preferences[attachmentPreferenceKey(record.Product, record.SessionID)] = preference
		}
	}); err != nil {
		return AttachmentRecord{}, err
	}
	return cloneAttachmentRecord(record), nil
}

// AttestConnector verifies a connector against durable launch authority and current actor evidence.
func (registry *AttachmentRegistry) AttestConnector(ctx context.Context, attestation ConnectorAttestation) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attestation.AttachmentID]
	if !ok || record.Product != attestation.Product || !attachmentCapabilityMatches(record.LaunchCapabilityHash, attestation.Capability) {
		return AttachmentRecord{}, ErrAttachmentNotAttested
	}
	if record.State == AttachmentStatePrepared || record.SessionID == "" {
		return AttachmentRecord{}, ErrAttachmentSelecting
	}
	if record.State != AttachmentStateAttached || (attestation.SessionID != "" && attestation.SessionID != record.SessionID) {
		return AttachmentRecord{}, ErrAttachmentNotAttested
	}
	actor, err := registry.adapters[record.Product].Corroborate(ctx, cloneAttachmentRecord(record), cloneAttachmentEvidence(attestation.NativeActor))
	if err != nil {
		return AttachmentRecord{}, err
	}
	record.NativeActor = actor
	record.ConnectorIdentity = map[string]any{"product": record.Product, "verified_at": registry.now().UnixMilli()}
	registry.advanceAttachment(&record)
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		attachments[record.AttachmentID] = record
	}); err != nil {
		return AttachmentRecord{}, err
	}
	return cloneAttachmentRecord(record), nil
}

// PreparedConnector validates only the daemon-issued launch reservation. It
// grants no peer authority while native session selection is unresolved.
func (registry *AttachmentRegistry) PreparedConnector(product, attachmentID, capability string) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attachmentID]
	if !ok || record.Product != product || record.State != AttachmentStatePrepared ||
		!attachmentCapabilityMatches(record.LaunchCapabilityHash, capability) {
		return AttachmentRecord{}, ErrAttachmentNotAttested
	}
	return cloneAttachmentRecord(record), nil
}

// AdoptObservedConnector binds a late-selected native session only when the
// product adapter can corroborate it from exact kernel process evidence.
func (registry *AttachmentRegistry) AdoptObservedConnector(
	ctx context.Context,
	product, attachmentID, capability string,
	evidence ConnectorProcessEvidence,
) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attachmentID]
	if !ok || record.Product != product || record.State != AttachmentStatePrepared ||
		!attachmentCapabilityMatches(record.LaunchCapabilityHash, capability) {
		return AttachmentRecord{}, ErrAttachmentNotAttested
	}
	observer, ok := registry.adapters[product].(ObservedConnectorAdapter)
	if !ok {
		return AttachmentRecord{}, ErrAttachmentSelecting
	}
	sessionID, suppliedActor, err := observer.ObserveConnector(ctx, cloneAttachmentRecord(record), evidence)
	if err != nil {
		return AttachmentRecord{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return AttachmentRecord{}, ErrAttachmentSelecting
	}
	actor, err := registry.adapters[product].Corroborate(ctx, cloneAttachmentRecord(record), suppliedActor)
	if err != nil {
		return AttachmentRecord{}, err
	}
	record.SessionID = sessionID
	record.NativeActor = actor
	record.State = AttachmentStateAttached
	registry.advanceAttachment(&record)
	if err := registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		attachments[record.AttachmentID] = record
	}); err != nil {
		return AttachmentRecord{}, err
	}
	return cloneAttachmentRecord(record), nil
}

// Select resolves an exact address or one unambiguous visible display name.
func (registry *AttachmentRegistry) Select(_ context.Context, selector AttachmentSelector) (AttachmentRecord, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	matches := make([]AttachmentRecord, 0, 1)
	for _, record := range registry.attachments {
		if record.State != AttachmentStateAttached {
			continue
		}
		exact := selector.SessionID != "" && record.SessionID == selector.SessionID && (selector.HostID == "" || record.HostID == selector.HostID)
		named := selector.SessionID == "" && selector.Name != "" && record.Name == selector.Name
		if exact || named {
			matches = append(matches, cloneAttachmentRecord(record))
		}
	}
	if len(matches) == 0 {
		return AttachmentRecord{}, ErrAttachmentNotFound
	}
	if len(matches) > 1 {
		return AttachmentRecord{}, ErrAttachmentAmbiguous
	}
	return matches[0], nil
}

// SessionPreferences returns the retained non-transcript choices for one native session.
func (registry *AttachmentRegistry) SessionPreferences(_ context.Context, product, sessionID string) (AttachmentPreferences, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	preference, ok := registry.preferences[attachmentPreferenceKey(product, sessionID)]
	if !ok {
		return AttachmentPreferences{}, ErrAttachmentNotFound
	}
	return cloneAttachmentPreferences(preference), nil
}

func (registry *AttachmentRegistry) attachedByID(attachmentID string) (AttachmentRecord, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attachmentID]
	if !ok || record.State != AttachmentStateAttached {
		return AttachmentRecord{}, false
	}
	return cloneAttachmentRecord(record), true
}

func (registry *AttachmentRegistry) attachmentByID(attachmentID string) (AttachmentRecord, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.attachments[attachmentID]
	return cloneAttachmentRecord(record), ok
}

func (registry *AttachmentRegistry) attachedRecords() []AttachmentRecord {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	result := make([]AttachmentRecord, 0, len(registry.attachments))
	for _, record := range registry.attachments {
		if record.State == AttachmentStateAttached {
			result = append(result, cloneAttachmentRecord(record))
		}
	}
	sort.Slice(result, func(i, j int) bool { return attachmentAddress(result[i]) < attachmentAddress(result[j]) })
	return result
}

// Reconcile re-corroborates all visible native actors before a new daemon generation publishes them.
func (registry *AttachmentRegistry) Reconcile(ctx context.Context) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	reconciled := cloneAttachmentMap(registry.attachments)
	changed := false
	for id, record := range reconciled {
		if record.State != AttachmentStateAttached {
			continue
		}
		adapter := registry.adapters[record.Product]
		if adapter == nil {
			return fmt.Errorf("reconcile attachment %s: product adapter is unavailable", id)
		}
		actor, err := adapter.Reconnect(ctx, cloneAttachmentRecord(record))
		if err != nil {
			return fmt.Errorf("reconcile attachment %s: %w", id, err)
		}
		record.NativeActor = actor
		registry.advanceAttachment(&record)
		reconciled[id] = record
		changed = true
	}
	if !changed {
		return nil
	}
	return registry.commitAttachmentCatalog(ctx, func(attachments map[string]AttachmentRecord, _ map[string]AttachmentPreferences) {
		for id, record := range reconciled {
			attachments[id] = record
		}
	})
}

func (registry *AttachmentRegistry) validatePrepare(request AttachmentPrepareRequest) error {
	if _, ok := productcatalog.ProductByID(request.Product); !ok || registry.adapters[request.Product] == nil {
		return fmt.Errorf("unsupported attachment product %q", request.Product)
	}
	if request.Kind != "interactive" || strings.TrimSpace(request.Cwd) == "" {
		return errors.New("attachment prepare requires interactive kind and cwd")
	}
	return nil
}

func validateNativeLaunchPlan(request AttachmentPrepareRequest, plan NativeLaunchPlan) error {
	if strings.TrimSpace(plan.Executable) == "" || strings.TrimSpace(plan.Cwd) == "" || plan.Cwd != request.Cwd {
		return errors.New("native launch plan requires an executable and exact requested cwd")
	}
	for key := range plan.Environment {
		if strings.TrimSpace(key) == "" || strings.Contains(key, "=") {
			return errors.New("native launch plan contains an invalid environment name")
		}
	}
	return nil
}

func cloneNativeLaunchPlan(plan NativeLaunchPlan) NativeLaunchPlan {
	plan.Arguments = append([]string(nil), plan.Arguments...)
	plan.ExpectedNativeActor = cloneAttachmentEvidence(plan.ExpectedNativeActor)
	if plan.Environment != nil {
		environment := make(map[string]string, len(plan.Environment))
		for key, value := range plan.Environment {
			environment[key] = value
		}
		plan.Environment = environment
	}
	return plan
}

func (registry *AttachmentRegistry) advanceAttachment(record *AttachmentRecord) {
	record.Revision++
	record.Generation = registry.generation
	record.UpdatedAt = registry.now().UnixMilli()
}

func (registry *AttachmentRegistry) commitAttachmentCatalog(
	ctx context.Context,
	mutate func(map[string]AttachmentRecord, map[string]AttachmentPreferences),
) error {
	attachments := cloneAttachmentMap(registry.attachments)
	preferences := cloneAttachmentPreferenceMap(registry.preferences)
	mutate(attachments, preferences)
	catalog := attachmentCatalog{
		Attachments: make([]AttachmentRecord, 0, len(attachments)),
		Preferences: make([]AttachmentPreferences, 0, len(preferences)),
	}
	for _, record := range attachments {
		catalog.Attachments = append(catalog.Attachments, record)
	}
	for _, preference := range preferences {
		catalog.Preferences = append(catalog.Preferences, preference)
	}
	sort.Slice(catalog.Attachments, func(i, j int) bool { return catalog.Attachments[i].AttachmentID < catalog.Attachments[j].AttachmentID })
	sort.Slice(catalog.Preferences, func(i, j int) bool {
		left := attachmentPreferenceKey(catalog.Preferences[i].Product, catalog.Preferences[i].SessionID)
		right := attachmentPreferenceKey(catalog.Preferences[j].Product, catalog.Preferences[j].SessionID)
		return left < right
	})
	next, err := registry.state.compareAndSwapAttachmentCatalog(ctx, registry.storeRevision, catalog)
	if err != nil {
		return err
	}
	registry.storeRevision = next
	registry.attachments = attachments
	registry.preferences = preferences
	return nil
}

func randomAttachmentCapability() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomAttachmentID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create attachment id: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func attachmentCapabilityHash(capability string) string {
	digest := sha256.Sum256([]byte(capability))
	return hex.EncodeToString(digest[:])
}

func attachmentCapabilityMatches(wantHash, capability string) bool {
	want, err := hex.DecodeString(wantHash)
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(capability))
	return len(want) == len(digest) && subtle.ConstantTimeCompare(want, digest[:]) == 1
}

func normalizeAttachmentGroups(groups []string) []string {
	seen := make(map[string]struct{}, len(groups))
	result := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		result = append(result, group)
	}
	sort.Strings(result)
	return result
}

func cloneAttachmentRecord(record AttachmentRecord) AttachmentRecord {
	record.ProfileIdentity = cloneAttachmentEvidence(record.ProfileIdentity)
	record.Groups = append([]string(nil), record.Groups...)
	record.NativeActor = cloneAttachmentEvidence(record.NativeActor)
	record.ConnectorIdentity = cloneAttachmentEvidence(record.ConnectorIdentity)
	record.CleanupDebtIDs = append([]string(nil), record.CleanupDebtIDs...)
	return record
}

func cloneAttachmentEvidence(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	body, err := json.Marshal(source)
	if err == nil {
		var result map[string]any
		if json.Unmarshal(body, &result) == nil {
			return result
		}
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneAttachmentMap(source map[string]AttachmentRecord) map[string]AttachmentRecord {
	result := make(map[string]AttachmentRecord, len(source))
	for key, record := range source {
		result[key] = cloneAttachmentRecord(record)
	}
	return result
}

func cloneAttachmentPreferences(source AttachmentPreferences) AttachmentPreferences {
	source.Groups = append([]string(nil), source.Groups...)
	return source
}

func cloneAttachmentPreferenceMap(source map[string]AttachmentPreferences) map[string]AttachmentPreferences {
	result := make(map[string]AttachmentPreferences, len(source))
	for key, preference := range source {
		result[key] = cloneAttachmentPreferences(preference)
	}
	return result
}

func attachmentPreferenceKey(product, sessionID string) string { return product + "\x00" + sessionID }
