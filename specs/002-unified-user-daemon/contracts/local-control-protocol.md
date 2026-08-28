# Contract: Unified Local Control Protocol

## Purpose

This protocol is the only Agent Sessions-owned local listener on a user-host. Short-lived launchers,
hooks, administrative commands, and vendor-required MCP relays use it to reach the one daemon. Daemon
subsystems do not use this protocol to call one another.

## Endpoint

- Linux: `$XDG_RUNTIME_DIR/agent-sessions/daemon.sock`
- macOS: `/tmp/agent-sessions-$UID/daemon.sock`
- Parent directory: owned by the current UID, mode `0700`, real directory, exact expected path
- Socket: owned by the current UID, mode `0600`, real Unix socket, within the platform `sun_path` limit
- The production endpoint is not selected from `TMPDIR`, a profile, cwd, product, group, or session.
- Test-only path injection is an in-process test seam and is not a second supported daemon environment.

The daemon holds an exclusive lock adjacent to the endpoint. A second daemon fails before unlinking or
binding anything. A stale endpoint is removed only after its durable record and exact process identity
prove the prior daemon absent.

## Transport

- Unix stream socket
- UTF-8 newline-delimited JSON
- Maximum encoded local frame: 2 MiB
- Existing message/tool content maximum: 1 MiB
- One request produces one response with the same `request_id`
- Long-running connections may carry daemon notifications only after an explicit subscription

An explicit subscription is a generation-bound frame on an already attested connection:

```json
{"type":"subscribe","version":1,"request_id":"unpredictable-id","expected_generation":42,"topics":["lane.notice","peer.inbox"]}
```

Success returns `subscribe.result` with the same request ID, current `daemon_generation`, and the
canonical sorted topic list. The closed topic inventory is role-scoped: admin/service may subscribe to
`runtime.state`; launcher may subscribe to `attachment.state` and `lane.notice`; hook may subscribe to
`attachment.state`; connector may subscribe to `lane.notice` and `peer.inbox`. Empty, duplicate,
unknown, forbidden, or stale-generation subscriptions are rejected before any notification is sent.

## Connection identity

The daemon obtains the peer UID, PID, and kernel process-start evidence from the socket and shared
process primitives before accepting a role. Client-supplied process fields are corroboration only.

The API assigns one role per connection:

- `admin`: metadata-only status/doctor and explicit administrative transactions
- `launcher`: peer preparation, native selection adoption, attachment detach, and lane lifecycle
- `hook`: product hook events for an already prepared attachment
- `connector`: model-facing MCP requests for one exact attachment
- `service`: installer/migrator transaction under the exact install lock

Role scoping prevents accidental capability exposure through model-facing tool inventories. It is not
claimed as a security boundary against arbitrary code already running as the owning OS user.

A connector launched by a bare native session sends only its product. The daemon admits that
connection solely for MCP initialization, ping, and tool discovery; `tools/call` returns the canonical
inactive result. A managed connector additionally sends the exact attachment ID and raw launch
capability. A known native session ID is optional corroboration; late-bound connectors remain in the
selecting state until the daemon adapter or native hook adopts the authoritative session. Partial
attachment claims are rejected rather than interpreted as either bare or managed.

## Hello frame

The first frame must be:

```json
{
  "type": "hello",
  "version": 1,
  "request_id": "unpredictable-id",
  "role": "connector",
  "product": "qwen",
  "attachment_id": "launch-scoped-id",
  "session_id": "optional-native-selection",
  "capability": "raw-daemon-issued-capability"
}
```

Fields not applicable to the selected role are omitted. The daemon validates:

1. protocol compatibility;
2. same-user kernel identity;
3. role-specific executable/ancestry evidence where required;
4. exact prepared attachment, product, and raw capability where required;
5. current daemon generation and state readiness.

Success returns:

```json
{
  "type": "hello.result",
  "version": 1,
  "request_id": "unpredictable-id",
  "daemon_generation": 42,
  "runtime_version": "0.3.0",
  "role": "connector",
  "attachment_id": "launch-scoped-id",
  "session_id": "authoritative-native-id"
}
```

The response may omit `session_id` while a late-bound native selection remains unresolved. That
attachment cannot use discovery, messaging, or lane operations until adoption commits.

## Request envelope

```json
{
  "type": "request",
  "version": 1,
  "request_id": "unpredictable-id",
  "operation": "peer.send",
  "expected_generation": 42,
  "expected_revision": "optional-resource-revision",
  "payload": {}
}
```

## Response envelope

```json
{
  "type": "response",
  "version": 1,
  "request_id": "unpredictable-id",
  "operation": "peer.send",
  "daemon_generation": 42,
  "accepted": true,
  "resource_revision": "new-revision",
  "result": {}
}
```

Failures return `accepted: false` with:

```json
{
  "error": {
    "code": "attachment_not_attested",
    "message": "managed Qwen attachment is no longer corroborated",
    "retryable": false
  }
}
```

`accepted: true` means the exact operation ownership was durably committed. A transport disconnect
before a response is ambiguous; retrying the same idempotency key returns the committed result or the
current retryable state rather than duplicating work.

## Operation inventory

This is the inventory of operations accepted by the running daemon endpoint. Offline install,
rollback, removal continuation after service stop, and explicit purge invoke
the same implementation packages directly from the canonical executable under the install lock. They
do not start a daemon or send mutation requests to an unavailable daemon endpoint.

| Operation | Allowed role | Contract |
|---|---|---|
| `runtime.status` | admin, service | Metadata-only runtime, adapter, lane, federation, and debt state |
| `runtime.doctor` | admin | Read-only readiness and cause-specific remediation |
| `attachment.prepare` | launcher | Durably reserve launch identity, preferences, capability hash, and expected native evidence |
| `attachment.adopt` | launcher, hook | Atomically bind a late-selected authoritative native session ID |
| `attachment.refresh` | launcher, hook | Update exact live native evidence without changing functional identity |
| `attachment.detach` | launcher, hook | End one exact attachment revision; retain durable session preferences |
| `mcp.forward` | connector | Relay one bounded MCP method and parameters; the daemon owns tool inventory, authorization, routing, and result shaping |
| `peer.identity` | connector | Return the connector's own attested participant identity |
| `peer.discover` | connector | Existing group-filtered peer discovery |
| `peer.send` | connector | Existing direct or explicit-target multicast delivery |
| `peer.broadcast` | connector | Existing named-global-group broadcast |
| `peer.inbox` | connector | Existing bounded pending/handled message projection |
| `peer.rename` | connector | Existing managed display-name update |
| `lane.start` | launcher, connector | Durably accept one local/remote product lane start |
| `lane.resume` | launcher, connector | Resume an existing durable lane through its vendor adapter |
| `lane.followup` | launcher, connector | Durably accept a turn for an idle lane |
| `lane.status` | launcher, connector | Return exact lane and active-turn state |
| `lane.list` | launcher, connector | Existing group/parent-scoped lane inventory |
| `lane.interrupt` | launcher, connector | Interrupt one exact running native turn |
| `lane.collect` | launcher, connector | Idempotently advance and return the durable collection cursor |
| `lane.archive` | launcher, connector | Archive through the exact product-native contract |
| `remove.inspect` | admin, service | Enumerate exact active blockers and removal targets |

Connector role operations are further limited by the existing attested session, product, groups,
parent context, and permission rules. No daemon-administration operation appears in MCP `tools/list`.

## Daemon unavailable contract

If the socket is absent, refused, incompatible, or names an uncorroborated daemon:

- workflow commands exit nonzero;
- MCP tool results report `agent_sessions` unavailable/inactive without native fallback;
- no command starts, stops, restarts, replaces, or repairs the service;
- no success or acceptance result is returned;
- the diagnostic names the standard service-manager command used to inspect the daemon.

## Restart contract

- Clients reconnect to the fixed endpoint and receive the new committed generation.
- A request carrying an obsolete generation is rejected before mutation and may be safely retried after
  re-reading status.
- Accepted message/turn IDs remain idempotent across generations.
- Stateless connectors may remain running across daemon restart and reconnect; all behavior decisions
  are made by the new daemon.

## Logging contract

The daemon may log request ID, operation, role, product, bounded identity references, state transition,
duration, error code, and revision. It must never log `payload`, message content, lane input/result,
tool arguments/results containing user content, raw launch capability, or vendor transcript data.
