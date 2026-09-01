# Product adapter protocol

This document is the normative integration contract for the Agent Sessions
unified user daemon. Product research and reverse-engineered native details are
inputs to an adapter; they are not lifecycle authority. The words **MUST**,
**MUST NOT**, **SHOULD**, and **MAY** are used normatively.

The current local lane contract is version 2. Federation remains wire protocol
3. Adding a catalog product or an opaque federation capability does not change
either wire format.

## Authority and package boundaries

`internal/daemon` is the sole durable authority for attachments, deliveries,
lanes, turns, lane inputs, native-session leases, component bindings, and
cleanup debt. A native product process or plugin can prove that an operation
was accepted, but it does not own Agent Sessions lifecycle state.

`internal/productcatalog` is the sole authored product inventory. It is data
only and MUST NOT import runtime drivers. `internal/productruntime` defines
secret-free records, optional driver interfaces, stable error categories, and
an explicitly constructed registry. The only production enumeration and
driver composition point is `cmd/agent-sessions/product_registry.go`. Packages
MUST NOT register products from `init` functions.

The registry MUST reject all of the following before the daemon starts:

- a declared interactive capability without peer and message drivers;
- a declared lane capability without a lane driver;
- a parent-capable descriptor without a parent attester;
- a visible product without a doctor probe;
- a driver whose corresponding capability is absent;
- duplicate, missing, or non-catalog product IDs; and
- invalid transport, policy, doctor, install, or federation tokens.

Every product ID and opaque capability token uses the one catalog token
grammar: 1 through 64 ASCII lowercase letters, digits, and single hyphens,
starting with a letter and without a leading, trailing, or repeated hyphen.
Labels are presentation only and never grant authority.

## Runtime driver contract

Product behavior is divided into five optional drivers:

1. `PeerDriver` prepares/adopts a user-owned interactive process, constructs
   the native launch, and applies a native rename where supported.
2. `MessageDriver` delivers to one already-attested managed attachment and
   returns a native acceptance for that exact session.
3. `LaneDriver` opens a native session, starts and waits for turns, optionally
   steers an active turn, interrupts, archives, and recovers an exact session.
4. `ParentAttester` binds a tool caller to one managed attachment and one
   independently proven native session.
5. `DoctorProbe` performs bounded presence, version, feature, and integration
   checks without fabricating readiness.

`NativeSessionRef` contains only lane ID, native session ID, and generation.
`NativeTurnRef` adds only the native turn ID. Endpoints, bearer tokens,
passwords, bootstrap material, and process handles MUST NOT enter either ref or
durable state. A driver MAY retain ephemeral clients keyed by those refs and
MUST rebuild them in `Recover`.

`Steer` is an explicit optional lane capability. A driver that cannot prove
mid-turn acceptance returns `ErrUnsupportedSteer`; the daemon queues the same
durable receipt for the next turn. It MUST NOT pretend that boundary-queued
input was steered. `Recover` returns `ErrUnsupportedRecovery` when exact resume
is impossible; it MUST NOT silently substitute a new native session ID.

Errors exposed at the runtime boundary use stable machine categories:
unavailable, incompatible, unauthorized, stale, ambiguous-session,
unsupported-policy, unsupported-steer, unsupported-recovery, native-rejected,
protocol, timed-out, and cleanup-debt. Diagnostics MAY add bounded redacted
detail but MUST NOT change the category or expose a secret.

## Attachment and parent identity

Interactive lifecycle remains:

```text
preparing -> prepared -> selecting -> attached -> detaching -> detached
```

The wrapper obtains a one-time bootstrap capability, records preparation with
the daemon, then replaces itself with the native executable. Native selection
can be late-bound. The prelaunch attachment ID is lifecycle identity only; the
product's authoritative session ID is adopted after a product-owned surface
corroborates it. A transient boot ID MUST NOT become a catalog or model-visible
identity.

Adoption and every privileged refresh MUST corroborate the product-appropriate
subset of:

- kernel peer UID/PID and strong process-start identity;
- ancestry to the managed wrapper or product-owned service;
- exact executable and launch arguments;
- canonical profile and working directory;
- a real owned socket or other supported product endpoint;
- the selected native session ID; and
- the attachment capability and daemon generation.

PID alone, a model-supplied `session_id`, a copied environment variable, a
writable registry row, or an endpoint URL is never authority. A stale registry
row is rejected if its socket has been recycled or no longer belongs to the
attested process. On Linux the socket inode-to-PID relation is checked through
kernel process data; macOS uses its supported local process/socket evidence.

Parent tool calls require both process/transport evidence and exact native
session context. A claim is corroborating input only. Unknown, empty,
ambiguous, or mismatched products and entrypoints fail loudly; they MUST NOT
fall back to a Claude or Codex label or authority path.

## Component protocol

Products with a supported in-process plugin or extension use a long-lived
component connection. The broker listens on a dedicated private
`component.sock`, separate from the one-request `daemon.sock` control plane.
Both live below a `0700` user-owned runtime root; the socket is `0600`, bounded
by the shared platform path budget, and never reached through a mutable
symlink.

Initial component attach requires the wrapper's one-time bootstrap capability,
kernel peer identity, strong process-start identity, ancestry, and the durable
managed attachment. A reconnect rechecks the kernel/process evidence against
that record and echoes the current generation nonce. Agent Sessions does not
use an Ed25519 component identity: the same-UID trust model supplies no named
adversary that such a key would exclude.

A component launched without a managed bootstrap remains inert. It MUST NOT
publish a peer, open a delivery endpoint, register an authoritative lane tool,
or mutate user state merely because a global plugin was loaded.

Frames are bounded length-prefixed JSON with stable operation keys. They cover
bootstrap/reconnect, session announce/rebind/rename/state/close, delivery
present/accept/reject, turn events, tool call/cancel/result, heartbeat, and
generation retirement. Same-generation frame replay is bounded. Durable
delivery, lane, and tool idempotency belongs to daemon operation IDs, not to
transport sequence numbers.

CodeBuddy is not a component. Its interactive peer is adopted through the
managed wrapper and product worker registry, then addressed through the
product-owned loopback endpoint after socket-to-PID verification. That peer
surface has only the product's constant CSRF header and no Agent Sessions
secret. An Agent Sessions-owned CodeBuddy lane server is a different surface:
the daemon enables native password authentication and retains that password
only in memory. The two auth contracts MUST NOT be conflated.

## Session tools and message delivery

`internal/sessiontools` owns product-neutral MCP schemas/instructions, the
private daemon control client and relay, pure Codex metadata/ancestry helpers,
peer-name normalization, product labels, lane help, and cross-session envelope
wrapping. Product-native readiness, observers, hook dispatch, and transport
state remain product-owned.

Discovery and delivery operate only after the daemon re-attests the source and
checks group visibility. Source identity is derived from the managed
attachment, never from the message body. Delivery is durable per target and
progresses through prepared, accepted, presented, and acknowledged states.
Content is not persisted in the attachment catalog.

Cross-session envelopes carry a catalog-validated product ID and escaped
attributes/body. Closing `cross-session-message` sequences in user content are
escaped before native transport. Unknown products are errors, not generic
Claude messages.

Bare or unattested MCP callers receive the one canonical inactive tool result
before any roster, inbox, rename, lane, or send operation reaches the daemon.
`check_inbox` is recovery-only; active delivery is push-based and callers MUST
NOT poll it.

## Durable lane-input ledger

Every lane input is accepted through one daemon-owned bounded ledger. Its body
is written to a private `0600` spool beneath a `0700` no-follow root; durable
state stores only receipt metadata, a digest, byte count, and spool reference.

Acceptance order is mandatory:

1. write, fsync, and verify the spool body;
2. commit the `prepared` or `queued` receipt to daemon state; and
3. acknowledge caller acceptance.

Before invoking a native driver, the daemon commits `dispatching`. A successful
native acceptance commits `injected` with a secret-free
`NativeAcceptanceRef`. A definite unsupported steer requeues the **same**
receipt and sequence for the next turn.

If the daemon crashes after native I/O and before a proven acceptance, recovery
marks the receipt `ambiguous` and creates cleanup debt. It MUST NOT blindly
replay unless the native protocol independently proves idempotent replay for
that exact input. Spool content is removed only after proven injection or
terminal retirement. Count and byte limits fail before unbounded disk growth.

This ledger also replaces volatile original-product pending input; queued
messages must survive daemon restart.

## Native-session leases and recovery

Products whose native stores permit concurrent resume use a durable exclusive
lease keyed by `(product ID, profile identity, native session ID)`. Lease state
progresses through prepared, held, releasing, released, or cleanup-debt. A
second owner is rejected. Reacquisition after an owner disappears requires
proof of exact process death and generation fencing; elapsed time or PID reuse
is insufficient.

Recovery reopens the exact recorded session or reports unsupported recovery.
Drivers MUST preserve the native ID across restart, resume, interrupt, and
archive. Product protocols that expose no durable resume remain explicitly
narrow rather than manufacturing continuity.

DSH additionally requires one exact CLI/application/plugin version tuple,
`pnpm`, the native `DSH_SESSION_ID` witness, and one lease per profile/session.
ACP busy rejection queues the receipt. Cancel is an ACP notification; the
request form is a protocol error. Projection-cache metadata is not a liveness
signal. Component sockets for a sandboxed DSH process MUST live below the
reachable home/XDG state root, never `/tmp`, because the native sandbox masks
`/tmp`.

## Shared mechanics, distinct product semantics

Mechanical transport is shared only where the invariant is truly common:

- `internal/localtransport`: bounded local framing and platform peer identity;
- `internal/component`: component broker and state machine;
- `internal/productserver`: authenticated literal-loopback HTTP/event client,
  redirect/proxy refusal, bounded bodies/decompression, and owned supervision;
- `internal/structuredprocess`: exact child/process-group ownership, bounded
  ordered framed I/O, cancellation, and exit evidence; and
- `internal/sessiontools`: session/MCP/control helpers described above.

Typed protocols remain separate above those mechanics. Pi/OMP JSONL RPC and
DSH ACP do not share a fake universal command schema. OpenCode-family shared
operations do not hide Kilo's distinct TUI routes. CodeBuddy peer and lane
surfaces retain distinct authentication. DRY means one invariant and one
mechanical primitive, not a lowest-common-denominator state machine.

`internal/pathidentity`, `internal/socketpath`, `internal/procinfo`,
`internal/envutil`, and `internal/permissionmode` are the authoritative host
primitives. Adapters MUST NOT invent product-local path alias, socket-length,
PID-liveness, environment, or permission parsers.

## Permission policy

Permission mapping is product-owned and must be equal to or narrower than the
durable requested mode. An adapter MUST NOT silently widen, mutate, or guess a
native policy. Unsupported mappings fail with the stable unsupported-policy
category before native callbacks or process mutation.

In particular, the Grok ACP lane accepts only an explicit
`bypassPermissions` request. Default, empty, constrained, and unknown values
are rejected unchanged before native dispatch. Interactive native permission
changes remain product-owned where the product exposes them.

## Federation protocol 4

Federation wire protocol 4 is uniform. Capabilities are bounded opaque
catalog tokens. The hub validates syntax, size, count, deduplication, and that
the destination advertised the requested capability; it does not resolve a
closed product switch. The destination runtime registry is authoritative for
product support, doctor readiness, parent proof, and driver selection.

The first handshake carries exact version 4. Any other version, including an
N+1 participant against an N hub, is rejected before registration. Every
accepted client receives the same complete roster, and every lane request must
carry one explicit capability; there is no partial admission, per-client
filter, or empty-capability inference. Generation fencing and destination-side
authorization still apply.

Federation currently assumes a trusted network and has no TLS or peer
authentication. That limitation MUST be documented in deployments; adding an
authenticated transport is a separate security design rather than an implied
property of product adapters. Hub tests run against the live
`internal/federation` implementation, including hostile bounded frames and
generation fencing, not a frozen legacy hub.

## Install and catalog projection

The staged binary emits the deterministic, secret-free authored inventory via:

```text
agent-sessions catalog --json
```

Output is canonical sorted JSON followed by exactly one newline and requires no
daemon. Release/install code derives plans from that projection. Shell scripts,
workflows, plugin trees, and federation MUST NOT author a second product list.
Catalog/projection drift is a CI failure.

That no-second-list rule is the required release end state, not a claim that
the Phase-A compatibility tree has already reached it. Until the T088 release
projection work lands, `scripts/release-inventory` remains a legacy shrinking
projection guarded by drift tests; it MUST NOT gain new authored products or
be treated as the authority. T088 removes those shell-owned arrays before any
six-product release is credited.

Installation is transactional: stage, validate, run a narrowly owned native
registration when present, then atomically switch. Rollback restores exact
prior owned identities and never removes user credentials, profiles, plugins,
or unrelated product data. Experimental products remain hidden from
federation until their declared real-product acceptance cells pass.

## Linux and macOS acceptance gate

Every declared capability has an acceptance cell on physical Linux and macOS.
A cross-compile, mocked product, static schema, or unit test is useful but does
not earn real-product credit. A mock model behind a real product protocol is
allowed when the cell is about protocol behavior.

Before general support, each product must prove all applicable classes:

1. **Selection and identity:** fresh, exact resume, title/name resolution,
   rename, cwd, stable native ID, provisional-ID rejection, and archive.
2. **Attestation:** PID plus strong start, ancestry, profile, canonical cwd,
   exact socket ownership, native session context, stale row/PID/port reuse,
   and inert bare sessions.
3. **Permissions:** every declared mode maps without widening; unsupported
   modes fail before native invocation.
4. **Peer messaging:** discovery, direct/multicast/broadcast, idle wake, busy
   steer or honest queue, visible rendering, deduplication, restart, and wrong-
   session isolation.
5. **Parent:** a product tool call is bound to the exact native session, can
   start lanes, and receives terminal notices without TTY scraping.
6. **Lane lifecycle:** open/start, steer or queue, collect, interrupt, exact
   recovery/resume, archive, ambiguity handling, and cleanup debt.
7. **Failure ownership:** normal exit, Ctrl-C, SIGTERM, crash, partial startup,
   PID/path reuse, process-group cleanup, and preservation of unrelated state.
8. **Install/doctor/release:** exact version or tuple, required features,
   transactional upgrade/remove, nonmutation, secret-free state, prebuilt
   install, projection drift, and physical release gates.
9. **Platform primitives:** `/tmp` and `/var` aliases, mutable symlink
   rejection, socket budget/fallback, readable and unavailable process
   environment, real socket ownership, and service environment on launchd and
   systemd.

A product remains experimental when an account-gated cell cannot be executed;
pending is never reported as passed. DSH earns credit only for its recorded
exact tuple. CodeBuddy's model-turn GA cell requires a Tencent account.

## Frozen legacy compatibility surface

The unified daemon is the live authority. `bridge.Main`, legacy federator
entrypoints, and the historical per-session manager path are not current
architecture. Phase-0 static analysis found that unreachable entrypoints are
file-entangled with live original-product helpers, so bulk deletion is deferred
until those remaining helpers are extracted safely.

Until deletion:

- no new product or shared runtime code may import `internal/bridge` or
  `internal/federator`;
- an exact shrinking import allowlist is enforced by a parser-based test;
- live MCP/session helpers move to `internal/sessiontools` with compatibility
  wrappers only where existing original-product code still needs them;
- the authored federator product table is removed in favor of a one-way
  projection from `productcatalog`; and
- legacy code and comments MUST NOT claim to be the current lifecycle
  authority.

Original-product native transports and observers may remain temporarily in the
frozen tree. New adapters are implemented only through the catalog, runtime
registry, shared mechanics, and product packages described above.

## Product-specific compatibility boundaries

Supported interfaces are preferred. Reverse-engineered surfaces must be
isolated behind typed product code and called out explicitly. Current examples
include Claude's native registry/socket carrier and Grok's private leader
observer. An installed product upgrade requires focused protocol probes plus
the full physical-platform acceptance cells for every affected capability.

File transfer and Windows named pipes are not implemented.
