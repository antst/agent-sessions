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

## 2. Initial Bootstrap

Managed wrapper launch provides:

- attachment ID;
- opaque one-time bootstrap capability ID;
- raw one-time bootstrap value in a redacted ephemeral environment entry;
- expected product and executable/process lineage.

The first component frame is:

```text
bootstrap {
  product_id,
  attachment_id,
  bootstrap_capability_id,
  bootstrap_value,
  process_start,
  strong_start,
  component_version
}
```

The daemon validates the capability hash/revision, consumes it once, validates
kernel peer PID/UID, captures exact process identity and ancestry, and checks
them against the prepared attachment. Success returns:

```text
ready { binding_id, attachment_id, daemon_generation, protocol_version,
        max_frame_bytes, heartbeat_interval_ms }
```

An ambient globally installed component without a prepared bootstrap remains
inert and does not retry privileged operations.

### Handshake idempotency and adoption

1. `ComponentBinding` keyed by `(attachment_id, bootstrap_revision)` is the
   durable handshake-idempotency anchor. `Authorizer.Bootstrap` and
   `Authorizer.Reconnect` MUST transactionally create-or-return its exact
   secret-free `binding_id`; the broker MUST use that ID and MUST NOT mint a
   replacement after authorization.
2. A lost `ready` MAY be retried only with the identical capability ID/value,
   attachment, product, bootstrap revision, and freshly recaptured kernel
   PID/UID, process start/strong-start, executable, and ancestry. While the
   binding remains unadopted, an exact retry returns the same committed
   `binding_id`, including after daemon restart. Any foreign, stale,
   conflicting, or already-adopted replay is rejected.
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

## 3. Reconnect

After daemon restart, the same native process sends:

```text
reconnect { attachment_id, prior_binding_id, prior_generation,
            process_start, strong_start, last_received_seq }
```

The daemon does not require a reusable bearer secret or Ed25519 signature. It
re-captures kernel peer PID/UID, process start, strong start, executable, and
ancestry and compares them with durable `ManagedAttachment` evidence. Any
change rejects reconnect. Success creates a new generation-scoped binding and
rebinds the exact durable attachment/native session.

Attachment IDs and product-native session IDs are separate namespaces. An
explicitly selected and re-attested resume may bind a fresh attachment to an
existing native session (for example, Claude resume-by-name). Reconnect must
validate the recorded attachment-to-native-session relationship and exact
process evidence; it must not require the two identifier strings to be equal.

## 4. Component-to-Daemon Frames

| Type | Required payload | Meaning |
|---|---|---|
| `session.announce` | binding, native session ID, cwd, native name, product event seq | Corroborate/select exact session |
| `session.rebind` | old/new native session ID evidence | Resume/switch; daemon applies product rules |
| `session.rename` | native session ID, native name, product event seq | Native-confirmed external name update |
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
| `delivery.present` | delivery ID, optional receipt ID, mode, bounded AgentFrame body | Inject/wake/steer exact session |
| `tool.result` | call ID, success/error category, bounded result | Complete parent tool operation |
| `generation.retire` | binding ID, generation | Stop accepting old-generation work |
| `heartbeat.ack` | binding ID, last received seq | Liveness response |
| `reject` | operation/frame ID, stable category, redacted detail | Fail-closed rejection |

`delivery.present` remains outstanding until accept/reject. A disconnected
component does not imply delivery failure; the durable delivery engine applies
its bounded retry/recovery policy.

## 6. Ordering and Idempotency

- Sequence numbers are binding-local and monotonic.
- The broker maintains a bounded same-generation replay window.
- Operation IDs (`delivery_id`, `call_id`) are the idempotency authority, not
  frame sequence.
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
- No component may broaden native permission policy or make a pluginless native
  session managed by ambient installation alone.
