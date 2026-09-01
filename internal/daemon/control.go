package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const maxControlFrameBytes uint32 = 1 << 20

// ControlRole identifies one least-privilege local client class.
type ControlRole string

const (
	// RoleAdmin permits read-only same-user operator metadata.
	RoleAdmin ControlRole = "admin"
	// RoleLauncher permits attachment and lane transactions.
	RoleLauncher ControlRole = "launcher"
	// RoleHook permits exact managed lifecycle events.
	RoleHook ControlRole = "hook"
	// RoleConnector permits MCP initialization, discovery, and attested calls.
	RoleConnector ControlRole = "connector"
)

const (
	// ErrorInvalidRequest identifies malformed, disallowed, or incomplete calls.
	ErrorInvalidRequest = "invalid_request"
	// ErrorForbidden identifies a role-policy violation.
	ErrorForbidden = "forbidden"
	// ErrorStaleGeneration identifies a request for a different daemon generation.
	ErrorStaleGeneration = "stale_generation"
	// ErrorIdempotencyConflict identifies changed reuse of a mutation key.
	ErrorIdempotencyConflict = "idempotency_conflict"
	// ErrorInactive identifies a connector outside an attested managed attachment.
	ErrorInactive = "inactive"
	// ErrorHandler identifies a cause-specific operation failure.
	ErrorHandler = "operation_failed"

	// CanonicalInactiveMessage is returned to bare product MCP calls.
	CanonicalInactiveMessage = "agent_sessions is inactive outside an attested peer session"
)

// ControlRequest is one bounded correlated local operation.
type ControlRequest struct {
	ID             string          `json:"id"`
	Role           ControlRole     `json:"role"`
	Operation      string          `json:"operation"`
	Generation     uint64          `json:"generation"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	AttachmentID   string          `json:"attachment_id,omitempty"`
	Capability     string          `json:"capability,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// ControlFailure is one metadata-only operation failure.
type ControlFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ControlResponse is one correlated local result.
type ControlResponse struct {
	ID         string          `json:"id"`
	Generation uint64          `json:"generation"`
	OK         bool            `json:"ok"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Error      *ControlFailure `json:"error,omitempty"`
}

// ControlHandler executes one already-framed, authorized operation.
type ControlHandler func(context.Context, ControlRequest) (json.RawMessage, error)

type controlMutationCacheKey struct {
	role           ControlRole
	idempotencyKey string
}

type cachedControlResponse struct {
	digest   [sha256.Size]byte
	response ControlResponse
}

type inFlightControlResponse struct {
	digest   [sha256.Size]byte
	done     chan struct{}
	response ControlResponse
}

type controlPolicy struct {
	generation uint64
	handler    ControlHandler
	mu         sync.Mutex
	cache      map[controlMutationCacheKey]cachedControlResponse
	inFlight   map[controlMutationCacheKey]*inFlightControlResponse
}

type classifiedControlError struct{ code string }

func (e classifiedControlError) Error() string { return e.code }

// InactiveControlError returns an opaque classified error whose public text is
// always the canonical bare/inexact-attestation result.
func InactiveControlError() error { return classifiedControlError{code: ErrorInactive} }

func (p *controlPolicy) handle(ctx context.Context, request ControlRequest) ControlResponse {
	if failure := p.validate(request); failure != nil {
		return failedControlResponse(request.ID, failure.Code, failure.Message)
	}
	if request.Role == RoleConnector && request.Operation == "connector.call" &&
		strings.TrimSpace(request.AttachmentID) == "" {
		return failedControlResponse(request.ID, ErrorInactive, CanonicalInactiveMessage)
	}
	if !controlMutation(request) {
		return p.invoke(ctx, request)
	}

	digest, err := controlRequestDigest(request)
	if err != nil {
		return failedControlResponse(request.ID, ErrorInvalidRequest, err.Error())
	}
	cacheKey := controlMutationKey(request)
	p.mu.Lock()
	if cached, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		if cached.digest != digest {
			return failedControlResponse(request.ID, ErrorIdempotencyConflict, "idempotency key was reused for a different request")
		}
		return correlatedControlResponse(cached.response, request.ID)
	}
	if active, ok := p.inFlight[cacheKey]; ok {
		if active.digest != digest {
			p.mu.Unlock()
			return failedControlResponse(request.ID, ErrorIdempotencyConflict, "idempotency key was reused for a different request")
		}
		p.mu.Unlock()
		select {
		case <-active.done:
			return correlatedControlResponse(active.response, request.ID)
		case <-ctx.Done():
			return failedControlResponse(request.ID, ErrorHandler, "control operation cancelled")
		}
	}
	if p.inFlight == nil {
		p.inFlight = map[controlMutationCacheKey]*inFlightControlResponse{}
	}
	active := &inFlightControlResponse{digest: digest, done: make(chan struct{})}
	p.inFlight[cacheKey] = active
	p.mu.Unlock()

	response := p.invoke(ctx, request)
	response.ID = ""
	p.mu.Lock()
	p.cache[cacheKey] = cachedControlResponse{digest: digest, response: response}
	active.response = response
	delete(p.inFlight, cacheKey)
	close(active.done)
	p.mu.Unlock()
	return correlatedControlResponse(response, request.ID)
}

func controlMutationKey(request ControlRequest) controlMutationCacheKey {
	return controlMutationCacheKey{
		role: request.Role, idempotencyKey: request.IdempotencyKey,
	}
}

func correlatedControlResponse(response ControlResponse, requestID string) ControlResponse {
	response.ID = requestID
	response.Payload = append(json.RawMessage(nil), response.Payload...)
	return response
}

func (p *controlPolicy) validate(request ControlRequest) *ControlFailure {
	if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Operation) == "" {
		return &ControlFailure{Code: ErrorInvalidRequest, Message: "control request identity is incomplete"}
	}
	if request.Generation != p.generation {
		return &ControlFailure{Code: ErrorStaleGeneration, Message: "control request daemon generation is stale"}
	}
	if lifecycleOperation(request.Operation) {
		return &ControlFailure{Code: ErrorForbidden, Message: "service lifetime is managed only by the user service manager"}
	}
	if !roleAllowsOperation(request.Role, request.Operation) {
		return &ControlFailure{Code: ErrorForbidden, Message: "control role does not permit this operation"}
	}
	if len(request.Payload) != 0 && !json.Valid(request.Payload) {
		return &ControlFailure{Code: ErrorInvalidRequest, Message: "control request payload is invalid JSON"}
	}
	if controlMutation(request) && strings.TrimSpace(request.IdempotencyKey) == "" {
		return &ControlFailure{Code: ErrorInvalidRequest, Message: "mutating control request requires an idempotency key"}
	}
	return nil
}

func (p *controlPolicy) invoke(ctx context.Context, request ControlRequest) ControlResponse {
	if p.handler == nil {
		return failedControlResponse(request.ID, ErrorHandler, "control operation handler is unavailable")
	}
	payload, err := p.handler(ctx, request)
	if err != nil {
		var classified classifiedControlError
		if errors.As(err, &classified) && classified.code == ErrorInactive {
			return failedControlResponse(request.ID, ErrorInactive, CanonicalInactiveMessage)
		}
		// Product callbacks may receive prompts, results, transcript fragments,
		// or vendor diagnostics. Never echo an arbitrary callback error across
		// the metadata-only control failure surface.
		return failedControlResponse(request.ID, ErrorHandler, "control operation failed")
	}
	return ControlResponse{ID: request.ID, OK: true, Payload: append(json.RawMessage(nil), payload...)}
}

func roleAllowsOperation(role ControlRole, operation string) bool {
	switch role {
	case RoleAdmin:
		return operation == "status" || operation == "doctor" || operation == "roster"
	case RoleLauncher:
		return strings.HasPrefix(operation, "attachment.") || strings.HasPrefix(operation, "lane.")
	case RoleHook:
		return operation == "hook.event"
	case RoleConnector:
		return operation == "connector.initialize" || operation == "connector.tools" || operation == "connector.call"
	default:
		return false
	}
}

func controlMutation(request ControlRequest) bool {
	return request.Role != RoleAdmin && (request.Role != RoleConnector ||
		request.Operation != "connector.initialize" && request.Operation != "connector.tools")
}

func lifecycleOperation(operation string) bool {
	switch operation {
	case "start", "stop", "restart", "shutdown", "service.start", "service.stop", "service.restart", "service.shutdown":
		return true
	default:
		return false
	}
}

func controlRequestDigest(request ControlRequest) ([sha256.Size]byte, error) {
	request.ID = ""
	request.IdempotencyKey = ""
	body, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode idempotent control request: %w", err)
	}
	return sha256.Sum256(body), nil
}

func failedControlResponse(id, code, message string) ControlResponse {
	return ControlResponse{ID: id, Error: &ControlFailure{Code: code, Message: message}}
}

func writeControlFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control frame: %w", err)
	}
	if len(body) == 0 || uint64(len(body)) > uint64(maxControlFrameBytes) {
		return errors.New("control frame exceeds configured bound")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body))) //nolint:gosec // bounded by maxControlFrameBytes above.
	if _, err := io.CopyN(writer, bytes.NewReader(header[:]), int64(len(header))); err != nil {
		return fmt.Errorf("write control frame header: %w", err)
	}
	if _, err := io.CopyN(writer, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("write control frame body: %w", err)
	}
	return nil
}

func readControlFrame(reader io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read control frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxControlFrameBytes {
		return errors.New("control frame size is invalid")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return fmt.Errorf("read control frame body: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode control frame: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("control frame contains trailing data")
	}
	return nil
}
