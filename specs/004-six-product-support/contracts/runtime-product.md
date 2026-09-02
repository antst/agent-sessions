# Contract: Runtime Product Registry

This is the frozen target contract after all phase-0 truth gates pass. Names may
change mechanically during implementation, but authority, inputs, results, and
error semantics may not.

## 1. Registry

```go
type RuntimeProduct struct {
    Descriptor        productcatalog.Descriptor
    Peer              PeerDriver
    Message           MessageDriver
    Lane              LaneDriver
    Parent            ParentAttester
    Doctor            DoctorProbe
    ComponentResolver ComponentResolver
    ComponentRebinder ComponentSessionRebinder
}

type Registry interface {
    ByID(productID string) (RuntimeProduct, bool)
    All() []RuntimeProduct
}
```

The constructor accepts an explicit ordered slice from the sole composition
root. It rejects duplicate IDs, unknown descriptors, missing required drivers,
undeclared drivers, duplicate aliases/capabilities, or a visible product
without a doctor probe. The optional component resolver/rebinder fields are
accepted only for an interactive descriptor whose peer transport is exactly
`component`; a rebinder without its resolver is rejected. There is no
package-init registration.

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

`Rename` is a pure write-through request to the product-owned native title. It
does not authorize an independent daemon alias or durable mutable title copy.
For a live connection the daemon may hold the product's authenticated native
title as a generation-local in-memory observation. It discards that observation
on disconnect, rebind, or generation change and never persists, logs, or
serializes it as authority.

### Component Resolver/Rebinder

The component mechanics package remains below the product-runtime package
boundary, so the registry exposes a minimal product-neutral seam. The shared
component authority adapts this seam without product switches:

```go
type ComponentPeerEvidence struct {
    Peer    localtransport.PeerIdentity
    Process procinfo.Identity
}

type ComponentResolution struct {
    BootstrapCapabilityID string // optional correlation only; never authority
    BootstrapRevision     uint64
    LiveEvidence          daemon.NativeEvidence
}

type ComponentResolver interface {
    ResolveComponent(
        context.Context,
        HostDeps,
        daemon.ManagedAttachment,
        ComponentPeerEvidence,
    ) (ComponentResolution, error)
}

type ComponentSessionRebinder interface {
    ReattestSessionRebind(
        context.Context,
        HostDeps,
        daemon.ManagedAttachment,
        oldNativeSessionID string,
        newNativeSessionID string,
        evidence []byte,
    ) (daemon.NativeEvidence, error)
}
```

Every resolver and rebinder call MUST use the supplied live host capabilities
to re-capture process, executable, artifact, tuple, and ancestry evidence. A
stored identity or prepared-launch handoff may be a correlation hint but is
never authority. The shared adapter retains no evidence between calls.

These fields are additive and optional so a transitional composition can still
use its existing central fallback. When supplied, the typed seam is preferred
and the composition root must not select it through package initialization or a
per-product switch.

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
    Steer                  bool
    DurableResume          bool
    DeferredSessionBinding bool
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

### Deferred native-session binding

`DeferredSessionBinding` is an explicit per-driver semantic capability for a
native product whose session does not exist until its first accepted turn. It
does not add a durable field or change `NativeSessionRef`:

1. `ValidateLaneOpenResult` requires an empty `NativeSessionID` when the
   capability is true and `LaneOpenRequest.ResumeNativeID` is empty, and
   forbids an empty ID for every nonflagged Open. `LaneID` and `Generation`
   remain mandatory authority throughout this window.
   After validation, the shared coordinator uses
   `NativeSessionBindingFromOpen`: `bindAtOpen=true` is the only authorization
   to call `LaneEngine.SetNativeSessionID`. A deferred empty result returns
   `bindAtOpen=false`; a deferred driver MUST NOT use that exact-at-Open path.
2. Resume is never deferred. Every resume `Open` returns the exact requested
   native session ID, including for a deferred-binding driver; omission or
   substitution fails closed.
3. Fresh deferred `Open` stages only the lane and owned runtime prerequisites.
   It MUST NOT create a native session/job, dispatch input, spend a model turn,
   or invent an ID from `LaneID` or the caller's request.
4. The first `StartTurn` is the sole operation that may receive an unbound
   `NativeSessionRef`. `ValidateLaneStartTurnResult` requires it to return the
   same `LaneID` and `Generation`, a non-empty product-generated native session
   ID, and an exact native turn ID. Every other lane operation requires a bound
   native session; leases and native-session addressing are forbidden before
   binding.
5. The lane engine commits the returned session ID, exact native acceptance,
   receipt `Injected` state, and daemon turn native dispatch ID in one CAS.
   If the lane is already bound the returned ID must equal it exactly. A bound
   ID is immutable and an exact replay is idempotent. Existing-lane Accept,
   Dispatch, and lane-input admission mutations may preserve an omitted durable
   ID but MUST reject any nonempty mismatch and MUST NOT bind an empty ID.
   Terminal completion preserves the durable ID; a conflicting product result
   converges to failed terminal evidence with a diagnostic instead of changing
   session authority or stranding the Turn.
6. An unbound lane surviving restart has no native session to recover. Its
   proven-pre-I/O first input remains queued and is redispatched by `LaneID`.
   A possible native write without exact acceptance is `Ambiguous`, never a
   clean failure or automatic replay, and does not authorize a placeholder ID.
   Only authoritative product reconciliation may resolve it through the same
   atomic binding/acceptance commit; otherwise cleanup debt remains explicit.
7. From binding onward, `Recover`, resume, delivery, wait, interrupt, archive,
   and later turns use the exact immutable native session ID. A product may not
   adopt an arbitrary session merely because cwd or display metadata matches.
   A process-local lane actor is never routing authority by itself: resolution
   requires a non-staged durable Lane row, and the actor cache's native session
   ID must exactly equal that row's `NativeSessionID`. Equality intentionally
   includes empty-to-empty for a non-staged deferred lane, which remains
   addressable by `LaneID`; the guard does not impose premature non-emptiness.
   This blocks both the pre-create reservation window and the
   post-bind/pre-cache-sync window.

These rules are Amendment Round 2 to the LaneDriver semantics. They preserve
the existing durable catalog schema and record versions.

Production source MUST keep a mechanical tripwire over writes to lane
`NativeSessionID`: the only binding writes are `SetNativeSessionID` after the
validated exact-at-Open guard and `MarkInjectedAndSetNativeDispatch` at exact
first native acceptance. Preservation-only copies may not introduce another
binding boundary.

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
depth.

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
