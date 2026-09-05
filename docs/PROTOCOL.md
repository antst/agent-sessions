## 1. Wire

### 1.1 Roles and connection model

A session is one JSON-RPC 2.0 connection. A peer is a session opened by a
product. A lane is a session spawned by the daemon with a one-use launch token
and therefore also accepts daemon-directed session and turn methods. The same
connection carries presence, tools, message delivery, lane control, and turn
results. There is no child presence connection and no relay connection to the
daemon.

Each frame is one UTF-8 JSON object followed by a newline and is at most 1 MiB;
an oversized frame is closed silently because no request ID can be assumed.
Request IDs are integers in `[1, 2^53-1]`, with each direction counting from
one in its independent ID space. When JSON repeats a member, the last value
wins on both Go and JavaScript implementations. Notifications, batches,
unknown fields, explicit nulls, and a second hello are invalid. The first
request is `session.hello`. The daemon admits no other request from a spawned
worker until `session.open` has returned a native ID and that ID is durably
committed.

Every session ID assigned by the daemon, including an ID placed into a thin
peer launcher's environment, is an RFC 4122 version-4 UUID. This grammar is a
daemon check because the shared schema deliberately has no `pattern` keyword.
The wire has no generic tool frame: after hello, a peer or committed worker
originates the ordinary client-to-daemon methods in this section. Product-facing
start/wait/status/interrupt/list/send tools are caller-kit sugar over them.

Turn input and result strings, `message.send.message`, and
`message.deliver.body` are each limited to 262,144 decoded characters through
the schema's `maxLength` keyword. The raw 1 MiB framing guard remains an earlier,
independent byte limit because UTF-8 width and JSON escaping are not character
counts. A worker kit truncates a longer native turn result to 262,144 characters
and sets `truncated:true`; the daemon persists that flag in `last_turn`. Both
kits emit compact JSON with no insignificant whitespace.

The closed parameter and result shapes are authoritative in
`integrations/shared/session.schema.json`. The schema uses only the shared
minimal validator vocabulary, including `maxLength` alongside `minLength`; both
validator allowlists and their shared authority test name it. An implementation rejects a frame before invoking
product code if the corresponding definition does not validate it. Its
**440-line cap** includes the optional federated product-discovery and bounded
turn-result shapes; this
is list data, never launch authority.

Authorization is visibility: a session may run, interrupt, close, or message a
lane exactly when it can see that lane through a shared group in `session.list`.
This trusted protocol has no ownership field or per-caller ACL.
Peer identity and groups are asserted rather than attested on the trusted local
socket, and federation trusts the remote daemon's assertions. No peer
credentials, signatures, or other security machinery belong in this protocol.

There are eleven methods.

#### `session.hello`

The session sends `session.hello` first. The request is one closed union. A peer
supplies `session_id`, `name`, `groups`, and `info`. A worker instead supplies a
one-use `launch_token`, its supported non-identity open fields, its ordered
extra-argument descriptions, and optionally the product version. The branches are mutually
exclusive: a request with both discriminants or neither is invalid. A worker
sends hello only after its product and plugin are app-ready, so hello success is
the sole readiness fact. A peer ID matching a durable lane row is invalid. A
worker's product must equal the product recorded by its launch-token
reservation. The result is `{}`.

#### `session.superseded`

The daemon sends the displaced connection the ID-bearing request
`session.superseded` with `{}` when a new peer connection claims the same
identity. The displaced client marks that identity terminal before its
best-effort `{}` response, closes, and never reconnects that identity. The new
connection is already current, so every request from the displaced connection
is rejected. The daemon writes the supersession request before closing the old
local socket with an immediate write deadline and never waits for an
acknowledgement; inability to write is indistinguishable from EOF to the old
client. If a displaced client races one final reconnect, that new claim
may cause one more swap; because each displaced instance becomes terminal, the
race is bounded and cannot flap indefinitely.

#### `session.list`

A connected session sends `session.list` with an optional `session_id` filter.
The result contains the matching visible sessions, or all visible sessions when
the filter is absent. Each item reports identity, whether its one
connection is open, whether one `turn.run` is outstanding, whether it was
explicitly closed, the lane native ID when applicable, and the lane's optional
`last_turn`. A remote item also carries its federated `host`. This single method
replaces peer listing, lane listing, lane status, and lost-caller result
recovery. Its optional `hosts` array advertises product names by host. Whenever
the local optional product list is configured, the daemon's effective local
host identity is present, even when that list is empty; federated hosts
contribute their published lists.
Advertisement never gates launch: the service PATH remains authoritative.
Lane-row identity is immutable. Peers have no durable rows: same-ID
peer hello wholly replaces the transient peer's name, groups, info, and
connection. A session outside the caller's visibility is indistinguishable
from a missing session and yields `unknown_session`. Federated `session_id`
values are host-qualified composite IDs; `host` is only informational, and a
local ID never crosses a host boundary unqualified. Host presence is a daemon
invariant rather than a JSON-schema constraint.

#### `message.send`

A connected session sends `message.send` to exactly one `target`, one explicit
`targets` list, or one `group`. The daemon resolves recipients from current
connections, sends each one `message.deliver`, and returns one truthful receipt
per attempted delivery. The request retains one message body; explicit
multicast and group expansion do not create another protocol method.
Resolution tries exact session ID, then host-qualified ID, then visible name.
An ambiguous name produces a rejected receipt with reason `ambiguous` and no
resolved IDs. Recipients are deduplicated by session ID, with one receipt each.

#### `message.deliver`

The daemon sends `message.deliver` to the target session with the message ID,
authoritative source identity, and body. Every product implements delivery while
idle and while a turn is running. Its result is exactly one closed receipt:
`injected`, `queued_for_next_turn`, or `rejected` with a nonempty reason. A
non-native wrapper may implement `queued_for_next_turn` by prepending the body to
its next native turn; that queue is invisible to the daemon and to this wire.

#### `lane.describe`

A session sends `lane.describe` with a product token and optional `host`.
Absent `host`, or `host` equal to the local daemon, runs locally; another
connected host forwards the identical request one hop and performs the complete
probe there. The authoritative daemon starts `<product> --lane` from its
service PATH with a rowless one-use token, consumes its worker hello, returns the declared open
fields, extra arguments, and optional product version, then closes it without
sending `session.open`. The hello is emitted only after app-ready. Exit before
hello fails with the exit code and bounded trailing stderr; there is no
readiness object or readiness phase.

#### `lane.spawn`

A session sends `lane.spawn` either with a caller-chosen `name`, product, open
options, optional `extra_groups`, and optional `host` for a new lane, or with
`resume_session_id` alone for a durable offline lane. A new row's groups are the
union of the caller's host-qualified groups, including its private session
group, and `extra_groups`; a caller never sends the complete authoritative
group set. Absent `host`, or `host` equal to the local daemon, spawns locally;
another connected host forwards the identical request one hop and performs the
entire transaction there. The row exists only on that authoritative target.
Resume never carries `host`: its session ID already selects the host. The
authoritative daemon resolves and starts the worker, waits for hello, sends
`session.open`, durably
commits its native ID, and only then returns `{session_id,native_id}`. The row
stores the original closed open-options object as one JSON value. Resume replays
that value verbatim with the stored native ID; this version permits no resume
overrides. The one spawn/open transaction timeout covers all of those steps.
Unsupported supplied
open fields, an invalid native ID, exit, or timeout fail truthfully and do not
publish a live session.

One in-memory per-row lock covers the whole spawn, resume, or close transaction;
it is coordination, not persisted state. Events within spawn/open are handled
sequentially in arrival order: an open result commits before a later EOF can
make the row offline, while EOF observed first fails the spawn. Resume requires
the returned native ID to equal the stored ID. The table enforces global
uniqueness of `(product,native_id)`; a fresh collision fails the spawn.

#### `session.open`

The daemon sends `session.open` only to a token-authenticated worker. It carries
the daemon session ID, always-present row-authoritative `name` and `groups`, the
stored native ID when resuming, and one closed `open` object containing only the
non-identity options accepted by `lane.spawn`. Values of `permission_mode`,
`model`, and `reasoning_effort` are product-native strings passed through
verbatim: the daemon checks only shape and declared field support, while the
worker rejects an unsupported value as `spawn_failed` with
`stderr_tail:["unsupported value <field>=<value>"]`. `arguments` is an ordered
array passed verbatim to the product integration. Hello's `extra_arguments`
entries document those strings for callers; the daemon never interprets or
enforces them. The worker creates or resumes the native session and returns
`{native_id}`. A successful result is
the commit point that turns the provisional worker connection into lane
presence. A probe never receives this method.

#### `turn.run`

Either a caller sends `turn.run` to the daemon or the daemon forwards the same
request to the addressed lane. Its parameters are exactly `{session_id,input}`.
The worker permits one outstanding call, invokes the native run primitive, and
answers only with the terminal outcome. The outstanding RPC is the running
fact; no start acknowledgement, wait method, status update, projection stream,
or collector exists. Before resolving or abandoning the caller's RPC, the
daemon overwrites the row's single `last_turn` value with the input, terminal
reply, and daemon completion time.

Bounded agent-tool behavior is caller-kit policy, not wire. The Go and JavaScript
caller kits keep `turn.run` outstanding and expose local `start`, `wait(timeout)`,
and `status` operations with a local-only turn ID. A wait timeout never cancels
the wire call. If the caller process disappears, the daemon still stores the
terminal result; a replacement caller reads it through `session.list`.
The worker kit is full-duplex: its reader never blocks on the native run, so
delivery, interrupt, and worker-originated session methods dispatch concurrently
with it. A terminal response may include `truncated:true` only when its result
was shortened to the wire limit; `last_turn` preserves the same fact.

#### `turn.interrupt`

Either a caller sends `turn.interrupt` to the daemon or the daemon forwards it
to the addressed lane. The request carries only `session_id`; `{}` means the
product accepted the interrupt request. An idle target returns the closed
not-running error. There is no interrupt grace timer and no `timed_out` outcome:
if the native run remains unresponsive, the caller closes the session.
The worker kit coalesces interrupts: it invokes the native interrupt primitive
at most once per run, and later interrupt requests return `{}`.

#### `session.close`

Either a caller sends `session.close` to the daemon or the daemon sends it to
the addressed lane. The worker asks the product to close and returns `{}` when
it does. One constant `closeBound = 10s`, measured from the daemon sending this
request, bounds the entire close path. A result before the bound makes the
daemon close the socket, send TERM, and reap; expiry makes it close the socket,
send KILL, and reap with no second waiting period. The spawn/open transaction
bound and `closeBound` are the only two lane-path timeouts.

If close arrives during a run, the worker kit interrupts once, awaits that same
run's terminal result, and then closes natively; the whole sequence must fit
inside `closeBound`. A terminal result observed in time is persisted before the
row is closed. If the
bound forces a kill, the row is still closed, `last_turn` remains unchanged,
and worker EOF fails the outstanding caller exactly once; the daemon never
fabricates an interrupted result.

An unrequested EOF marks a durable lane offline and resumable; it does not mark
the row explicitly closed. There is no daemon auto-archive policy. A future
protocol may add an idle-close option implemented wholly by a product kit, but
this version has none.

### 1.2 Edge rules

- A hello with both `session_id` and `launch_token`, with neither, or with fields
  from the other branch is invalid and closes the connection.
- A peer hello whose ID belongs to a lane row is invalid. A worker hello whose
  product differs from its token reservation is invalid; the token lookup is
  the authority for both facts.
- A rowless describe token can authenticate hello but can never authorize
  `session.open`; EOF after describe is the worker's normal exit.
- Worker-originated session methods before the native ID commit are rejected.
  After commit, the same connection is the lane's presence and uses those
  ordinary methods; there is no tool frame.
- A second `turn.run` while one is outstanding returns busy. There is one
  running boolean in a product kit and one pending RPC in the daemon.
- `lane.spawn` with `resume_session_id` naming a connected row returns
  `already_connected`; lanes never use supersession to manufacture a second
  worker connection.
- New `lane.spawn` requires a name not held by any non-closed row on that
  daemon; collision returns `name_taken`. Closed rows remain visible history
  but do not reserve names.
- `lane.spawn` with `resume_session_id` naming an explicitly closed row returns
  `closed`. The row remains visible only for identity and `last_turn` history.
- The per-row lock serializes concurrent spawn, resume, and close transactions.
  A waiter rechecks the row after acquiring it: a second resume sees
  `already_connected`, a second close sees `closed`, and resume cannot exec
  until synchronous supervisor cleanup of the prior worker has finished.
- `turn.run`, `turn.interrupt`, or `session.close` addressed to a durable row
  without a connection returns `not_connected`. Resume is an explicit
  `lane.spawn`; a caller kit may compose that automatically without changing the
  wire. The `closed` check precedes this connection check.
- `turn.interrupt` while no run is outstanding returns not-running. An accepted
  interrupt does not promise that the native product has already stopped.
- Caller timeout or disappearance does not cancel the forwarded run. The daemon
  drains its response, writes `last_turn` first, and discards only the abandoned
  response. There is no result queue beyond the one overwrite-only row value.
- Worker EOF interrupts or closes any native run through product cleanup, fails
  pending calls exactly once, and leaves a non-closed durable row offline.
- `session.close` has one 10-second deadline from request send through process
  reap. Deadline expiry closes the socket, sends KILL, and adds no grace period.
- Same-identity replacement atomically makes the new peer current, sends
  `session.superseded` to the old exact connection, and prevents reconnection by
  that displaced identity instance.
- Delivery is mandatory. Products that cannot inject during a native run queue
  for the next turn in their wrapper and report that disposition; the daemon
  has no injection capability flag or queue.
- A worker binary absent from PATH fails as `unknown_product`. A binary that
  starts but exits before hello fails describe or spawn with bounded trailing
  stderr and its exit code when one exists. Neither fabricates readiness.
- Optional `host` on `lane.describe` or new `lane.spawn`, and host-qualified
  session IDs on later operations, select the same one-hop daemon forwarder.
  An unconnected explicit or qualified host returns `unknown_host`; federation
  never creates a second request shape or retry path.

This protocol deliberately does not compensate for five losses. A daemon crash
before commit can orphan native files; workers exit on EOF and those files are
garbage, not state. A successful spawn reply can be lost with its caller; the
caller kit lists by name before spawning again. A crash between native close
and row commit can leave a natively closed session looking resumable; its next
resume fails truthfully. A wrapper can die after truthfully accepting a queued
delivery but before the next turn; that private queue is not durable protocol
state. After a daemon crash, an old worker may take milliseconds to observe EOF
while the restarted daemon spawns a resume; a persisted-PID reap barrier is
refused state, and both workers must obey EOF and native-session exclusivity.

### 1.3 Method convergence

| Today | Universal wire | Reason |
| --- | --- | --- |
| `session.hello` | `session.hello` | Survives and gains the token-discriminated worker branch. |
| `lane.worker.hello` | `session.hello` | Merged; two first-frame methods would duplicate framing and validation. |
| `session.update` | — | Dies; connection presence and outstanding `turn.run` are the complete live facts. |
| `session.superseded` | `session.superseded` | Survives unchanged so replacement is terminal rather than a reconnect flap. |
| `peers.list` | `session.list` | Merged with lane list and status because peers and lanes are sessions. |
| `message.send` | `message.send` | Survives as the single outbound messaging operation. |
| `lane.doctor` | `lane.describe` | Renamed because hello success reports support; there is no readiness state. |
| `lane.list` | `session.list` | Merged because a lane is a session row plus an optional connection. |
| `lane.start` | `lane.spawn` + `turn.run` | Creation and the first turn are two ordinary composable operations. |
| `lane.run` | `lane.spawn` + `turn.run` | Synchronous convenience belongs in the caller kit, not the wire. |
| `lane.resume` | `lane.spawn` + `turn.run` | `resume_session_id` selects the durable row; running remains separate. |
| `lane.steer` | `message.send` / `message.deliver` | Steering is mandatory message injection or truthful wrapper queuing. |
| `lane.wait` | — | Dies; `turn.run` is the one outstanding terminal-result RPC. |
| `lane.status` | `session.list` | A filtered list returns connection, running, and `last_turn`. |
| `lane.interrupt` | `turn.interrupt` | Merged with the worker-side interrupt under one end-to-end shape. |
| `lane.archive` | `session.close` | Explicit lifetime termination is one session operation. |
| `message.deliver` | `message.deliver` | Survives; its result becomes the truthful closed delivery receipt. |
| `lane.turn.start` | `turn.run` | Merged with wait into one RPC whose lifetime is the turn. |
| `lane.turn.wait` | `turn.run` | Dies as a separate collection protocol. |
| `lane.turn.interrupt` | `turn.interrupt` | Merged with the caller-side operation. |
| `lane.session.archive` | `session.close` | Merged with the caller-side lifetime operation. |

The closed method authority therefore shrinks from twenty-one methods to eleven.

### 1.4 Error authority

Every correlated failure uses exactly one numeric JSON-RPC code and symbolic
message from this table. Kits match the code, never free-form text. Only
`spawn_failed` has `data`: the closed object
`{exit_code?:integer,stderr_tail:[string]}`. `exit_code` is absent when process
creation itself failed, because no child existed from which to obtain one. If an
invalid frame has no valid request ID, the daemon cannot correlate a response
and closes the connection without writing one.

| Code | Message | Raised by |
| ---: | --- | --- |
| `-32600` | `invalid_frame` | Any method whose envelope or closed params are invalid, but only when a valid request ID is recoverable. |
| `-32602` | `invalid_hello` | `session.hello` when its union, protocol, identity, or token is invalid. |
| `-32001` | `unknown_session` | `message.send`, resume `lane.spawn`, `turn.run`, `turn.interrupt`, or `session.close` when the named row or peer does not exist or is invisible to the caller. |
| `-32002` | `not_connected` | `turn.run`, `turn.interrupt`, or `session.close` when a non-closed durable row has no connection. |
| `-32003` | `busy` | `turn.run` when the target already has an outstanding run. |
| `-32004` | `not_running` | `turn.interrupt` when the target has no outstanding run. |
| `-32005` | `already_connected` | Resume `lane.spawn` when the durable row already has its worker connection. |
| `-32006` | `closed` | Resume or control of an explicitly closed row; this check precedes connection state. |
| `-32007` | `unknown_product` | `lane.describe` or new `lane.spawn` when the product token is invalid or its binary is absent from the target host's service PATH. |
| `-32008` | `unsupported_open_field` | New or resumed `lane.spawn` when the supplied or stored `open` object contains a field absent from the new worker's hello declaration. |
| `-32009` | `spawn_failed` | `lane.describe` or `lane.spawn` when exec, hello, open, or native creation fails before commit. |
| `-32010` | `timeout` | `lane.describe` or `lane.spawn` when its one spawn/open transaction bound expires. |
| `-32011` | `not_committed` | Any worker-originated client-to-daemon session method received after hello but before native-ID commit. |
| `-32012` | `superseded` | Any request from a peer connection displaced by the atomic same-identity swap. |
| `-32013` | `name_taken` | New `lane.spawn` when another non-closed row on that daemon already holds the requested name. |
| `-32014` | `unknown_host` | `lane.describe` or new `lane.spawn` naming an unfederated `host`, or any operation using a host-qualified ID whose host is not connected. |
| `-32603` | `internal` | A durable-table write fails after in-memory cleanup, or a worker interrupt/close callback fails; it has no other use. |

### 3.1 Product contract

A native product links one dependency-free reference kit and supplies six
members. This is the complete product-facing contract:

| Member | Product responsibility |
| --- | --- |
| `hello(cancel)` | Return fixed product, version, supported open fields, and ordered extra-argument declarations after app-ready. |
| `open(cancel, request)` | Create or resume from the typed request and return the exact native ID. |
| `run(cancel, input)` | Start one native turn, observe it to a terminal result, and return that result. |
| `interrupt(cancel)` | Ask the one current native turn to stop. |
| `deliver(cancel, message)` | Inject now or queue for the next turn and return the truthful closed receipt. |
| `close(cancel)` | Stop accepting work, close native state, and release product resources. |

These are primitives, not a daemon adapter interface. They live in the product
process, receive only closed wire values, and never expose a product type to the
daemon. A product can replace our kit with its own implementation by passing the
same conformance fixtures; no daemon or schema change follows.
`cancel` is a Go context or JavaScript `AbortSignal`; control EOF cancels every
callback, and close cancels remaining work after the terminal boundary.

`permission_mode`, `model`, and `reasoning_effort` are opaque product-native
strings. The kit checks only whether each field was declared; `open()` validates
its value and reports `spawn_failed` with
`stderr_tail:["unsupported value <field>=<value>"]` when unsupported.
`open.arguments` is handed to the product in its exact order. Hello's
`extra_arguments` entries are caller documentation, not a daemon parser. Every
product owns one canonical native-argument builder; an argument selecting the
same native control as a typed open field fails open with `spawn_failed` and
`stderr_tail:["argument conflicts with typed field <name>"]`.

Callback failures map exactly once:

| Callback | Wire result |
| --- | --- |
| `open` | `spawn_failed` with `stderr_tail:[message]`; the daemon passes it through unchanged. |
| `run` | Terminal `{outcome:"failed",result:message}`; a run callback never returns an RPC error. |
| `interrupt` | `internal`. |
| `deliver` | Rejected receipt with the callback message as `reason`. |
| `close` | `internal`, followed by ordinary kit exit. |

The worker kit reads `AGENT_SESSIONS_LAUNCH_TOKEN` once, removes it from the
process environment, and connects to the daemon endpoint. It sends the worker
branch of `session.hello` only after `hello()` succeeds. It never logs, returns,
or copies the token into product configuration. `session.open` is the only call
that invokes `open()`. Until that result is written, the kit has no public
session identity and the daemon rejects its worker-originated session methods.

The kit has only two live facts: the connection is open or closed, and a run is
present or absent. The product's opened native reference is data, not a
lifecycle state. There is no generation, projection, collector, archive phase,
deadline, or reconnect state machine.
