package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

var (
	ErrAttachmentPreparation = errors.New("attachment preparation failed")
	ErrAttachmentAdoption    = errors.New("attachment adoption failed")
	ErrAttachmentRefresh     = errors.New("attachment refresh failed")
	ErrAttachmentDetach      = errors.New("attachment detach failed")
	ErrAttachmentRollback    = errors.New("attachment rollback failed")
	ErrAttachmentConflict    = errors.New("attachment transaction conflict")
)

// NativeEvidence is one process-local product observation.
type NativeEvidence struct {
	Process          procinfo.Identity
	Ancestry         []procinfo.Identity
	Executable       string
	RegistryPath     string
	SocketPath       string
	ThreadID         string
	LaunchTokenHash  string
	LeaderIdentity   string
	RosterRevision   string
	ArtifactPath     string
	ArtifactType     string
	ArtifactMode     uint32
	ArtifactOwner    uint32
	ArtifactRevision string
	ArtifactDevice   uint64
	ArtifactInode    uint64
	ArtifactPrefix   string
	ArtifactBytes    int64
	RegistryDevice   uint64
	RegistryInode    uint64
	RegistryPrefix   string
	RegistryBytes    int64
}

// ManagedAttachment is one live peer connection known only to this process.
type ManagedAttachment struct {
	ID                 string
	CapabilityHash     string
	Product            string
	ProfileIdentity    string
	LaunchIntent       string
	NativeSessionID    string
	NativeProfileRoot  string
	Cwd                string
	Groups             []string
	Info               map[string]string
	PermissionMode     string
	ExpectedEvidence   NativeEvidence
	Evidence           NativeEvidence
	DaemonGeneration   uint64
	CatalogRevision    uint64
	ComponentProtocol  string
	ComponentRevision  uint64
	IntegrationVersion string
	State              string
}

type AttachmentAdapter struct {
	Prepare   func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Adopt     func(context.Context, ManagedAttachment, NativeEvidence) (NativeEvidence, error)
	Refresh   func(context.Context, ManagedAttachment) (NativeEvidence, error)
	Authorize func(context.Context, ManagedAttachment, NativeEvidence) error
	Detach    func(context.Context, ManagedAttachment) error
	Rollback  func(context.Context, ManagedAttachment) error
}

// AttachmentEngine is the process-local live peer registry.
type AttachmentEngine struct {
	mu         sync.Mutex
	generation uint64
	adapters   map[string]AttachmentAdapter
	active     map[string]ManagedAttachment
	titles     map[string]liveNativeTitle
}

type liveNativeTitle struct {
	nativeSessionID string
	value           string
}

func NewAttachmentEngine(generation uint64, adapters map[string]AttachmentAdapter) (*AttachmentEngine, error) {
	if generation == 0 {
		return nil, errors.New("attachment daemon generation must be positive")
	}
	copyAdapters := make(map[string]AttachmentAdapter, len(adapters))
	for product, adapter := range adapters {
		if _, ok := productcatalog.ByID(product); !ok {
			return nil, fmt.Errorf("attachment adapter has unknown product %q", product)
		}
		copyAdapters[product] = adapter
	}
	return &AttachmentEngine{
		generation: generation, adapters: copyAdapters,
		active: make(map[string]ManagedAttachment), titles: make(map[string]liveNativeTitle),
	}, nil
}

func (e *AttachmentEngine) SetAdapter(product string, adapter AttachmentAdapter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := productcatalog.ByID(product); ok {
		e.adapters[product] = adapter
	}
}

func (e *AttachmentEngine) Prepare(ctx context.Context, requested ManagedAttachment) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := validateAttachmentRequest(requested); err != nil {
		return ManagedAttachment{}, err
	}
	if existing, ok := e.active[requested.ID]; ok {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s already exists in state %s", ErrAttachmentConflict, requested.ID, existing.State)
	}
	adapter, ok := e.adapters[requested.Product]
	if !ok || adapter.Prepare == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s prepare callback is unavailable", ErrAttachmentPreparation, requested.Product)
	}
	requested.State = "preparing"
	requested.DaemonGeneration = e.generation
	e.active[requested.ID] = cloneAttachment(requested)
	expected, err := adapter.Prepare(ctx, cloneAttachment(requested))
	if err != nil {
		rollbackErr := e.rollbackPreparation(ctx, requested, adapter)
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentPreparation, errors.Join(err, rollbackErr))
	}
	requested.ExpectedEvidence = cloneEvidence(expected)
	requested.State = "prepared"
	e.active[requested.ID] = cloneAttachment(requested)
	return cloneAttachment(requested), nil
}

func (e *AttachmentEngine) Adopt(ctx context.Context, id string, observed NativeEvidence) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State == "attached" {
		if reflect.DeepEqual(attachment.Evidence, observed) {
			return attachment, nil
		}
		return ManagedAttachment{}, fmt.Errorf("%w: attached evidence changed", ErrAttachmentConflict)
	}
	if attachment.State != "prepared" && attachment.State != "selecting" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s cannot adopt from %s", ErrAttachmentConflict, id, attachment.State)
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Adopt == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s adopt callback is unavailable", ErrAttachmentAdoption, attachment.Product)
	}
	attachment.State = "selecting"
	e.active[id] = cloneAttachment(attachment)
	corroborated, err := adapter.Adopt(ctx, cloneAttachment(attachment), cloneEvidence(observed))
	if err != nil {
		rollbackErr := e.rollbackPreparation(ctx, attachment, adapter)
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentAdoption, errors.Join(err, rollbackErr))
	}
	attachment.Evidence = cloneEvidence(corroborated)
	attachment.State = "attached"
	e.active[id] = cloneAttachment(attachment)
	return cloneAttachment(attachment), nil
}

func (e *AttachmentEngine) SelectNative(id, nativeSessionID, cwd, permissionMode string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(nativeSessionID) == "" {
		return ManagedAttachment{}, fmt.Errorf("%w: selected native session is empty", ErrAttachmentConflict)
	}
	attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	if attachment.State != "prepared" && attachment.State != "selecting" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s cannot select from %s", ErrAttachmentConflict, id, attachment.State)
	}
	if attachment.NativeSessionID != "" && attachment.NativeSessionID != nativeSessionID {
		return ManagedAttachment{}, fmt.Errorf("%w: selected native session changed", ErrAttachmentConflict)
	}
	attachment.NativeSessionID = nativeSessionID
	if strings.TrimSpace(cwd) != "" {
		attachment.Cwd = cwd
	}
	if strings.TrimSpace(permissionMode) != "" {
		attachment.PermissionMode = permissionMode
	}
	attachment.State = "selecting"
	e.active[id] = cloneAttachment(attachment)
	return cloneAttachment(attachment), nil
}

func (e *AttachmentEngine) Refresh(ctx context.Context, id string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, err := e.currentAttachment(id)
	if err != nil || attachment.State != "attached" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s is not active", ErrAttachmentConflict, id)
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Refresh == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s refresh callback is unavailable", ErrAttachmentRefresh, attachment.Product)
	}
	evidence, err := adapter.Refresh(ctx, cloneAttachment(attachment))
	if err != nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %w", ErrAttachmentRefresh, err)
	}
	attachment.Evidence = cloneEvidence(evidence)
	e.active[id] = cloneAttachment(attachment)
	return cloneAttachment(attachment), nil
}

func (e *AttachmentEngine) Detach(ctx context.Context, id, _ string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Detach == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s detach callback is unavailable", ErrAttachmentDetach, attachment.Product)
	}
	return e.remove(ctx, attachment, adapter.Detach, ErrAttachmentDetach)
}

func (e *AttachmentEngine) Rollback(ctx context.Context, id, _ string) (ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, err := e.currentAttachment(id)
	if err != nil {
		return ManagedAttachment{}, err
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Rollback == nil {
		return ManagedAttachment{}, fmt.Errorf("%w: %s rollback callback is unavailable", ErrAttachmentRollback, attachment.Product)
	}
	return e.remove(ctx, attachment, adapter.Rollback, ErrAttachmentRollback)
}

func (e *AttachmentEngine) ListActive() ([]ManagedAttachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]ManagedAttachment, 0, len(e.active))
	for id, attachment := range e.active {
		if _, reported := e.titles[id]; reported && attachment.State == "attached" {
			result = append(result, cloneAttachment(attachment))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (e *AttachmentEngine) ActiveAttachment(id string) (ManagedAttachment, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, ok := e.active[id]
	_, reported := e.titles[id]
	if !ok || !reported || attachment.State != "attached" && attachment.State != "lane" {
		return ManagedAttachment{}, false, nil
	}
	return cloneAttachment(attachment), true, nil
}

// ReportLive accepts the product session currently speaking over the local
// presence socket. The report is process-local reality, not durable state.
func (e *AttachmentEngine) ReportLive(id, name, product string, groups []string, info map[string]string, lane bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment := e.active[id]
	attachment.ID = id
	attachment.NativeSessionID = id
	attachment.Product = product
	attachment.Cwd = info["cwd"]
	attachment.Groups = append([]string(nil), groups...)
	attachment.Info = cloneStringMap(info)
	attachment.State = "attached"
	if lane {
		attachment.State = "lane"
	}
	e.active[id] = cloneAttachment(attachment)
	e.titles[id] = liveNativeTitle{nativeSessionID: id, value: name}
}

// ForgetLive removes a disconnected session from the process-local registry.
func (e *AttachmentEngine) ForgetLive(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.active, id)
	delete(e.titles, id)
}

func (e *AttachmentEngine) LiveNativeTitle(id string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, ok := e.active[id]
	observation, observed := e.titles[id]
	if !ok || attachment.State != "attached" || !observed || observation.nativeSessionID != attachment.NativeSessionID {
		return "", false, nil
	}
	return observation.value, true, nil
}

func (e *AttachmentEngine) Authorize(ctx context.Context, id, capability, product string, evidence NativeEvidence) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	attachment, ok := e.active[id]
	if !ok || attachment.State != "attached" || product != "" && product != attachment.Product {
		return InactiveControlError()
	}
	if capability != "" && !capabilityMatches(attachment.CapabilityHash, capability) {
		return InactiveControlError()
	}
	adapter, ok := e.adapters[attachment.Product]
	if !ok || adapter.Authorize == nil {
		if capability != "" && capabilityMatches(attachment.CapabilityHash, capability) {
			return nil
		}
		return InactiveControlError()
	}
	if err := adapter.Authorize(ctx, cloneAttachment(attachment), cloneEvidence(evidence)); err != nil {
		return InactiveControlError()
	}
	return nil
}

func (e *AttachmentEngine) rollbackPreparation(ctx context.Context, attachment ManagedAttachment, adapter AttachmentAdapter) error {
	delete(e.active, attachment.ID)
	delete(e.titles, attachment.ID)
	if adapter.Rollback == nil {
		return fmt.Errorf("%w: rollback callback is unavailable", ErrAttachmentRollback)
	}
	if err := adapter.Rollback(ctx, cloneAttachment(attachment)); err != nil {
		return fmt.Errorf("%w: %w", ErrAttachmentRollback, err)
	}
	return nil
}

func (e *AttachmentEngine) remove(ctx context.Context, attachment ManagedAttachment, cleanup func(context.Context, ManagedAttachment) error, root error) (ManagedAttachment, error) {
	target := cloneAttachment(attachment)
	target.State = "detaching"
	if err := cleanup(ctx, target); err != nil {
		return cloneAttachment(attachment), fmt.Errorf("%w: %w", root, err)
	}
	delete(e.active, attachment.ID)
	delete(e.titles, attachment.ID)
	attachment.State = "detached"
	return cloneAttachment(attachment), nil
}

func (e *AttachmentEngine) currentAttachment(id string) (ManagedAttachment, error) {
	if strings.TrimSpace(id) == "" {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment id is empty", ErrAttachmentConflict)
	}
	attachment, ok := e.active[id]
	if !ok {
		return ManagedAttachment{}, fmt.Errorf("%w: attachment %s does not exist", ErrAttachmentConflict, id)
	}
	return cloneAttachment(attachment), nil
}

func validateAttachmentRequest(attachment ManagedAttachment) error {
	if strings.TrimSpace(attachment.ID) == "" || strings.TrimSpace(attachment.CapabilityHash) == "" ||
		strings.TrimSpace(attachment.Product) == "" || strings.TrimSpace(attachment.ProfileIdentity) == "" {
		return fmt.Errorf("%w: attachment identity, capability, product, and profile are required", ErrAttachmentConflict)
	}
	if attachment.State != "" {
		return fmt.Errorf("%w: caller cannot select attachment lifecycle state", ErrAttachmentConflict)
	}
	if _, ok := productcatalog.ByID(attachment.Product); !ok {
		return fmt.Errorf("%w: unknown product %q", ErrAttachmentConflict, attachment.Product)
	}
	return nil
}

func cloneAttachment(attachment ManagedAttachment) ManagedAttachment {
	attachment.Groups = append([]string(nil), attachment.Groups...)
	attachment.Info = cloneStringMap(attachment.Info)
	attachment.ExpectedEvidence = cloneEvidence(attachment.ExpectedEvidence)
	attachment.Evidence = cloneEvidence(attachment.Evidence)
	return attachment
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneEvidence(evidence NativeEvidence) NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
