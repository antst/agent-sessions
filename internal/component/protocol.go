// Package component implements managed component protocol v1 without owning
// durable daemon lifecycle state or product-specific native semantics.
package component

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolVersion = 1
	RedactedValue   = "<redacted>"
	maxIdentifier   = 256
	maxDetailBytes  = 512
)

// FrameType is one protocol-v1 operation.
type FrameType string

const (
	TypeBootstrap        FrameType = "bootstrap"
	TypeReady            FrameType = "ready"
	TypeReconnect        FrameType = "reconnect"
	TypeSessionAnnounce  FrameType = "session.announce"
	TypeSessionRebind    FrameType = "session.rebind"
	TypeSessionRename    FrameType = "session.rename"
	TypeSessionState     FrameType = "session.state"
	TypeSessionClose     FrameType = "session.close"
	TypeSessionBound     FrameType = "session.bound"
	TypeDeliveryPresent  FrameType = "delivery.present"
	TypeDeliveryAccept   FrameType = "delivery.accept"
	TypeDeliveryReject   FrameType = "delivery.reject"
	TypeTurnEvent        FrameType = "turn.event"
	TypeToolCall         FrameType = "tool.call"
	TypeToolCancel       FrameType = "tool.cancel"
	TypeToolResult       FrameType = "tool.result"
	TypeGenerationRetire FrameType = "generation.retire"
	TypeHeartbeat        FrameType = "heartbeat"
	TypeHeartbeatAck     FrameType = "heartbeat.ack"
	TypeReject           FrameType = "reject"
)

var knownFrameTypes = map[FrameType]struct{}{
	TypeBootstrap: {}, TypeReady: {}, TypeReconnect: {}, TypeSessionAnnounce: {},
	TypeSessionRebind: {}, TypeSessionRename: {}, TypeSessionState: {}, TypeSessionClose: {},
	TypeSessionBound: {}, TypeDeliveryPresent: {}, TypeDeliveryAccept: {}, TypeDeliveryReject: {},
	TypeTurnEvent: {}, TypeToolCall: {}, TypeToolCancel: {}, TypeToolResult: {},
	TypeGenerationRetire: {}, TypeHeartbeat: {}, TypeHeartbeatAck: {}, TypeReject: {},
}

// Category is a stable rejection class. Detail is always non-authoritative and
// redacted before it crosses the component boundary.
type Category string

const (
	CategoryInvalidFrame       Category = "invalid-frame"
	CategoryUnsupportedVersion Category = "unsupported-version"
	CategoryUnknownType        Category = "unknown-type"
	CategoryProtocol           Category = "protocol"
	CategoryUnauthorized       Category = "unauthorized"
	CategoryStaleProcess       Category = "stale-process"
	CategoryReplay             Category = "replay"
	CategorySequenceGap        Category = "sequence-gap"
	CategoryTooManyOutstanding Category = "too-many-outstanding"
	CategoryInternal           Category = "internal"
)

// ProtocolError carries a machine category and bounded diagnostic.
type ProtocolError struct {
	Category Category
	Detail   string
}

func (e *ProtocolError) Error() string {
	if e.Detail == "" {
		return string(e.Category)
	}
	return string(e.Category) + ": " + e.Detail
}

func protocolError(category Category, format string, arguments ...any) error {
	return &ProtocolError{Category: category, Detail: Redact(fmt.Sprintf(format, arguments...))}
}

// Frame is the additive protocol-v1 envelope. Authority is derived only from
// the known payload fields decoded for its known Type.
type Frame struct {
	Version int             `json:"version"`
	Type    FrameType       `json:"type"`
	ID      string          `json:"id"`
	Seq     uint64          `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

// NewFrame constructs an envelope. EncodeFrame and ValidatePayload enforce
// protocol constraints at the boundary.
func NewFrame(frameType FrameType, id string, seq uint64, payload any) (Frame, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, fmt.Errorf("encode component payload: %w", err)
	}
	return Frame{Version: ProtocolVersion, Type: frameType, ID: id, Seq: seq, Payload: body}, nil
}

// EncodeFrame encodes one known valid v1 frame as a JSON object. Local
// transport adds the length prefix.
func EncodeFrame(frame Frame) ([]byte, error) {
	if err := validateEnvelope(frame); err != nil {
		return nil, err
	}
	if err := ValidatePayload(frame); err != nil {
		return nil, err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("encode component frame: %w", err)
	}
	return body, nil
}

// DecodeFrame decodes the additive v1 envelope. Unknown frame types and
// versions fail before any payload field can be treated as authority.
func DecodeFrame(body []byte) (Frame, error) {
	var frame Frame
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, protocolError(CategoryInvalidFrame, "decode envelope: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Frame{}, protocolError(CategoryInvalidFrame, "trailing JSON value")
	}
	if err := validateEnvelope(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func validateEnvelope(frame Frame) error {
	if frame.Version != ProtocolVersion {
		return protocolError(CategoryUnsupportedVersion, "component protocol version %d is unsupported", frame.Version)
	}
	if _, ok := knownFrameTypes[frame.Type]; !ok {
		return protocolError(CategoryUnknownType, "component frame type %q is unsupported", frame.Type)
	}
	if !validIdentifier(frame.ID) {
		return protocolError(CategoryInvalidFrame, "frame id is missing or invalid")
	}
	if frame.Seq == 0 {
		return protocolError(CategoryInvalidFrame, "frame sequence must be positive")
	}
	if len(frame.Payload) == 0 || !json.Valid(frame.Payload) || bytes.Equal(bytes.TrimSpace(frame.Payload), []byte("null")) {
		return protocolError(CategoryInvalidFrame, "frame payload must be a JSON object")
	}
	trimmed := bytes.TrimSpace(frame.Payload)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return protocolError(CategoryInvalidFrame, "frame payload must be a JSON object")
	}
	return nil
}

// PayloadInto decodes a known payload while permitting protocol-v1 additive
// fields. Call ValidatePayload before using required authority fields.
func (f Frame) PayloadInto(value any) error {
	decoder := json.NewDecoder(bytes.NewReader(f.Payload))
	if err := decoder.Decode(value); err != nil {
		return protocolError(CategoryInvalidFrame, "decode %s payload: %v", f.Type, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return protocolError(CategoryInvalidFrame, "trailing %s payload", f.Type)
	}
	return nil
}

// BootstrapClaim carries the one-time memory-only value and exact process
// start asserted on the first connection.
type BootstrapClaim struct {
	ProductID             string `json:"product_id"`
	AttachmentID          string `json:"attachment_id"`
	BootstrapCapabilityID string `json:"bootstrap_capability_id"`
	BootstrapValue        string `json:"bootstrap_value"`
	ProcessStart          string `json:"process_start"`
	StrongStart           string `json:"strong_start"`
	ComponentVersion      string `json:"component_version"`
}

type ReconnectClaim struct {
	AttachmentID    string `json:"attachment_id"`
	PriorBindingID  string `json:"prior_binding_id"`
	PriorGeneration uint64 `json:"prior_generation"`
	ProcessStart    string `json:"process_start"`
	StrongStart     string `json:"strong_start"`
	LastReceivedSeq uint64 `json:"last_received_seq"`
}

type Ready struct {
	BindingID           string `json:"binding_id"`
	AttachmentID        string `json:"attachment_id"`
	DaemonGeneration    uint64 `json:"daemon_generation"`
	ProtocolVersion     int    `json:"protocol_version"`
	MaxFrameBytes       uint32 `json:"max_frame_bytes"`
	HeartbeatIntervalMS int64  `json:"heartbeat_interval_ms"`
}

type SessionAnnounce struct {
	BindingID       string `json:"binding_id"`
	NativeSessionID string `json:"native_session_id"`
	Cwd             string `json:"cwd"`
	NativeName      string `json:"native_name"`
	ProductEventSeq uint64 `json:"product_event_seq"`
}

type SessionRebind struct {
	BindingID          string          `json:"binding_id"`
	OldNativeSessionID string          `json:"old_native_session_id"`
	NewNativeSessionID string          `json:"new_native_session_id"`
	Evidence           json.RawMessage `json:"evidence"`
	ProductEventSeq    uint64          `json:"product_event_seq"`
}

type SessionRename struct {
	NativeSessionID string `json:"native_session_id"`
	NativeName      string `json:"native_name"`
	ProductEventSeq uint64 `json:"product_event_seq"`
}

type SessionState struct {
	NativeSessionID string `json:"native_session_id"`
	State           string `json:"state"`
	ProductEventSeq uint64 `json:"product_event_seq"`
}

type SessionClose struct {
	NativeSessionID string `json:"native_session_id"`
	Reason          string `json:"reason"`
}

type SessionBound struct {
	BindingID       string `json:"binding_id"`
	AttachmentID    string `json:"attachment_id"`
	NativeSessionID string `json:"native_session_id"`
	PublicName      string `json:"public_name"`
}

type DeliveryPresent struct {
	DeliveryID string          `json:"delivery_id"`
	ReceiptID  string          `json:"receipt_id,omitempty"`
	Mode       string          `json:"mode"`
	Body       json.RawMessage `json:"body"`
}

type DeliveryAccept struct {
	DeliveryID      string `json:"delivery_id"`
	NativeSessionID string `json:"native_session_id"`
	NativeMessageID string `json:"native_message_id"`
	AcceptedAt      int64  `json:"accepted_at"`
}

type DeliveryReject struct {
	DeliveryID string   `json:"delivery_id"`
	Category   Category `json:"category"`
	Detail     string   `json:"detail,omitempty"`
}

type TurnEvent struct {
	NativeSessionID string          `json:"native_session_id"`
	EventSeq        uint64          `json:"event_seq"`
	Kind            string          `json:"kind"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

type ToolCall struct {
	CallID    string          `json:"call_id"`
	Operation string          `json:"operation"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolCancel struct {
	CallID string `json:"call_id"`
}

type ToolResult struct {
	CallID   string          `json:"call_id"`
	Success  bool            `json:"success"`
	Category Category        `json:"category,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

type GenerationRetire struct {
	BindingID  string `json:"binding_id"`
	Generation uint64 `json:"generation"`
}

type Heartbeat struct {
	BindingID       string `json:"binding_id"`
	LastReceivedSeq uint64 `json:"last_received_seq"`
}

type Reject struct {
	OperationID string   `json:"operation_id"`
	Category    Category `json:"category"`
	Detail      string   `json:"detail,omitempty"`
}

// ValidatePayload checks the required, authoritative v1 fields for a known
// frame. Additive fields remain ignored.
func ValidatePayload(frame Frame) error { //nolint:gocyclo // Explicit frame vocabulary is safer than reflection-driven authority.
	if err := validateEnvelope(frame); err != nil {
		return err
	}
	required := func(values ...string) bool {
		for _, value := range values {
			if !validIdentifier(value) {
				return false
			}
		}
		return true
	}
	invalid := func() error {
		return protocolError(CategoryInvalidFrame, "%s payload is missing required fields", frame.Type)
	}
	switch frame.Type {
	case TypeBootstrap:
		var value BootstrapClaim
		if frame.PayloadInto(&value) != nil || !validProductID(value.ProductID) || !required(value.AttachmentID, value.BootstrapCapabilityID, value.ProcessStart, value.StrongStart, value.ComponentVersion) || strings.TrimSpace(value.BootstrapValue) == "" {
			return invalid()
		}
	case TypeReconnect:
		var value ReconnectClaim
		if frame.PayloadInto(&value) != nil || !required(value.AttachmentID, value.PriorBindingID, value.ProcessStart, value.StrongStart) || value.PriorGeneration == 0 {
			return invalid()
		}
	case TypeReady:
		var value Ready
		if frame.PayloadInto(&value) != nil || !required(value.BindingID, value.AttachmentID) || value.DaemonGeneration == 0 || value.ProtocolVersion != ProtocolVersion || value.MaxFrameBytes == 0 || value.HeartbeatIntervalMS <= 0 {
			return invalid()
		}
	case TypeSessionAnnounce:
		var value SessionAnnounce
		if frame.PayloadInto(&value) != nil || !required(value.BindingID, value.NativeSessionID) || !validText(value.Cwd, 4096) || !validText(value.NativeName, 1024) || value.ProductEventSeq == 0 {
			return invalid()
		}
	case TypeSessionRebind:
		var value SessionRebind
		if frame.PayloadInto(&value) != nil || !required(value.BindingID, value.OldNativeSessionID, value.NewNativeSessionID) || value.ProductEventSeq == 0 || !validJSONObject(value.Evidence) {
			return invalid()
		}
	case TypeSessionRename:
		var value SessionRename
		if frame.PayloadInto(&value) != nil || !required(value.NativeSessionID, value.NativeName) || value.ProductEventSeq == 0 {
			return invalid()
		}
	case TypeSessionState:
		var value SessionState
		if frame.PayloadInto(&value) != nil || !required(value.NativeSessionID) || value.ProductEventSeq == 0 || value.State != "idle" && value.State != "busy" {
			return invalid()
		}
	case TypeSessionClose:
		var value SessionClose
		if frame.PayloadInto(&value) != nil || !required(value.NativeSessionID, value.Reason) {
			return invalid()
		}
	case TypeSessionBound:
		var value SessionBound
		if frame.PayloadInto(&value) != nil || !required(value.BindingID, value.AttachmentID, value.NativeSessionID, value.PublicName) {
			return invalid()
		}
	case TypeDeliveryPresent:
		var value DeliveryPresent
		if frame.PayloadInto(&value) != nil || !required(value.DeliveryID, value.Mode) || !validJSONObject(value.Body) {
			return invalid()
		}
	case TypeDeliveryAccept:
		var value DeliveryAccept
		if frame.PayloadInto(&value) != nil || !required(value.DeliveryID, value.NativeSessionID, value.NativeMessageID) || value.AcceptedAt <= 0 {
			return invalid()
		}
	case TypeDeliveryReject:
		var value DeliveryReject
		if frame.PayloadInto(&value) != nil || !required(value.DeliveryID, string(value.Category)) {
			return invalid()
		}
	case TypeTurnEvent:
		var value TurnEvent
		if frame.PayloadInto(&value) != nil || !required(value.NativeSessionID, value.Kind) || value.EventSeq == 0 || len(value.Metadata) > 0 && !validJSONObject(value.Metadata) {
			return invalid()
		}
	case TypeToolCall:
		var value ToolCall
		if frame.PayloadInto(&value) != nil || !required(value.CallID, value.Operation) || !validJSONObject(value.Arguments) {
			return invalid()
		}
	case TypeToolCancel:
		var value ToolCancel
		if frame.PayloadInto(&value) != nil || !required(value.CallID) {
			return invalid()
		}
	case TypeToolResult:
		var value ToolResult
		if frame.PayloadInto(&value) != nil || !required(value.CallID) || value.Success && len(value.Result) > 0 && !json.Valid(value.Result) || !value.Success && value.Category == "" {
			return invalid()
		}
	case TypeGenerationRetire:
		var value GenerationRetire
		if frame.PayloadInto(&value) != nil || !required(value.BindingID) || value.Generation == 0 {
			return invalid()
		}
	case TypeHeartbeat, TypeHeartbeatAck:
		var value Heartbeat
		if frame.PayloadInto(&value) != nil || !required(value.BindingID) {
			return invalid()
		}
	case TypeReject:
		var value Reject
		if frame.PayloadInto(&value) != nil || !required(value.OperationID, string(value.Category)) {
			return invalid()
		}
	default:
		return protocolError(CategoryUnknownType, "component frame type %q is unsupported", frame.Type)
	}
	return nil
}

func validJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func validIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]byte(value)) <= maxIdentifier && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validText(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len([]byte(value)) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

var productIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func validProductID(value string) bool {
	if strings.Contains(value, "--") || strings.HasSuffix(value, "-") {
		return false
	}
	return productIDPattern.MatchString(value)
}

var sensitiveAssignment = regexp.MustCompile(`(?i)(bootstrap_value|password|secret|token)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)`)

// Redact removes supplied ephemeral values and common sensitive assignments,
// then caps diagnostics without splitting UTF-8.
func Redact(detail string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, RedactedValue)
		}
	}
	detail = sensitiveAssignment.ReplaceAllString(detail, `${1}${2}`+RedactedValue)
	detail = strings.ReplaceAll(detail, "\x00", "")
	if len([]byte(detail)) <= maxDetailBytes {
		return detail
	}
	bytes := []byte(detail)
	bytes = bytes[:maxDetailBytes]
	for !utf8.Valid(bytes) {
		bytes = bytes[:len(bytes)-1]
	}
	return string(bytes)
}
