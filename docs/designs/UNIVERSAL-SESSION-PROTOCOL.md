# Universal Session Protocol

Status: design in progress. Section 1 is the proposed wire contract; later
sections will derive the daemon, native product kits, DSH integration, wrappers,
and migration from it.

## 1. Wire

### 1.1 Roles and connection model

A session is one JSON-RPC 2.0 connection. A peer is a session opened by a
product. A lane is a session spawned by the daemon with a one-use launch token
and therefore also accepts daemon-directed session and turn methods. The same
connection carries presence, tools, message delivery, lane control, and turn
results. There is no child presence connection and no relay connection to the
daemon.

Each frame is one UTF-8 JSON object followed by a newline. Requests have an
integer `id`; the two directions have independent ID spaces. Notifications,
batches, unknown fields, explicit nulls, and a second hello are invalid. The
first request is `session.hello`. The daemon admits no other request from a
spawned worker until `session.open` has returned a native ID and that ID is
durably committed.

The closed parameter and result shapes are authoritative in
`integrations/shared/session.schema.json`. The schema uses only the shared
minimal validator vocabulary. An implementation rejects a frame before invoking
product code if the corresponding definition does not validate it.

Authorization is visibility: a session may run, interrupt, close, or message a
lane exactly when it can see that lane through a shared group in `session.list`.
This trusted protocol has no ownership field or per-caller ACL.

There are eleven methods.

#### `session.hello`

The session sends `session.hello` first. The request is one closed union. A peer
supplies `session_id`, `name`, `groups`, and `info`. A worker instead supplies a
one-use `launch_token`, its supported non-identity open fields, its ordered
extra-argument descriptions, and optionally the product version. The branches are mutually
exclusive: a request with both discriminants or neither is invalid. A worker
sends hello only after its product and plugin are app-ready, so hello success is
the sole readiness fact. The result is `{}`.

#### `session.superseded`

The daemon sends the displaced connection the ID-bearing request
`session.superseded` with `{}` when a new peer connection claims the same
identity. The displaced client marks that identity terminal before its
best-effort `{}` response, closes, and never reconnects that identity. The new
connection is already current, so every request from the displaced connection
is rejected.

#### `session.list`

A connected session sends `session.list` with an optional `session_id` filter.
The result contains the matching visible sessions, or all visible sessions when
the filter is absent. Each item reports immutable identity, whether its one
connection is open, whether one `turn.run` is outstanding, whether it was
explicitly closed, the lane native ID when applicable, and the lane's optional
`last_turn`. A remote item also carries its federated `host`. This single method
replaces peer listing, lane listing, lane status,
and lost-caller result recovery.

#### `message.send`

A connected session sends `message.send` to exactly one `target`, one explicit
`targets` list, or one `group`. The daemon resolves recipients from current
connections, sends each one `message.deliver`, and returns one truthful receipt
per attempted delivery. The request retains one message body; explicit
multicast and group expansion do not create another protocol method.

#### `message.deliver`

The daemon sends `message.deliver` to the target session with the message ID,
authoritative source identity, and body. Every product implements delivery while
idle and while a turn is running. Its result is exactly one closed receipt:
`injected`, `queued_for_next_turn`, or `rejected` with a nonempty reason. A
non-native wrapper may implement `queued_for_next_turn` by prepending the body to
its next native turn; that queue is invisible to the daemon and to this wire.

#### `lane.describe`

A session sends `lane.describe` with a product token. The daemon starts the
resolved executable with a rowless one-use token, consumes its worker hello,
returns the declared open fields, extra arguments, and optional product version,
then closes it without sending `session.open`. The hello is emitted only after
app-ready. Exit before hello fails with the exit code and bounded trailing
stderr; there is no readiness object or readiness phase.

#### `lane.spawn`

A session sends `lane.spawn` either with a product plus open options and optional
`extra_groups` for a new lane, or with `resume_session_id` alone for a durable
offline lane. A new row's groups are the union of the caller's groups and
`extra_groups`; a caller never sends the complete authoritative group set. The
daemon resolves and starts the worker, waits for hello, sends `session.open`, durably
commits its native ID, and only then returns `{session_id,native_id}`. The row
stores the original closed open-options object as one JSON value. Resume replays
that value verbatim with the stored native ID; this version permits no resume
overrides. The one spawn/open transaction timeout covers all of those steps.
Unsupported supplied
open fields, an invalid native ID, exit, or timeout fail truthfully and do not
publish a live session.

#### `session.open`

The daemon sends `session.open` only to a token-authenticated worker. It carries
the daemon session ID, always-present row-authoritative `name` and `groups`, the
stored native ID when resuming, and one closed `open` object containing only the
non-identity options accepted by `lane.spawn`. The worker creates or resumes the
native session and returns `{native_id}`. A successful result is
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

#### `turn.interrupt`

Either a caller sends `turn.interrupt` to the daemon or the daemon forwards it
to the addressed lane. The request carries only `session_id`; `{}` means the
product accepted the interrupt request. An idle target returns the closed
not-running error. There is no interrupt grace timer and no `timed_out` outcome:
if the native run remains unresponsive, the caller closes the session.

#### `session.close`

Either a caller sends `session.close` to the daemon or the daemon sends it to
the addressed lane. The worker asks the product to close and returns `{}` when
it does. The daemon then closes the connection and records the row closed. If
the worker does not finish, the process supervisor's existing bounded
TERM-to-KILL close completes ownership cleanup. This supervisor close bound and
the spawn/open transaction bound are the only two lane-path timeouts.

An unrequested EOF marks a durable lane offline and resumable; it does not mark
the row explicitly closed. There is no daemon auto-archive policy. A future
protocol may add an idle-close option implemented wholly by a product kit, but
this version has none.

### 1.2 Edge rules

- A hello with both `session_id` and `launch_token`, with neither, or with fields
  from the other branch is invalid and closes the connection.
- A rowless describe token can authenticate hello but can never authorize
  `session.open`; EOF after describe is the worker's normal exit.
- Worker-originated tools before the native ID commit are rejected. After the
  commit, the same connection is the lane's presence and uses the ordinary
  session methods.
- A second `turn.run` while one is outstanding returns busy. There is one
  running boolean in a product kit and one pending RPC in the daemon.
- `lane.spawn` with `resume_session_id` naming a connected row returns
  `already_connected`; lanes never use supersession to manufacture a second
  worker connection.
- `lane.spawn` with `resume_session_id` naming an explicitly closed row returns
  `closed`. The row remains visible only for identity and `last_turn` history.
- `turn.run`, `turn.interrupt`, or `session.close` addressed to a durable row
  without a connection returns `not_connected`. Resume is an explicit
  `lane.spawn`; a caller kit may compose that automatically without changing the
  wire.
- `turn.interrupt` while no run is outstanding returns not-running. An accepted
  interrupt does not promise that the native product has already stopped.
- Caller timeout or disappearance does not cancel the forwarded run. The daemon
  drains its response, writes `last_turn` first, and discards only the abandoned
  response. There is no result queue beyond the one overwrite-only row value.
- Worker EOF interrupts or closes any native run through product cleanup, fails
  pending calls exactly once, and leaves a non-closed durable row offline.
- Same-identity replacement atomically makes the new peer current, sends
  `session.superseded` to the old exact connection, and prevents reconnection by
  that displaced identity instance.
- Delivery is mandatory. Products that cannot inject during a native run queue
  for the next turn in their wrapper and report that disposition; the daemon
  has no injection capability flag or queue.
- A worker executable that is absent or exits before hello fails describe or
  spawn with its exit code and bounded trailing stderr. It never fabricates a
  readiness report.
- A remote session may carry `host` in `session.list`. Whether a turn crosses a
  host boundary is a daemon-routing question for Section 2, not a second wire
  shape.

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
| `-32001` | `unknown_session` | `message.send`, resume `lane.spawn`, `turn.run`, `turn.interrupt`, or `session.close` when the named row or peer does not exist. |
| `-32002` | `not_connected` | `turn.run`, `turn.interrupt`, or `session.close` when a durable row has no connection. |
| `-32003` | `busy` | `turn.run` when the target already has an outstanding run. |
| `-32004` | `not_running` | `turn.interrupt` when the target has no outstanding run. |
| `-32005` | `already_connected` | Resume `lane.spawn` when the durable row already has its worker connection. |
| `-32006` | `closed` | Resume or control of an explicitly closed row. |
| `-32007` | `unknown_product` | `lane.describe` or new `lane.spawn` when no executable resolves. |
| `-32008` | `unsupported_open_field` | New `lane.spawn` when `open` contains a field absent from the worker hello declaration. |
| `-32009` | `spawn_failed` | `lane.describe` or `lane.spawn` when exec, hello, open, or native creation fails before commit. |
| `-32010` | `timeout` | `lane.describe` or `lane.spawn` when its one spawn/open transaction bound expires. |
| `-32011` | `not_committed` | Any worker-originated tool request received after hello but before native-ID commit. |
| `-32012` | `superseded` | Any request from a peer connection displaced by the atomic same-identity swap. |
