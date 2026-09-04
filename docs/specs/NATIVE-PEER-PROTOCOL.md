# Native Agent Sessions Presence Protocol

Status: protocol version 1.

This specification defines the small native connection a coding-agent product
implements to participate in Agent Sessions without a product-specific plugin.
Agent Sessions implements this protocol directly.

## 1. Purpose and trust model

Agent Sessions is not a workflow or graph orchestrator; every session or lane
runs whatever it likes internally. It is where independent agents and their
humans discover one another, exchange messages, and converge through discussion.
Disagreement is information to resolve with conversation and evidence, not
noise to average across copies. Persistent participants with different skills,
products, and models can keep talking across hours and restarts, and a human can
step in at any seam. A native presence client lets a product join as a
first-class participant without an adapter.

Agent Sessions assumes a trusted environment: one held connection per live
session is the complete liveness proof, and there is no authentication layer
inside a host. Socket permissions and network isolation are deployment
boundaries, not parts of this protocol.

## 2. Finding the daemon

The daemon listens on a Unix-domain socket named `run/presence.sock` beneath
its state root. A client resolves the socket in this order:

1. `AGENT_SESSIONS_PRESENCE_SOCKET`, when non-empty;
2. `$AGENT_SESSIONS_STATE_ROOT/run/presence.sock`;
3. `$XDG_STATE_HOME/agent-sessions/run/presence.sock`;
4. `$HOME/.local/state/agent-sessions/run/presence.sock`.

The daemon creates the containing directory with mode `0700` and the socket
with mode `0600`. A custom daemon state root must be published through
`AGENT_SESSIONS_PRESENCE_SOCKET` or `AGENT_SESSIONS_STATE_ROOT` so clients can
find it.

### Launch environment

When Agent Sessions launches an interactive product, these existing variables
carry launch context. A native client treats them as hints; the product remains
the authority for the session UUID and stored title.

| Variable | Meaning |
| --- | --- |
| `AGENT_SESSIONS_PRODUCT` | Product string identifier. |
| `AGENT_SESSIONS_SESSION_ID` | Exact session ID when identity is known at launch; absent for a fresh product-generated identity. It is never a request to invent a product session ID. |
| `AGENT_SESSIONS_SESSION_NAME` | Requested start-time title, or an empty string. |
| `AGENT_SESSIONS_GROUPS` | JSON array of group strings collected from the launch arguments. |

Every Agent Sessions launcher uses this one context. A tools-only connector
also receives `AGENT_SESSIONS_LANE_CAPABILITY` and
`AGENT_SESSIONS_HOST_BINARY` when those values apply. Launchers preserve an
explicit socket or state root already present in their environment. A product
started outside Agent Sessions may set the socket directly or use the
conventional path, choose its own groups, and report its built-in product
identifier. No registration command, plugin manifest, secret, or prior catalog
entry is required.

## 3. The first frame: `session.hello`

The first line on every connection is a JSON-RPC 2.0 request:

```json
{"jsonrpc":"2.0","id":1,"method":"session.hello","params":{"protocol":1,"uuid":"01J8YF6M7W1A2B3C4D5E6F7G8H","name":"reviewer","groups":["team"],"product":"opencode","info":{"model":"gpt-5.6-sol","cwd":"/work/project"}}}
```

The daemon validates and acknowledges the report, then installs the connection
as live:

```json
{"jsonrpc":"2.0","id":1,"result":{}}
```

The client must wait for this result before sending another method. The fields
mean:

- `protocol` is the integer `1`. No other value is accepted.
- `uuid` is the stable, opaque session ID issued by the product. The historical field need not be an RFC UUID. It is 1..128 UTF-8 bytes with no whitespace, control characters, or `/`; the client never generates or substitutes it.
- `name` is the product-owned title carried verbatim. It may be empty before a title exists and may contain whitespace or `/`, but is at most 256 UTF-8 bytes and contains no control characters. Rosters display `uuid` while it is empty.
- `groups` is the array of group strings passed from the launch arguments,
  unchanged and in order. An operator/wire group label is 1..192 UTF-8 bytes with no whitespace, control characters, or `/`. The daemon-derived private anchor is the typed structural value `session:<host>/<uuid>` and is valid only when its host and native-ID components satisfy their own slash-free grammars. The array is fixed for the life of this connection.
- `product` is an opaque token-shaped label: 1..64 bytes, a lowercase ASCII letter first, then lowercase ASCII letters, digits, or single nonterminal hyphens. The daemon carries it verbatim and never interprets it as a launch capability.
- `info` is a string-to-string object owned by the product. The daemon passes
  every key and value through verbatim to rosters and `peers.list` results and
  never interprets them except that a present `cwd` must be absolute and
  non-empty and becomes the caller's lane directory.
- `capabilities` is optional. It is a closed object; version 1 defines only
  `"lane":true`, meaning this session accepts the daemon-to-product lane
  requests in section 4. Unknown keys and `lane:false` are invalid.

Products may add any `info` keys. Unknown keys are valid by design. Two keys
have well-known display meanings and SHOULD be set whenever the product knows
them:

- `model` is the model currently used by the session, such as `gpt-5.6-sol` or
  `opus-4.8`. The product updates it whenever the user switches models.
- `cwd` is the session's working directory. A product that connects on its own
  is responsible for knowing this value because the daemon cannot infer it.

The map is informational only and never affects routing or permissions. It is
live, in-memory, and product-owned. The daemon stores none of it durably: it
retains the map only on the live connection and discards it when that
connection ends.

An unsupported protocol version receives error `-32004`, after which the
daemon closes the connection:

```json
{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Unsupported protocol version","data":{"supported":1,"received":2}}}
```

Invalid identity shapes receive `-32602` with `data:{"method":"session.hello"}` and close the connection.
A second `session.hello`, any notification, batch, or malformed frame closes without a response.

An unknown product identifier is not an error. It is accepted for presence,
rosters, peer discovery, and messaging, and is displayed as reported. Catalog
knowledge is required only when Agent Sessions is asked to launch or drive
that product as a lane. This is the adoption rule: **you can join with four
identity fields and an info map; nothing else is required.** The map may be
empty and capabilities may be omitted.

## 4. The JSON-RPC stream

After `session.hello`, each line is one UTF-8 JSON-RPC 2.0 object followed by
`\n`. Requests contain `jsonrpc`, `id`, `method`, and `params`.
Notifications omit `id`. Responses contain the same string or numeric `id`
and exactly one of `result` or `error`. Batch arrays are not used.

Products may reuse their existing JSON-RPC libraries with newline framing and
batching disabled. Calls can be concurrent; an ID must be unique while its
call is outstanding. Every version 1 method is a request with an ID. Although
JSON-RPC supports notifications, version 1 assigns none because every operation
needs a success or error result.

The encoded object plus its newline is at most 1 MiB. Clean between-frame EOF
ends the stream; any non-empty tail is truncated and an over-bound frame invalid.

### Error codes

Errors use the standard JSON-RPC error object
`{"code": INTEGER, "message": STRING, "data": OPTIONAL_JSON}`. Version 1 has
this closed error table:

| Code | Canonical message | Meaning |
| ---: | --- | --- |
| `-32602` | `Invalid params` | The method is known, but its parameters do not match the method schema. |
| `-32001` | `Unknown session or target` | A session or message destination does not exist or is not visible to the caller. |
| `-32002` | `Session busy` | The addressed product session cannot accept an inbound message, or a non-steer lane operation is blocked by its current state. `lane.steer` does not use this code. |
| `-32003` | `Operation not permitted` | The caller is not allowed to perform the operation, the method is not part of version 1, or `lane.steer` cannot be performed. A steer failure sets `data.reason` to exactly `"no running turn"` or `"steer unsupported"`. |
| `-32004` | `Unsupported protocol version` | `session.hello.params.protocol` is not exactly `1`. |
| `-32005` | `Product not launchable` | A lane method names a product the daemon cannot launch or drive. |
| `-32006` | `Product operation failed` | A native product or driver operation failed for a reason not represented by another code. The product's `message` and `data` values are preserved without reclassification. |

Messages use these closed data shapes: `-32602` has `{"method":...}`; `-32001` has `{"target":...}`; `-32002` has `{"uuid":...}`; `-32003` has the applicable `method`, `reason`, or `group`; `-32004` has `{"supported":1,"received":...}`; and `-32005` has `{"product":...}`. A `method` value is non-empty with no whitespace or control characters, and `received` is an integer in the JSON-RPC safe range. Fallback `-32006` has `detail` and `agent_sessions_bug_report`. A product's valid structured `-32006` keeps its code, message, and data JSON values unchanged, though the enclosing frame may be reserialized. No other error code is emitted. Every accepted request receives exactly one response.

### Product to daemon

#### `session.update`

`session.update` replaces the product-owned name and complete info map:

```json
{"jsonrpc":"2.0","id":"update-1","method":"session.update","params":{"name":"reviewer: tests complete","info":{"model":"opus-4.8","cwd":"/work/project"}}}
{"jsonrpc":"2.0","id":"update-1","result":{}}
```

Both fields are required and replace their previous values; omitted `info`
keys are removed. The UUID, product, and groups remain those from
`session.hello`. Any attempt to include or change them is rejected, so the
product learns whether its update was applied. The complete update applies the same absolute-or-absent `info.cwd` rule:

```json
{"jsonrpc":"2.0","id":"update-2","method":"session.update","params":{"name":"reviewer","info":{},"groups":["other-team"]}}
{"jsonrpc":"2.0","id":"update-2","error":{"code":-32602,"message":"Invalid params","data":{"method":"session.update"}}}
```

Changing groups requires a new connection and a new `session.hello`.

#### `peers.list`

```json
{"jsonrpc":"2.0","id":"peers-1","method":"peers.list","params":{}}
{"jsonrpc":"2.0","id":"peers-1","result":{"peers":[{"id":"target-native","session_id":"target-native","name":"builder","product":"new-agent","status":"live","cwd":"","groups":["team"],"permission_mode":"default","info":{"model":"opus-4.8","cwd":"/work/project"}}]}}
```

The daemon returns live destinations that share a group with the caller. An
uncatalogued product appears with its reported product identifier. Every peer's
`info` is the exact current map supplied by that product.

#### `message.send`

```json
{"jsonrpc":"2.0","id":"send-1","method":"message.send","params":{"target":"target-native","message":"Please review this."}}
{"jsonrpc":"2.0","id":"send-1","result":{"message_id":"9a58e98f39cc2d47a1c3f09a77bc8310","deliveries":[{"target":"target-native","session_id":"target-native","delivery_id":"delivery-718e3cdb1e3f61786d95879632973bb7","status":"accepted"}]}}
```

`target` selects one visible peer by exact identity or unique name. `targets`
may instead select a non-empty array of distinct peer selectors. `group`
selects every other current local or federated member of one group the sender
belongs to. The three forms are mutually exclusive. A group containing no
other current members succeeds with an empty `deliveries` array. Otherwise, success means every
selected product accepted its delivery. A missing, invisible, ambiguous, or
self target returns:

```json
{"jsonrpc":"2.0","id":"send-2","method":"message.send","params":{"target":"missing","message":"Hello"}}
{"jsonrpc":"2.0","id":"send-2","error":{"code":-32001,"message":"Unknown session or target","data":{"target":"missing"}}}
```

A group outside the sender's effective membership returns `-32003` with
`{"group":"<requested-group>"}` as its data.

#### Lane methods

The version 1 lane methods are `lane.doctor`, `lane.list`, `lane.start`,
`lane.run`, `lane.resume`, `lane.steer`, `lane.wait`, `lane.status`,
`lane.interrupt`, and `lane.archive`.

Start, run, resume, and steer use:

```json
{"product":"qwen","arguments":["--name","reviewer","--group","team"],"input":"Review the change.","cwd":"/work/project","host":"optional-remote-host"}
```

Doctor, list, wait, status, interrupt, and archive omit `input`:

```json
{"product":"qwen","arguments":["reviewer","--timeout","300"],"cwd":"/work/project","host":"optional-remote-host"}
```

`product` selects the lane product. `arguments` is the bounded lane command
argument array without the method name. `input` is required and non-empty
where shown. `cwd` is an optional absolute override; when omitted, the daemon
uses the live caller's `session.hello.info.cwd`. `host` is optional.
The daemon returns `-32005` when it has no launcher for the requested product.

`lane.steer` addresses a lane that already has a running turn. The daemon
delivers its non-empty `input` to that exact turn and returns `turn.steered`
with the lane's product-native `session_id`, the running `turn_id`, and `native_message_id`,
the message ID returned by the product. The input must influence that current
turn. An implementation that merely queues it for a later turn is not
conforming.

An unknown lane returns `-32001`. A known lane with no running turn returns
`-32003` with `data.reason:"no running turn"`. A product or lane driver that
does not support steer returns `-32003` with
`data.reason:"steer unsupported"`. `lane.steer` never returns `-32002` merely
because the lane is not running.

### Daemon to product: `message.deliver`

The daemon sends plain message text and a structured sender:

```json
{"jsonrpc":"2.0","id":"delivery-1","method":"message.deliver","params":{"message_id":"9a58e98f39cc2d47a1c3f09a77bc8310","from":{"uuid":"source-native","name":"reviewer","product":"opencode","groups":["team"]},"body":"Please review this."}}
{"jsonrpc":"2.0","id":"delivery-1","result":{}}
```

The product renders sender metadata natively. It must route `body` to the exact
session represented by this connection and wake a model turn without terminal
keystrokes or screen scraping. `result:{}` means the input was accepted and a
model turn will run. A product that cannot accept the input returns a standard
error:

```json
{"jsonrpc":"2.0","id":"delivery-2","method":"message.deliver","params":{"message_id":"message-2","from":{"uuid":"source-native","name":"reviewer","product":"opencode","groups":["team"]},"body":"One more check."}}
{"jsonrpc":"2.0","id":"delivery-2","error":{"code":-32002,"message":"Session busy","data":{"uuid":"target-native"}}}
```

The daemon does not wrap `body`, queue it for a disconnected product, or
synthesize acceptance.

The daemon carries only structured `message_id`, `from`, and `body` values. Exactly
one client-side renderer wraps them immediately before native input as:

```text
<cross-session-message from="NAME_OR_UUID" from-session="UUID">
[codex-peer-metadata: {"fromProduct":"PRODUCT","messageId":"ID","groups":[...]}]
BODY
</cross-session-message>
```

That renderer owns attribute filtering and JSON escaping; no earlier layer pre-renders, truncates, or strips the message ID.

### Daemon to lane-capable product sessions

A normal session accepts only `message.deliver`; a lane-capable session also
accepts exactly the four methods below. Any other daemon request returns
`-32003` with `data.method` before a product callback runs.

A session that reports `capabilities:{"lane":true}` is driven over its held
presence connection. The daemon sends the following requests to that exact
session; a client that did not advertise the capability rejects them with
`-32003`.

`lane.turn.start` submits one input and acknowledges only after the product has
accepted it into its native inbox:

```json
{"jsonrpc":"2.0","id":"daemon.input-1","method":"lane.turn.start","params":{"input_id":"input-1","body":"Review this change.","mode":"followup"}}
{"jsonrpc":"2.0","id":"daemon.input-1","result":{"native_message_id":"message-product-1"}}
```

`mode` is exactly `followup` or `steer`. `followup` starts its own ordinary
turn. `steer` enters the running turn at the product's next native step
boundary, or starts a turn if the product is idle. `input_id` is the daemon's
in-flight call identity; `native_message_id` is the identity issued by the
product. Neither is durable Agent Sessions state.

`lane.turn.wait` waits for the product turn that consumes that native message:

```json
{"jsonrpc":"2.0","id":"daemon.wait-1","method":"lane.turn.wait","params":{"native_message_id":"message-product-1"}}
{"jsonrpc":"2.0","id":"daemon.wait-1","result":{"outcome":"completed","result":"The change is sound.","reason":{"kind":"completed"}}}
```

`outcome` is `completed`, `interrupted`, or `failed`; `result` is the product's
assistant text; and `reason` is the product's native terminal reason without
reinterpretation. For DSH this is its discriminated object, such as
`{"kind":"completed"}`. DSH 0.1.2-rc.1 defines `TurnEndReason` through a merge-extensible `TurnEndReasonMap`; plugins may add variants. The adapter returns the `turn/end` event's `reason` value unchanged. The product owns input-to-turn correlation.
An unknown `native_message_id` returns `-32001` with that value in `data.target`;
a second waiter for known in-flight work remains a product operation failure.

`lane.turn.interrupt` takes `{}` and asks the product to cancel the active turn
without discarding already accepted follow-up input. It returns `{}` after the
native cancellation request is accepted. `lane.session.archive` also takes
`{}`; it cancels the session, flushes the product store, returns `{}`, and then
closes the presence connection as the product process exits. Title and info
changes continue to use `session.update`; there is no separate lane update
method.

A UI-less lane-capable session must never block on an interactive approval.
It resolves permissions from the invocation's launch policy or fails the
native operation. The Agent Sessions DSH adapter selects
`workspace-write-noninteractive` (workspace-write sandbox, approval `never`)
for ordinary lanes and DSH's `danger-full-access` preset for `--yolo` lanes.

### Model exposure

The presence methods are the wire contract, not a required model-facing tool
layout. A product may expose one tool with a closed operation enum or several
separate tools. Whichever layout it chooses, calls to `peers.list`,
`message.send`, and the supported lane methods must be structured and
promptless for the model.

## 5. Lifecycle semantics

- EOF or any connection loss removes the session immediately. There is no
  heartbeat and no grace period.
- After a daemon restart, the product reconnects and sends `session.hello`
  again with the same native session ID, current product-owned name, original groups, and
  current info map. The daemon rebuilds its live roster from hello requests.
- A newer connection whose successful hello reports the same UUID replaces and
  closes the older connection. The displaced connection cannot later remove
  the replacement.
- A process hosting several live sessions holds one independent connection per
  session. A multi-root product emits created, changed, and disposed events per
  root and owns one presence loop per root.
- On disconnect, outstanding calls in both directions fail. Neither side
  replays calls after reconnect; the caller decides whether an operation is
  safe to retry.
- Shipped clients reconnect every two seconds while the product session lives; there is no replay or heartbeat.

## 6. Product responsibilities

A conforming product provides:

- A stable, product-issued native session ID at or near session start.
- A requested start-time name written into the product's own session store,
  not merely displayed in the terminal or remembered by Agent Sessions.
- A current product-owned info map, including `model` and `cwd` whenever known,
  with changes reported for the exact session that changed.
- Native resume by exact native session ID and, where the product supports named sessions,
  lookup by name that returns exactly one product session or fails truthfully
  on zero or multiple matches.
- An addressable API that accepts an inbound message for one exact session and
  wakes a model turn without keystroke injection. A session that cannot accept
  that inbound message returns `-32002`; lane steering instead follows the
  `lane.steer` running-turn and `-32003` rules.
- A finite, structured, promptless model surface for the version 1 Agent
  Sessions methods.
- Truthful exit: the presence connection closes as soon as the product session
  is no longer live.
- Per-session lifecycle, title, and info events when one process hosts several
  sessions.

## 7. Minimal implementation

The following pseudocode is the complete shape of a native client. A production
client adds normal cancellation, reconnect backoff, logging, and bounded I/O.

```text
presence(session):
  while session.is_live:
    socket, rpc = null, null
    try:
      socket = connect(discover_presence_socket())
      rpc = JsonRpc2(socket, delimiter="\\n", batching=false)
      rpc.on_request("message.deliver", params => {
        if not session.can_accept_input:
          throw RpcError(-32002, "Session busy")
        session.accept_user_input(params.body, sender=params.from)
        return {}
      })
      await rpc.call("session.hello", {
        protocol: 1,
        uuid: session.uuid,
        name: session.stored_name_or_empty,
        groups: session.launch_groups,
        product: PRODUCT,
        info: session.current_info,
        capabilities: session.can_drive_lanes ? {lane: true} : omitted
      })
      session.on_presence_change((name, info) =>
        rpc.call("session.update", {name: name, info: info}))
      session.expose_promptless_methods(method =>
        rpc.call(method.name, method.params))
      await first_of(rpc.eof, session.disposed)
    catch ConnectionFailedOrDisconnected:
      if rpc: rpc.fail_all_pending_without_replay()
      if session.is_live: continue
    finally:
      close_if_open(socket)
  return
```

## 8. Conformance

Conformance is checked end to end against the real product. Version 1 applies the runtime's standard IEEE-754 parser before checking numeric IDs and `data.received`; raw numeric lexical equivalence (including rounded fractions, exponents, and minus zero) remains explicit 0.4.1 debt. Client-side state-root/presence discovery errors still synthesize an unavailable endpoint; non-mutating discovery with the original error preserved is also 0.4.1 debt. Concurrent client calls have unique monotonic IDs but no response-order guarantee; ordering remains 0.4.1 debt. Identifier generation relies on the terminating `crypto/rand.Read` contract in the supported Go 1.24 or newer toolchain. The named 0.4.1 **product adapter boundary** move covers caller-surface policy, launch dispatch, executable resolution, lane bootstrap environment, the four connector product paths and their delivery/presence/native-ID guessing, title and candidate-name resolvers, compile-time driver population, Codex-only attachment registration, and hook attestation; shared code will retain only generic dispatch over adapter/descriptor declarations.

| Cell | Required observation |
| --- | --- |
| Named start | Starting with a name and groups creates a real product session that is immediately visible with the same stored name, exact groups, UUID, product string, and current info. |
| Live update | Changing the session name, model, or other info produces a successful `session.update`; the next roster read contains the complete replacement values. |
| Model turn | The named session completes a real model turn through the product's normal runtime. |
| Inbound round trip | `message.deliver` wakes that exact session; its model can call `message.send`, and the reply is accepted without an interactive approval prompt. |
| Steer | A replacement prompt sent while a long lane turn runs changes that turn's final output. |
| Daemon restart | While the product remains live, restarting the daemon makes the session reappear after a new hello with the same UUID, current stored name, original groups, and current info. |
| Exit | Exiting or disposing the product session makes it disappear immediately. |
| Resume | Resume by exact UUID and resume by unique name both restore and report the same product UUID. |

## Appendix A. Schemas and examples

All examples below are complete newline-delimited frames. Line breaks between
frames are shown literally by placing each JSON object on its own line.
The schemas describe JSON structure. The UTF-8 byte limits and character
grammars in section 3, and the canonical error messages and data shapes above,
are additional normative constraints because JSON Schema counts characters,
not encoded UTF-8 bytes.

### A.1 JSON-RPC envelopes

Request:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["jsonrpc", "id", "method", "params"],
  "properties": {
    "jsonrpc": {"const": "2.0"},
    "id": {"oneOf": [{"type": "string"}, {"type": "integer", "minimum": -9007199254740991, "maximum": 9007199254740991}]},
    "method": {
      "enum": [
        "session.hello", "session.update", "peers.list", "message.send",
        "lane.doctor", "lane.list", "lane.start", "lane.run", "lane.resume", "lane.steer", "lane.wait",
        "lane.status", "lane.interrupt", "lane.archive", "message.deliver",
        "lane.turn.start", "lane.turn.wait", "lane.turn.interrupt",
        "lane.session.archive"
      ]
    },
    "params": {"type": "object"}
  }
}
```

Success response:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["jsonrpc", "id", "result"],
  "properties": {
    "jsonrpc": {"const": "2.0"},
    "id": {"oneOf": [{"type": "string"}, {"type": "integer", "minimum": -9007199254740991, "maximum": 9007199254740991}]},
    "result": {}
  }
}
```

Error response:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["jsonrpc", "id", "error"],
  "properties": {
    "jsonrpc": {"const": "2.0"},
    "id": {"oneOf": [{"type": "string"}, {"type": "integer", "minimum": -9007199254740991, "maximum": 9007199254740991}]},
    "error": {
      "type": "object",
      "additionalProperties": false,
      "required": ["code", "message"],
      "properties": {
        "code": {"enum": [-32602, -32001, -32002, -32003, -32004, -32005, -32006]},
        "message": {"type": "string"},
        "data": {}
      }
    }
  }
}
```

### A.2 `session.hello`

Parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["protocol", "uuid", "name", "groups", "product", "info"],
  "properties": {
    "protocol": {"const": 1},
    "uuid": {"type": "string", "minLength": 1},
    "name": {"type": "string"},
    "groups": {"type": "array", "items": {"type": "string"}},
    "product": {"type": "string", "minLength": 1},
    "info": {"type": "object", "additionalProperties": {"type": "string"}},
    "capabilities": {
      "type": "object",
      "additionalProperties": false,
      "required": ["lane"],
      "properties": {"lane": {"const": true}}
    }
  }
}
```

Result: an empty object.

```json
{"jsonrpc":"2.0","id":1,"method":"session.hello","params":{"protocol":1,"uuid":"01J8YF6M7W1A2B3C4D5E6F7G8H","name":"reviewer","groups":["team"],"product":"opencode","info":{"model":"gpt-5.6-sol","cwd":"/work/project"}}}
{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"method":"session.hello","params":{"protocol":2,"uuid":"01J8YF6M7W1A2B3C4D5E6F7G8H","name":"reviewer","groups":["team"],"product":"opencode","info":{}}}
{"jsonrpc":"2.0","id":2,"error":{"code":-32004,"message":"Unsupported protocol version","data":{"supported":1,"received":2}}}
```

### A.3 `session.update`

Parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "info"],
  "properties": {
    "name": {"type": "string"},
    "info": {"type": "object", "additionalProperties": {"type": "string"}}
  }
}
```

Result: an empty object.

```json
{"jsonrpc":"2.0","id":"update-1","method":"session.update","params":{"name":"reviewer: tests complete","info":{"model":"opus-4.8","cwd":"/work/project"}}}
{"jsonrpc":"2.0","id":"update-1","result":{}}
{"jsonrpc":"2.0","id":"update-2","method":"session.update","params":{"name":"reviewer","info":{},"groups":["other-team"]}}
{"jsonrpc":"2.0","id":"update-2","error":{"code":-32602,"message":"Invalid params","data":{"method":"session.update"}}}
```

### A.4 Peer and message schemas

`peers.list` parameters are exactly `{}`.

```json
{
  "type": "object",
  "additionalProperties": false,
  "maxProperties": 0
}
```

`Peer`:

```json
{
  "$id": "urn:agent-sessions:v1:peer",
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "session_id", "name", "product", "status", "cwd", "groups", "permission_mode", "info"],
  "properties": {
    "id": {"type": "string"},
    "session_id": {"type": "string"},
    "name": {"type": "string"},
    "product": {"type": "string"},
    "status": {"enum": ["live", "idle", "busy"]},
    "cwd": {"type": "string"},
    "groups": {"type": "array", "items": {"type": "string"}},
    "permission_mode": {"type": "string"},
    "info": {"type": "object", "additionalProperties": {"type": "string"}},
    "kind": {"enum": ["lane", "remote-peer"]},
    "host_id": {"type": "string"}
  }
}
```

`peers.list` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["peers"],
  "properties": {
    "peers": {"type": "array", "items": {"$ref": "urn:agent-sessions:v1:peer"}}
  }
}
```

Frames:

```json
{"jsonrpc":"2.0","id":"peers-1","method":"peers.list","params":{}}
{"jsonrpc":"2.0","id":"peers-1","result":{"peers":[{"id":"target-native","session_id":"target-native","name":"builder","product":"new-agent","status":"live","cwd":"","groups":["team"],"permission_mode":"default","info":{"model":"opus-4.8","cwd":"/work/project"}}]}}
```

`message.send` parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["message"],
  "properties": {
    "target": {"type": "string", "minLength": 1},
    "targets": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {"type": "string", "minLength": 1}
    },
    "group": {"type": "string", "minLength": 1},
    "message": {"type": "string", "minLength": 1}
  },
  "oneOf": [
    {"required": ["target"]},
    {"required": ["targets"]},
    {"required": ["group"]}
  ]
}
```

`message.send` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["message_id", "deliveries"],
  "properties": {
    "message_id": {"type": "string", "minLength": 1},
    "deliveries": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["target", "session_id", "delivery_id", "status"],
        "properties": {
          "target": {"type": "string"},
          "session_id": {"type": "string"},
          "delivery_id": {"type": "string"},
          "status": {"const": "accepted"}
        }
      }
    }
  }
}
```

Frames:

```json
{"jsonrpc":"2.0","id":"send-1","method":"message.send","params":{"target":"target-native","message":"Please review this."}}
{"jsonrpc":"2.0","id":"send-1","result":{"message_id":"9a58e98f39cc2d47a1c3f09a77bc8310","deliveries":[{"target":"target-native","session_id":"target-native","delivery_id":"delivery-718e3cdb1e3f61786d95879632973bb7","status":"accepted"}]}}
{"jsonrpc":"2.0","id":"send-group","method":"message.send","params":{"group":"team","message":"Please review this."}}
{"jsonrpc":"2.0","id":"send-group","result":{"message_id":"47e671b79fa4412696d0df3557cad74f","deliveries":[]}}
{"jsonrpc":"2.0","id":"send-2","method":"message.send","params":{"target":"missing","message":"Hello"}}
{"jsonrpc":"2.0","id":"send-2","error":{"code":-32001,"message":"Unknown session or target","data":{"target":"missing"}}}
```

`message.deliver` parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["message_id", "from", "body"],
  "properties": {
    "message_id": {"type": "string", "minLength": 1},
    "from": {
      "type": "object",
      "additionalProperties": false,
      "required": ["uuid", "name", "product", "groups"],
      "properties": {
        "uuid": {"type": "string", "minLength": 1},
        "name": {"type": "string"},
        "product": {"type": "string", "minLength": 1},
        "groups": {"type": "array", "items": {"type": "string"}}
      }
    },
    "body": {"type": "string"}
  }
}
```

Result: an empty object.

```json
{"jsonrpc":"2.0","id":"delivery-1","method":"message.deliver","params":{"message_id":"9a58e98f39cc2d47a1c3f09a77bc8310","from":{"uuid":"source-native","name":"reviewer","product":"opencode","groups":["team"]},"body":"Please review this."}}
{"jsonrpc":"2.0","id":"delivery-1","result":{}}
{"jsonrpc":"2.0","id":"delivery-2","method":"message.deliver","params":{"message_id":"message-2","from":{"uuid":"source-native","name":"reviewer","product":"opencode","groups":["team"]},"body":"One more check."}}
{"jsonrpc":"2.0","id":"delivery-2","error":{"code":-32002,"message":"Session busy","data":{"uuid":"target-native"}}}
```

### A.5 Native lane-session schemas

`lane.turn.start` parameters and result:

```json
{"type":"object","additionalProperties":false,"required":["input_id","body","mode"],"properties":{"input_id":{"type":"string","minLength":1},"body":{"type":"string","minLength":1},"mode":{"enum":["followup","steer"]}}}
{"type":"object","additionalProperties":false,"required":["native_message_id"],"properties":{"native_message_id":{"type":"string","minLength":1}}}
```

`lane.turn.wait` parameters and result:

```json
{"type":"object","additionalProperties":false,"required":["native_message_id"],"properties":{"native_message_id":{"type":"string","minLength":1}}}
{"type":"object","additionalProperties":false,"required":["outcome","result","reason"],"properties":{"outcome":{"enum":["completed","interrupted","failed"]},"result":{"type":"string"},"reason":{}}}
```

`lane.turn.interrupt` and `lane.session.archive` each take `{}` and return `{}`.

### A.6 Agent Sessions lane-control schemas

`lane.start`, `lane.run`, `lane.resume`, and `lane.steer` parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["product", "arguments", "input"],
  "properties": {
    "product": {"type": "string", "minLength": 1},
    "arguments": {"type": "array", "items": {"type": "string"}},
    "input": {"type": "string", "minLength": 1},
    "cwd": {"type": "string", "minLength": 1},
    "host": {"type": "string", "minLength": 1}
  }
}
```

`lane.doctor`, `lane.list`, `lane.wait`, `lane.status`, `lane.interrupt`, and `lane.archive` parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["product", "arguments"],
  "properties": {
    "product": {"type": "string", "minLength": 1},
    "arguments": {"type": "array", "items": {"type": "string"}},
    "cwd": {"type": "string", "minLength": 1},
    "host": {"type": "string", "minLength": 1}
  }
}
```

For `lane.doctor`, let `P` be the exact requested product token. The result is
the following closed schema after substituting `P` in the four property names:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "type", "contract_version", "authority", "product", "ready",
    "native_path", "runtime_path", "daemon_reachable",
    "supervisor_reachable", "P_available", "P_path", "P_version"
  ],
  "properties": {
    "type": {"const": "lane.doctor"},
    "contract_version": {"const": 2},
    "authority": {"const": "daemon"},
    "product": {"const": "P"},
    "ready": {"type": "boolean"},
    "native_path": {"type": "string"},
    "runtime_path": {"type": "string"},
    "daemon_reachable": {"type": "boolean"},
    "supervisor_reachable": {"type": "boolean"},
    "P_available": {"type": "boolean"},
    "P_path": {"type": "string"},
    "P_version": {"type": "string"},
    "P_error": {"type": "string"},
    "readiness_error": {"type": "string"}
  }
}
```

`lane.list` returns this closed envelope; each item uses the closed
`lane.status` schema below and its product must equal the envelope product:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["type", "product", "lanes"],
  "properties": {
    "type": {"const": "lane.list"},
    "product": {"type": "string"},
    "lanes": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": [
          "type", "product", "session_id", "name", "cwd", "groups",
          "permission_mode", "state", "turn_id", "outcome", "exit",
          "owner_session_id", "persistent", "auto_archive",
          "auto_archive_after_seconds", "auto_archive_at"
        ],
        "properties": {
          "type": {"const": "lane.status"},
          "product": {"type": "string"},
          "session_id": {"type": "string"},
          "name": {"type": "string"},
          "cwd": {"type": "string"},
          "groups": {"type": "array", "items": {"type": "string"}},
          "permission_mode": {"type": "string"},
          "state": {"type": "string"},
          "turn_id": {"type": "string"},
          "outcome": {"type": "string"},
          "exit": {"type": ["integer", "null"]},
          "owner_session_id": {"type": "string"},
          "persistent": {"type": "boolean"},
          "auto_archive": {"type": "boolean"},
          "auto_archive_after_seconds": {"type": "number"},
          "auto_archive_at": {"type": "integer"}
        }
      }
    }
  }
}
```

`lane.ready` and `lane.status` result fields:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "type", "product", "session_id", "name", "cwd", "groups",
    "permission_mode", "state", "turn_id", "outcome", "exit",
    "owner_session_id", "persistent", "auto_archive",
    "auto_archive_after_seconds", "auto_archive_at"
  ],
  "properties": {
    "type": {"enum": ["lane.ready", "lane.status"]},
    "contract_version": {"const": 2},
    "product": {"type": "string"},
    "session_id": {"type": "string"},
    "name": {"type": "string"},
    "cwd": {"type": "string"},
    "groups": {"type": "array", "items": {"type": "string"}},
    "permission_mode": {"type": "string"},
    "state": {"type": "string"},
    "turn_id": {"type": "string"},
    "outcome": {"type": "string"},
    "exit": {"type": ["integer", "null"]},
    "owner_session_id": {"type": "string"},
    "persistent": {"type": "boolean"},
    "auto_archive": {"type": "boolean"},
    "auto_archive_after_seconds": {"type": "number"},
    "auto_archive_at": {"type": "integer"}
  },
  "allOf": [
    {
      "if": {"properties": {"type": {"const": "lane.ready"}}},
      "then": {"required": ["contract_version"]}
    }
  ]
}
```

`turn.completed` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["type", "product", "session_id", "turn_id", "status", "outcome", "exit", "result", "diagnostic"],
  "properties": {
    "type": {"const": "turn.completed"},
    "product": {"type": "string"},
    "session_id": {"type": "string"},
    "turn_id": {"type": "string"},
    "status": {"type": "string"},
    "outcome": {"type": "string"},
    "exit": {"type": ["integer", "null"]},
    "result": {"type": "string"},
    "diagnostic": {"type": "string"},
    "native_stop_reason": {"type": "string", "minLength": 1}
  }
}
```

`native_stop_reason` is the exact non-empty, driver-supplied terminal reason. It is transient and omitted when empty or when lane state was reconstructed; it never overloads `result` or `diagnostic`. This pre-0.4.0 greenfield v1 extension is incompatible with strict validators generated from earlier closed drafts.

`turn.steered` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["type", "session_id", "turn_id", "native_message_id"],
  "properties": {
    "type": {"const": "turn.steered"},
    "session_id": {"type": "string", "minLength": 1},
    "turn_id": {"type": "string", "minLength": 1},
    "native_message_id": {"type": "string", "minLength": 1}
  }
}
```

`turn.interrupting` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["type", "session_id", "turn_id"],
  "properties": {
    "type": {"const": "turn.interrupting"},
    "session_id": {"type": "string"},
    "turn_id": {"type": "string"}
  }
}
```

`lane.archived` result:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["type", "product", "session_id", "name"],
  "properties": {
    "type": {"const": "lane.archived"},
    "product": {"type": "string"},
    "session_id": {"type": "string"},
    "name": {"type": "string"},
    "already_archived": {"type": "boolean"}
  }
}
```

#### `lane.start`

```json
{"jsonrpc":"2.0","id":"lane-start-1","method":"lane.start","params":{"product":"qwen","arguments":["--name","reviewer","--group","team"],"input":"Review the change."}}
{"jsonrpc":"2.0","id":"lane-start-1","result":{"type":"lane.ready","contract_version":2,"product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer","cwd":"/work/project","groups":["team"],"permission_mode":"default","state":"running","turn_id":"turn-1","outcome":"","exit":null,"owner_session_id":"parent-native","persistent":false,"auto_archive":true,"auto_archive_after_seconds":60,"auto_archive_at":0}}
{"jsonrpc":"2.0","id":"lane-start-2","method":"lane.start","params":{"product":"new-agent","arguments":["--name","reviewer"],"input":"Review the change."}}
{"jsonrpc":"2.0","id":"lane-start-2","error":{"code":-32005,"message":"Product not launchable","data":{"product":"new-agent"}}}
```

#### `lane.run`

```json
{"jsonrpc":"2.0","id":"lane-run-1","method":"lane.run","params":{"product":"qwen","arguments":["--name","reviewer"],"input":"Review the change."}}
{"jsonrpc":"2.0","id":"lane-run-1","result":{"type":"turn.completed","product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-1","status":"completed","outcome":"completed","exit":0,"result":"Looks good.","diagnostic":""}}
```

#### `lane.resume`

```json
{"jsonrpc":"2.0","id":"lane-resume-1","method":"lane.resume","params":{"product":"qwen","arguments":["reviewer"],"input":"Check one more thing."}}
{"jsonrpc":"2.0","id":"lane-resume-1","result":{"type":"turn.completed","product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2","status":"completed","outcome":"completed","exit":0,"result":"Done.","diagnostic":""}}
{"jsonrpc":"2.0","id":"lane-resume-2","method":"lane.resume","params":{"product":"qwen","arguments":["missing"],"input":"Continue."}}
{"jsonrpc":"2.0","id":"lane-resume-2","error":{"code":-32001,"message":"Unknown session or target","data":{"target":"missing"}}}
```

#### `lane.steer`

```json
{"jsonrpc":"2.0","id":"lane-steer-1","method":"lane.steer","params":{"product":"pi","arguments":["reviewer"],"input":"Replace the current answer with: STEERED."}}
{"jsonrpc":"2.0","id":"lane-steer-1","result":{"type":"turn.steered","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2","native_message_id":"message-6f9e8475"}}
```

#### `lane.wait`

```json
{"jsonrpc":"2.0","id":"lane-wait-1","method":"lane.wait","params":{"product":"qwen","arguments":["reviewer","--timeout","300"]}}
{"jsonrpc":"2.0","id":"lane-wait-1","result":{"type":"turn.completed","product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2","status":"completed","outcome":"completed","exit":0,"result":"Done.","diagnostic":""}}
```

#### `lane.status`

```json
{"jsonrpc":"2.0","id":"lane-status-1","method":"lane.status","params":{"product":"qwen","arguments":["reviewer"]}}
{"jsonrpc":"2.0","id":"lane-status-1","result":{"type":"lane.status","product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer","cwd":"/work/project","groups":["team"],"permission_mode":"default","state":"idle","turn_id":"turn-2","outcome":"completed","exit":0,"owner_session_id":"parent-native","persistent":false,"auto_archive":true,"auto_archive_after_seconds":60,"auto_archive_at":0}}
```

#### `lane.interrupt`

```json
{"jsonrpc":"2.0","id":"lane-interrupt-1","method":"lane.interrupt","params":{"product":"qwen","arguments":["reviewer"]}}
{"jsonrpc":"2.0","id":"lane-interrupt-1","result":{"type":"turn.interrupting","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2"}}
```

#### `lane.archive`

```json
{"jsonrpc":"2.0","id":"lane-archive-1","method":"lane.archive","params":{"product":"qwen","arguments":["reviewer"]}}
{"jsonrpc":"2.0","id":"lane-archive-1","result":{"type":"lane.archived","product":"qwen","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer"}}
{"jsonrpc":"2.0","id":"lane-archive-2","method":"lane.archive","params":{"product":"qwen","arguments":["invisible-lane"]}}
{"jsonrpc":"2.0","id":"lane-archive-2","error":{"code":-32001,"message":"Unknown session or target","data":{"target":"invisible-lane"}}}
```

Any caller that can resolve an idle lane through ownership or shared-group
visibility may archive it. An invisible lane is unknown, not owner-forbidden.
