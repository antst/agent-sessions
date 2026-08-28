package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	// LaneStatePrepared identifies a lane whose first turn is durably accepted but not dispatched.
	LaneStatePrepared = "prepared"
	// LaneStateRunning identifies a lane with one exact active native turn.
	LaneStateRunning = "running"
	// LaneStateIdle identifies a lane whose latest turn is terminal and collectable.
	LaneStateIdle = "idle"
	// LaneStateArchiving identifies a lane whose native archive contract is in progress.
	LaneStateArchiving = "archiving"
	// LaneStateArchived identifies a durably retired lane.
	LaneStateArchived = "archived"
	// LaneStateDebt identifies exact unresolved native cleanup work.
	LaneStateDebt = "debt"

	// LaneDispatchAccepted is committed before external native execution begins.
	LaneDispatchAccepted = "accepted"
	// LaneDispatchRunning identifies one native turn which has started exactly once.
	LaneDispatchRunning = "running"
	// LaneDispatchCompleted identifies a naturally completed native turn.
	LaneDispatchCompleted = "completed"
	// LaneDispatchInterrupted identifies the one explicit evidence-approved interrupted outcome.
	LaneDispatchInterrupted = "interrupted"
	// LaneDispatchFailed identifies a terminal native failure.
	LaneDispatchFailed = "failed"
	// LaneDispatchCollected identifies a terminal turn whose collection cursor advanced.
	LaneDispatchCollected = "collected"

	// LaneDispatchStateAccepted is the compatibility alias for an accepted dispatch.
	LaneDispatchStateAccepted = LaneDispatchAccepted
	// LaneDispatchStateRunning is the compatibility alias for a running dispatch.
	LaneDispatchStateRunning = LaneDispatchRunning
	// LaneTerminalCompleted is the compatibility alias for a completed terminal outcome.
	LaneTerminalCompleted = LaneDispatchCompleted
	// LaneTerminalInterrupted is the compatibility alias for an interrupted terminal outcome.
	LaneTerminalInterrupted = LaneDispatchInterrupted
	// LaneTerminalFailed is the compatibility alias for a failed terminal outcome.
	LaneTerminalFailed = LaneDispatchFailed
)

var (
	// ErrLaneNotFound identifies an absent durable lane, turn, or notice.
	ErrLaneNotFound = errors.New("lane record was not found")
	// ErrLaneIdempotencyConflict rejects reuse of a stable lane or turn identity for different work.
	ErrLaneIdempotencyConflict = errors.New("lane idempotency key conflicts with accepted work")
	// ErrLaneNotTerminal rejects collection before a durable terminal outcome exists.
	ErrLaneNotTerminal = errors.New("lane turn is not terminal")
	// ErrLaneArchived rejects new work on an archived lane.
	ErrLaneArchived = errors.New("lane is archived")
)

// LaneResourceError reports one bounded local resource gate before durable acceptance.
type LaneResourceError struct{ Resource string }

func (failure *LaneResourceError) Error() string {
	return fmt.Sprintf("lane %s capacity is unavailable before acceptance", failure.Resource)
}

// LaneDispatchResult is product-native evidence returned after one accepted turn starts.
type LaneDispatchResult struct {
	LaneSessionID      string         `json:"lane_session_id,omitempty"`
	NativeActor        map[string]any `json:"native_actor,omitempty"`
	NativeTurnIdentity map[string]any `json:"native_turn_identity,omitempty"`
	DispatchState      string         `json:"dispatch_state,omitempty"`
}

// LaneReconnectResult classifies one active turn without redispatching it.
type LaneReconnectResult struct {
	NativeActor        map[string]any `json:"native_actor,omitempty"`
	NativeTurnIdentity map[string]any `json:"native_turn_identity,omitempty"`
	DispatchState      string         `json:"dispatch_state"`
	TerminalOutcome    string         `json:"terminal_outcome,omitempty"`
	ResultReference    map[string]any `json:"result_reference,omitempty"`
}

// LaneTerminalResult is stable native terminal metadata returned for collection.
type LaneTerminalResult struct {
	TerminalOutcome    string         `json:"terminal_outcome"`
	ResultReference    map[string]any `json:"result_reference,omitempty"`
	NativeTurnIdentity map[string]any `json:"native_turn_identity,omitempty"`
}

// LaneAdapter is the minimum product-native dispatch and archive boundary.
type LaneAdapter interface {
	// Dispatch starts one durably accepted product-native turn.
	Dispatch(context.Context, LaneRecord, LaneTurnRecord) (LaneDispatchResult, error)
	// Archive retires the exact product-native lane actor.
	Archive(context.Context, LaneRecord) error
}

// LaneStartAdapter dispatches one durably accepted turn through the shared lane engine.
type LaneStartAdapter interface {
	// StartTurn dispatches one durably accepted product-native turn.
	StartTurn(context.Context, LaneRecord, LaneTurnRecord) (LaneDispatchResult, error)
}

// LaneReconnectAdapter reconnects one active native turn without redispatching it.
type LaneReconnectAdapter interface {
	// ReconnectTurn classifies one active native turn without redispatching it.
	ReconnectTurn(context.Context, LaneRecord, LaneTurnRecord) (LaneReconnectResult, error)
}

// LaneInterruptAdapter interrupts one exact active native turn.
type LaneInterruptAdapter interface {
	// InterruptTurn interrupts one exact active native turn.
	InterruptTurn(context.Context, LaneRecord, LaneTurnRecord) error
}

// LaneCollectAdapter collects terminal metadata for one exact native turn.
type LaneCollectAdapter interface {
	// CollectTurn returns terminal metadata for one exact native turn.
	CollectTurn(context.Context, LaneRecord, LaneTurnRecord) (LaneTerminalResult, error)
}

// LaneWaitAdapter blocks on one exact native turn until terminal metadata is
// available or the daemon generation's lifetime ends. Products with a native
// completion channel implement this directly; other products are polled
// through LaneCollectAdapter without redispatch.
type LaneWaitAdapter interface {
	// WaitTurn waits for one exact native turn to produce terminal metadata.
	WaitTurn(context.Context, LaneRecord, LaneTurnRecord) (LaneTerminalResult, error)
}

// LaneCleanupAdapter removes product-native runtime resources after archive.
type LaneCleanupAdapter interface {
	// Cleanup removes product-native runtime resources after archive.
	Cleanup(context.Context, LaneRecord) error
}

// LaneStartRequest durably accepts one first or follow-up turn for an exact parent attachment.
type LaneStartRequest struct {
	LaneSessionID       string         `json:"lane_session_id"`
	TurnID              string         `json:"turn_id"`
	SourceAttachmentID  string         `json:"source_attachment_id"`
	Product             string         `json:"product"`
	Name                string         `json:"name"`
	Cwd                 string         `json:"cwd"`
	Groups              []string       `json:"groups,omitempty"`
	InheritParentGroups bool           `json:"inherit_parent_groups"`
	PermissionMode      string         `json:"permission_mode"`
	InputReference      map[string]any `json:"input_reference,omitempty"`
	AllowDuplicateName  bool           `json:"allow_duplicate_name,omitempty"`
	RemoteRequestID     string         `json:"remote_request_id,omitempty"`
	RemoteFingerprint   string         `json:"remote_fingerprint,omitempty"`
	RemoteEnvelope      map[string]any `json:"remote_envelope,omitempty"`
}

// RemoteLaneSourceRequest durably records source-side ownership before a
// typed lane_exec is emitted. It never invokes a local vendor adapter.
type RemoteLaneSourceRequest struct {
	TargetHostID string
	Request      LaneStartRequest
}

// LaneTerminalRequest commits one adapter-observed terminal outcome without logging result content.
type LaneTerminalRequest struct {
	LaneSessionID      string         `json:"lane_session_id"`
	TurnID             string         `json:"turn_id"`
	Outcome            string         `json:"outcome"`
	NativeTurnIdentity map[string]any `json:"native_turn_identity,omitempty"`
	ResultReference    map[string]any `json:"result_reference,omitempty"`
}

// LaneCollectRequest advances one exact durable collection cursor.
type LaneCollectRequest struct {
	LaneSessionID      string `json:"lane_session_id"`
	TurnID             string `json:"turn_id"`
	SourceAttachmentID string `json:"source_attachment_id,omitempty"`
}

// LaneArchiveRequest retires one exact durable lane through its product-native contract.
type LaneArchiveRequest struct {
	LaneSessionID      string `json:"lane_session_id"`
	SourceAttachmentID string `json:"source_attachment_id,omitempty"`
}

// LaneListRequest selects the existing group-visible lane inventory.
type LaneListRequest struct {
	SourceAttachmentID string `json:"source_attachment_id"`
	All                bool   `json:"all,omitempty"`
	Mine               bool   `json:"mine,omitempty"`
}

// LaneCollection is stable for every collector after the first cursor advance.
type LaneCollection struct {
	Lane               LaneRecord     `json:"lane"`
	Turn               LaneTurnRecord `json:"turn"`
	LaneSessionID      string         `json:"lane_session_id"`
	TurnID             string         `json:"turn_id"`
	Outcome            string         `json:"outcome"`
	ResultReference    map[string]any `json:"result_reference,omitempty"`
	CollectionRevision uint64         `json:"collection_revision"`
	AlreadyCollected   bool           `json:"already_collected"`
}

// LaneNotice is a content-free terminal pointer for the exact parent.
type LaneNotice struct {
	RecordHeader
	NoticeID        string `json:"notice_id"`
	LaneSessionID   string `json:"lane_session_id"`
	TurnID          string `json:"turn_id"`
	ParentHostID    string `json:"parent_host_id"`
	ParentSessionID string `json:"parent_session_id"`
	Outcome         string `json:"outcome"`
}

// LaneObservation is metadata-only and intentionally excludes prompt and result references.
type LaneObservation struct {
	LaneSessionID string `json:"lane_session_id"`
	TurnID        string `json:"turn_id,omitempty"`
	Product       string `json:"product,omitempty"`
	State         string `json:"state"`
	Outcome       string `json:"outcome,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
}

// LaneEngineOptions composes the single lane authority for one daemon generation.
type LaneEngineOptions struct {
	State       *StateStore
	Attachments *AttachmentRegistry
	Adapters    map[string]LaneAdapter
	Generation  uint64
	Lifetime    context.Context
	Now         func() time.Time
	Preflight   func(context.Context, LaneStartRequest) error
	Observe     func(LaneObservation)
}

type laneCatalog struct {
	Lanes   []LaneRecord     `json:"lanes"`
	Turns   []LaneTurnRecord `json:"turns"`
	Notices []LaneNotice     `json:"notices,omitempty"`
}

// LaneEngine serializes lane acceptance, native dispatch, collection, archive, and recovery.
type LaneEngine struct {
	mu            sync.Mutex
	state         *StateStore
	storeRevision statestore.Revision
	attachments   *AttachmentRegistry
	adapters      map[string]LaneAdapter
	generation    uint64
	now           func() time.Time
	preflight     func(context.Context, LaneStartRequest) error
	observe       func(LaneObservation)
	lanes         map[string]LaneRecord
	turns         map[string]LaneTurnRecord
	notices       map[string]LaneNotice
	lifetime      context.Context
	watching      map[string]struct{}
	changed       chan struct{}
}

// NewLaneEngine loads durable lane decisions without dispatching native work.
func NewLaneEngine(options LaneEngineOptions) (*LaneEngine, error) {
	if options.State == nil || options.Attachments == nil || options.Generation == 0 {
		return nil, errors.New("lane engine requires state, attachment authority, and generation")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Lifetime == nil {
		options.Lifetime = context.Background()
	}
	engine := &LaneEngine{
		state: options.State, attachments: options.Attachments, generation: options.Generation,
		now: options.Now, preflight: options.Preflight, observe: options.Observe,
		adapters: make(map[string]LaneAdapter), lanes: make(map[string]LaneRecord),
		turns: make(map[string]LaneTurnRecord), notices: make(map[string]LaneNotice),
		lifetime: options.Lifetime, watching: make(map[string]struct{}), changed: make(chan struct{}),
	}
	for product, adapter := range options.Adapters {
		if adapter != nil {
			engine.adapters[product] = adapter
		}
	}
	catalog, revision, err := options.State.readLaneCatalog(context.Background())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load lane catalog: %w", err)
	}
	if err == nil {
		engine.storeRevision = revision
		for _, lane := range catalog.Lanes {
			engine.lanes[lane.LaneSessionID] = cloneLaneRecord(lane)
		}
		for _, turn := range catalog.Turns {
			engine.turns[laneTurnKey(turn.LaneSessionID, turn.TurnID)] = cloneLaneTurnRecord(turn)
		}
		for _, notice := range catalog.Notices {
			engine.notices[notice.NoticeID] = notice
		}
	}
	return engine, nil
}

// Start commits the exact parent context and accepted turn before invoking the native adapter.
func (engine *LaneEngine) Start(ctx context.Context, request LaneStartRequest) (LaneRecord, LaneTurnRecord, error) {
	return engine.start(ctx, request, nil, "")
}

// startWithParent is the daemon-internal remote-federation admission path. Its
// caller must supply the parent already attested by the federation dispatcher;
// it shares all durable acceptance, digest, and idempotency logic with Start.
func (engine *LaneEngine) startWithParent(
	ctx context.Context,
	request LaneStartRequest,
	attestedParent AttachmentRecord,
	laneHostID string,
) (LaneRecord, LaneTurnRecord, error) {
	return engine.start(ctx, request, &attestedParent, laneHostID)
}

//nolint:gocyclo // Acceptance, idempotency, native dispatch, and durable outcome form one atomic lane transition.
func (engine *LaneEngine) start(
	ctx context.Context,
	request LaneStartRequest,
	attestedParent *AttachmentRecord,
	laneHostID string,
) (LaneRecord, LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.preflight != nil {
		if err := engine.preflight(ctx, request); err != nil {
			engine.emit(request.LaneSessionID, request.TurnID, request.Product, "rejected", "", "resource_capacity")
			return LaneRecord{}, LaneTurnRecord{}, err
		}
	}
	digest, err := laneRequestDigest(request)
	if err == nil && attestedParent != nil {
		digest, err = attestedLaneRequestDigest(request, *attestedParent, laneHostID)
	}
	if err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	key := laneTurnKey(request.LaneSessionID, request.TurnID)
	if prior, ok := engine.turns[key]; ok {
		if prior.RequestDigest != digest {
			return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
		return cloneLaneRecord(engine.lanes[request.LaneSessionID]), cloneLaneTurnRecord(prior), nil
	}
	var parent AttachmentRecord
	if attestedParent == nil {
		var ok bool
		parent, ok = engine.attachments.attachedByID(request.SourceAttachmentID)
		if !ok {
			return LaneRecord{}, LaneTurnRecord{}, ErrAttachmentNotAttested
		}
	} else {
		parent = cloneAttachmentRecord(*attestedParent)
		if parent.AttachmentID != request.SourceAttachmentID || parent.HostID == "" || parent.SessionID == "" ||
			parent.Product == "" || parent.State != AttachmentStateAttached {
			return LaneRecord{}, LaneTurnRecord{}, ErrAttachmentNotAttested
		}
	}
	if err := engine.validateStart(request); err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	if !request.AllowDuplicateName && strings.TrimSpace(request.Name) != "" {
		for existingID, existing := range engine.lanes {
			if existingID == request.LaneSessionID || existing.Product != request.Product ||
				existing.Name != strings.TrimSpace(request.Name) || existing.State == LaneStateArchived ||
				!laneVisibleToAttachment(existing, parent) {
				continue
			}
			return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
	}
	adapter := engine.adapters[request.Product]
	if adapter == nil {
		return LaneRecord{}, LaneTurnRecord{}, fmt.Errorf("lane adapter %s is unavailable", request.Product)
	}
	lane, exists := engine.lanes[request.LaneSessionID]
	if exists {
		if lane.Product != request.Product || lane.ParentHostID != parent.HostID ||
			lane.ParentSessionID != parent.SessionID || lane.State == LaneStateArchived || lane.ActiveTurnID != "" {
			if lane.State == LaneStateArchived {
				return LaneRecord{}, LaneTurnRecord{}, ErrLaneArchived
			}
			return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
	} else {
		now := engine.now().UnixMilli()
		permission := strings.TrimSpace(request.PermissionMode)
		if permission == "" {
			permission = parent.PermissionMode
		}
		lane = LaneRecord{
			RecordHeader:  RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: engine.generation, CreatedAt: now, UpdatedAt: now},
			LaneSessionID: request.LaneSessionID, Name: strings.TrimSpace(request.Name), Product: request.Product,
			ParentHostID: parent.HostID, ParentSessionID: parent.SessionID,
			ParentAttachmentID: parent.AttachmentID, ParentProduct: parent.Product,
			ParentGroups: append([]string(nil), parent.Groups...), InheritParentGroups: request.InheritParentGroups,
			Groups: laneEffectiveGroups(parent, request, laneHostID), PermissionMode: permission, Cwd: request.Cwd,
			State: LaneStatePrepared,
		}
	}
	now := engine.now().UnixMilli()
	turn := LaneTurnRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: engine.generation, CreatedAt: now, UpdatedAt: now},
		TurnID:       request.TurnID, LaneSessionID: request.LaneSessionID, ParentContextRevision: parent.Revision,
		RequestDigest: digest, RemoteRequestID: request.RemoteRequestID, RemoteFingerprint: request.RemoteFingerprint,
		RemoteEnvelope: cloneAttachmentEvidence(request.RemoteEnvelope),
		InputReference: cloneAttachmentEvidence(request.InputReference), DispatchState: LaneDispatchAccepted,
	}
	lane.ActiveTurnID, lane.State = turn.TurnID, LaneStatePrepared
	advanceLaneRecord(&lane, now)
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID], turns[key] = lane, turn
	}); err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, LaneDispatchAccepted, "", "")

	dispatched, dispatchErr := dispatchLaneTurn(ctx, adapter, cloneLaneRecord(lane), cloneLaneTurnRecord(turn))
	if dispatched.LaneSessionID != "" && dispatched.LaneSessionID != lane.LaneSessionID {
		dispatchErr = errors.Join(dispatchErr, ErrLaneIdempotencyConflict)
	}
	if dispatchErr != nil {
		turn.DispatchState, turn.TerminalOutcome = LaneDispatchFailed, LaneDispatchFailed
		lane.State, lane.ActiveTurnID = LaneStateIdle, ""
		notice := newLaneNotice(lane, turn, engine.now().UnixMilli())
		turn.TerminalNoticeID = notice.NoticeID
		advanceLaneTurn(&turn, engine.now().UnixMilli())
		advanceLaneRecord(&lane, turn.UpdatedAt)
		commitErr := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, notices map[string]LaneNotice) {
			lanes[lane.LaneSessionID], turns[key], notices[notice.NoticeID] = lane, turn, notice
		})
		engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, LaneDispatchFailed, LaneDispatchFailed, "native_dispatch")
		return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), errors.Join(dispatchErr, commitErr)
	}
	turn.DispatchState = defaultLaneValue(dispatched.DispatchState, LaneDispatchRunning)
	turn.NativeTurnIdentity = cloneAttachmentEvidence(dispatched.NativeTurnIdentity)
	lane.NativeActor = cloneAttachmentEvidence(dispatched.NativeActor)
	lane.State = LaneStateRunning
	advanceLaneTurn(&turn, engine.now().UnixMilli())
	advanceLaneRecord(&lane, turn.UpdatedAt)
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID], turns[key] = lane, turn
	}); err != nil {
		return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), err
	}
	engine.watchTurnLocked(lane, turn)
	engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, turn.DispatchState, "", "")
	return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), nil
}

// AcceptRemoteSource commits the exact source parent, target, request id and
// turn before federation may send lane_exec. Replaying the same identity is
// idempotent and never redispatches accepted work.
//
//nolint:gocyclo // Remote source admission, replay identity, and durable acceptance form one fail-closed transaction.
func (engine *LaneEngine) AcceptRemoteSource(
	ctx context.Context,
	request RemoteLaneSourceRequest,
) (LaneRecord, LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	targetHostID := strings.TrimSpace(request.TargetHostID)
	start := request.Request
	if targetHostID == "" || strings.TrimSpace(start.RemoteRequestID) == "" {
		return LaneRecord{}, LaneTurnRecord{}, errors.New("remote lane source identity is incomplete")
	}
	parent, ok := engine.attachments.attachedByID(start.SourceAttachmentID)
	if !ok {
		return LaneRecord{}, LaneTurnRecord{}, ErrAttachmentNotAttested
	}
	if err := engine.validateStart(start); err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	digest, err := attestedLaneRequestDigest(start, parent, targetHostID)
	if err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	key := laneTurnKey(start.LaneSessionID, start.TurnID)
	if prior, exists := engine.turns[key]; exists {
		lane := engine.lanes[start.LaneSessionID]
		if prior.RequestDigest != digest || prior.RemoteRequestID != start.RemoteRequestID || lane.RemoteHostID != targetHostID {
			return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
		return cloneLaneRecord(lane), cloneLaneTurnRecord(prior), nil
	}
	if existing, exists := engine.lanes[start.LaneSessionID]; exists {
		if existing.Product != start.Product || existing.ParentHostID != parent.HostID ||
			existing.ParentSessionID != parent.SessionID || existing.RemoteHostID != targetHostID ||
			existing.State == LaneStateArchived || existing.ActiveTurnID != "" {
			return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
	}
	now := engine.now().UnixMilli()
	permission := strings.TrimSpace(start.PermissionMode)
	if permission == "" {
		permission = parent.PermissionMode
	}
	lane, exists := engine.lanes[start.LaneSessionID]
	if !exists {
		lane = LaneRecord{
			RecordHeader:  RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: engine.generation, CreatedAt: now, UpdatedAt: now},
			LaneSessionID: start.LaneSessionID, Name: strings.TrimSpace(start.Name), Product: start.Product,
			ParentHostID: parent.HostID, ParentSessionID: parent.SessionID,
			ParentAttachmentID: parent.AttachmentID, ParentProduct: parent.Product,
			ParentGroups: append([]string(nil), parent.Groups...), InheritParentGroups: start.InheritParentGroups,
			Groups: laneEffectiveGroups(parent, start, targetHostID), PermissionMode: permission, Cwd: start.Cwd,
			RemoteHostID: targetHostID,
		}
	}
	turn := LaneTurnRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: engine.generation, CreatedAt: now, UpdatedAt: now},
		TurnID:       start.TurnID, LaneSessionID: start.LaneSessionID, ParentContextRevision: parent.Revision,
		RequestDigest: digest, RemoteRequestID: start.RemoteRequestID, RemoteFingerprint: start.RemoteFingerprint,
		RemoteEnvelope: cloneAttachmentEvidence(start.RemoteEnvelope),
		InputReference: cloneAttachmentEvidence(start.InputReference), DispatchState: LaneDispatchAccepted,
	}
	lane.ActiveTurnID, lane.State = turn.TurnID, LaneStatePrepared
	advanceLaneRecord(&lane, now)
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID], turns[key] = lane, turn
	}); err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), nil
}

// MarkRemoteSourceRunning records destination acceptance without invoking a
// local adapter. Recovery sees running ownership and waits for its terminal
// notice; it never emits lane_exec again.
func (engine *LaneEngine) MarkRemoteSourceRunning(ctx context.Context, requestID string, acceptedRevision uint64) (LaneRecord, LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.remoteTurn(requestID)
	if err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	if lane.RemoteHostID == "" {
		return LaneRecord{}, LaneTurnRecord{}, ErrLaneIdempotencyConflict
	}
	if turn.TerminalOutcome != "" || turn.DispatchState == LaneDispatchRunning {
		return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), nil
	}
	turn.DispatchState = LaneDispatchRunning
	turn.NativeTurnIdentity = map[string]any{"remote_request_id": requestID, "accepted_revision": acceptedRevision}
	lane.State = LaneStateRunning
	now := engine.now().UnixMilli()
	advanceLaneTurn(&turn, now)
	advanceLaneRecord(&lane, now)
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID], turns[key] = lane, turn
	}); err != nil {
		return LaneRecord{}, LaneTurnRecord{}, err
	}
	engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, turn.DispatchState, "", "")
	return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), nil
}

// RequestRemoteCancellation durably records an unresolved remote decision
// before the source sends lane_cancel.
func (engine *LaneEngine) RequestRemoteCancellation(ctx context.Context, requestID string) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.remoteTurn(requestID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	if lane.RemoteHostID == "" || turn.TerminalOutcome != "" {
		return LaneTurnRecord{}, ErrLaneNotTerminal
	}
	if turn.RemoteCancellationState == "pending" || turn.RemoteCancellationState == "accepted" {
		return cloneLaneTurnRecord(turn), nil
	}
	turn.RemoteCancellationState, turn.RemoteCancellationError = "pending", ""
	advanceLaneTurn(&turn, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(_ map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		turns[key] = turn
	}); err != nil {
		return LaneTurnRecord{}, err
	}
	return cloneLaneTurnRecord(turn), nil
}

// ResolveRemoteCancellation records the exact destination decision. Unknown
// transport outcomes remain pending and are retried after reconnect.
func (engine *LaneEngine) ResolveRemoteCancellation(
	ctx context.Context,
	requestID string,
	accepted bool,
	detail string,
) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	_, turn, key, err := engine.remoteTurn(requestID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	want := "refused"
	if accepted {
		want, detail = "accepted", ""
	}
	if turn.RemoteCancellationState == want && turn.RemoteCancellationError == detail {
		return cloneLaneTurnRecord(turn), nil
	}
	if turn.RemoteCancellationState != "pending" {
		return LaneTurnRecord{}, ErrLaneIdempotencyConflict
	}
	turn.RemoteCancellationState, turn.RemoteCancellationError = want, strings.TrimSpace(detail)
	advanceLaneTurn(&turn, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(_ map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		turns[key] = turn
	}); err != nil {
		return LaneTurnRecord{}, err
	}
	return cloneLaneTurnRecord(turn), nil
}

// StageRemoteResult durably records authenticated destination result evidence
// before the separate content-free terminal notice completes the source turn.
func (engine *LaneEngine) StageRemoteResult(
	ctx context.Context,
	requestID string,
	outcome string,
	resultReference map[string]any,
) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.remoteTurn(requestID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	if lane.RemoteHostID == "" || !validLaneTerminalOutcome(outcome) {
		return LaneTurnRecord{}, ErrLaneIdempotencyConflict
	}
	if turn.RemoteResultOutcome != "" {
		if turn.RemoteResultOutcome != outcome || !equalLaneEvidence(turn.RemoteResultReference, resultReference) {
			return LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
		return cloneLaneTurnRecord(turn), nil
	}
	turn.RemoteResultOutcome = outcome
	turn.RemoteResultReference = cloneAttachmentEvidence(resultReference)
	advanceLaneTurn(&turn, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(_ map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		turns[key] = turn
	}); err != nil {
		return LaneTurnRecord{}, err
	}
	return cloneLaneTurnRecord(turn), nil
}

// AcknowledgeRemoteNotice marks one destination terminal outbox entry only
// after both result evidence and the content-free notice were acknowledged.
func (engine *LaneEngine) AcknowledgeRemoteNotice(ctx context.Context, requestID string) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.remoteTurn(requestID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	if turn.TerminalOutcome == "" || lane.RemoteHostID != "" {
		return LaneTurnRecord{}, ErrLaneNotTerminal
	}
	if turn.RemoteNoticeAcknowledgedAt != 0 {
		return cloneLaneTurnRecord(turn), nil
	}
	turn.RemoteNoticeAcknowledgedAt = engine.now().UnixMilli()
	advanceLaneTurn(&turn, turn.RemoteNoticeAcknowledgedAt)
	if err := engine.commitCatalog(ctx, func(_ map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		turns[key] = turn
	}); err != nil {
		return LaneTurnRecord{}, err
	}
	return cloneLaneTurnRecord(turn), nil
}

func (engine *LaneEngine) remoteTurn(requestID string) (LaneRecord, LaneTurnRecord, string, error) {
	for key, turn := range engine.turns {
		if turn.RemoteRequestID == requestID {
			lane, ok := engine.lanes[turn.LaneSessionID]
			if ok {
				return lane, turn, key, nil
			}
		}
	}
	return LaneRecord{}, LaneTurnRecord{}, "", ErrLaneNotFound
}

// RemoteTurn returns the durable lane and turn accepted for a remote request.
func (engine *LaneEngine) RemoteTurn(requestID string) (LaneRecord, LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, _, err := engine.remoteTurn(requestID)
	return cloneLaneRecord(lane), cloneLaneTurnRecord(turn), err
}

// RemoteTurns returns durable remote provenance for federation recovery. The
// boolean selects source proxies (true) or destination-native turns (false).
func (engine *LaneEngine) RemoteTurns(source bool) []LaneTurnRecord {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	result := make([]LaneTurnRecord, 0)
	for _, turn := range engine.turns {
		if turn.RemoteRequestID == "" {
			continue
		}
		lane, ok := engine.lanes[turn.LaneSessionID]
		if !ok || (lane.RemoteHostID != "") != source {
			continue
		}
		result = append(result, cloneLaneTurnRecord(turn))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LaneSessionID != result[j].LaneSessionID {
			return result[i].LaneSessionID < result[j].LaneSessionID
		}
		return result[i].TurnID < result[j].TurnID
	})
	return result
}

// Complete commits one stable terminal result and one content-free parent notice.
func (engine *LaneEngine) Complete(ctx context.Context, request LaneTerminalRequest) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.exactTurn(request.LaneSessionID, request.TurnID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	if !validLaneTerminalOutcome(request.Outcome) {
		return LaneTurnRecord{}, ErrLaneNotTerminal
	}
	if turn.TerminalOutcome != "" {
		if turn.TerminalOutcome != request.Outcome || !equalLaneEvidence(turn.ResultReference, request.ResultReference) {
			return LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
		return cloneLaneTurnRecord(turn), nil
	}
	turn.DispatchState, turn.TerminalOutcome = request.Outcome, request.Outcome
	turn.NativeTurnIdentity = cloneAttachmentEvidence(request.NativeTurnIdentity)
	turn.ResultReference = cloneAttachmentEvidence(request.ResultReference)
	lane.State, lane.ActiveTurnID = LaneStateIdle, ""
	now := engine.now().UnixMilli()
	notice := newLaneNotice(lane, turn, now)
	turn.TerminalNoticeID = notice.NoticeID
	advanceLaneTurn(&turn, now)
	advanceLaneRecord(&lane, now)
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, notices map[string]LaneNotice) {
		lanes[lane.LaneSessionID], turns[key], notices[notice.NoticeID] = lane, turn, notice
	}); err != nil {
		return LaneTurnRecord{}, err
	}
	engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, turn.DispatchState, turn.TerminalOutcome, "")
	return cloneLaneTurnRecord(turn), nil
}

// Collect advances the cursor once and returns the same stable result to every later collector.
func (engine *LaneEngine) Collect(ctx context.Context, request LaneCollectRequest) (LaneCollection, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, turn, key, err := engine.exactTurn(request.LaneSessionID, request.TurnID)
	if err != nil {
		return LaneCollection{}, err
	}
	if err := engine.authorizeLaneSource(lane, request.SourceAttachmentID); err != nil {
		return LaneCollection{}, err
	}
	if !validLaneTerminalOutcome(turn.TerminalOutcome) {
		return LaneCollection{}, ErrLaneNotTerminal
	}
	already := turn.CollectionRevision != 0
	if !already {
		if collector, ok := engine.adapters[lane.Product].(LaneCollectAdapter); ok && lane.RemoteHostID == "" {
			terminal, collectErr := collector.CollectTurn(ctx, cloneLaneRecord(lane), cloneLaneTurnRecord(turn))
			if collectErr != nil {
				return LaneCollection{}, collectErr
			}
			if terminal.TerminalOutcome != "" && terminal.TerminalOutcome != turn.TerminalOutcome {
				return LaneCollection{}, ErrLaneIdempotencyConflict
			}
			if len(terminal.ResultReference) != 0 {
				turn.ResultReference = cloneAttachmentEvidence(terminal.ResultReference)
			}
			if len(terminal.NativeTurnIdentity) != 0 {
				turn.NativeTurnIdentity = cloneAttachmentEvidence(terminal.NativeTurnIdentity)
			}
		}
		turn.CollectionRevision, turn.CollectedAt, turn.DispatchState = 1, engine.now().UnixMilli(), LaneDispatchCollected
		lane.CollectionCursor = turn.TurnID
		advanceLaneTurn(&turn, turn.CollectedAt)
		advanceLaneRecord(&lane, turn.CollectedAt)
		if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, _ map[string]LaneNotice) {
			lanes[lane.LaneSessionID], turns[key] = lane, turn
		}); err != nil {
			return LaneCollection{}, err
		}
	}
	return LaneCollection{
		Lane: cloneLaneRecord(lane), Turn: cloneLaneTurnRecord(turn), LaneSessionID: lane.LaneSessionID,
		TurnID: turn.TurnID, Outcome: turn.TerminalOutcome, ResultReference: cloneAttachmentEvidence(turn.ResultReference),
		CollectionRevision: turn.CollectionRevision, AlreadyCollected: already,
	}, nil
}

// Wait blocks without polling the control socket until one exact turn is
// durably terminal, then advances the same idempotent collection cursor used
// by an ordinary collect call.
func (engine *LaneEngine) Wait(ctx context.Context, request LaneCollectRequest) (LaneCollection, error) {
	for {
		engine.mu.Lock()
		lane, turn, _, err := engine.exactTurn(request.LaneSessionID, request.TurnID)
		if err == nil {
			err = engine.authorizeLaneSource(lane, request.SourceAttachmentID)
		}
		if err != nil {
			engine.mu.Unlock()
			return LaneCollection{}, err
		}
		if validLaneTerminalOutcome(turn.TerminalOutcome) {
			engine.mu.Unlock()
			return engine.Collect(ctx, request)
		}
		changed := engine.changed
		engine.mu.Unlock()
		select {
		case <-ctx.Done():
			return LaneCollection{}, ctx.Err()
		case <-engine.lifetime.Done():
			return LaneCollection{}, engine.lifetime.Err()
		case <-changed:
		}
	}
}

// Interrupt signals only the exact current native turn and commits one interrupted outcome.
func (engine *LaneEngine) Interrupt(ctx context.Context, request LaneCollectRequest) (LaneTurnRecord, error) {
	engine.mu.Lock()
	lane, turn, _, err := engine.exactTurn(request.LaneSessionID, request.TurnID)
	if err != nil {
		engine.mu.Unlock()
		return LaneTurnRecord{}, err
	}
	if err := engine.authorizeLaneSource(lane, request.SourceAttachmentID); err != nil {
		engine.mu.Unlock()
		return LaneTurnRecord{}, err
	}
	if turn.TerminalOutcome != "" {
		engine.mu.Unlock()
		return cloneLaneTurnRecord(turn), nil
	}
	adapter, ok := engine.adapters[lane.Product].(LaneInterruptAdapter)
	if !ok {
		engine.mu.Unlock()
		return LaneTurnRecord{}, fmt.Errorf("lane adapter %s cannot interrupt turns", lane.Product)
	}
	engine.mu.Unlock()
	if err := adapter.InterruptTurn(ctx, cloneLaneRecord(lane), cloneLaneTurnRecord(turn)); err != nil {
		return LaneTurnRecord{}, err
	}
	return engine.Complete(ctx, LaneTerminalRequest{
		LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, Outcome: LaneDispatchInterrupted,
		NativeTurnIdentity: turn.NativeTurnIdentity,
	})
}

// Archive invokes and verifies native retirement, recording exact cleanup debt on failure.
func (engine *LaneEngine) Archive(ctx context.Context, request LaneArchiveRequest) (LaneRecord, error) {
	engine.mu.Lock()
	lane, ok := engine.lanes[request.LaneSessionID]
	if !ok {
		engine.mu.Unlock()
		return LaneRecord{}, ErrLaneNotFound
	}
	if err := engine.authorizeLaneSource(lane, request.SourceAttachmentID); err != nil {
		engine.mu.Unlock()
		return LaneRecord{}, err
	}
	if lane.State == LaneStateArchived {
		engine.mu.Unlock()
		return cloneLaneRecord(lane), nil
	}
	if lane.State == LaneStateArchiving || lane.State == LaneStateDebt {
		engine.mu.Unlock()
		return LaneRecord{}, ErrLaneIdempotencyConflict
	}
	if lane.ActiveTurnID != "" {
		engine.mu.Unlock()
		return LaneRecord{}, ErrLaneNotTerminal
	}
	lane.State = LaneStateArchiving
	advanceLaneRecord(&lane, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, _ map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID] = lane
	}); err != nil {
		engine.mu.Unlock()
		return LaneRecord{}, err
	}
	adapter := engine.adapters[lane.Product]
	if adapter == nil {
		engine.mu.Unlock()
		return LaneRecord{}, fmt.Errorf("lane adapter %s is unavailable", lane.Product)
	}
	engine.mu.Unlock()

	archiveErr := adapter.Archive(ctx, cloneLaneRecord(lane))
	if archiveErr == nil {
		if cleanup, ok := adapter.(LaneCleanupAdapter); ok {
			archiveErr = cleanup.Cleanup(ctx, cloneLaneRecord(lane))
		}
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane = engine.lanes[request.LaneSessionID]
	if archiveErr != nil {
		debt, debtErr := engine.recordCleanupDebt(ctx, lane, archiveErr)
		if debtErr == nil && !containsLaneString(lane.CleanupDebtIDs, debt.DebtID) {
			lane.CleanupDebtIDs = append(lane.CleanupDebtIDs, debt.DebtID)
			sort.Strings(lane.CleanupDebtIDs)
		}
		lane.State = LaneStateDebt
		advanceLaneRecord(&lane, engine.now().UnixMilli())
		commitErr := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, _ map[string]LaneTurnRecord, _ map[string]LaneNotice) {
			lanes[lane.LaneSessionID] = lane
		})
		engine.emit(lane.LaneSessionID, "", lane.Product, LaneStateDebt, "", "cleanup_debt")
		return cloneLaneRecord(lane), errors.Join(archiveErr, debtErr, commitErr)
	}
	lane.State, lane.ActiveTurnID = LaneStateArchived, ""
	lane.ArchiveRevision++
	advanceLaneRecord(&lane, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, _ map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID] = lane
	}); err != nil {
		return LaneRecord{}, err
	}
	engine.emit(lane.LaneSessionID, "", lane.Product, LaneStateArchived, "", "")
	return cloneLaneRecord(lane), nil
}

// ArchiveRemoteSource records a destination-confirmed archive on the source
// proxy without invoking any local product adapter.
func (engine *LaneEngine) ArchiveRemoteSource(
	ctx context.Context,
	laneSessionID string,
	remoteHostID string,
	archiveRevision uint64,
) (LaneRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, ok := engine.lanes[laneSessionID]
	if !ok {
		return LaneRecord{}, ErrLaneNotFound
	}
	if lane.RemoteHostID == "" || lane.RemoteHostID != remoteHostID || lane.ActiveTurnID != "" || archiveRevision == 0 {
		return LaneRecord{}, ErrLaneIdempotencyConflict
	}
	if lane.State == LaneStateArchived {
		if lane.ArchiveRevision != archiveRevision {
			return LaneRecord{}, ErrLaneIdempotencyConflict
		}
		return cloneLaneRecord(lane), nil
	}
	lane.State = LaneStateArchived
	lane.ArchiveRevision = archiveRevision
	advanceLaneRecord(&lane, engine.now().UnixMilli())
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, _ map[string]LaneTurnRecord, _ map[string]LaneNotice) {
		lanes[lane.LaneSessionID] = lane
	}); err != nil {
		return LaneRecord{}, err
	}
	engine.emit(lane.LaneSessionID, "", lane.Product, LaneStateArchived, "", "")
	return cloneLaneRecord(lane), nil
}

// Reconcile reconnects accepted/running turns without invoking StartTurn again.
//
//nolint:gocyclo // One recovery pass must classify and durably publish every active lane without redispatch.
func (engine *LaneEngine) Reconcile(ctx context.Context) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	changed := false
	recoveredLanes := make(map[string]LaneRecord)
	recoveredTurns := make(map[string]LaneTurnRecord)
	recoveredNotices := make(map[string]LaneNotice)
	keys := make([]string, 0, len(engine.lanes))
	for key := range engine.lanes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, sessionID := range keys {
		lane := engine.lanes[sessionID]
		if lane.ActiveTurnID == "" {
			continue
		}
		// Source-side remote turns are already accepted by the destination.
		// Federation recovery reattaches the connection and terminal notice
		// path; invoking a local adapter here would duplicate native work.
		if lane.RemoteHostID != "" {
			continue
		}
		turnKey := laneTurnKey(sessionID, lane.ActiveTurnID)
		turn, ok := engine.turns[turnKey]
		if !ok {
			return fmt.Errorf("lane %s active turn %s is missing", sessionID, lane.ActiveTurnID)
		}
		adapter := engine.adapters[lane.Product]
		if adapter == nil {
			return fmt.Errorf("lane adapter %s is unavailable", lane.Product)
		}
		reconnector, ok := adapter.(LaneReconnectAdapter)
		if !ok {
			return fmt.Errorf("lane adapter %s cannot reconnect turns", lane.Product)
		}
		result, err := reconnector.ReconnectTurn(ctx, cloneLaneRecord(lane), cloneLaneTurnRecord(turn))
		if err != nil {
			return fmt.Errorf("reconnect %s lane %s: %w", lane.Product, sessionID, err)
		}
		lane.Generation, turn.Generation = engine.generation, engine.generation
		lane.NativeActor = cloneAttachmentEvidence(result.NativeActor)
		turn.NativeTurnIdentity = cloneAttachmentEvidence(result.NativeTurnIdentity)
		turn.DispatchState = defaultLaneValue(result.DispatchState, LaneDispatchRunning)
		now := engine.now().UnixMilli()
		if result.TerminalOutcome != "" {
			if !validLaneTerminalOutcome(result.TerminalOutcome) {
				return fmt.Errorf("reconnect %s lane %s returned invalid outcome %q", lane.Product, sessionID, result.TerminalOutcome)
			}
			turn.TerminalOutcome = result.TerminalOutcome
			turn.ResultReference = cloneAttachmentEvidence(result.ResultReference)
			lane.State, lane.ActiveTurnID = LaneStateIdle, ""
			notice := newLaneNotice(lane, turn, now)
			turn.TerminalNoticeID = notice.NoticeID
			recoveredNotices[notice.NoticeID] = notice
		} else {
			lane.State = LaneStateRunning
		}
		advanceLaneRecord(&lane, now)
		advanceLaneTurn(&turn, now)
		recoveredLanes[sessionID], recoveredTurns[turnKey] = lane, turn
		changed = true
	}
	if !changed {
		return nil
	}
	if err := engine.commitCatalog(ctx, func(lanes map[string]LaneRecord, turns map[string]LaneTurnRecord, notices map[string]LaneNotice) {
		for key, lane := range recoveredLanes {
			lanes[key] = lane
		}
		for key, turn := range recoveredTurns {
			turns[key] = turn
		}
		for key, notice := range recoveredNotices {
			notices[key] = notice
		}
	}); err != nil {
		return err
	}
	for key, turn := range recoveredTurns {
		if turn.TerminalOutcome == "" {
			engine.watchTurnLocked(recoveredLanes[turn.LaneSessionID], recoveredTurns[key])
		}
	}
	return nil
}

// ReadLane returns one independent durable lane snapshot.
func (engine *LaneEngine) ReadLane(_ context.Context, laneSessionID string) (LaneRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, ok := engine.lanes[laneSessionID]
	if !ok {
		return LaneRecord{}, ErrLaneNotFound
	}
	return cloneLaneRecord(lane), nil
}

// Status returns one exact visible lane for a parent-scoped caller.
func (engine *LaneEngine) Status(_ context.Context, request LaneArchiveRequest) (LaneRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, ok := engine.lanes[request.LaneSessionID]
	if !ok {
		return LaneRecord{}, ErrLaneNotFound
	}
	if err := engine.authorizeLaneSource(lane, request.SourceAttachmentID); err != nil {
		return LaneRecord{}, err
	}
	return cloneLaneRecord(lane), nil
}

// ReadTurn returns one independent durable turn snapshot.
func (engine *LaneEngine) ReadTurn(_ context.Context, identifiers ...string) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(identifiers) == 2 {
		var laneSessionID, turnID string
		for index, identifier := range identifiers {
			switch index {
			case 0:
				laneSessionID = identifier
			case 1:
				turnID = identifier
			}
		}
		turn, ok := engine.turns[laneTurnKey(laneSessionID, turnID)]
		if !ok {
			return LaneTurnRecord{}, ErrLaneNotFound
		}
		return cloneLaneTurnRecord(turn), nil
	}
	if len(identifiers) != 1 {
		return LaneTurnRecord{}, ErrLaneNotFound
	}
	var found *LaneTurnRecord
	for _, turn := range engine.turns {
		if turn.TurnID != identifiers[0] {
			continue
		}
		if found != nil {
			return LaneTurnRecord{}, ErrLaneIdempotencyConflict
		}
		clonedTurn := cloneLaneTurnRecord(turn)
		found = &clonedTurn
	}
	if found == nil {
		return LaneTurnRecord{}, ErrLaneNotFound
	}
	return *found, nil
}

// ReadNotice returns one content-free terminal pointer.
func (engine *LaneEngine) ReadNotice(_ context.Context, noticeID string) (LaneNotice, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	notice, ok := engine.notices[noticeID]
	if !ok {
		return LaneNotice{}, ErrLaneNotFound
	}
	return notice, nil
}

// List returns one stable group-visible lane inventory.
func (engine *LaneEngine) List(_ context.Context, request LaneListRequest) ([]LaneRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	source, ok := engine.attachments.attachedByID(request.SourceAttachmentID)
	if !ok {
		return nil, ErrAttachmentNotAttested
	}
	result := make([]LaneRecord, 0)
	for _, lane := range engine.lanes {
		if !request.All && lane.State == LaneStateArchived {
			continue
		}
		if request.Mine && (lane.ParentHostID != source.HostID || lane.ParentSessionID != source.SessionID) {
			continue
		}
		if !laneVisibleToAttachment(lane, source) {
			continue
		}
		result = append(result, cloneLaneRecord(lane))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LaneSessionID < result[j].LaneSessionID })
	return result, nil
}

func (engine *LaneEngine) validateStart(request LaneStartRequest) error {
	if !durableRecordID.MatchString(request.LaneSessionID) || !durableRecordID.MatchString(request.TurnID) ||
		strings.TrimSpace(request.SourceAttachmentID) == "" || strings.TrimSpace(request.Product) == "" ||
		strings.TrimSpace(request.Cwd) == "" {
		return errors.New("lane start has incomplete durable identity")
	}
	for _, descriptor := range productcatalog.Catalog().Products {
		if descriptor.ID == request.Product {
			return nil
		}
	}
	return fmt.Errorf("unsupported lane product %q", request.Product)
}

func (engine *LaneEngine) exactTurn(laneSessionID, turnID string) (LaneRecord, LaneTurnRecord, string, error) {
	lane, ok := engine.lanes[laneSessionID]
	if !ok {
		return LaneRecord{}, LaneTurnRecord{}, "", ErrLaneNotFound
	}
	key := laneTurnKey(laneSessionID, turnID)
	turn, ok := engine.turns[key]
	if !ok {
		return LaneRecord{}, LaneTurnRecord{}, "", ErrLaneNotFound
	}
	return lane, turn, key, nil
}

func (engine *LaneEngine) terminalTurn(laneSessionID, turnID string) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	_, turn, _, err := engine.exactTurn(laneSessionID, turnID)
	if err != nil {
		return LaneTurnRecord{}, err
	}
	if !validLaneTerminalOutcome(turn.TerminalOutcome) {
		return LaneTurnRecord{}, ErrLaneNotTerminal
	}
	return cloneLaneTurnRecord(turn), nil
}

func (engine *LaneEngine) laneSnapshot(laneSessionID string) (LaneRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, ok := engine.lanes[laneSessionID]
	if !ok {
		return LaneRecord{}, ErrLaneNotFound
	}
	return cloneLaneRecord(lane), nil
}

func (engine *LaneEngine) authorizeLaneSource(lane LaneRecord, sourceAttachmentID string) error {
	if strings.TrimSpace(sourceAttachmentID) == "" {
		return nil
	}
	source, ok := engine.attachments.attachedByID(sourceAttachmentID)
	if !ok || !laneVisibleToAttachment(lane, source) {
		return ErrAttachmentNotAttested
	}
	return nil
}

func (engine *LaneEngine) commitCatalog(
	ctx context.Context,
	mutate func(map[string]LaneRecord, map[string]LaneTurnRecord, map[string]LaneNotice),
) error {
	lanes, turns, notices := cloneLaneMaps(engine.lanes, engine.turns, engine.notices)
	mutate(lanes, turns, notices)
	catalog := laneCatalog{
		Lanes: make([]LaneRecord, 0, len(lanes)), Turns: make([]LaneTurnRecord, 0, len(turns)),
		Notices: make([]LaneNotice, 0, len(notices)),
	}
	for _, lane := range lanes {
		catalog.Lanes = append(catalog.Lanes, lane)
	}
	for _, turn := range turns {
		catalog.Turns = append(catalog.Turns, turn)
	}
	for _, notice := range notices {
		catalog.Notices = append(catalog.Notices, notice)
	}
	sort.Slice(catalog.Lanes, func(i, j int) bool { return catalog.Lanes[i].LaneSessionID < catalog.Lanes[j].LaneSessionID })
	sort.Slice(catalog.Turns, func(i, j int) bool {
		left, right := catalog.Turns[i], catalog.Turns[j]
		return laneTurnKey(left.LaneSessionID, left.TurnID) < laneTurnKey(right.LaneSessionID, right.TurnID)
	})
	sort.Slice(catalog.Notices, func(i, j int) bool { return catalog.Notices[i].NoticeID < catalog.Notices[j].NoticeID })
	next, err := engine.state.compareAndSwapLaneCatalog(ctx, engine.storeRevision, catalog)
	if err != nil {
		return err
	}
	engine.storeRevision, engine.lanes, engine.turns, engine.notices = next, lanes, turns, notices
	close(engine.changed)
	engine.changed = make(chan struct{})
	return nil
}

func (engine *LaneEngine) watchTurnLocked(lane LaneRecord, turn LaneTurnRecord) {
	key := laneTurnKey(lane.LaneSessionID, turn.TurnID)
	if _, exists := engine.watching[key]; exists || validLaneTerminalOutcome(turn.TerminalOutcome) {
		return
	}
	adapter := engine.adapters[lane.Product]
	collector, canCollect := adapter.(LaneCollectAdapter)
	waiter, canWait := adapter.(LaneWaitAdapter)
	if !canCollect && !canWait {
		return
	}
	engine.watching[key] = struct{}{}
	go func(lane LaneRecord, turn LaneTurnRecord) {
		defer func() {
			engine.mu.Lock()
			delete(engine.watching, key)
			engine.mu.Unlock()
		}()
		terminal, err := engine.waitNativeTurn(lane, turn, waiter, collector)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, turn.DispatchState, "", "native_terminal_observation")
			}
			return
		}
		_, completeErr := engine.Complete(engine.lifetime, LaneTerminalRequest{
			LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, Outcome: terminal.TerminalOutcome,
			NativeTurnIdentity: terminal.NativeTurnIdentity, ResultReference: terminal.ResultReference,
		})
		if completeErr != nil && !errors.Is(completeErr, context.Canceled) {
			engine.emit(lane.LaneSessionID, turn.TurnID, lane.Product, turn.DispatchState, "", "terminal_commit")
		}
	}(cloneLaneRecord(lane), cloneLaneTurnRecord(turn))
}

func (engine *LaneEngine) waitNativeTurn(
	lane LaneRecord,
	turn LaneTurnRecord,
	waiter LaneWaitAdapter,
	collector LaneCollectAdapter,
) (LaneTerminalResult, error) {
	if waiter != nil {
		return waiter.WaitTurn(engine.lifetime, lane, turn)
	}
	delay := 250 * time.Millisecond
	for {
		terminal, err := collector.CollectTurn(engine.lifetime, lane, turn)
		if err == nil {
			return terminal, nil
		}
		if !errors.Is(err, ErrLaneNotTerminal) {
			return LaneTerminalResult{}, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-engine.lifetime.Done():
			timer.Stop()
			return LaneTerminalResult{}, engine.lifetime.Err()
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

func (engine *LaneEngine) recordCleanupDebt(ctx context.Context, lane LaneRecord, cause error) (DebtRecord, error) {
	now := engine.now().UnixMilli()
	digest := sha256.Sum256([]byte(lane.LaneSessionID))
	debt := DebtRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: engine.generation, CreatedAt: now, UpdatedAt: now},
		DebtID:       "lane-cleanup-" + hex.EncodeToString(digest[:8]), Operation: "archive",
		ResourceKind: "lane", ResourceIdentity: lane.LaneSessionID,
		CauseCode: "native_cleanup_failed", CauseDetail: diagnostics.BoundedCauseDetail(cause.Error()),
		RetryPredicate: "exact native lane cleanup succeeds", ProhibitedScope: "unrelated native sessions and vendor profile data",
	}
	if prior, _, err := engine.state.ReadDebt(ctx, debt.DebtID); err == nil {
		return prior, nil
	}
	if _, err := engine.state.CompareAndSwapDebt(ctx, 0, debt); err != nil {
		return DebtRecord{}, err
	}
	return debt, nil
}

func (engine *LaneEngine) emit(laneSessionID, turnID, product, state, outcome, errorCode string) {
	if engine.observe != nil {
		engine.observe(LaneObservation{
			LaneSessionID: laneSessionID, TurnID: turnID, Product: product,
			State: state, Outcome: outcome, ErrorCode: errorCode,
		})
	}
}

func laneRequestDigest(request LaneStartRequest) (string, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode lane request identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func attestedLaneRequestDigest(request LaneStartRequest, parent AttachmentRecord, laneHostID string) (string, error) {
	body, err := json.Marshal(struct {
		Request    LaneStartRequest `json:"request"`
		ParentID   string           `json:"parent_attachment_id"`
		ParentHost string           `json:"parent_host_id"`
		Parent     string           `json:"parent_session_id"`
		Product    string           `json:"parent_product"`
		Groups     []string         `json:"parent_groups"`
		Permission string           `json:"parent_permission_mode"`
		LaneHost   string           `json:"lane_host_id"`
	}{
		Request: request, ParentID: parent.AttachmentID, ParentHost: parent.HostID,
		Parent: parent.SessionID, Product: parent.Product, Groups: append([]string(nil), parent.Groups...),
		Permission: parent.PermissionMode, LaneHost: laneHostID,
	})
	if err != nil {
		return "", fmt.Errorf("encode attested lane request identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func dispatchLaneTurn(
	ctx context.Context,
	adapter LaneAdapter,
	lane LaneRecord,
	turn LaneTurnRecord,
) (LaneDispatchResult, error) {
	if starter, ok := adapter.(LaneStartAdapter); ok {
		return starter.StartTurn(ctx, lane, turn)
	}
	return adapter.Dispatch(ctx, lane, turn)
}

func laneEffectiveGroups(parent AttachmentRecord, request LaneStartRequest, laneHostID ...string) []string {
	groups := append([]string(nil), request.Groups...)
	parentAnchor := "session:" + parent.HostID + "/" + parent.SessionID
	if request.InheritParentGroups {
		groups = append(groups, parent.Groups...)
	} else {
		groups = append(groups, parentAnchor)
	}
	hostID := parent.HostID
	if len(laneHostID) > 0 && strings.TrimSpace(laneHostID[0]) != "" {
		hostID = laneHostID[0]
	}
	groups = append(groups, "session:"+hostID+"/"+request.LaneSessionID)
	return normalizeAttachmentGroups(groups)
}

func laneVisibleToAttachment(lane LaneRecord, attachment AttachmentRecord) bool {
	if lane.ParentHostID == attachment.HostID && lane.ParentSessionID == attachment.SessionID {
		return true
	}
	for _, group := range lane.Groups {
		if attachmentHasGroup(attachment, group) {
			return true
		}
	}
	return false
}

func newLaneNotice(lane LaneRecord, turn LaneTurnRecord, now int64) LaneNotice {
	return LaneNotice{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: lane.Generation, CreatedAt: now, UpdatedAt: now},
		NoticeID:     turn.LaneSessionID + "." + turn.TurnID + ".terminal", LaneSessionID: turn.LaneSessionID,
		TurnID: turn.TurnID, ParentHostID: lane.ParentHostID, ParentSessionID: lane.ParentSessionID, Outcome: turn.TerminalOutcome,
	}
}

func advanceLaneRecord(lane *LaneRecord, now int64) {
	lane.Revision++
	lane.UpdatedAt = now
}

func advanceLaneTurn(turn *LaneTurnRecord, now int64) {
	turn.Revision++
	turn.UpdatedAt = now
}

func laneTurnKey(laneSessionID, turnID string) string { return laneSessionID + "\x00" + turnID }

func validLaneTerminalOutcome(outcome string) bool {
	return outcome == LaneDispatchCompleted || outcome == LaneDispatchInterrupted || outcome == LaneDispatchFailed
}

func defaultLaneValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func equalLaneEvidence(left, right map[string]any) bool {
	leftBody, leftErr := json.Marshal(left)
	rightBody, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftBody) == string(rightBody)
}

func containsLaneString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cloneLaneRecord(record LaneRecord) LaneRecord {
	record.ParentGroups = append([]string(nil), record.ParentGroups...)
	record.Groups = append([]string(nil), record.Groups...)
	record.NativeActor = cloneAttachmentEvidence(record.NativeActor)
	record.CleanupDebtIDs = append([]string(nil), record.CleanupDebtIDs...)
	return record
}

func cloneLaneTurnRecord(record LaneTurnRecord) LaneTurnRecord {
	record.RemoteEnvelope = cloneAttachmentEvidence(record.RemoteEnvelope)
	record.RemoteResultReference = cloneAttachmentEvidence(record.RemoteResultReference)
	record.InputReference = cloneAttachmentEvidence(record.InputReference)
	record.NativeTurnIdentity = cloneAttachmentEvidence(record.NativeTurnIdentity)
	record.ResultReference = cloneAttachmentEvidence(record.ResultReference)
	return record
}

func cloneLaneMaps(
	lanes map[string]LaneRecord,
	turns map[string]LaneTurnRecord,
	notices map[string]LaneNotice,
) (map[string]LaneRecord, map[string]LaneTurnRecord, map[string]LaneNotice) {
	laneCopy := make(map[string]LaneRecord, len(lanes))
	for key, lane := range lanes {
		laneCopy[key] = cloneLaneRecord(lane)
	}
	turnCopy := make(map[string]LaneTurnRecord, len(turns))
	for key, turn := range turns {
		turnCopy[key] = cloneLaneTurnRecord(turn)
	}
	noticeCopy := make(map[string]LaneNotice, len(notices))
	for key, notice := range notices {
		noticeCopy[key] = notice
	}
	return laneCopy, turnCopy, noticeCopy
}
