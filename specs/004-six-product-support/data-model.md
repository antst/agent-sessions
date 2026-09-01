# Data Model: Six-Product Symmetric Support

The daemon catalog remains the sole durable lifecycle authority. Product
drivers may cache live clients and connections, but those caches are
reconstructible and never authoritative.

## 1. Product Descriptor

Data-only record in `internal/productcatalog`.

| Field | Meaning / validation |
|---|---|
| `ID` | Unique lower-case simple identifier (`opencode`, `kilo`, `pi`, `omp`, `codebuddy`, `dsh`) |
| `Label` | User-facing product label |
| `NativeExecutable` | Default native executable name |
| `PeerAlias`, `LaneAlias` | Unique managed command aliases |
| `Capabilities` | Declared shared behaviors, including new `CapabilityParent`; registry must supply matching drivers |
| `SupportState` | `hidden`, `experimental`, or `general` |
| `TestedVersion` | Exact evidence baseline |
| `Compatibility` | Version/tuple policy and required feature keys |
| `PeerTransport`, `MessageTransport`, `LaneTransport` | Stable keys resolved only by the runtime composition root |
| `ConnectorAttesterKey`, `DoctorProbeKey` | Stable runtime keys |
| `PermissionProfileKey` | Product-specific fail-closed permission mapper |
| `InstallRoot` | Integration asset root below the packaged `integrations/` tree |
| `FederationCapabilities` | Bounded opaque capability strings, currently one `*-lane` value; uses the shared catalog token validator |
| Existing fields | Existing label, role, archive, resume, and transcript metadata remain until consumers migrate |

The catalog contains no callback, endpoint, bearer credential, or native
profile secret.

`Capabilities` and `FederationCapabilities` are distinct namespaces but use
one lower-case bounded token grammar and validator. `parent` is an explicit
catalog capability, not inferred from a non-nil runtime driver.

## 2. Runtime Product

Ephemeral registry entry in `internal/productruntime`.

```text
RuntimeProduct
├── Descriptor (productcatalog.Descriptor)
├── Peer (optional PeerDriver)
├── Message (optional MessageDriver)
├── Lane (optional LaneDriver)
├── Parent (optional ParentAttester)
└── Doctor (required DoctorProbe for non-hidden products)
```

Registry validation is bidirectional: every declared capability has its driver,
and no driver is supplied without the corresponding declared capability.

## 3. Native References

### NativeSessionRef

Secret-free durable reference used by a lane driver.

| Field | Validation |
|---|---|
| `LaneID` | Existing exact daemon lane |
| `NativeSessionID` | Non-empty immutable native ID |
| `Generation` | Daemon generation that last opened/recovered the live client |

The lane row remains authoritative for product, profile identity, cwd, and
policy. The reference contains no URL, port, socket, password, connection token,
or raw capability.

### NativeTurnRef

| Field | Validation |
|---|---|
| `NativeSessionRef` | Exact owning session |
| `NativeTurnID` | Native turn/job/run ID, empty only if the protocol has no distinct ID and the driver documents that fact |

Drivers may hold ephemeral client state keyed by these references and rebuild it
through `Recover`.

## 4. Lane Input Receipt

Durable catalog entity. The catalog stores metadata only; content is in the
private spool.

| Field | Meaning / validation |
|---|---|
| `ReceiptID` | Stable unique receipt ID |
| `LaneID` | Exact owning lane |
| `Sequence` | Strictly increasing lane-local input order |
| `Digest` | SHA-256 of exact content bytes |
| `Bytes` | Bounded non-negative byte count |
| `SpoolObjectID` | Opaque generated ID; path is derived, never caller-supplied |
| `State` | Receipt lifecycle below |
| `TargetTurnID` | Assigned daemon turn, if any |
| `DispatchAttempt` | Stable attempt ID committed before native I/O |
| `NativeAcceptance` | Optional secret-free `NativeAcceptanceRef` |
| `Revision` | Monotonic mutation revision |
| `AcceptedAt`, `UpdatedAt` | Daemon timestamps |
| `AmbiguityCause` | Stable redacted category only when ambiguous |

### Receipt states

```text
Prepared -> Queued -> Dispatching -> Injected -> Retired
                         |
                         +-> Ambiguous -> Retired
Queued -> Retired                    (lane retires before dispatch)
```

- `Prepared`: spool object is proven, but admission/ordering is not yet
  caller-visible.
- `Queued`: admission and sequence are durable; caller may receive acceptance.
- `Dispatching`: intent and stable attempt ID committed before native I/O.
- `Injected`: exact native API accepted the input for the exact native session.
- `Ambiguous`: native I/O may have succeeded but durable proof is unavailable;
  never automatically replayed unless the protocol proves idempotency.
- `Retired`: content removed after proven injection, explicit abandonment, or
  terminal cleanup; receipt metadata remains available according to catalog
  retention.

### NativeAcceptanceRef

Durable projection of the one transient `NativeAcceptance` type:

| Field | Meaning |
|---|---|
| `NativeSessionID` | Corroborates exact destination session |
| `NativeMessageID` | Product-native message/prompt/job ID when available |
| `AcceptedAt` | Native acceptance timestamp |

No message body is duplicated in this record.

## 5. Lane Input Spool Object

Private file below a daemon-owned `0700` root; file mode `0600`.

| Property | Invariant |
|---|---|
| Addressing | Derived only from generated `SpoolObjectID` |
| Creation | no-follow, exclusive create, bounded write, fsync file and parent |
| Verification | type, owner, device/inode identity, exact size, and SHA-256 |
| Limits | per-input, per-lane, total-count, and total-byte bounds |
| Removal | exact identity rechecked; missing/changed object becomes debt |
| Recovery | orphan object without committed receipt was never acknowledged and may be collected safely |

Catalog and spool are ordered rather than pretending to be one filesystem
transaction: spool commit precedes catalog admission.

## 6. Native Session Lease

Durable exclusive ownership record, initially required by DSH and reusable for
other products that allow dual writers.

Composite key:

```text
(ProductID, ProfileIdentity, NativeSessionID)
```

| Field | Meaning |
|---|---|
| `ProductID`, `ProfileIdentity`, `NativeSessionID` | Exact lease key |
| `OwnerLaneID` | Exact daemon lane owner |
| `Generation` | Current daemon generation |
| `ProcessGroup` | Exact supervised process identity when present |
| `State` | `Prepared`, `Held`, `Releasing`, `Released`, `CleanupDebt` |
| `Revision`, `CreatedAt`, `UpdatedAt` | Mutation evidence |

Transitions:

```text
Prepared -> Held -> Releasing -> Released
             |          |
             +----------+-> CleanupDebt -> Released
```

Only one non-terminal lease may exist for a composite key. Recovery never
steals a lease from a live corroborated owner.

## 7. Component Binding and Session

### ComponentBinding

Generation-scoped connection record used by `internal/component`.

| Field | Meaning |
|---|---|
| `BindingID` | Daemon-issued unique live binding |
| `AttachmentID` | Exact managed attachment |
| `ProcessIdentity` | PID/start/strong-start corroborated with kernel peer creds |
| `BootstrapRevision` | One-time capability revision consumed at initial bind |
| `Generation` | Owning daemon generation |
| `State` | `Binding`, `Ready`, `Retiring`, `Closed` |
| `LastInboundSeq`, `LastOutboundSeq` | Bounded same-generation replay window |

The raw connection is ephemeral. Durable managed attachment evidence remains
the authority used to re-attest after restart. No signing key or raw bootstrap
secret is stored.

### ComponentSession

Operational mapping for products whose one component process can observe more
than one native session.

| Field | Meaning |
|---|---|
| `BindingID` | Current component binding |
| `AttachmentID` | Durable authority row |
| `NativeSessionID` | Product-native session |
| `State` | `Announced`, `Idle`, `Busy`, `Closing`, `Closed` |
| `LastEventSeq` | Monotonic component event sequence |

Name, cwd, groups, permission, and native evidence remain authoritative on
`ManagedAttachment`; they are not duplicated here.

## 8. Managed Attachment Extensions

Existing `ManagedAttachment` remains the durable peer authority. New fields are
limited to:

| Field | Meaning |
|---|---|
| `ComponentProtocol` | Negotiated supported component protocol, if used |
| `ComponentRevision` | Last committed component session event revision |
| `IntegrationVersion` | Installed plugin/extension/native integration version used for doctor/recovery |

Existing `NativeSessionID`, `NativeName`, `Cwd`, `ExpectedEvidence`, `Evidence`,
`CapabilityHash`, `DaemonGeneration`, and lifecycle `State` remain authoritative.

## 9. Catalog Additions

```text
Catalog
├── existing Host / Attachments / Deliveries / Lanes / Turns / CleanupDebts
├── LaneInputs map[ReceiptID]LaneInputReceipt
├── NativeLeases map[CompositeLeaseKey]NativeSessionLease
├── ComponentBindings map[BindingID]ComponentBinding
└── ComponentSessions map[AttachmentID]ComponentSession
```

All maps are normalized non-nil, validated on read and commit, included in the
existing catalog byte bound, and revisioned through the host's lane/attachment
revisions as applicable. Input bodies remain outside this catalog.

## 10. Install Projection and Ownership Receipt

### InstallProjection

Deterministic binary-emitted description derived from product descriptors:

| Field | Meaning |
|---|---|
| `ProductID`, `SupportState`, `TestedVersion` | Product contract |
| `Aliases` | Peer and lane aliases |
| `ArchivePaths` | Integration payload paths |
| `NativeRegistration` | Typed installer strategy key and arguments |
| `DoctorFeatures` | Required post-install probes |
| `FederationCapabilities` | Advertised only when doctor-ready |

### IntegrationOwnershipReceipt

Records the exact Agent Sessions-owned native registration/artifact baseline,
revision, prior identity, replacement identity, and rollback/removal rule.
It never contains credentials. DSH profile/plugin installation uses this record
instead of broad profile mutation.

## 11. Transient Runtime Records

These cross driver boundaries but are not durable catalog entities:

- `NativeCommand`: executable, arguments, ordinary env, separately redacted
  sensitive env, cwd. Sensitive env may contain the one-time bootstrap value
  and is never logged or persisted.
- `DeliveryRequest`: durable delivery/receipt IDs and delivery mode; content is
  supplied through an authorized reader rather than copied into metadata.
- `NativeAcceptance`: exact native session/message ID and accepted timestamp;
  projected to `NativeAcceptanceRef` when persisted.
- `NativeTerminal`: outcome, exit-like code, native stop reason, bounded result,
  and result digest; the lane engine persists the result once.
- `ProbeReport`: stable readiness state, native version, required feature map,
  optional tuple result, and redacted detail.

## 12. Stable Error Categories

Every product driver maps native errors to one of:

```text
Unavailable
Incompatible
Unauthorized
Stale
AmbiguousSession
UnsupportedPolicy
UnsupportedSteer
UnsupportedRecovery
NativeRejected
Protocol
TimedOut
CleanupDebt
```

Machine categories remain stable; diagnostics may contain bounded redacted
product detail. Unknown products and entrypoints fail loudly rather than
defaulting to another product label or driver.
