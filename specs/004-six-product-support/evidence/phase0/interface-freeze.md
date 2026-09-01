# Phase A interface-freeze evidence

Status: **PASS — interfaces and records frozen**

## Identity

- Frozen base: `679fe9d3068b6362df867f8d78ce6708c4ce1342`
- Planning commit: `49c5a33`
- Phase A candidate: `f66229e457ce3278929b8e25f1741cc028324770`
- Candidate tree: `36b11e22a327e3ab79a9058873dbf57da89c945d`
- Phase-0 review: `evidence/phase0/review.md` (Fable sign-off granted)
- Queued-Claude ordering fix incorporated from reviewed commit: `17b4139`

## Frozen runtime surface

The actual source of record is `internal/productruntime/`, not this summary.

```go
type RuntimeProduct struct {
    Descriptor productcatalog.Descriptor
    Peer PeerDriver
    Message MessageDriver
    Lane LaneDriver
    Parent ParentAttester
    Doctor DoctorProbe
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
```

- `ErrUnsupportedSteer` means the daemon queues the same receipt for the next
  turn; it is not dropped and receives no new sequence.
- `ErrUnsupportedRecovery` is explicit. Recovery never substitutes a different
  native session silently.
- `NativeSessionRef` contains only lane ID, native session ID, and generation;
  `NativeTurnRef` adds only the native turn ID. Neither contains an endpoint,
  URL, socket, token, password, or bearer credential.
- Drivers may retain ephemeral clients keyed by those references and rebuild
  them in `Recover`.
- `NativeCommand` and bootstrap secrets are transient in-memory values whose
  JSON encoding fails; durable records remain secret-free.
- `HostDeps` exposes narrow product-neutral receipt, process, owned-process,
  component lookup, product-server lookup, clock, and bounded test-hook
  capabilities. It exposes no daemon coordinator or durable state authority.
- `productruntime.NewRegistry` is the sole explicit registration path. It
  validates descriptor equality and capability/driver symmetry; there is no
  `init()` registration.

## Durable record schemas

The exact constants in `internal/daemon/state.go` are:

```text
agent-sessions.lane-input-receipt.v1
agent-sessions.native-session-lease.v1
agent-sessions.component-binding.v1
agent-sessions.component-session.v1
```

Missing maps in an older catalog normalize to empty. Every present record must
carry the exact schema for its domain; missing or unknown present schemas fail
closed. `Lane.Schema` remains the user-declared lane schema and is not a record
format version.

The lane-input lifecycle is
`prepared -> queued -> dispatching -> injected|ambiguous -> retired`, with only
the contract's allowed reversions. Admission order is private spool
write/fsync/verify, durable receipt commit, then caller acknowledgment. The
daemon commits `dispatching` before native I/O. A crash without authoritative
native acceptance becomes `ambiguous`, never blind replay. An ambiguous receipt
may become injected only with an exact authoritative `NativeAcceptanceRef`.

The native lease key is exactly
`(ProductID, ProfileIdentity, NativeSessionID)`. Component reconnect checks
kernel/process/ancestry evidence against the durable binding and does not use
Ed25519. Attachment IDs and native session IDs are separate namespaces: an
explicitly re-attested resume may bind a fresh attachment to an existing native
session; string equality between those IDs is neither required nor sufficient.

## Other Phase A decisions encoded in source

- Federation wire protocol remains version 3. Product and federation
  capability tokens share one bounded validator.
- `CapabilityParent` is an explicit catalog capability and must match a
  `ParentAttester`.
- Live MCP relay, AgentFrame, instruction, identity, name, and help sources now
  live in `internal/sessiontools`; legacy bridge wrappers are compatibility
  projections and unknown products/entrypoints fail loudly.
- The dead legacy tree is frozen by a shrinking importer baseline. New product
  code may not enter it; full deletion stays a separate follow-up.
- The no-`init()` registration and reverse-cycle guard is enforced in
  `internal/productruntime/architecture_test.go`.
- Grok no longer silently widens permission policy. Bypass requires an explicit
  `bypassPermissions` request before readiness/state/native invocation.
- The normative `docs/ADAPTER-PROTOCOL.md` now describes the unified runtime,
  component, ledger, catalog, federation, and acceptance contracts.
- Its two transitional allowlist references remain explicit shrinking debt:
  the release-inventory projection and bridge/federator importer allowlists.
  T088 must continue shrinking them; they are not current product authority.
- The reviewed issue-42 fix releases a completed Claude worker registry entry
  before synchronous queued-turn dispatch; it adds no competing queue or ledger.

## Validation on the candidate tree

All commands completed successfully on Linux:

```text
go test ./internal/permissionmode ./internal/localtransport \
  ./internal/productcatalog ./internal/productruntime ./internal/federator \
  ./internal/sessiontools ./internal/bridge ./internal/daemon \
  ./internal/launcher ./cmd/agent-sessions -count=1

go test ./cmd/agent-sessions \
  -run '^TestClaudeBusyLaneMessageDispatchesExactlyOnceAfterWorkerExitWithoutCollector$' \
  -count=5

./scripts/test

go test -race ./internal/permissionmode ./internal/localtransport \
  ./internal/productcatalog ./internal/productruntime ./internal/federator \
  ./internal/sessiontools ./internal/bridge ./internal/daemon \
  ./internal/launcher ./cmd/agent-sessions -count=1

go vet ./...
git diff --check
go run ./cmd/agent-sessions catalog --json
```

Catalog projection SHA-256:
`ebb4c892d73784f33025aa02b00dfc94ecd3d6b1ef14ddb9e4be0ec604e918c2`.

Physical macOS evidence for this exact candidate has not yet been claimed and
must remain pending until a gate artifact is produced for this commit.

## Review

- Reviewer: real Agent Sessions peer `fable-architect`
- Requested candidate: `f66229e457ce3278929b8e25f1741cc028324770`
- Verdict: **SIGN-OFF GRANTED; blocking source deltas: none**
- Delivery/message evidence:
  `delivery-2a9a612f02c9725ce9f9d953bfec8270`
- Reviewed source tree: `36b11e22a327e3ab79a9058873dbf57da89c945d`
- Reviewer independently re-ran the architecture guard and core tests on the
  exact candidate. Physical macOS remains pending/no-credit, as declared above.
