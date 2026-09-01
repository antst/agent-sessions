# Contract: Runtime Product Registry

This is the frozen target contract after all phase-0 truth gates pass. Names may
change mechanically during implementation, but authority, inputs, results, and
error semantics may not.

## 1. Registry

```go
type RuntimeProduct struct {
    Descriptor productcatalog.Descriptor
    Peer       PeerDriver
    Message    MessageDriver
    Lane       LaneDriver
    Parent     ParentAttester
    Doctor     DoctorProbe
}

type Registry interface {
    ByID(productID string) (RuntimeProduct, bool)
    All() []RuntimeProduct
}
```

The constructor accepts an explicit ordered slice from the sole composition
root. It rejects duplicate IDs, unknown descriptors, missing required drivers,
undeclared drivers, duplicate aliases/capabilities, or a visible product
without a doctor probe. There is no package-init registration.

## 2. Peer Driver

```go
type PeerDriver interface {
    AttachmentAdapter(HostDeps) (daemon.AttachmentAdapter, error)
    BuildLaunch(context.Context, PeerLaunchRequest) (NativeCommand, error)
    Rename(context.Context, daemon.ManagedAttachment, string) (NativeName, error)
}

type PeerLaunchRequest struct {
    ProductID            string
    AttachmentID         string
    Cwd                  string
    Args                 []string
    Env                  []EnvVar
    BootstrapCapabilityID string
    BootstrapSecret      SensitiveValue // ephemeral; never logged/persisted
}

type NativeCommand struct {
    Path         string
    Args         []string
    Env          []EnvVar
    SensitiveEnv []SensitiveEnvVar // redacted by formatting/diagnostics
    Cwd          string
}

type NativeName struct {
    Applied         string
    NativeConfirmed bool
}
```

`BuildLaunch` returns an exact exec-in-place command. It may not mutate global
product settings to prepare a launch. `AttachmentAdapter` retains the existing
daemon prepare/adopt/refresh/authorize/detach/rollback lifecycle.

## 3. Message Driver

```go
type MessageDriver interface {
    Deliver(context.Context, daemon.ManagedAttachment, DeliveryRequest) (NativeAcceptance, error)
}

type DeliveryMode string
const (
    DeliveryIdleWake   DeliveryMode = "idle-wake"
    DeliveryBusySteer  DeliveryMode = "busy-steer"
    DeliveryBusyFollow DeliveryMode = "busy-follow-up"
)

type DeliveryRequest struct {
    DeliveryID string
    ReceiptID  string // optional lane/input correlation
    Mode       DeliveryMode
    Body       []byte // transient bounded AgentFrame presentation; never retained here
}

type NativeAcceptance struct {
    NativeSessionID string
    NativeMessageID string
    AcceptedAt      time.Time
}
```

Success means the product-native API accepted the exact destination session,
not merely that a local goroutine queued work. A product that cannot establish
that fact returns a typed failure or ambiguity.

## 4. Lane Driver

```go
type LaneCapabilitySet struct {
    Steer         bool
    DurableResume bool
}

type LaneDriver interface {
    Capabilities() LaneCapabilitySet
    Open(context.Context, LaneOpenRequest) (NativeSessionRef, error)
    StartTurn(context.Context, NativeSessionRef, TurnStartRequest) (NativeTurnRef, error)
    WaitTurn(context.Context, NativeTurnRef) (NativeTerminal, error)
    Steer(context.Context, NativeTurnRef, TurnStartRequest) (NativeAcceptance, error)
    Interrupt(context.Context, NativeTurnRef) error
    Archive(context.Context, NativeSessionRef) error
    Recover(context.Context, LaneRecoveryRequest) (NativeSessionRef, error)
}

type LaneOpenRequest struct {
    ProductID       string
    LaneID          string
    ResumeNativeID  string // empty means fresh
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

type NativeTerminal struct {
    Outcome          TurnOutcome // completed|interrupted|failed|timed-out
    ExitLike         int
    Result           string      // transient bounded result, persisted once by LaneEngine
    ResultDigest     [32]byte
    NativeStopReason string
}

type LaneRecoveryRequest struct {
    ProductID           string
    LaneID              string
    PriorNativeSessionID string
    PriorGeneration     uint64
}
```

`StartTurn` and `Steer` consume the same durable receipt/spool path. The driver
opens content through a bounded host dependency by receipt ID and must verify
digest/length before native I/O. `ErrUnsupportedSteer` is not a generic error:
the daemon transitions the same receipt back to durable next-turn queueing.

Drivers may hold ephemeral clients keyed by native references. `Recover` must
rebuild those clients for the exact native ID or return
`ErrUnsupportedRecovery`; it may not substitute a new session.

## 5. Parent Attester

```go
type ParentAttester interface {
    Attest(context.Context, ConnectorAttempt) (ParentBinding, error)
}

type ConnectorAttempt struct {
    ProductID               string
    PeerCredential          localtransport.PeerIdentity
    ProcessIdentity         procinfo.Identity
    ClaimedNativeSessionID  string // corroborating only
    ComponentBindingID      string
}

type ParentBinding struct {
    AttachmentID   string
    NativeSessionID string
    Verified       bool
}
```

The claimed session ID is never authority. Implementations corroborate it with
the managed attachment and product-native context (component binding,
`shell.env`, extension context, registry, or exact process evidence).

## 6. Doctor Probe

```go
type ProbeDepth string
const (
    ProbePresence    ProbeDepth = "presence"
    ProbeVersion     ProbeDepth = "version"
    ProbeFeature     ProbeDepth = "feature"
    ProbeIntegration ProbeDepth = "integration"
)

type DoctorProbe interface {
    Probe(context.Context, ProbeRequest) (ProbeReport, error)
}

type ProbeRequest struct {
    ProductID      string
    ExecutablePath string
    Depth          ProbeDepth
}

type ProbeReport struct {
    State         ProbeState // ready|missing|incompatible|unconfigured|error
    NativeVersion string
    Features      map[string]bool
    TupleOK       *bool
    Detail        RedactedString
}
```

Federation advertisement requires `ready` at the product's declared feature
depth. CodeBuddy additionally requires its catalog support state to permit
advertisement.

## 7. Host Dependencies

Drivers receive narrow capabilities rather than the coordinator or catalog:

- current daemon generation and read-only host identity;
- secure runtime/state roots;
- exact process capture and owned-process supervision;
- receipt reader that verifies spool identity/digest/size;
- component/product-server registries appropriate to the product;
- clock and bounded test hooks.

The source contract exposes those capabilities through product-neutral
interfaces: `ReceiptReader`, `ProcessInspector`, `OwnedProcessSupervisor`,
`ComponentLookup`, `ProductServerLookup`, and `TestHooks`. Component and server
lookups return secret-free session views; protocol clients and credentials stay
ephemeral inside their owning driver/mechanics package. Test hook points use the
same bounded token grammar and production hosts leave the hook interface nil.

`RedactedString` has private storage and is constructed only through the
bounded sanitizer, which normalizes controls and replaces explicitly supplied
or commonly formatted bearer/credential values. Product code cannot cast raw
native stderr into a probe diagnostic.

Drivers cannot commit daemon lifecycle state directly. They return native facts
to the shared engines.

## 8. Error Contract

All native errors map to stable categories:

```text
ErrUnavailable
ErrIncompatible
ErrUnauthorized
ErrStale
ErrAmbiguousSession
ErrUnsupportedPolicy
ErrUnsupportedRename
ErrUnsupportedSteer
ErrUnsupportedRecovery
ErrNativeRejected
ErrProtocol
ErrTimedOut
ErrCleanupDebt
```

Machine category is stable; bounded redacted details may vary. Unknown products,
transports, and entrypoints fail loudly.

## 9. Permission Contract

Each product owns a typed mapper from `permissionmode.Mode` to native policy.
The mapper returns `ErrUnsupportedPolicy` when an equal-or-narrower mapping is
impossible. It never defaults to a broader/yolo policy. Existing unconditional
Grok widening must be removed or recorded as an explicit reviewed exception.
