# Native Agent Sessions Presence Protocol

This document specifies the native product connection implemented by Agent
Sessions today. It is intended for coding-agent products that want to expose
their own sessions directly, without an Agent Sessions product plugin.

## 1. Purpose and trust model

Agent Sessions lets coding agents on a user's own machine or LAN discover and
message one another. It assumes a trusted environment: one held connection per
live session is the complete liveness proof, and there is no authentication
layer inside a host. Network or host isolation, when wanted, belongs to the
user's deployment.

## 2. Finding the daemon

The daemon listens on a Unix-domain socket named `run/presence.sock` beneath
its state root. The daemon chooses that root from `--state-root`, then
`AGENT_SESSIONS_STATE_ROOT`, then `$HOME/.local/state/agent-sessions`.

The shared reference client resolves the socket in this order:

1. `AGENT_SESSIONS_PRESENCE_SOCKET`, when non-empty;
2. `$AGENT_SESSIONS_STATE_ROOT/run/presence.sock`;
3. `$XDG_STATE_HOME/agent-sessions/run/presence.sock`;
4. `$HOME/.local/state/agent-sessions/run/presence.sock`.

The daemon creates the containing directory with mode `0700` and the socket
with mode `0600`. Those operating-system permissions are not an application
authentication protocol.

### Launch environment

Current interactive launchers are not uniform. The following table lists only
generic `AGENT_SESSIONS_*` variables that launcher code actually writes for an
interactive product process; inherited variables are not counted.

| Variable | Value written today |
| --- | --- |
| `AGENT_SESSIONS_SESSION_NAME` | The requested start name or the title resolved for a Codex resume, otherwise `""`. Written for Codex, Claude Code, Grok, Qwen, OpenCode, Kilo, Pi, and OMP. |
| `AGENT_SESSIONS_GROUPS` | A JSON array containing the repeated launch `--group` values. Written for all of the products above. |
| `AGENT_SESSIONS_PRODUCT` | The catalog ID. Written for Codex, Grok, Qwen, OpenCode, Kilo, Pi, and OMP; the current Claude Code launcher does not write it. |
| `AGENT_SESSIONS_PRODUCT_ID` | The catalog ID. Written only by the shared OpenCode/Kilo/Pi/OMP launch path. |
| `AGENT_SESSIONS_SESSION_ID` | The known exact ID for Codex and Grok; an exact OpenCode/Kilo resume ID; otherwise currently `""` on the Qwen and shared launch paths. The current Claude Code launcher does not write it. |

Grok's current wrapper also writes `AGENT_SESSIONS_GROK_SESSION_ID` and
`AGENT_SESSIONS_GROK_LEADER_SOCKET`. Qwen's writes
`AGENT_SESSIONS_QWEN_INPUT_FILE` and `AGENT_SESSIONS_QWEN_EVENTS_FILE`. These
are private mechanics of those wrappers, not requirements of this protocol.
Likewise, lane worker launchers write `AGENT_SESSIONS_HOST_BINARY`,
`AGENT_SESSIONS_LANE_CAPABILITY`, and the applicable session, product, name,
and group variables; a native presence client must still report the product's
actual session identity.

The peer launchers do **not** currently write
`AGENT_SESSIONS_PRESENCE_SOCKET` or `AGENT_SESSIONS_STATE_ROOT`; they only
preserve values already present in their environment. A product started
outside Agent Sessions can therefore use the conventional path or set an
explicit presence socket. The shared client becomes active only when it can
resolve a socket and has `AGENT_SESSIONS_PRODUCT_ID` (or the supported fallback
`AGENT_SESSIONS_PRODUCT`); missing groups mean `[]`, while malformed group JSON
disables the client. There is no separate registration request. The daemon
must already be running, and the reported product must already be in its
catalog.

## 3. The first frame

Immediately after connecting, the product sends one complete report followed
by `\n`:

```json
{"uuid":"native","name":"before","groups":["team"],"product":"pi"}
```

The fields mean:

- `uuid` is the stable session ID issued by the product. A presence client
  receives it from the product and never generates or substitutes one.
- `name` is the title owned by the product and stored with that product's
  session. `""` is valid when the product has not assigned a title yet.
- `groups` contains the group strings passed from the Agent Sessions launch
  arguments, verbatim and in order; the presence client must not infer extra
  membership. The current reference client and daemon remove exact duplicate
  strings.
- `product` is the exact Agent Sessions catalog ID. The catalog at this writing
  accepts `codex`, `claude`, `grok`, `qwen`, `opencode`, `kilo`, `pi`, `omp`,
  and `dsh`.

The first object is not an RPC request. Unknown first-report fields, an empty
UUID, invalid field types, or an unknown product cause the daemon to close the
connection. Later title or group changes are sent as a full `session.update`
report; they are not deltas.

## 4. The stream

After the first report, both sides exchange newline-delimited JSON-RPC-shaped
objects on the same connection. The wire does not include a `jsonrpc` member.
Every implemented call is a request with a non-empty string `id`, and every
request receives a method-less response with the same `id` and either `result`
or a plain-string `error`. Notifications are not implemented; a client-to-
daemon frame without an ID closes the connection.

Implementations should use `session.`-prefixed IDs for product-to-daemon calls.
The daemon uses `daemon.`-prefixed IDs for calls to a product. IDs must be
unique among calls still awaiting a response. The shared JavaScript client
disconnects if its unprocessed receive buffer grows beyond 1 MiB.

### Daemon to product: `message.deliver`

```json
{"id":"daemon.message","method":"message.deliver","params":{"message_id":"message","body":"hello"}}
{"id":"daemon.message","result":{}}
```

`message_id` is the delivery identity. `body` is the exact user input to place
into the addressed session; normal daemon routing wraps peer text with sender
metadata before this call. The product must deliver the body to this exact
session and wake a model turn without terminal keystrokes or screen scraping.
It returns `{}` only after accepting the input, or returns an error:

```json
{"id":"daemon.message","error":"session is busy"}
```

Agent Sessions does not queue the message for a disconnected product. The
sender's `message.send` call succeeds only if every selected destination
acknowledges delivery.

### Product to daemon: `session.update`

```json
{"id":"session.update","method":"session.update","params":{"uuid":"session","name":"product title","groups":["group"],"product":"qwen"}}
{"id":"session.update","result":{}}
```

The parameters have the same shape as the first report. `uuid` and `product`
must equal their values in the first report; `name` and `groups` may change.
An invalid update receives `{"error":"live session update is invalid"}` and
does not replace the current report.

### Product to daemon: `tool.call`

A product exposes one finite, promptless tool to its model and forwards it as:

```json
{"id":"session.tool-one","method":"tool.call","params":{"operation":"peers.list","arguments":{}}}
```

The exact operation enum is:

```text
peers.list
message.send
lane.start
lane.run
lane.resume
lane.wait
lane.status
lane.steer
lane.interrupt
lane.collect
lane.archive
```

The implemented argument and result shapes are:

| Operation | `arguments` | Successful `result` |
| --- | --- | --- |
| `peers.list` | `{}` | `{"peers":[Peer, ...]}`. Only live, group-visible destinations are returned. |
| `message.send` | `{"target":"ID","message":"TEXT"}` or `{"targets":["ID", ...],"message":"TEXT"}` | `{"message_id":"ID","deliveries":[Delivery, ...]}`. `target` and `targets` are mutually exclusive; duplicates are rejected. |
| `lane.start` | Lane arguments described below | A `lane.ready` object after native start is accepted. |
| `lane.run` | Lane arguments described below | A `turn.completed` object after the turn ends. |
| `lane.resume` | Lane arguments described below | A `turn.completed` object after the resumed turn ends. |
| `lane.wait` | Lane arguments described below | A `turn.completed` object for the live turn. |
| `lane.status` | Lane arguments described below | A `lane.status` object. |
| `lane.steer` | Lane arguments described below | No success shape today: downstream dispatch returns `unsupported lane command "steer"`. |
| `lane.interrupt` | Lane arguments described below | A `turn.interrupting` object after native interruption is accepted. |
| `lane.collect` | Lane arguments described below | No success shape today: downstream dispatch returns `unsupported lane command "collect"`. |
| `lane.archive` | Lane arguments described below | A `lane.archived` object after native archive is accepted. |

Lane calls use one structured object:

```json
{
  "product": "qwen",
  "arguments": ["--name", "reviewer"],
  "input": "Review the change.",
  "host": "optional-remote-host"
}
```

`arguments` is the daemon's finite lane command argument array without the
operation name. `input` is required and non-empty for `start`, `run`, and
`resume`. `product` selects the lane product. `host` is optional. The daemon
uses the calling session's working directory unless `arguments` includes an
accepted working-directory option. Appendix A records every result object.

For compatibility with current MCP connectors, the daemon also accepts
`tools/call` with `{"name":"list_peers","arguments":{}}`. Its implemented
names are `list_peers`, `identity`, `rename_session`, `send_message`,
`broadcast`, and `lane`; `rename_session` currently always reports that no
native rename driver is composed. This compatibility method returns MCP-style
`content`, `structuredContent`, and `isError` values inside the RPC `result`.
New native clients should use `tool.call`, whose errors use the top-level
string `error` frame.

## 5. Lifecycle semantics

- EOF or any connection loss removes the session immediately. There is no
  heartbeat and no grace period.
- After a daemon restart, the product reconnects and sends the full first
  report again. The daemon begins with an empty live roster and rebuilds it
  entirely from those reports. The reference clients retry after 100 ms.
- A newer connection reporting the same `uuid` replaces and closes the older
  connection. The replacement is continuous from the roster's point of view;
  the displaced connection cannot later remove it.
- A process hosting several simultaneously live sessions holds one independent
  connection per session. Creation, title changes, disposal, and reconnection
  are handled independently for each one.
- On disconnect, all outstanding calls in both directions fail. The shared
  client rejects product-to-daemon promises as unavailable, forgets unanswered
  inbound deliveries, and does not replay either kind of call after reconnect.
  The caller owns any safe retry.

## 6. Product responsibilities

A conforming product integration provides all of the following:

- A stable, product-issued session UUID available at or near session start.
- A requested start-time name written into the product's own session store,
  not merely painted in the terminal or held by Agent Sessions.
- Native resume by exact UUID and, where the product exposes named sessions,
  an exact name lookup that selects one product session or fails truthfully on
  zero or multiple matches.
- An addressable API that delivers an inbound message to one exact session and
  wakes a model turn without injecting keystrokes. A busy session either
  accepts a supported native steer or returns an error.
- A finite, structured, promptless model tool surface for exactly the
  operations enumerated above. Unsupported operations return errors rather
  than opening an approval prompt.
- A truthful exit boundary: close the presence connection as soon as the
  product session is no longer live.
- Per-session created, changed, and disposed events when one product process
  can host several live sessions.

## 7. Conformance

Conformance is checked end to end against the real product, not just a socket
mock:

| Cell | Required observation |
| --- | --- |
| Named start | Starting with a name and groups creates a real product session that is immediately visible with the same name and groups. |
| Model turn | The named session completes a real model turn through the product's normal runtime. |
| Inbound round trip | `message.deliver` wakes that exact session; its model can call `message.send`, and the reply is delivered without an interactive approval prompt. |
| Daemon restart | While the product remains live, restarting the daemon makes the session reappear with the same UUID, product-owned name, and groups. |
| Exit | Exiting or disposing the product session makes it disappear immediately. |
| Resume | Resume by exact UUID and resume by unique name both restore the same product UUID and report it unchanged. |

## Appendix A. Exact schemas and frame examples

### A.1 Initial report schema

This is the producer contract. The current daemon is slightly more permissive
about omitted `name` and `groups`; see Appendix B.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Agent Sessions initial report",
  "type": "object",
  "additionalProperties": false,
  "required": ["uuid", "name", "groups", "product"],
  "properties": {
    "uuid": {"type": "string", "minLength": 1},
    "name": {"type": "string"},
    "groups": {
      "type": "array",
      "items": {"type": "string"},
      "uniqueItems": true
    },
    "product": {
      "enum": ["codex", "claude", "grok", "qwen", "opencode", "kilo", "pi", "omp", "dsh"]
    }
  }
}
```

Example from the shared client test:

```json
{"uuid":"native","name":"before","groups":["team"],"product":"pi"}
```

### A.2 Post-report frame schemas

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Agent Sessions RPC request",
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "method", "params"],
  "properties": {
    "id": {"type": "string", "minLength": 1},
    "method": {
      "enum": ["message.deliver", "session.update", "tool.call", "tools/call"]
    },
    "params": {"type": "object"}
  }
}
```

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Agent Sessions RPC response",
  "oneOf": [
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "result"],
      "properties": {
        "id": {"type": "string", "minLength": 1},
        "result": {}
      }
    },
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "error"],
      "properties": {
        "id": {"type": "string", "minLength": 1},
        "error": {"type": "string", "minLength": 1}
      }
    }
  ]
}
```

Each JSON object below is one complete line on the wire.

### A.3 `message.deliver`

Parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["message_id", "body"],
  "properties": {
    "message_id": {"type": "string", "minLength": 1},
    "body": {"type": "string"}
  }
}
```

Frames exercised by the shared client:

```json
{"id":"daemon.message","method":"message.deliver","params":{"message_id":"message","body":"hello"}}
{"id":"daemon.message","result":{}}
{"id":"daemon.message","error":"native delivery failed"}
```

### A.4 `session.update`

`params` uses the initial-report schema. The request ID is caller-selected.

```json
{"id":"session.update","method":"session.update","params":{"uuid":"session","name":"product title","groups":["group"],"product":"qwen"}}
{"id":"session.update","result":{}}
{"id":"session.update","error":"live session update is invalid"}
```

### A.5 `tool.call`

Parameters:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["operation", "arguments"],
  "properties": {
    "operation": {
      "enum": [
        "peers.list", "message.send", "lane.start", "lane.run",
        "lane.resume", "lane.wait", "lane.status", "lane.steer",
        "lane.interrupt", "lane.collect", "lane.archive"
      ]
    },
    "arguments": {"type": "object"}
  }
}
```

`peers.list` request and response:

```json
{"id":"session.tool-one","method":"tool.call","params":{"operation":"peers.list","arguments":{}}}
{"id":"session.tool-one","result":{"peers":[{"id":"target-native","session_id":"target-native","name":"reviewer","product":"codex","status":"live","cwd":"/work/project","groups":["team"],"permission_mode":"default"}]}}
```

A `Peer` has required `id`, `session_id`, `name`, `product`, `status`, `cwd`,
`groups`, and `permission_mode` fields. A lane also has `"kind":"lane"` and
uses `idle` or `busy` status. A peer reached through another host has
`"kind":"remote-peer"` and `host_id`.

`message.send` request and response:

```json
{"id":"session.send-one","method":"tool.call","params":{"operation":"message.send","arguments":{"target":"target-native","message":"Please review this."}}}
{"id":"session.send-one","result":{"message_id":"9a58e98f39cc2d47a1c3f09a77bc8310","deliveries":[{"target":"target-native","session_id":"target-native","delivery_id":"delivery-718e3cdb1e3f61786d95879632973bb7","status":"accepted"}]}}
```

A failed tool call uses the response error frame:

```json
{"id":"session.send-one","error":"destination target-native did not accept the delivery"}
```

### A.6 Lane operation frames

All lane requests have the same outer shape. These examples use the exact
objects serialized by the current dispatcher; IDs and product-issued UUIDs are
illustrative.

`lane.start`:

```json
{"id":"session.lane-start","method":"tool.call","params":{"operation":"lane.start","arguments":{"product":"qwen","arguments":["--name","reviewer","--group","team"],"input":"Review the change."}}}
{"id":"session.lane-start","result":{"type":"lane.ready","contract_version":2,"product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer","cwd":"/work/project","groups":["team","session:host-id/parent-native","session:host-id/parent-native/27c1a11b-5716-4dc4-a158-a8177c1e7365"],"permission_mode":"default","state":"running","turn_id":"turn-1","outcome":"","exit":null,"owner_session_id":"parent-native","persistent":false,"auto_archive":true,"auto_archive_after_seconds":60,"auto_archive_at":0}}
```

`lane.run`:

```json
{"id":"session.lane-run","method":"tool.call","params":{"operation":"lane.run","arguments":{"product":"qwen","arguments":["--name","reviewer","--group","team"],"input":"Review the change."}}}
{"id":"session.lane-run","result":{"type":"turn.completed","product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-1","status":"completed","outcome":"completed","exit":0,"result":"Looks good.","diagnostic":""}}
```

`lane.resume`:

```json
{"id":"session.lane-resume","method":"tool.call","params":{"operation":"lane.resume","arguments":{"product":"qwen","arguments":["reviewer"],"input":"Check one more thing."}}}
{"id":"session.lane-resume","result":{"type":"turn.completed","product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2","status":"completed","outcome":"completed","exit":0,"result":"Done.","diagnostic":""}}
```

`lane.wait`:

```json
{"id":"session.lane-wait","method":"tool.call","params":{"operation":"lane.wait","arguments":{"product":"qwen","arguments":["reviewer","--timeout","300"]}}}
{"id":"session.lane-wait","result":{"type":"turn.completed","product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","turn_id":"turn-2","status":"completed","outcome":"completed","exit":0,"result":"Done.","diagnostic":""}}
```

`lane.status`:

```json
{"id":"session.lane-status","method":"tool.call","params":{"operation":"lane.status","arguments":{"product":"qwen","arguments":["reviewer"]}}}
{"id":"session.lane-status","result":{"type":"lane.status","product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer","cwd":"/work/project","groups":["team","session:host-id/parent-native","session:host-id/parent-native/27c1a11b-5716-4dc4-a158-a8177c1e7365"],"permission_mode":"default","state":"idle","turn_id":"turn-2","outcome":"completed","exit":0,"owner_session_id":"parent-native","persistent":false,"auto_archive":true,"auto_archive_after_seconds":60,"auto_archive_at":0}}
```

`lane.steer` and `lane.collect` are in the accepted enum but currently return
errors:

```json
{"id":"session.lane-steer","method":"tool.call","params":{"operation":"lane.steer","arguments":{"product":"qwen","arguments":["reviewer"],"input":"Focus on tests."}}}
{"id":"session.lane-steer","error":"unsupported lane command \"steer\""}
{"id":"session.lane-collect","method":"tool.call","params":{"operation":"lane.collect","arguments":{"product":"qwen","arguments":["reviewer"]}}}
{"id":"session.lane-collect","error":"unsupported lane command \"collect\""}
```

`lane.interrupt`:

```json
{"id":"session.lane-interrupt","method":"tool.call","params":{"operation":"lane.interrupt","arguments":{"product":"qwen","arguments":["reviewer"]}}}
{"id":"session.lane-interrupt","result":{"type":"turn.interrupting","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","turn_id":"turn-2"}}
```

`lane.archive`:

```json
{"id":"session.lane-archive","method":"tool.call","params":{"operation":"lane.archive","arguments":{"product":"qwen","arguments":["reviewer"]}}}
{"id":"session.lane-archive","result":{"type":"lane.archived","product":"qwen","thread_id":"7e1a58f4-c2dd-4d79-a5b4-0a560acdf590","session_id":"27c1a11b-5716-4dc4-a158-a8177c1e7365","name":"reviewer"}}
```

### A.7 Connector-compatible `tools/call`

```json
{"id":"session.17","method":"tools/call","params":{"name":"list_peers","arguments":{}}}
{"id":"session.17","result":{"content":[{"type":"text","text":"No live peers share a group with this session."}],"structuredContent":{"peers":[]}}}
```

Connector tool failures remain RPC successes with an MCP error result:

```json
{"id":"session.18","method":"tools/call","params":{"name":"rename_session","arguments":{"name":"new name"}}}
{"id":"session.18","result":{"content":[{"type":"text","text":"native rename driver is not composed"}],"isError":true}}
```

## Appendix B. Open questions and code/document discrepancies

1. The shared client honors `XDG_STATE_HOME`, but the daemon's default state
   root currently does not. With `XDG_STATE_HOME` set and no explicit state
   root or socket, they can choose different paths.
2. The DSH design requires a live interactive root, but the current product
   catalog marks `dsh` as lane-only. The daemon accepts a DSH report, then does
   not expose a standalone DSH root as an interactive peer.
3. `lane.steer` and `lane.collect` are accepted by the public operation enum,
   but the current lane dispatcher has no cases for them and returns the exact
   errors shown above. Existing adapter documentation describes steer more
   broadly than the code provides.
4. The daemon accepts an initial report with omitted `name` or `groups` by
   decoding them as empty values, although the reference clients always send
   all four report fields and this specification requires them.
5. The server accepts group changes through full `session.update` reports, but
   the shared JavaScript client currently exposes only `updateName`; a native
   product must send the full update itself when groups change.
6. The persistence design mentions up to two seconds to drain accepted work on
   graceful daemon shutdown. The presence layer itself closes connections and
   cancels in-flight requests; no drain negotiation or replay signal exists on
   this wire.
7. There is no protocol version or capability negotiation. It cannot be
   determined from the wire alone how a future incompatible change would be
   distinguished from an unsupported method.
