package productruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/procinfo"
)

type EnvVar struct {
	Name  string
	Value string
}

// SensitiveValue is transient launch material. Formatting and JSON encoding
// never reveal it; native launch code must call Reveal deliberately.
type SensitiveValue struct{ value string }

func NewSensitiveValue(value string) SensitiveValue { return SensitiveValue{value: value} }
func (SensitiveValue) String() string               { return "[REDACTED]" }
func (SensitiveValue) GoString() string             { return "[REDACTED]" }
func (s SensitiveValue) Empty() bool                { return s.value == "" }
func (s SensitiveValue) Reveal() string             { return s.value }
func (SensitiveValue) MarshalJSON() ([]byte, error) {
	return nil, errors.New("sensitive value cannot be serialized")
}

type SensitiveEnvVar struct {
	Name  string
	Value SensitiveValue
}

const maxRedactedDetailBytes = 4096

var (
	bearerCredentialPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	namedCredentialPattern  = regexp.MustCompile(`(?i)(\b(?:api[-_]?key|authorization|password|secret|token)\b\s*[:=]\s*)[^\s,;&]+`)
)

// RedactedString contains bounded operator detail after control characters and
// every explicitly supplied sensitive value have been removed. Its private
// representation prevents product adapters from bypassing construction with a
// raw type conversion.
type RedactedString struct{ value string }

// NewRedactedString constructs diagnostic text safe for JSON and log output.
// Callers pass every credential that may occur in native output; empty secrets
// are ignored. The result is a single bounded line.
func NewRedactedString(detail string, sensitive ...SensitiveValue) RedactedString {
	for _, value := range sensitive {
		if !value.Empty() {
			detail = strings.ReplaceAll(detail, value.Reveal(), "[REDACTED]")
		}
	}
	detail = bearerCredentialPattern.ReplaceAllString(detail, "Bearer [REDACTED]")
	detail = namedCredentialPattern.ReplaceAllString(detail, "${1}[REDACTED]")
	detail = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, detail)
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > maxRedactedDetailBytes {
		detail = detail[:maxRedactedDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return RedactedString{value: detail}
}

func (r RedactedString) String() string { return r.value }
func (r RedactedString) Empty() bool    { return r.value == "" }
func (r RedactedString) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.value)
}

type PeerLaunchRequest struct {
	ProductID    string
	AttachmentID string
	Cwd          string
	Args         []string
	Env          []EnvVar
}

type NativeCommand struct {
	Path         string
	Args         []string
	Env          []EnvVar
	SensitiveEnv []SensitiveEnvVar
	Cwd          string
}

func (c NativeCommand) String() string { return c.Path + " [arguments and environment redacted]" }
func (NativeCommand) MarshalJSON() ([]byte, error) {
	return nil, errors.New("native command is transient and cannot be serialized")
}

type NativeName struct {
	Applied         string
	NativeConfirmed bool
}

type NativeAcceptance struct {
	NativeSessionID string
	NativeMessageID string
	AcceptedAt      time.Time
}

type LaneCapabilitySet struct {
	Steer                   bool `json:"steer"`
	DurableResume           bool `json:"durable_resume"`
	CallerSuppliedSessionID bool `json:"caller_supplied_session_id"`
	Model                   bool `json:"model"`
	ReasoningEffort         bool `json:"reasoning_effort"`
	Agent                   bool `json:"agent"`
	ToolPolicy              bool `json:"tool_policy"`
	OutputSchema            bool `json:"output_schema"`
	Sandbox                 bool `json:"sandbox"`
	PermissionDefault       bool `json:"permission_default"`
	PermissionBypass        bool `json:"permission_bypass"`
}

type LaneExtraArgument struct {
	Name        string `json:"name"`
	TakesValue  bool   `json:"takes_value"`
	Cardinality string `json:"cardinality"`
	Description string `json:"description"`
}

type LaneReadiness struct {
	Available     bool   `json:"available"`
	NativePath    string `json:"native_path"`
	NativeVersion string `json:"native_version"`
	Error         string `json:"error,omitempty"`
}

// WireInteger accepts a parsed finite integer in the signed 64-bit range.
// The lexical and IEEE-754 rounding boundary remains a recorded follow-up.
type WireInteger int64

func (value *WireInteger) UnmarshalJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var raw any
	if decoder.Decode(&raw) != nil {
		return errors.New("wire integer is invalid")
	}
	number, ok := raw.(json.Number)
	parsed, err := number.Float64()
	if !ok || err != nil || !wireInteger(parsed) {
		return errors.New("wire integer is invalid")
	}
	*value = WireInteger(parsed)
	return nil
}

type LaneWorkerHello struct {
	Protocol       WireInteger         `json:"protocol"`
	LaunchToken    string              `json:"launch_token"`
	Product        string              `json:"product"`
	Capabilities   LaneCapabilitySet   `json:"capabilities"`
	ExtraArguments []LaneExtraArgument `json:"extra_arguments"`
	Readiness      LaneReadiness       `json:"readiness"`
}

type LaneToolPolicy struct {
	Tools *[]string `json:"tools,omitempty"`
	Allow *[]string `json:"allow,omitempty"`
	Deny  *[]string `json:"deny,omitempty"`
}

type LaneOpenRequest struct {
	Name                    string              `json:"name"`
	Cwd                     string              `json:"cwd"`
	PermissionMode          permissionmode.Mode `json:"permission_mode"`
	Resume                  bool                `json:"resume"`
	SessionID               string              `json:"session_id,omitempty"`
	Model                   string              `json:"model,omitempty"`
	ReasoningEffort         string              `json:"reasoning_effort,omitempty"`
	Agent                   string              `json:"agent,omitempty"`
	ToolPolicy              *LaneToolPolicy     `json:"tool_policy,omitempty"`
	OutputSchema            json.RawMessage     `json:"output_schema,omitempty"`
	Sandbox                 string              `json:"sandbox,omitempty"`
	AutoArchiveAfterSeconds float64             `json:"auto_archive_after_seconds"`
	Arguments               []string            `json:"arguments"`

	// These fields are private adapter context while the product bodies move
	// behind the worker process. They never cross the worker wire.
	ProductID       string   `json:"-"`
	LaneID          string   `json:"-"`
	Groups          []string `json:"-"`
	ResumeNativeID  string   `json:"-"`
	ProfileIdentity string   `json:"-"`
	Capability      string   `json:"-"`
	Environment     []string `json:"-"`
	ApprovalPolicy  string   `json:"-"`
	Effort          string   `json:"-"`
}

type LaneDoctorResult struct {
	Type                string              `json:"type"`
	ContractVersion     WireInteger         `json:"contract_version"`
	Authority           string              `json:"authority"`
	Product             string              `json:"product"`
	Ready               bool                `json:"ready"`
	NativePath          string              `json:"native_path"`
	NativeVersion       string              `json:"native_version"`
	RuntimePath         string              `json:"runtime_path"`
	DaemonReachable     bool                `json:"daemon_reachable"`
	SupervisorReachable bool                `json:"supervisor_reachable"`
	ReadinessError      string              `json:"readiness_error,omitempty"`
	Capabilities        LaneCapabilitySet   `json:"capabilities"`
	ExtraArguments      []LaneExtraArgument `json:"extra_arguments"`
}

type LaneStatusProjection struct {
	Name          string      `json:"name"`
	State         string      `json:"state"`
	TurnID        string      `json:"turn_id"`
	Outcome       string      `json:"outcome"`
	AutoArchiveAt WireInteger `json:"auto_archive_at"`
}

func (p LaneStatusProjection) Valid() bool {
	terminal := p.Outcome == "completed" || p.Outcome == "interrupted" || p.Outcome == "failed" || p.Outcome == "timed_out"
	if p.Name == "" || p.AutoArchiveAt < 0 || p.AutoArchiveAt > 0 && p.State != "idle" {
		return false
	}
	switch p.State {
	case "running", "interrupting":
		return p.TurnID != "" && p.Outcome == "" && p.AutoArchiveAt == 0
	case "terminal":
		return p.TurnID != "" && terminal && p.AutoArchiveAt == 0
	case "idle":
		return p.Outcome == "" && p.TurnID == "" || terminal && p.TurnID != ""
	case "archived":
		return p.AutoArchiveAt == 0 && (p.Outcome == "" && p.TurnID == "" || terminal && p.TurnID != "")
	default:
		return false
	}
}

type NativeSessionRef struct {
	LaneID          string
	NativeSessionID string
	Generation      uint64
}

type LaneTurnStartRequest struct {
	InputID        string   `json:"input_id"`
	Body           string   `json:"body"`
	Mode           string   `json:"mode"`
	TimeoutSeconds *float64 `json:"timeout_seconds,omitempty"`

	// Private adapter context; the worker derives it from the open binding.
	Prompt         string              `json:"-"`
	PermissionMode permissionmode.Mode `json:"-"`
	Arguments      []string            `json:"-"`
	ApprovalPolicy string              `json:"-"`
	Sandbox        string              `json:"-"`
	Effort         string              `json:"-"`
	SchemaPath     string              `json:"-"`
}

type TurnStartRequest = LaneTurnStartRequest

type LaneWireSchema struct{ document map[string]any }

func ParseLaneWireSchema(body []byte) (*LaneWireSchema, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if decoder.Decode(&document) != nil || len(document) != 3 || document["$schema"] == nil || document["$id"] == nil || document["$defs"] == nil {
		return nil, errors.New("lane worker schema document is invalid")
	}
	schema := &LaneWireSchema{document: document}
	if !schema.keywords(document["$defs"], true) {
		return nil, errors.New("lane worker schema contains an unsupported keyword")
	}
	return schema, nil
}

func (s *LaneWireSchema) Decode(name string, body []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || !s.valid(s.definition(name), value) {
		return fmt.Errorf("%w: %s does not match the lane worker schema", ErrProtocol, name)
	}
	return decodeClosed(body, output)
}

func (s *LaneWireSchema) definition(name string) map[string]any {
	definitions, _ := s.document["$defs"].(map[string]any)
	definition, _ := definitions[name].(map[string]any)
	return definition
}

func (s *LaneWireSchema) valid(node map[string]any, value any) bool {
	if node == nil {
		return false
	}
	if ref, ok := node["$ref"].(string); ok {
		resolved := s.document
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			resolved, _ = resolved[part].(map[string]any)
		}
		return s.valid(resolved, value)
	}
	if constant, ok := node["const"]; ok && !equalJSON(constant, value) {
		return false
	}
	if options, ok := node["enum"].([]any); ok && !slices.ContainsFunc(options, func(option any) bool { return equalJSON(option, value) }) {
		return false
	}
	typeName, _ := node["type"].(string)
	if typeName == "object" || node["properties"] != nil || node["required"] != nil || node["additionalProperties"] != nil {
		object, ok := value.(map[string]any)
		properties, _ := node["properties"].(map[string]any)
		if !ok || !requiredProperties(object, node["required"]) || node["additionalProperties"] == false && unknownProperty(object, properties) || len(object) < int(number(node["minProperties"])) {
			return false
		}
		for key, item := range object {
			if child, ok := properties[key].(map[string]any); ok && !s.valid(child, item) {
				return false
			}
		}
	} else if typeName == "array" {
		array, ok := value.([]any)
		items, _ := node["items"].(map[string]any)
		if !ok || slices.ContainsFunc(array, func(item any) bool { return !s.valid(items, item) }) || node["uniqueItems"] == true && !uniqueJSON(array) {
			return false
		}
	} else if typeName == "string" {
		text, ok := value.(string)
		if !ok || len([]rune(text)) < int(number(node["minLength"])) {
			return false
		}
	} else if typeName == "boolean" {
		if _, ok := value.(bool); !ok {
			return false
		}
	} else if typeName == "number" || typeName == "integer" {
		numeric, ok := value.(json.Number)
		parsed, err := numeric.Float64()
		if !ok || err != nil || typeName == "integer" && !wireInteger(parsed) {
			return false
		}
	}
	numeric, numericOK := value.(json.Number)
	actual, _ := numeric.Float64()
	if numericOK && (node["minimum"] != nil && actual < number(node["minimum"]) || node["exclusiveMinimum"] != nil && actual <= number(node["exclusiveMinimum"])) {
		return false
	}
	if all, ok := node["allOf"].([]any); ok && slices.ContainsFunc(all, func(item any) bool { child, _ := item.(map[string]any); return !s.valid(child, value) }) {
		return false
	}
	if condition, ok := node["if"].(map[string]any); ok {
		branch := "else"
		if s.valid(condition, value) {
			branch = "then"
		}
		if child, ok := node[branch].(map[string]any); ok && !s.valid(child, value) {
			return false
		}
	}
	if child, ok := node["not"].(map[string]any); ok && s.valid(child, value) {
		return false
	}
	return true
}

func (s *LaneWireSchema) keywords(value any, definitions bool) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if definitions {
		return !slices.ContainsFunc(mapsValues(object), func(child any) bool { return !s.keywords(child, false) })
	}
	allowed := map[string]bool{"$ref": true, "const": true, "enum": true, "type": true, "required": true, "additionalProperties": true, "minProperties": true, "properties": true, "items": true, "uniqueItems": true, "minLength": true, "minimum": true, "exclusiveMinimum": true, "allOf": true, "if": true, "then": true, "else": true, "not": true}
	for key, item := range object {
		if key == "properties" {
			children, ok := item.(map[string]any)
			if !ok || slices.ContainsFunc(mapsValues(children), func(child any) bool { return !s.keywords(child, false) }) {
				return false
			}
			continue
		}
		if !allowed[key] {
			return false
		}
		if key == "type" {
			typeName, ok := item.(string)
			if !ok || !slices.Contains([]string{"object", "array", "string", "boolean", "number", "integer"}, typeName) {
				return false
			}
		}
		if key == "items" || key == "if" || key == "then" || key == "else" || key == "not" {
			if !s.keywords(item, false) {
				return false
			}
		} else if key == "allOf" {
			children, ok := item.([]any)
			if !ok || slices.ContainsFunc(children, func(child any) bool { return !s.keywords(child, false) }) {
				return false
			}
		}
	}
	return true
}

func requiredProperties(object map[string]any, value any) bool {
	required, _ := value.([]any)
	return !slices.ContainsFunc(required, func(item any) bool { name, ok := item.(string); _, present := object[name]; return !ok || !present })
}

func unknownProperty(object, properties map[string]any) bool {
	for key := range object {
		if properties[key] == nil {
			return true
		}
	}
	return false
}

func mapsValues(object map[string]any) []any {
	values := make([]any, 0, len(object))
	for _, value := range object {
		values = append(values, value)
	}
	return values
}

func number(value any) float64 {
	numeric, _ := value.(json.Number)
	result, _ := numeric.Float64()
	return result
}

func uniqueJSON(values []any) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		body, _ := json.Marshal(value)
		if seen[string(body)] {
			return false
		}
		seen[string(body)] = true
	}
	return true
}

func equalJSON(left, right any) bool {
	leftNumber, leftNumeric := left.(json.Number)
	rightNumber, rightNumeric := right.(json.Number)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric {
			return false
		}
		leftValue, leftErr := leftNumber.Float64()
		rightValue, rightErr := rightNumber.Float64()
		return leftErr == nil && rightErr == nil && leftValue == rightValue
	}
	return reflect.DeepEqual(left, right)
}

func wireInteger(value float64) bool {
	const int64Limit = float64(uint64(1) << 63)
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value && value >= -int64Limit && value < int64Limit
}

func decodeClosed(body []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", ErrProtocol)
	}
	return nil
}

// DecodeClosed decodes one closed private-wire value after its schema or
// method-specific validator has accepted the raw JSON.
func DecodeClosed(body []byte, output any) error { return decodeClosed(body, output) }

type NativeTurnRef struct {
	NativeSessionRef
	NativeTurnID string
}

type TurnOutcome string

const (
	TurnCompleted   TurnOutcome = "completed"
	TurnInterrupted TurnOutcome = "interrupted"
	TurnFailed      TurnOutcome = "failed"
	TurnTimedOut    TurnOutcome = "timed_out"
)

type NativeTerminal struct {
	Outcome          TurnOutcome
	ExitLike         int
	Result           string
	ResultDigest     [32]byte
	NativeStopReason string
}

type ProbeDepth string

const (
	ProbePresence    ProbeDepth = "presence"
	ProbeVersion     ProbeDepth = "version"
	ProbeFeature     ProbeDepth = "feature"
	ProbeIntegration ProbeDepth = "integration"
)

type ProbeState string

const (
	ProbeReady        ProbeState = "ready"
	ProbeMissing      ProbeState = "missing"
	ProbeIncompatible ProbeState = "incompatible"
	ProbeUnconfigured ProbeState = "unconfigured"
	ProbeError        ProbeState = "error"
)

type ProbeRequest struct {
	ProductID      string
	ExecutablePath string
	Depth          ProbeDepth
}

type ProbeReport struct {
	State         ProbeState
	NativeVersion string
	Features      map[string]bool
	TupleOK       *bool
	Detail        RedactedString
}

type ProcessSignal string

const (
	ProcessInterrupt ProcessSignal = "interrupt"
	ProcessTerminate ProcessSignal = "terminate"
	ProcessKill      ProcessSignal = "kill"
)

// OwnedProcessRef is an ephemeral exact owned child/process-group reference.
// It contains no native protocol endpoint or credential.
type OwnedProcessRef struct {
	Process      procinfo.Identity
	ProcessGroup int
}

type ProcessExit struct {
	ExitLike int
	Signal   string
}

// OwnedProcessSupervisor owns native child creation and bounded termination.
// Structured protocol I/O remains in the structuredprocess client above this
// process-lifetime capability.
type OwnedProcessSupervisor interface {
	Start(context.Context, NativeCommand) (OwnedProcessRef, error)
	Signal(context.Context, OwnedProcessRef, ProcessSignal) error
	Wait(context.Context, OwnedProcessRef) (ProcessExit, error)
}

// HostDeps contains only product-neutral ephemeral host capabilities.
type HostDeps struct {
	Generation     uint64
	OwnedProcesses OwnedProcessSupervisor
	Now            func() time.Time
}

// Compile-time assertions guard accidental serialization of secret-bearing
// transient records while keeping ordinary records JSON-compatible.
var _ json.Marshaler = SensitiveValue{}
var _ json.Marshaler = NativeCommand{}
var _ json.Marshaler = RedactedString{}
