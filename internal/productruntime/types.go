package productruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
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

type LaneWorkerHello struct {
	Protocol       int                 `json:"protocol"`
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
	ContractVersion     int                 `json:"contract_version"`
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
	Name          string `json:"name"`
	State         string `json:"state"`
	TurnID        string `json:"turn_id"`
	Outcome       string `json:"outcome"`
	AutoArchiveAt int64  `json:"auto_archive_at"`
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

func DecodeLaneOpenRequest(body []byte) (LaneOpenRequest, error) {
	var request LaneOpenRequest
	if err := decodeClosed(body, &request); err != nil {
		return LaneOpenRequest{}, err
	}
	if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.Cwd) == "" ||
		!request.PermissionMode.Valid() || request.Arguments == nil || request.AutoArchiveAfterSeconds < 0 ||
		math.IsInf(request.AutoArchiveAfterSeconds, 0) || math.IsNaN(request.AutoArchiveAfterSeconds) ||
		request.Resume != (request.SessionID != "") || request.OutputSchema != nil && !jsonObject(request.OutputSchema) ||
		request.ToolPolicy != nil && !validToolPolicy(*request.ToolPolicy) {
		return LaneOpenRequest{}, fmt.Errorf("%w: lane open request is invalid", ErrProtocol)
	}
	return request, nil
}

func DecodeLaneWorkerHello(body []byte) (LaneWorkerHello, error) {
	var hello LaneWorkerHello
	if err := decodeClosed(body, &hello); err != nil {
		return LaneWorkerHello{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || !hasJSONFields(fields["capabilities"],
		"steer", "durable_resume", "caller_supplied_session_id", "model", "reasoning_effort", "agent", "tool_policy",
		"output_schema", "sandbox", "permission_default", "permission_bypass") {
		return LaneWorkerHello{}, fmt.Errorf("%w: lane worker capabilities are incomplete", ErrProtocol)
	}
	if hello.Protocol != 1 || strings.TrimSpace(hello.LaunchToken) == "" || strings.TrimSpace(hello.Product) == "" ||
		hello.ExtraArguments == nil || strings.TrimSpace(hello.Readiness.NativePath) == "" || strings.TrimSpace(hello.Readiness.NativeVersion) == "" {
		return LaneWorkerHello{}, fmt.Errorf("%w: lane worker hello is invalid", ErrProtocol)
	}
	seen := make(map[string]bool, len(hello.ExtraArguments))
	for _, rule := range hello.ExtraArguments {
		if strings.TrimSpace(rule.Name) == "" || strings.TrimSpace(rule.Description) == "" ||
			(rule.Cardinality != "zero-or-one" && rule.Cardinality != "zero-or-more") || seen[rule.Name] {
			return LaneWorkerHello{}, fmt.Errorf("%w: lane worker extra argument is invalid", ErrProtocol)
		}
		seen[rule.Name] = true
	}
	return hello, nil
}

func hasJSONFields(body json.RawMessage, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if fields[name] == nil {
			return false
		}
	}
	return true
}

func DecodeLaneDoctorResult(body []byte) (LaneDoctorResult, error) {
	var result LaneDoctorResult
	if err := decodeClosed(body, &result); err != nil {
		return LaneDoctorResult{}, err
	}
	if result.Type != "lane.doctor" || result.ContractVersion != 2 || result.Authority != "daemon" ||
		strings.TrimSpace(result.Product) == "" || strings.TrimSpace(result.NativePath) == "" ||
		strings.TrimSpace(result.NativeVersion) == "" || strings.TrimSpace(result.RuntimePath) == "" || result.ExtraArguments == nil {
		return LaneDoctorResult{}, fmt.Errorf("%w: lane doctor result is invalid", ErrProtocol)
	}
	return result, nil
}

func DecodeLaneTurnStartRequest(body []byte) (LaneTurnStartRequest, error) {
	var request LaneTurnStartRequest
	if err := decodeClosed(body, &request); err != nil {
		return LaneTurnStartRequest{}, err
	}
	if strings.TrimSpace(request.InputID) == "" || strings.TrimSpace(request.Body) == "" ||
		(request.Mode != "followup" && request.Mode != "steer") || request.TimeoutSeconds != nil &&
		(*request.TimeoutSeconds <= 0 || math.IsInf(*request.TimeoutSeconds, 0) || math.IsNaN(*request.TimeoutSeconds)) {
		return LaneTurnStartRequest{}, fmt.Errorf("%w: lane turn start request is invalid", ErrProtocol)
	}
	return request, nil
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

func jsonObject(body json.RawMessage) bool {
	var value map[string]any
	return decodeClosed(body, &value) == nil && value != nil
}

func validToolPolicy(policy LaneToolPolicy) bool {
	if policy.Tools == nil && policy.Allow == nil && policy.Deny == nil {
		return false
	}
	for _, values := range []*[]string{policy.Tools, policy.Allow, policy.Deny} {
		if values == nil {
			continue
		}
		seen := make(map[string]bool, len(*values))
		for _, value := range *values {
			if strings.TrimSpace(value) == "" || seen[value] {
				return false
			}
			seen[value] = true
		}
	}
	return true
}

type NativeTurnRef struct {
	NativeSessionRef
	NativeTurnID string
}

type TurnOutcome string

const (
	TurnCompleted   TurnOutcome = "completed"
	TurnInterrupted TurnOutcome = "interrupted"
	TurnFailed      TurnOutcome = "failed"
	TurnTimedOut    TurnOutcome = "timed-out"
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
