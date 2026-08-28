package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

var (
	// ErrRemoteLaneParentMismatch rejects a request whose parent differs from the hub-attested source.
	ErrRemoteLaneParentMismatch = errors.New("remote lane parent does not match its attested source")
	// ErrRemoteLaneIdempotencyConflict rejects reuse of a remote request identity for different work.
	ErrRemoteLaneIdempotencyConflict = errors.New("remote lane idempotency conflict")
)

// RemoteLaneEnvelope is the logical, product-neutral lane request routed by
// the hub. It contains durable identities and references, never native CLI
// argv or a second lane-manager protocol.
type RemoteLaneEnvelope struct {
	RequestID           string         `json:"request_id"`
	SourceID            string         `json:"source_id"`
	TargetHostID        string         `json:"target_host_id"`
	Parent              Peer           `json:"parent"`
	Product             string         `json:"product"`
	LaneSessionID       string         `json:"lane_session_id"`
	TurnID              string         `json:"turn_id"`
	Name                string         `json:"name"`
	Cwd                 string         `json:"cwd"`
	Groups              []string       `json:"groups,omitempty"`
	InheritParentGroups bool           `json:"inherit_parent_groups"`
	AllowDuplicateName  bool           `json:"allow_duplicate_name,omitempty"`
	PermissionMode      string         `json:"permission_mode"`
	InputReference      map[string]any `json:"input_reference,omitempty"`
}

// RemoteLaneRequest is the normalized callback shape implemented by the host
// daemon. It deliberately does not import daemon, keeping federation reusable
// by the central hub and wire tests.
type RemoteLaneRequest struct {
	RequestID            string         `json:"request_id"`
	SourceID             string         `json:"source_id"`
	TargetHostID         string         `json:"target_host_id"`
	Product              string         `json:"product"`
	LaneSessionID        string         `json:"lane_session_id"`
	TurnID               string         `json:"turn_id"`
	Name                 string         `json:"name"`
	Cwd                  string         `json:"cwd"`
	ParentHostID         string         `json:"parent_host_id"`
	ParentSessionID      string         `json:"parent_session_id"`
	ParentProduct        string         `json:"parent_product"`
	ParentInstanceID     string         `json:"parent_instance_id"`
	ParentPermissionMode string         `json:"parent_permission_mode"`
	ParentGroups         []string       `json:"parent_groups,omitempty"`
	Groups               []string       `json:"groups,omitempty"`
	InheritParentGroups  bool           `json:"inherit_parent_groups"`
	AllowDuplicateName   bool           `json:"allow_duplicate_name,omitempty"`
	PermissionMode       string         `json:"permission_mode"`
	InputReference       map[string]any `json:"input_reference,omitempty"`
}

// RemoteLaneAccepted is exact durable destination acceptance evidence.
type RemoteLaneAccepted struct {
	RequestID        string `json:"request_id"`
	LaneSessionID    string `json:"lane_session_id"`
	TurnID           string `json:"turn_id"`
	AcceptedRevision uint64 `json:"accepted_revision"`
}

// RemoteLaneArchive requests destination-owned native archive and cleanup for
// one already accepted durable lane. RequestID is the operation correlation;
// RemoteRequestID identifies the destination lane provenance established by
// lane_exec.
type RemoteLaneArchive struct {
	RequestID       string `json:"request_id"`
	RemoteRequestID string `json:"remote_request_id"`
	SourceID        string `json:"source_id"`
	TargetHostID    string `json:"target_host_id"`
	Product         string `json:"product"`
	LaneSessionID   string `json:"lane_session_id"`
}

// RemoteLaneArchived is exact destination archive acknowledgement.
type RemoteLaneArchived struct {
	RequestID       string `json:"request_id"`
	LaneSessionID   string `json:"lane_session_id"`
	ArchiveRevision uint64 `json:"archive_revision"`
}

// RemoteLaneResult is one terminal destination result projected as a notice.
type RemoteLaneResult struct {
	RequestID       string         `json:"request_id"`
	LaneSessionID   string         `json:"lane_session_id"`
	TurnID          string         `json:"turn_id"`
	Outcome         string         `json:"outcome"`
	ResultReference map[string]any `json:"result_reference,omitempty"`
}

// RemoteLaneNotice is intentionally content-free. The source host resolves
// the durable result through its existing lane collection contract.
type RemoteLaneNotice struct {
	NoticeID        string `json:"notice_id"`
	RequestID       string `json:"request_id"`
	TargetHostID    string `json:"target_host_id"`
	TargetSessionID string `json:"target_session_id"`
	LaneSessionID   string `json:"lane_session_id"`
	TurnID          string `json:"turn_id"`
	Outcome         string `json:"outcome"`
}

// RemoteLaneHandler admits one normalized request into daemon lane authority.
type RemoteLaneHandler interface {
	// StartRemoteLane durably accepts or idempotently recovers one request.
	StartRemoteLane(context.Context, RemoteLaneRequest) (RemoteLaneAccepted, error)
}

// RemoteLaneDispatcherOptions configure destination admission and notice publication.
type RemoteLaneDispatcherOptions struct {
	LocalHostID   string
	Handler       RemoteLaneHandler
	PublishResult func(context.Context, RemoteLaneResult) error
	PublishNotice func(context.Context, RemoteLaneNotice) error
}

type remoteLaneDispatch struct {
	fingerprint string
	request     RemoteLaneRequest
	accepted    RemoteLaneAccepted
	err         error
	ready       chan struct{}
}

type remoteLaneTerminal struct {
	fingerprint string
	notice      RemoteLaneNotice
	ready       chan struct{}
	err         error
	publishing  bool
}

// RemoteLaneDispatcher serializes request-id idempotency while calling the
// daemon authority directly in process. It owns no process, socket, listener,
// native transcript, or lane lifecycle state.
type RemoteLaneDispatcher struct {
	localHostID   string
	handler       RemoteLaneHandler
	publishResult func(context.Context, RemoteLaneResult) error
	publishNotice func(context.Context, RemoteLaneNotice) error

	mu         sync.Mutex
	dispatches map[string]*remoteLaneDispatch
	terminals  map[string]*remoteLaneTerminal
}

// RemoteLaneRequestFingerprint returns the stable normalized request digest
// persisted beside destination lane state for dispatcher recovery.
func RemoteLaneRequestFingerprint(request RemoteLaneRequest) (string, error) {
	return remoteLaneFingerprint(request)
}

// ValidateRemoteLaneInput enforces the encoded 1 MiB logical input boundary
// before either source or destination persists a request.
func ValidateRemoteLaneInput(input map[string]any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode remote lane input: %w", err)
	}
	if len(body) > MaxLaneInputBytes {
		return fmt.Errorf("remote lane input exceeds %d bytes", MaxLaneInputBytes)
	}
	return nil
}

// RestoreAccepted rehydrates one destination request from daemon-owned lane
// state. It never calls the handler and therefore cannot redispatch work.
func (dispatcher *RemoteLaneDispatcher) RestoreAccepted(
	request RemoteLaneRequest,
	accepted RemoteLaneAccepted,
	fingerprint string,
) error {
	if request.RequestID == "" || request.ParentHostID == "" || request.ParentSessionID == "" ||
		accepted.RequestID != request.RequestID || accepted.LaneSessionID != request.LaneSessionID ||
		accepted.TurnID != request.TurnID || accepted.AcceptedRevision == 0 || fingerprint == "" {
		return errors.New("recovered remote lane acceptance is incomplete")
	}
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if prior := dispatcher.dispatches[request.RequestID]; prior != nil {
		if prior.fingerprint != fingerprint || prior.accepted != accepted {
			return ErrRemoteLaneIdempotencyConflict
		}
		return nil
	}
	ready := make(chan struct{})
	close(ready)
	dispatcher.dispatches[request.RequestID] = &remoteLaneDispatch{
		fingerprint: fingerprint, request: cloneRemoteLaneRequest(request), accepted: accepted, ready: ready,
	}
	return nil
}

// NewRemoteLaneDispatcher constructs the in-process destination dispatcher.
func NewRemoteLaneDispatcher(options RemoteLaneDispatcherOptions) (*RemoteLaneDispatcher, error) {
	hostID := strings.TrimSpace(options.LocalHostID)
	if hostID == "" || options.Handler == nil {
		return nil, errors.New("remote lane dispatcher requires a local host and daemon handler")
	}
	return &RemoteLaneDispatcher{
		localHostID: hostID, handler: options.Handler, publishResult: options.PublishResult, publishNotice: options.PublishNotice,
		dispatches: make(map[string]*remoteLaneDispatch), terminals: make(map[string]*remoteLaneTerminal),
	}, nil
}

// Dispatch validates and idempotently admits one typed remote request.
func (dispatcher *RemoteLaneDispatcher) Dispatch(ctx context.Context, envelope RemoteLaneEnvelope) (RemoteLaneAccepted, error) {
	request, fingerprint, err := dispatcher.normalize(envelope)
	if err != nil {
		return RemoteLaneAccepted{}, err
	}
	dispatcher.mu.Lock()
	if prior := dispatcher.dispatches[request.RequestID]; prior != nil {
		if prior.fingerprint != fingerprint {
			dispatcher.mu.Unlock()
			return RemoteLaneAccepted{}, ErrRemoteLaneIdempotencyConflict
		}
		ready := prior.ready
		dispatcher.mu.Unlock()
		select {
		case <-ctx.Done():
			return RemoteLaneAccepted{}, ctx.Err()
		case <-ready:
			return prior.accepted, prior.err
		}
	}
	record := &remoteLaneDispatch{fingerprint: fingerprint, request: request, ready: make(chan struct{})}
	dispatcher.dispatches[request.RequestID] = record
	dispatcher.mu.Unlock()

	accepted, dispatchErr := dispatcher.handler.StartRemoteLane(ctx, cloneRemoteLaneRequest(request))
	if dispatchErr == nil && (accepted.RequestID != request.RequestID ||
		accepted.LaneSessionID != request.LaneSessionID || accepted.TurnID != request.TurnID ||
		accepted.AcceptedRevision == 0) {
		dispatchErr = errors.New("remote lane daemon returned mismatched acceptance evidence")
	}
	dispatcher.mu.Lock()
	record.accepted, record.err = accepted, dispatchErr
	close(record.ready)
	dispatcher.mu.Unlock()
	return accepted, dispatchErr
}

// PublishResult emits one stable content-free terminal notice.
func (dispatcher *RemoteLaneDispatcher) PublishResult(ctx context.Context, result RemoteLaneResult) (RemoteLaneNotice, error) {
	fingerprint, err := remoteLaneFingerprint(result)
	if err != nil {
		return RemoteLaneNotice{}, err
	}
	for {
		dispatcher.mu.Lock()
		dispatch := dispatcher.dispatches[result.RequestID]
		if dispatch == nil {
			dispatcher.mu.Unlock()
			return RemoteLaneNotice{}, errors.New("remote lane result has no accepted request")
		}
		if terminal := dispatcher.terminals[result.RequestID]; terminal != nil {
			if terminal.fingerprint != fingerprint {
				dispatcher.mu.Unlock()
				return RemoteLaneNotice{}, ErrRemoteLaneIdempotencyConflict
			}
			if terminal.publishing {
				ready := terminal.ready
				dispatcher.mu.Unlock()
				select {
				case <-ctx.Done():
					return RemoteLaneNotice{}, ctx.Err()
				case <-ready:
					continue
				}
			}
			if terminal.err == nil {
				notice := terminal.notice
				dispatcher.mu.Unlock()
				return notice, nil
			}
			terminal.publishing = true
			terminal.ready = make(chan struct{})
			notice := terminal.notice
			resultPublisher, noticePublisher := dispatcher.publishResult, dispatcher.publishNotice
			dispatcher.mu.Unlock()
			return dispatcher.publishTerminal(ctx, result, notice, terminal, resultPublisher, noticePublisher)
		}
		if dispatch.err != nil || dispatch.accepted.LaneSessionID != result.LaneSessionID ||
			dispatch.accepted.TurnID != result.TurnID || !remoteLaneTerminalOutcome(result.Outcome) {
			dispatcher.mu.Unlock()
			return RemoteLaneNotice{}, ErrRemoteLaneIdempotencyConflict
		}
		notice := RemoteLaneNotice{
			NoticeID: remoteLaneNoticeID(result), RequestID: result.RequestID,
			TargetHostID: dispatch.request.ParentHostID, TargetSessionID: dispatch.request.ParentSessionID,
			LaneSessionID: result.LaneSessionID, TurnID: result.TurnID, Outcome: result.Outcome,
		}
		terminal := &remoteLaneTerminal{fingerprint: fingerprint, notice: notice, ready: make(chan struct{}), publishing: true}
		dispatcher.terminals[result.RequestID] = terminal
		resultPublisher, noticePublisher := dispatcher.publishResult, dispatcher.publishNotice
		dispatcher.mu.Unlock()
		return dispatcher.publishTerminal(ctx, result, notice, terminal, resultPublisher, noticePublisher)
	}
}

func (dispatcher *RemoteLaneDispatcher) publishTerminal(
	ctx context.Context,
	result RemoteLaneResult,
	notice RemoteLaneNotice,
	terminal *remoteLaneTerminal,
	resultPublisher func(context.Context, RemoteLaneResult) error,
	noticePublisher func(context.Context, RemoteLaneNotice) error,
) (RemoteLaneNotice, error) {
	var publishErr error
	if resultPublisher != nil {
		publishErr = resultPublisher(ctx, result)
	}
	if publishErr == nil && noticePublisher != nil {
		publishErr = noticePublisher(ctx, notice)
	}
	dispatcher.mu.Lock()
	terminal.err = publishErr
	terminal.publishing = false
	close(terminal.ready)
	dispatcher.mu.Unlock()
	return notice, publishErr
}

//nolint:gocyclo // The closed remote identity and permission gate is intentionally centralized.
func (dispatcher *RemoteLaneDispatcher) normalize(envelope RemoteLaneEnvelope) (RemoteLaneRequest, string, error) {
	if strings.TrimSpace(envelope.RequestID) == "" || strings.TrimSpace(envelope.SourceID) == "" ||
		envelope.TargetHostID != dispatcher.localHostID || strings.TrimSpace(envelope.LaneSessionID) == "" ||
		strings.TrimSpace(envelope.TurnID) == "" || strings.TrimSpace(envelope.Name) == "" ||
		strings.TrimSpace(envelope.Cwd) == "" || !validRemoteLanePermission(envelope.Product, envelope.PermissionMode) {
		return RemoteLaneRequest{}, "", errors.New("invalid remote lane request")
	}
	if _, ok := productcatalog.ProductByID(envelope.Product); !ok {
		return RemoteLaneRequest{}, "", fmt.Errorf("unsupported remote lane product %q", envelope.Product)
	}
	if err := ValidateRemoteLaneInput(envelope.InputReference); err != nil {
		return RemoteLaneRequest{}, "", err
	}
	parent := envelope.Parent
	if envelope.SourceID != parent.ID || parent.ID == "" || parent.HostID == "" || parent.SessionID == "" ||
		parent.Entrypoint == "" || parent.InstanceID == "" || !validRemoteLanePermission(parent.Entrypoint, parent.PermissionMode) {
		return RemoteLaneRequest{}, "", ErrRemoteLaneParentMismatch
	}
	if _, ok := productcatalog.ProductByID(parent.Entrypoint); !ok {
		return RemoteLaneRequest{}, "", ErrRemoteLaneParentMismatch
	}
	request := RemoteLaneRequest{
		RequestID: envelope.RequestID, SourceID: envelope.SourceID, TargetHostID: envelope.TargetHostID,
		Product: envelope.Product, LaneSessionID: envelope.LaneSessionID, TurnID: envelope.TurnID,
		Name: envelope.Name, Cwd: envelope.Cwd, ParentHostID: parent.HostID,
		ParentSessionID: parent.SessionID, ParentProduct: parent.Entrypoint,
		ParentInstanceID: parent.InstanceID, ParentPermissionMode: parent.PermissionMode,
		ParentGroups: sortedUniqueRemoteLane(parent.Groups), Groups: sortedUniqueRemoteLane(envelope.Groups),
		InheritParentGroups: envelope.InheritParentGroups, AllowDuplicateName: envelope.AllowDuplicateName,
		PermissionMode: envelope.PermissionMode,
		InputReference: cloneRemoteLaneMap(envelope.InputReference),
	}
	fingerprint, err := remoteLaneFingerprint(request)
	return request, fingerprint, err
}

func validRemoteLanePermission(product, mode string) bool {
	switch product {
	case "codex", "grok":
		return mode == "default" || mode == "bypassPermissions"
	case "claude":
		switch mode {
		case "default", "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan":
			return true
		}
	case "qwen":
		switch mode {
		case "default", "bypassPermissions", "yolo", "plan", "auto", "accept_edits":
			return true
		}
	}
	return false
}

func remoteLaneTerminalOutcome(outcome string) bool {
	switch outcome {
	case "completed", "failed", "interrupted", "timed_out":
		return true
	default:
		return false
	}
}

func remoteLaneNoticeID(result RemoteLaneResult) string {
	digest := sha256.Sum256([]byte(result.RequestID + "\x00" + result.LaneSessionID + "\x00" + result.TurnID + "\x00" + result.Outcome))
	return "remote-lane-" + hex.EncodeToString(digest[:12])
}

func remoteLaneFingerprint(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode remote lane idempotency input: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func sortedUniqueRemoteLane(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneRemoteLaneRequest(request RemoteLaneRequest) RemoteLaneRequest {
	request.ParentGroups = append([]string(nil), request.ParentGroups...)
	request.Groups = append([]string(nil), request.Groups...)
	request.InputReference = cloneRemoteLaneMap(request.InputReference)
	return request
}

func cloneRemoteLaneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		switch typed := value.(type) {
		case map[string]any:
			result[key] = cloneRemoteLaneMap(typed)
		case []any:
			result[key] = append([]any(nil), typed...)
		case []string:
			result[key] = append([]string(nil), typed...)
		default:
			result[key] = value
		}
	}
	return result
}
