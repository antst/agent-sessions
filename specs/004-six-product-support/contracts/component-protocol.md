# Contract: Managed Component Stream Protocol v1

The component protocol connects an Agent Sessions-managed in-process plugin or
extension to the host daemon. It uses
`run/component.sock`, never the one-shot `daemon.sock` control endpoint.

## 1. Transport

- AF_UNIX stream at a path selected through `internal/socketpath`.
- `0700` parent directory and user-owned socket.
- Four-byte big-endian unsigned length followed by one UTF-8 JSON object.
- Maximum frame, nesting, string, and outstanding-operation bounds are fixed by
  the daemon and tested on Linux and macOS.
- No compression, redirects, or nested stream upgrade.
- Linux and macOS kernel peer identity is captured at accept time; unsupported
  or unverifiable peer identity fails closed.

Every frame has:

```json
{
  "version": 1,
  "type": "session.announce",
  "id": "frame-or-operation-id",
  "seq": 7,
  "payload": {}
}
```

Unknown versions or frame types are rejected with a stable category. Unknown
fields are ignored only when the frame's version explicitly permits additive
fields; authority never comes from an unknown field.

The pinned daemon/client vocabulary revision is
`agent-sessions.component.v1-r2`. `ProtocolVersion` remains `1`: no component
v1 client has been released independently, and every integration component is
version-pinned and installed with its daemon. Doctor and tuple checks MUST
verify the exact contract revision before crediting an integration. If clients
ever acquire an independent release cadence, this additive-v1 policy is no
longer sufficient and the protocol MUST gain explicit revision negotiation.
The bootstrap `component_version` is this exact contract revision; the product
artifact/integration version remains a separate catalog and durable attachment
fact and is not a second handshake field.

## 2. Initial Bootstrap

Managed wrapper launch provides:

- attachment ID;
- opaque bootstrap correlation ID;
- raw one-time bootstrap value in a redacted ephemeral environment entry;
- expected product and executable/process lineage.

The first component frame is:

```text
bootstrap {
  product_id,
  attachment_id,
  bootstrap_capability_id,
  bootstrap_value,
  [process_start, strong_start],
  component_version
}
```

`process_start` and `strong_start` are an optional corroborating pair. Both MAY
be omitted. Supplying exactly one is invalid; when both are supplied they MUST
exactly match the daemon's independent live capture. The fields never grant
authority. `bootstrap_capability_id` is likewise non-authoritative correlation
metadata: it is never durable authority and a resolver-supplied ID is never an
authorization gate. The daemon validates the presented bootstrap value against
the durable capability hash, validates the bootstrap revision, consumes the
durable `(attachment_id, bootstrap_revision)` anchor once, validates kernel peer
PID/UID, independently captures exact process identity, executable, and
ancestry, and checks that live evidence against the exact prepared attachment.
Success returns:

```text
ready { binding_id, attachment_id, daemon_generation, protocol_version,
        max_frame_bytes, heartbeat_interval_ms }
```

An ambient globally installed component without a prepared bootstrap remains
inert and does not retry privileged operations.

### Handshake idempotency and adoption

1. `(attachment_id, bootstrap_revision)` is the durable handshake-idempotency
   anchor. Within one daemon generation, `Authorizer.Bootstrap` and
   `Authorizer.Reconnect` MUST transactionally create-or-return the exact
   secret-free `binding_id`; the broker MUST use that ID and MUST NOT mint a
   replacement after authorization. A successor daemon generation creates one
   fresh generation-scoped binding ID carrying the same anchor after fresh
   re-attestation.
2. A lost `ready` MAY be retried only with the identical bootstrap value,
   attachment, product, bootstrap revision, and freshly recaptured kernel
   PID/UID, process start/strong-start, executable, and ancestry. The opaque
   capability ID may be carried for correlation or changed without changing
   authorization; by itself it can neither authorize nor reject the retry.
   Optional claim-side process tokens, when supplied, must remain an exact pair
   matching that live capture; their omission does not weaken live
   re-attestation. While the binding remains unadopted, an exact authoritative
   retry in the same generation returns the same committed `binding_id`. On
   daemon restart, the atomic generation sweep retires the old binding and an
   exact, freshly re-attested retry creates or returns one fresh
   current-generation binding with the same attachment and bootstrap revision.
   Any foreign, stale, conflicting, or already-adopted replay is rejected.
3. A returned binding remains in `binding` state until the first authenticated
   non-handshake component frame that names its `binding_id` is durably
   admitted. That admission adopts the binding; no `ready` acknowledgment or
   new wire field is introduced.
4. Reconnect permits at most one unadopted successor per attachment and daemon
   generation. Only the immediate predecessor may retry it; an exact, freshly
   re-attested retry re-establishes the same successor binding, while a
   conflicting predecessor is rejected. Adoption fences the predecessor, so
   all later predecessor replay is rejected.
5. Any broker-local predecessor/successor or delivery replay memory is
   generation-scoped and strictly bounded (at most one unadopted successor per
   attachment/generation); durable `ComponentBinding` and delivery state remain
   authoritative across restart.
6. The same catalog CAS that publishes daemon generation `N+1` transitions all
   generation-`N` `binding|ready` rows to `retiring` and closes every referencing
   nonclosed `ComponentSession`. A committed successor closes its predecessor;
   adoption removes the closed predecessor through convergent bounded cleanup,
   leaving at most the current binding plus one transient predecessor per
   attachment.
7. A closed component session is removed in a separate idempotent commit before
   exact `session.announce` re-adds an `announced` row. `session.rebind` likewise
   closes the immutable old-session row, then atomically removes it while fresh
   product evidence changes the attachment-to-native-session relationship, then
   adds the immutable new-session row. A crash after any commit converges by
   retry; session-level operations before the new announcement fail closed.

## 3. Reconnect

After daemon restart, the same native process sends:

```text
reconnect { attachment_id, prior_binding_id, prior_generation,
            [process_start, strong_start], last_received_seq }
```

The daemon does not require a reusable bearer secret or Ed25519 signature. It
re-captures kernel peer PID/UID, process start, strong start, executable, and
ancestry and compares them with durable `ManagedAttachment` evidence. Any
change rejects reconnect. The two process fields are optional claim-side
corroboration under the same all-or-none/exact-match rule as bootstrap; they are
not substitutes for live capture. Success creates a new generation-scoped
binding and rebinds the exact durable attachment/native session.

Attachment IDs and product-native session IDs are separate namespaces. An
explicitly selected and re-attested resume may bind a fresh attachment to an
existing native session (for example, Claude resume-by-name). Reconnect must
validate the recorded attachment-to-native-session relationship and exact
process evidence; it must not require the two identifier strings to be equal.

## 4. Component-to-Daemon Frames

| Type | Required payload | Meaning |
|---|---|---|
| `session.announce` | binding, native session ID, cwd, native title observation, product event seq | Corroborate/select exact session; empty title is confirmed absence |
| `session.rebind` | old/new native session ID evidence | Resume/switch; daemon applies product rules |
| `session.rename` | native session ID, native name, product event seq | Native-confirmed external name update, or correlated response to `session.rename.request` |
| `session.state` | native session ID, `idle|busy`, product event seq | Presence/turn observation |
| `session.close` | native session ID, reason | Component observed session closure |
| `delivery.accept` | delivery ID, native session ID, native message ID, accepted time | Exact native acceptance |
| `delivery.reject` | delivery ID, stable error category, redacted detail | Native refusal/failure |
| `turn.event` | native session ID, event seq, kind, bounded metadata | Start/settled/failed/permission event |
| `tool.call` | call ID, tool operation, bounded arguments | Product-native Agent Sessions parent tool |
| `tool.cancel` | call ID | Cancel a still-live tool call |
| `heartbeat` | binding ID, last received seq | Liveness only; never session authority |

Product-provided native session IDs are corroborated with binding context and
product-specific evidence. They are not trusted merely because a JSON field
contains them.

## 5. Daemon-to-Component Frames

| Type | Required payload | Meaning |
|---|---|---|
| `session.bound` | binding ID, attachment ID, native session ID, public name | Daemon accepted exact session binding |
| `session.rename.request` | native session ID, requested name | Write through a title change to the exact product-native session |
| `delivery.present` | delivery ID, optional receipt ID, mode, bounded AgentFrame body | Inject/wake/steer exact session |
| `tool.result` | call ID, success/error category, bounded result | Complete parent tool operation |
| `generation.retire` | binding ID, generation | Stop accepting old-generation work |
| `heartbeat.ack` | binding ID, last received seq | Liveness response |
| `reject` | operation/frame ID, stable category, redacted detail | Fail-closed rejection |

`delivery.present` remains outstanding until accept/reject. A disconnected
component does not imply delivery failure; the durable delivery engine applies
its bounded retry/recovery policy.

### Native rename write-through

`session.rename.request` uses a stable daemon operation ID in the
`daemon.rename.` namespace. A supporting component calls its product-native
rename API and replies with the existing `session.rename` frame using the same
frame ID and exact `{native_session_id, native_name, product_event_seq}`. The
returned native name MUST equal the requested name; only that exact
product-native confirmation, durably admitted by the daemon handler, is
acceptance. The broker validates correlation first, commits the confirmed title
projection second, and only then completes the waiting caller. A transient
projection failure remains retryable under the same stable operation ID so the
client can replay its exact product confirmation and the projection can
converge without a second product rename. Unsupported or failed callbacks
return a correlated `reject` with a stable category. Callback execution has a
bounded deadline and cancellation signal; timeout, disconnect, stop, or an
older client rejecting the unknown request maps to the component-local typed
`unsupported`/`unavailable`/`timed-out` result seam and MUST NOT crash either
side. Late callback results after cancellation are ignored.

Daemon requests and component-originated observations occupy disjoint frame-ID
namespaces. `daemon.rename.*` identifies an outstanding daemon write-through;
`component.rename.*` identifies an unsolicited product-native title event. A
`session.rename` matching an outstanding daemon ID is its response; every
other valid `session.rename` MUST use the component namespace and is an
unsolicited observation. Namespace collisions and daemon-space responses with
no outstanding request fail closed.

Component-originated title observations have one rule in every Go and
JavaScript boundary: the value is valid UTF-8, at most 1024 bytes, and contains
no Unicode control rune. Empty is a confirmed absence and product whitespace
is preserved. This rule applies to `session.announce.native_name` and to a
`component.rename.*` `session.rename` observation. It does not relax the
daemon write path: `session.rename.request.requested_name` and its correlated
`daemon.rename.*` response remain nonempty, trim-safe, and exactly equal.
Unsafe or oversized product observations fail closed and MUST NOT be
truncated, fabricated, or admitted into a native-title follower.

The product-native title is the single mutable writer. Agent Sessions sends a
write-through request and derives its displayed name from the confirmed native
title; it MUST NOT establish or maintain an independent mutable rename
baseline. Neither the broker replay cache nor the shared client adds
product-side durable persistence: request/response replay state is bounded and
memory-only.

## 6. Ordering and Idempotency

- Sequence numbers are binding-local and monotonic.
- The broker maintains a bounded same-generation replay window.
- Operation IDs (`delivery_id`, `call_id`) are the idempotency authority, not
  frame sequence.
- Rename request IDs are stable operation IDs. Same ID plus the same exact
  request body replays the same bounded result without calling the product
  twice; the same ID with a different body is rejected.
- A rename response must match the outstanding operation's exact native
  session and requested name. A mismatched response is rejected and never
  updates the daemon's projected name.
- Cross-generation replay is resolved through durable delivery/tool/lane state.
- Duplicate `delivery.accept` with identical native evidence is idempotent;
  conflicting evidence is rejected.
- Component event gaps cause refresh/re-attestation, not guessed state.
- Heartbeats prove only that the binding connection is responsive.

## 7. Shared Client and Product Adapters

`integrations/shared/component` owns framing, reconnect, heartbeat, bounded
queues, redaction, and tool-call correlation. Product adapters supply only:

- native session discovery/identity;
- native rename/state events;
- delivery mapping (`wake`, `steer`, `follow-up`, or reject);
- turn/completion events;
- product-native tool registration.

The shared client additionally exposes one native rename callback used by all
component products. It returns the exact native name and product event
sequence, emits the correlated `session.rename`, and applies a bounded
same-operation replay cache. There is no product-specific rename side channel.

The shared client must work in OpenCode/Kilo plugins, Pi/OMP extensions, and the
DSH Cordis plugin without weakening the evidence required by any product.
CodeBuddy is not a component-broker participant: its product-owned peer endpoint
is adopted through native registry/process/socket evidence and its lane server
is supervised through the typed product-server client.

## 8. Security and Failure Rules

- Raw bootstrap values are memory-only, redacted, never persisted, and never
  emitted in diagnostics.
- Same UID is necessary but not sufficient for attachment selection; exact
  process/ancestry evidence remains required.
- Symlinked or changed runtime paths fail closed.
- Binding death never authorizes another native process.
- Rename diagnostics are redacted and bounded; outstanding and completed
  request state has the broker/client fixed bounds and is never durable.
- An unknown `session.rename.request` from a contract-mismatched pinned client
  degrades to typed unsupported/unavailable. It is never treated as success.
- No component may broaden native permission policy or make a pluginless native
  session managed by ambient installation alone.
