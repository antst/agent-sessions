package productruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
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

type DeliveryMode string

const (
	DeliveryIdleWake   DeliveryMode = "idle-wake"
	DeliveryBusySteer  DeliveryMode = "busy-steer"
	DeliveryBusyFollow DeliveryMode = "busy-follow-up"
)

type DeliveryRequest struct {
	DeliveryID string
	ReceiptID  string
	Mode       DeliveryMode
	Body       []byte
}

type NativeAcceptance struct {
	NativeSessionID string
	NativeMessageID string
	AcceptedAt      time.Time
}

type LaneCapabilitySet struct {
	Steer                  bool
	DurableResume          bool
	DeferredSessionBinding bool
}

type LaneOpenRequest struct {
	ProductID       string
	LaneID          string
	ResumeNativeID  string
	Cwd             string
	PermissionMode  permissionmode.Mode
	ProfileIdentity string
	Arguments       []string
}

type NativeSessionRef struct {
	LaneID          string
	NativeSessionID string
	Generation      uint64
}

type TurnStartRequest struct {
	ReceiptID      string
	PermissionMode permissionmode.Mode
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

type LaneRecoveryRequest struct {
	ProductID            string
	LaneID               string
	PriorNativeSessionID string
	PriorGeneration      uint64
}

type ConnectorAttempt struct {
	ProductID              string
	PeerCredential         localtransport.PeerIdentity
	ProcessIdentity        procinfo.Identity
	ClaimedNativeSessionID string
	ComponentBindingID     string
}

type ParentBinding struct {
	AttachmentID    string
	NativeSessionID string
	Verified        bool
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

// ReceiptReader is a narrow, bounded host capability. Drivers receive content
// only by a durable receipt ID and must verify the returned digest and length.
type ReceiptReader interface {
	OpenReceipt(receiptID string) (io.ReadCloser, int64, [32]byte, error)
}

// ProcessInspector provides exact process identity and ancestry checks without
// exposing product drivers to platform process-table mechanics.
type ProcessInspector interface {
	CaptureIdentity(context.Context, int) (procinfo.Identity, error)
	ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error)
	Executable(context.Context, procinfo.Identity) (string, error)
	DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error)
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

// ComponentSessionView and ProductServerView are secret-free observations of
// ephemeral registries. Product-specific clients remain in their drivers; the
// lookup never exposes a socket path, URL, password, or bearer token.
type ComponentSessionView struct {
	BindingID       string
	AttachmentID    string
	NativeSessionID string
	Generation      uint64
	State           string
}

type ComponentLookup interface {
	LookupComponent(context.Context, string, string) (ComponentSessionView, error)
}

type ProductServerView struct {
	ServerID        string
	ProductID       string
	NativeSessionID string
	Generation      uint64
}

type ProductServerLookup interface {
	LookupProductServer(context.Context, string, string) (ProductServerView, error)
}

type TestHookPoint struct{ value string }

func NewTestHookPoint(value string) (TestHookPoint, error) {
	if err := productcatalog.ValidateToken(value); err != nil {
		return TestHookPoint{}, err
	}
	return TestHookPoint{value: value}, nil
}

func (p TestHookPoint) String() string { return p.value }

// TestHooks is a bounded test-only checkpoint surface. Production hosts leave
// it nil. Implementations must reject unknown points instead of executing
// callbacks selected by arbitrary native input.
type TestHooks interface {
	Checkpoint(context.Context, TestHookPoint) error
}

// HostDeps contains only product-neutral ephemeral host capabilities.
type HostDeps struct {
	Generation     uint64
	HostID         string
	StateRoot      string
	RuntimeRoot    string
	Receipts       ReceiptReader
	Processes      ProcessInspector
	OwnedProcesses OwnedProcessSupervisor
	Components     ComponentLookup
	ProductServers ProductServerLookup
	Now            func() time.Time
	TestHooks      TestHooks
}

// Compile-time assertions guard accidental serialization of secret-bearing
// transient records while keeping ordinary records JSON-compatible.
var _ json.Marshaler = SensitiveValue{}
var _ json.Marshaler = NativeCommand{}
var _ json.Marshaler = RedactedString{}
