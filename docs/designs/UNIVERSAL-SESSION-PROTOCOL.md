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

Each frame is one UTF-8 JSON object followed by a newline and is at most 1 MiB;
an oversized frame is closed silently because no request ID can be assumed.
Request IDs are integers in `[1, 2^53-1]`, with each direction counting from
one in its independent ID space. When JSON repeats a member, the last value
wins on both Go and JavaScript implementations. Notifications, batches,
unknown fields, explicit nulls, and a second hello are invalid. The first
request is `session.hello`. The daemon admits no other request from a spawned
worker until `session.open` has returned a native ID and that ID is durably
committed.

The closed parameter and result shapes are authoritative in
`integrations/shared/session.schema.json`. The schema uses only the shared
minimal validator vocabulary. An implementation rejects a frame before invoking
product code if the corresponding definition does not validate it.

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
recovery. Lane-row identity is immutable. Peers have no durable rows: same-ID
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

A session sends `lane.describe` with a product token. The daemon starts the
resolved executable with a rowless one-use token, consumes its worker hello,
returns the declared open fields, extra arguments, and optional product version,
then closes it without sending `session.open`. The hello is emitted only after
app-ready. Exit before hello fails with the exit code and bounded trailing
stderr; there is no readiness object or readiness phase.

#### `lane.spawn`

A session sends `lane.spawn` either with a caller-chosen `name`, product, open
options, and optional `extra_groups` for a new lane, or with
`resume_session_id` alone for a durable offline lane. A new row's groups are the
union of the caller's groups and `extra_groups`; a caller never sends the
complete authoritative group set. The daemon resolves and starts the worker,
waits for hello, sends `session.open`, durably
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
The worker kit is full-duplex: its reader never blocks on the native run, so
delivery, interrupt, and outbound tools dispatch concurrently with it.

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
it does. The daemon then closes the connection and records the row closed. If
the worker does not finish, the process supervisor's existing bounded
TERM-to-KILL close completes ownership cleanup. This supervisor close bound and
the spawn/open transaction bound are the only two lane-path timeouts.

If close arrives during a run, the worker kit interrupts once, awaits that same
run's terminal result within the supervisor bound, and then closes natively. A
terminal result observed in time is persisted before the row is closed. If the
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
- Worker-originated tools before the native ID commit are rejected. After the
  commit, the same connection is the lane's presence and uses the ordinary
  session methods.
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
| `-32007` | `unknown_product` | `lane.describe` or new `lane.spawn` when no executable resolves. |
| `-32008` | `unsupported_open_field` | New or resumed `lane.spawn` when the supplied or stored `open` object contains a field absent from the new worker's hello declaration. |
| `-32009` | `spawn_failed` | `lane.describe` or `lane.spawn` when exec, hello, open, or native creation fails before commit. |
| `-32010` | `timeout` | `lane.describe` or `lane.spawn` when its one spawn/open transaction bound expires. |
| `-32011` | `not_committed` | Any worker-originated tool request received after hello but before native-ID commit. |
| `-32012` | `superseded` | Any request from a peer connection displaced by the atomic same-identity swap. |
| `-32013` | `name_taken` | New `lane.spawn` when another non-closed row on that daemon already holds the requested name. |
| `-32603` | `internal` | A durable-table write fails after in-memory transaction cleanup; this is its only use. |

## 2. Daemon

### 2.1 Authority and data

The daemon is a router around one durable table. It does not know how any
product creates a session, runs a turn, injects a message, or closes. Native
products and wrappers expose those operations through the eleven methods in
Section 1. The daemon contains no product switch, lane actor, product driver,
capability interface, status projection, result collector, archive
transaction, or idle timer.

Only lanes have durable rows. A row has exactly these columns:

| Column | Meaning |
| --- | --- |
| `session_id` | Daemon-assigned immutable lane ID and primary key. |
| `product` | Immutable product token used to resolve the worker executable. |
| `name` | Immutable caller-chosen lane name. |
| `groups` | Immutable union of the spawning caller's groups and `extra_groups`. |
| `native_id` | Absent before commit; immutable after `session.open`. |
| `open` | The original closed `SessionOpenOptions` JSON, replayed byte-for-byte on resume. |
| `last_turn` | Absent until a terminal turn; thereafter the one overwrite-only recovery value. |
| `closed` | False until explicit `session.close`; never becomes false again. |
| `created_at` | Daemon timestamp assigned with `session_id`. |

There are no durable peer rows. A peer hello creates or replaces one transient
entry containing its asserted identity and exact connection. EOF removes that
exact entry. Lane EOF removes only the live connection; its row remains offline
and resumable. `SessionSummary.kind` is derived: a durable row is a lane and a
transient entry is a peer.

Daemon start performs no recovery pass. Every durable row starts offline;
connections, pending turns, row locks, and reservations start empty. Nothing is
replayed or reaped from a prior incarnation because its workers exited on EOF.
Restart is therefore a table load, not a lifecycle transition.

Three in-memory indexes are the complete live authority:

- `connections[session_id]` points to the exact current connection and, for a
  spawned lane, its process supervisor. Admission checks pointer identity on
  every request.
- `pending[session_id]` contains at most one forwarded `turn.run`: its input,
  worker request, and optional caller reply sink. The entry is not a turn state
  machine; existence means running.
- `rowLocks[session_id]` is one mutex per durable row. It serializes spawn,
  resume, and close transactions and is never persisted or exposed.

One `mapsMutex` owns current connections, pending turns, transient peers, and
creation of row locks. The only lock order is row lock then `mapsMutex`, never
the reverse, and no code calls a socket, process, or table while holding
`mapsMutex`. Listing snapshots the maps in one critical section and takes no
row lock. Exact-pointer EOF removal and pending-call failure happen in that same
critical section, so a summary cannot expose `connected=false` with a stale
`running=true`.

The durable table separately indexes `(product,native_id)` uniquely. No live
fact, connection ID, generation, process ID, deadline, caller ID, or
authorization decision is stored in a row.

### 2.2 Connection admission

The daemon accepts a socket, enforces the framing limits in Section 1, and
requires `session.hello` first. A peer hello is installed by one atomic map
swap after consulting the durable row table and rejecting an ID owned by a lane
as specified in Section 1.1. The displaced exact pointer becomes inadmissible
immediately; the daemon writes `session.superseded` through its ordinary
per-connection request-ID sequence with an immediate write deadline and closes
it without waiting for the acknowledgement. All of its pending calls fail
once. No reconnect lease or grace timer exists.

A worker hello resolves a one-use reservation created by `lane.describe` or
`lane.spawn`. The reservation binds token, product, transaction, and expiry.
Validation and token consumption are one locked operation. Wrong-product,
unknown, expired, or repeated hello is `invalid_hello`; cancellation removes an
unclaimed reservation. The connection stays provisional until open commits.
Before commit it may answer daemon calls but every worker-originated tool call
returns `not_committed`.

The protocol is trusted by construction: local peers assert identity and
groups, and a federated daemon asserts its own summaries. The daemon adds no
PID, peer-credential, descendant, signature, or capability machinery.

### 2.3 Describe and spawn

The executable registry is immutable configuration from product token to
executable plus fixed arguments. Fixed arguments describe a product mode, such
as `dsh --profile agent-sessions`; caller data never enters argv. Resolution
falls back to `asl-lane-<product>` on the service PATH with no fixed arguments.
The daemon accepts a product token only when it matches
`^[a-z0-9][a-z0-9-]{0,31}$`; an invalid token is `unknown_product`, so no path
separator reaches fallback resolution. The one-use token is present only in
the child's environment.

`lane.describe` creates a rowless reservation, starts the process, and waits
for either a valid hello, process exit, or the single spawn timeout. Valid hello
is the complete result. The daemon returns its declarations and synchronously
closes the supervised process without sending `session.open`. Describe creates
no row, native session, connection entry, or product-specific readiness state.

`lane.spawn` runs one sequential transaction while holding the row lock. For a
new lane it validates the open object, allocates identity and groups, and keeps
the prospective row private. Its name must be unique among non-closed rows on
that daemon; closed rows do not reserve names. For resume it checks `closed`
before connection,
then reuses the immutable row identity and stored open object. It resolves the
worker, creates the reservation, starts the supervised process, consumes hello,
checks declared open-field support, and sends one `session.open`. Resume includes
the stored `native_id`; new open omits it.

The socket reader, process watcher, and timeout feed one transaction event
channel whose events are consumed sequentially. On process exit the transaction
first drains the already-closed child socket to EOF and applies every complete
frame the child wrote. A timeout event is judged immediately, so a frame racing
the bound loses. If the open result is therefore consumed first, the daemon verifies a nonempty native ID,
requires exact equality on resume, checks global `(product,native_id)`
uniqueness, durably commits the row, installs the connection, and returns
success. A following EOF merely makes the committed row offline. If EOF arrives
first, spawn fails. No compensating close is attempted for a process that may
have created native files before commit; that explicitly accepted crash window
does not justify another state.

A failed durable write returns `internal` after cleaning the provisional
in-memory connection, pending entry, and reservation. There is no storage
retry. If writing `closed=true` fails after native close, the row remains
resumable and a later resume reports the native failure truthfully; this is the
same accepted-loss class as a crash between native close and commit.

The spawn timeout covers exec, hello, open, and commit. On failure the daemon
closes the socket and synchronously reaps the owned process tree. A concurrent
transaction waits on the same row lock, so a resumed native session never has
two worker processes. A lost successful reply is recovered by listing visible
lanes by name, not by replaying spawn.

Caller EOF never cancels describe, spawn, resume, or close. The transaction
runs to its commit or cleanup boundary and only its reply sink disappears;
describe still reaps its probe and a successful spawn still publishes its row.

The bound is the single constant `spawnTransactionTimeout = 60s` for every
product and for both describe and spawn. It is not configuration and has no
per-product override.

### 2.4 Routing, turns, and close

Every request resolves the caller from its exact current connection, resolves
the target among visible sessions, and then forwards the closed method without
translation. Invisible and absent targets are both `unknown_session`. The
daemon derives source identity and groups from its current peer entry or lane
row; caller-supplied authority never crosses the route.

For `turn.run`, the daemon rejects an existing pending entry as `busy`, installs
one new entry, and sends the request on the target connection. Caller
cancellation removes only the reply sink, not the worker request. A terminal
reply is validated and written to `last_turn` before the pending entry is
removed or any caller response is attempted. Worker EOF fails the pending
request once and leaves the prior `last_turn` unchanged; the daemon never
manufactures a terminal outcome.

`turn.interrupt` is an ordinary concurrent call to the same connection. The
worker kit owns the single native interrupt invocation. `message.deliver` is
also concurrent and its returned disposition is copied into the sender's
receipt. The daemon never queues a turn, interrupt, result, or delivery.

`session.close` holds the row lock and checks `closed` before connection. It
sends close while any run remains outstanding. The worker kit interrupts and
awaits that same run, so a terminal reply that arrives within the supervisor
bound follows the normal `last_turn` write. After the close result, or when the
bound expires, the daemon closes the connection and synchronously reaps the
owned tree before writing `closed=true`. Forced cleanup fails the pending run
once and preserves the previous `last_turn`. The row can never resume after
that commit.

Once close begins, the kit keeps the existing run slot occupied until process
exit even if the native run has already settled. A later `turn.run` is `busy`
without invoking product code, and delivery during close is rejected with
reason `closing`. This uses the existing run-slot fact; there is no closing
state.

The process bound is the single constant `supervisorTerminationGrace = 2s`:
send TERM, wait two seconds, then KILL and reap. It is not configuration and has
no per-product override. Together with `spawnTransactionTimeout`, these are the
only daemon lane-path clocks.

Unrequested lane EOF takes the same row lock, removes only the matching
connection pointer, fails calls once, and synchronously reaps the process tree.
A resume waits behind that cleanup. Peer EOF only removes the matching
transient peer entry. These pointer checks make late EOF from a displaced peer
or old process harmless without a generation counter.

### 2.5 Listing, messaging, and federation

`session.list` snapshots transient peers, current connections, pending turns,
and row-lock identities once under `mapsMutex`, releases it, then reads durable
rows. `connected` comes only from the exact
connection pointer; `running` comes only from the pending entry. Filtering and
group visibility happen before results are assembled. No roster cache or
projection stream exists.

`message.send` resolves and deduplicates visible targets, sends one
`message.deliver` per resolved current connection, and returns the product's
receipt unchanged. Offline lanes and failed calls produce rejected receipts.
Names are resolved only after exact local and host-qualified IDs; ambiguity is
a rejected unresolved receipt rather than a guessed delivery.

Federation carries the same closed request and response objects between trusted
daemons. A daemon publishes only transient peers and durable lane summaries;
the receiving daemon qualifies every remote ID with its authoritative host and
never persists a remote row. `session.list` merges those summaries and
`message.send` forwards a host-qualified delivery once.

Visible remote lanes remain controllable without a second public wire. Resume
`lane.spawn`, `turn.run`, `turn.interrupt`, and `session.close` addressed by a
host-qualified ID are forwarded one hop to the authoritative daemon, which
performs the same row lock, visibility, connection, and pending checks. New
spawn and describe are always local because neither names a remote host. A
federated turn is one outstanding RPC at each hop; caller loss has the same
sink-only effect, and the authoritative daemon alone writes `last_turn`.
All of this forwarding is one function of at most 60 logical lines: resolve the
host-qualified ID, forward the identical request once, and return the identical
response or error. It stores no remote state and never retries.

### 2.6 Package boundary

`internal/daemon` owns the table, current connections, pending turns, row
locks, reservations, routing, and federation. `cmd/agent-sessions` only parses
CLI/MCP input, constructs the daemon and immutable executable registry, and
renders results. Product selection never enters either request router.

`internal/livepresence` stays but becomes the product-agnostic full-duplex
connection implementation: bounded framing, closed schema validation,
independent request IDs, pending-call failure, supersession terminality, and
exact-pointer close. Product reports, reconnect policy, and method-specific
routing leave that package.

`internal/structuredprocess` stays. Its newline framing and bounded
TERM-to-KILL process ownership are generic wrapper/worker infrastructure; it
must not import a product or protocol state type. `internal/laneworker` does not
exist at the c5b280d base and is not introduced. The universal Go and JavaScript
worker kits belong with product integrations, not inside the daemon.

`internal/productruntime` dies completely. Its driver interfaces, per-product
registry, environment carrier, native references, and daemon-facing errors are
the architectural seam this protocol removes. Wrapper-specific native code is
assessed in Sections 3 and 4; it may reuse product primitives, but it cannot
restore a daemon driver interface.

The implementation size contract is measured as final logical production
lines, not as additions hidden behind relocation accounting:

| Surface | Maximum | Constraint |
| --- | ---: | --- |
| `internal/daemon` | 1,100 | Table, router, transactions, delivery, and federation together. |
| Largest daemon router file | 450 | No product literal, argv parser, or product callback. |
| Cross-host control forwarder | 60 | One hop, no state, retry, or shape translation. |
| `internal/livepresence` | 650 | Framing and connection mechanics only. |
| `internal/structuredprocess` | 700 | Generic process ownership; current functionality may remain. |
| `cmd/agent-sessions` daemon composition | 350 | Construction and rendering only; no protocol state. |

The daemon migration must therefore delete at least 8,000 net production and
test lines across the surfaces listed below, before any product wrapper
deletions are credited. A size breach is a design finding, not an invitation to
move the same state machine into a differently named package.

### 2.7 Migration deletion floor

The following files die rather than being retained as compatibility shims.
Counts are physical lines at exact base
`c5b280d8db4fc0069dae50365f3515c6de6ab57e`. Files retained for CLI rendering,
non-native wrappers, state storage, generic framing, process supervision, and
federation are deliberately not claimed here; Sections 3 and 4 decide their
product-side fate.

| Lines | File | Reason | Replacement / rehoming |
| ---: | --- | --- | --- |
| 1,116 | `cmd/agent-sessions/codex_host.go` | Product-composed host coordinator and attachment authority die. | Generic daemon construction plus the Codex wrapper in Section 4. |
| 445 | `cmd/agent-sessions/codex_host_test.go` | Tests the deleted coordinator. | Daemon transaction tests and Codex wrapper conformance tests. |
| 169 | `cmd/agent-sessions/control_retry_test.go` | Tests the deleted side control protocol. | Universal connection pending-call and supersession tests. |
| 41 | `cmd/agent-sessions/dsh_lane.go` | Daemon-side DSH driver composition dies. | Immutable executable registry plus the native DSH plugin. |
| 348 | `cmd/agent-sessions/federation.go` | Product-aware federation router dies. | The generic one-hop forwarding function. |
| 444 | `cmd/agent-sessions/federation_test.go` | Tests the deleted router. | Daemon federation proofs in Section 5. |
| 1,628 | `cmd/agent-sessions/lane.go` | Lane actor, parsers, lifecycle, and product dispatch die. | Daemon table/router plus caller-kit composition. |
| 94 | `cmd/agent-sessions/lane_names.go` | Actor-derived name authority dies. | Durable-table non-closed name index. |
| 149 | `cmd/agent-sessions/lane_notice.go` | Terminal notice and collection machinery die. | `last_turn` write plus filtered `session.list`. |
| 1,245 | `cmd/agent-sessions/lane_test.go` | Tests the deleted lane machinery. | Daemon transaction tests and shared kit fixtures. |
| 746 | `cmd/agent-sessions/messaging.go` | Product-aware peer/lane routing dies. | Generic daemon resolution and delivery. |
| 662 | `cmd/agent-sessions/messaging_test.go` | Tests the deleted messaging router. | Daemon delivery and federation proofs in Section 5. |
| 434 | `cmd/agent-sessions/presence.go` | Report/projection presence server dies. | Universal connection admission in `internal/daemon`. |
| 1,257 | `cmd/agent-sessions/presence_test.go` | Tests the deleted presence server. | Universal admission, listing, EOF, and swap tests. |
| 68 | `cmd/agent-sessions/preparation.go` | Old host preparation composition dies. | Minimal command composition and executable registry. |
| 43 | `cmd/agent-sessions/socket_test.go` | Tests the deleted command-side socket server. | Socket helper moves with retained connector endpoint tests in Section 4. |
| 41 | `internal/daemon/admin.go` | Side-channel admin operation dies. | Ordinary `session.list` and `lane.describe` routes. |
| 164 | `internal/daemon/admin_test.go` | Tests deleted admin routing. | Daemon method-table tests. |
| 22 | `internal/daemon/adapter_authorization_test.go` | Adapter authorization seam dies. | Visibility-as-authority router tests. |
| 53 | `internal/daemon/adapter_claude.go` | Claude attachment adapter dies. | Claude wrapper owns native attachment. |
| 61 | `internal/daemon/adapter_claude_test.go` | Tests the deleted adapter. | Claude wrapper conformance tests. |
| 91 | `internal/daemon/adapter_codex.go` | Codex attachment adapter dies. | Codex wrapper owns app-server attachment. |
| 65 | `internal/daemon/adapter_codex_test.go` | Tests the deleted adapter. | Codex wrapper conformance tests. |
| 53 | `internal/daemon/adapter_grok.go` | Grok attachment adapter dies. | Grok wrapper owns native attachment. |
| 54 | `internal/daemon/adapter_grok_test.go` | Tests the deleted adapter. | Grok wrapper conformance tests. |
| 47 | `internal/daemon/adapter_qwen.go` | Qwen attachment adapter dies. | Qwen wrapper owns ACP attachment. |
| 67 | `internal/daemon/adapter_qwen_test.go` | Tests the deleted adapter. | Qwen wrapper conformance tests. |
| 412 | `internal/daemon/attachment.go` | Attachment transaction engine dies. | One hello admission path and one spawn/open transaction. |
| 140 | `internal/daemon/attachment_test.go` | Tests the deleted engine. | Daemon admission and transaction tests. |
| 314 | `internal/daemon/control.go` | Role-based side control envelope dies. | The universal schema-driven method router. |
| 268 | `internal/daemon/control_test.go` | Tests the deleted envelope. | Shared schema and method-direction tests. |
| 399 | `internal/daemon/control_unix.go` | Side control server dies. | One universal endpoint using `internal/livepresence`. |
| 189 | `internal/daemon/control_unix_test.go` | Tests the deleted server. | Universal endpoint framing and close tests. |
| 92 | `internal/daemon/lane.go` | Daemon lane transition helper dies. | Direct durable-row transaction functions. |
| 89 | `internal/daemon/lane_test.go` | Tests the deleted helper. | Row transaction table tests. |
| 13 | `internal/daemon/socket_test_helper_test.go` | Helper exists only for the deleted control server. | Helper moves with connector tests in Section 4. |
| 75 | `internal/productruntime/architecture_test.go` | Tests the deleted central driver seam. | Generic no-product-daemon architecture test. |
| 47 | `internal/productruntime/drivers.go` | Product driver interfaces die. | Native kit callbacks and Section 4 wrapper-local primitives. |
| 17 | `internal/productruntime/environment.go` | Hidden daemon-to-product environment carrier dies. | Each wrapper owns its child environment. |
| 23 | `internal/productruntime/environment_test.go` | Tests the deleted carrier. | Wrapper launch tests. |
| 19 | `internal/productruntime/errors.go` | Driver error vocabulary dies. | Closed Section 1.4 wire errors. |
| 29 | `internal/productruntime/fakes_test.go` | Fakes exist only for deleted drivers. | Shared kit fixtures and wrapper-local fakes. |
| 31 | `internal/productruntime/lane_registry.go` | In-process product driver registry dies. | Immutable product-to-executable registry in daemon composition. |
| 31 | `internal/productruntime/lane_registry_test.go` | Tests the deleted registry. | Executable resolution tests. |
| 110 | `internal/productruntime/registry.go` | Host dependency/product composition registry dies. | Command composition plus wrapper constructors. |
| 107 | `internal/productruntime/registry_test.go` | Tests the deleted registry. | Command composition and wrapper launch tests. |
| 251 | `internal/productruntime/types.go` | Daemon-facing native structs and capabilities die. | Shared schema types move to their connection, storage, process, or wrapper owners. |

This floor is **47 files and 12,263 deleted lines**: 16 command files / 8,889
lines, 20 daemon files / 2,634 lines, and all 11 product-runtime files / 740
lines. It excludes product wrapper and kit migration on purpose, so later
sections may only increase the deletion total, not use this number to hide
replacement code.

## 3. Native product kits

### 3.1 Product contract

A native product links one dependency-free reference kit and supplies six
members. This is the complete product-facing contract:

| Member | Product responsibility |
| --- | --- |
| `hello()` | Return fixed product, version, supported open fields, and ordered extra-argument declarations after app-ready. |
| `open(request)` | Create or resume from the typed request and return the exact native ID. |
| `run(input)` | Start one native turn, observe it to a terminal result, and return that result. |
| `interrupt()` | Ask the one current native turn to stop. |
| `deliver(message)` | Inject now or queue for the next turn and return the truthful closed receipt. |
| `close()` | Stop accepting work, close native state, and release product resources. |

These are primitives, not a daemon adapter interface. They live in the product
process, receive only closed wire values, and never expose a product type to the
daemon. A product can replace our kit with its own implementation by passing the
same conformance fixtures; no daemon, registry, or schema change follows.

The worker kit reads `AGENT_SESSIONS_LAUNCH_TOKEN` once, removes it from the
process environment, and connects to the daemon endpoint. It sends the worker
branch of `session.hello` only after `hello()` succeeds. It never logs, returns,
or copies the token into product configuration. `session.open` is the only call
that invokes `open()`. Until that result is written, the kit has no public
session identity and the daemon rejects its outbound tools.

The kit has only two live facts: the connection is open or closed, and a run is
present or absent. The product's opened native reference is data, not a
lifecycle state. There is no generation, projection, collector, archive phase,
deadline, or reconnect state machine.

### 3.2 Full-duplex lifecycle

One reader continuously validates and dispatches inbound requests; it never
awaits `run()` inline. One writer mutex preserves complete frames. Independent
request IDs correlate outbound tools and inbound results, so `deliver`,
`interrupt`, `session.close`, and product-originated tools all proceed while
`turn.run` is outstanding.

The run slot is installed before `run()` starts and cleared only after its
validated terminal response is written. A second run receives `busy` without
calling product code. Interrupt atomically marks that slot once and invokes
`interrupt()` once; concurrent and later interrupt requests for the same run
return `{}` without a second native call. No kit timeout is involved.

Close is idempotent. With no run, it calls `close()` once and responds. With a
run, it claims or joins the same one interrupt, awaits the same run result,
writes that terminal response, then calls `close()` and responds. If product
code does not settle, the daemon's two-second process supervisor terminates the
whole worker; the kit invents no result. An ordinary worker EOF cancels product
contexts, invokes close once, and exits rather than reconnecting with a consumed
token. From the instant close begins, the run slot remains occupied until exit:
new runs are `busy`, and delivery is rejected as `closing`.

A peer-mode connection behaves differently only at the connection boundary:
ordinary daemon EOF reconnects the same asserted peer identity, while
`session.superseded` tombstones that identity instance and prevents reconnect.
Worker mode never reconnects. The method router, closed validation, pending
calls, writer, delivery, and outbound-tool API are otherwise shared.

### 3.3 Go and JavaScript parity

The Go host and JavaScript worker mode implement the preceding algorithm, not
two interpretations of it. Both load `integrations/shared/session.schema.json`
and both run one declarative table,
`integrations/shared/session-lifecycle.fixtures.json`. The table drives fake
product callbacks and a fake duplex connection; it contains at least these
rows:

1. app-ready hello, one open, and outbound tools rejected before the open
   result but accepted on the same connection after it;
2. describe hello followed by EOF, proving open is never called and close is
   called once;
3. completed, interrupted, and failed run results, including empty output;
4. a blocked run plus a second run rejected before product code;
5. concurrent interrupt requests and close racing interrupt, with exactly one
   native interrupt;
6. delivery and an outbound tool completing while run remains blocked;
7. close during run, with the terminal run response written before the close
   response;
8. control EOF during run, with all pending calls failed and product close
   invoked once;
9. peer EOF reconnect versus supersession terminality; and
10. malformed, unknown, oversized, and out-of-range-ID frames rejected before
    a callback.

There are no product names, native IDs, clocks, sleeps, or network sockets in
the fixture data. Tests control every callback and frame boundary
deterministically.

The size contract is final logical lines:

| Reference surface | Production | Tests |
| --- | ---: | ---: |
| Go worker host | 280 | 300 |
| JavaScript client plus worker mode | 260 | 260 |
| Shared lifecycle fixture data | — | 180 |

Schema validation and generic connection framing are counted in Section 2,
not duplicated into either worker host. A kit exceeding these limits has grown
product policy or a third lifecycle fact and must be simplified.

### 3.4 DSH: the first native worker

DSH uses one Agent Sessions plugin and one connection per DSH root session. In
ordinary product mode the plugin sends peer hello without a launch token and
reconnects after daemon EOF. In lane mode it captures and scrubs the token,
waits for DSH app-ready, and sends worker hello on that same socket. It does not
also publish peer presence: successful `session.open` turns the worker
connection itself into the lane's presence, tool path, and delivery path.

The DSH `agent-sessions` profile is exactly headless DSH core plus the unified
plugin configured `mode: lane`; no TUI, second comms plugin, lane extension,
relay, or local socket is loaded. Its package profile is
`{"bundles":["@deepseek-ai/dsh-base","@deepseek-ai/dsh-headless"],"patchReload":"startup"}`
under `dsh.profile`, and its `cordis.patch.yml` inserts exactly
`{id: agent-sessions, name: '@agent-sessions/dsh', config: {mode: lane}}` after
bundle patches and before any `--patch` overlays. A non-disabled plugin row is
load-mandatory by DSH boot semantics: import failure exits one with "plugin tree
failed to load". The executable registry entry is fixed
`dsh --profile agent-sessions`. In a normal DSH profile the same plugin runs in
peer mode. Lane mode without a launch token is a startup error rather than an
accidental peer.

The c5b280d integration already proves the DSH core primitives the unified
plugin needs:

- `appReady.onReady` and `appExit` gate hello and terminate headless mode;
- `sessionController.create` and `resolveAgent` create or resume an exact
  session; `rename` and `selectModel` apply typed open identity/options;
- `permissionPresets.names` and `permissionPresets.set(session, name)`, plus
  `sessions.flush`, commit the open configuration; `apply` is private and is
  never part of the integration;
- `createUserMessage` with the exact `agent.followup(message)` call starts the
  one requested turn. At c5b280d this is the effective call made by
  `integrations/dsh/lane/plugin.cjs:118` as `agent[mode](message)`, with
  `internal/products/dsh/lane.go:157` supplying the constant mode `followup`;
- root-context `ctx.on('session/event', (session, event) => ...)`, together with
  `session/created` and `session/disposed`, supplies input receipt, turn start,
  assistant text, and terminal reason; no polling is required;
- `agent.cancel` interrupts, and `agent.whenIdle` supports orderly close;
- `agent.steer` injects during a run. While idle, the plugin calls
  `session.append('user/message', message, {surfaceOp: 'append'})`, the confirmed
  public primitive at `packages/core/session/src/index.ts:668-716`; it writes
  synchronously to the durable surface, starts no turn, and the next request is
  built from that surface. The plugin reports `injected`. `agent.inject` is not
  a substitute because its inbox splice is claimed only by a running turn; and
- `tools.register` exposes the product's Agent Sessions tool while the kit's
  outbound call API carries it on the same session socket.

Fresh open creates the DSH session only after the typed request arrives and
returns its exact ID. Resume resolves `resume_native_id` and returns that same
ID. Run submits one user message and converts the observed DSH terminal reason
to the three wire outcomes. Deliver never starts an unrequested DSH turn and
the native plugin contains no delivery queue; `queued_for_next_turn` exists for
non-native wrappers only. Close cancels if needed, flushes the session, and
calls `appExit`; control EOF follows the same product cleanup and exit path.

The DSH-specific layer is capped at 300 production and 300 test logical lines,
excluding the generic JavaScript kit and shared fixtures. Its conformance result
must state: **contract learned nothing from the DSH adapter: yes**.

### 3.5 DSH migration

The entire Go package `internal/products/dsh` dies: 10 files and 1,031 physical
lines at c5b280d. Product probing moves to `lane.describe`; lane process
ownership moves to the generic daemon supervisor; permission, model, session,
turn, and delivery translation move inside the native plugin where the DSH
primitives exist.

The separate `integrations/dsh/lane` package also dies in full: 5 files and 464
lines. Its extension registration, presence-served lane RPC, environment
translation, second package manifest, and second Cordis patch are all forbidden
by the one-plugin/one-connection design. The nine-line
`integrations/dsh/comms/prepack.cjs` dies in favor of the repository's generic
packaging step.

`integrations/dsh/comms` remains under its current package identity and is
documented as the universal DSH plugin. Its plugin and tests are rewritten
around the shared JavaScript kit; its Cordis patch installs peer mode, while the
headless `agent-sessions` profile pins lane mode. Package/install inventory must
contain one DSH integration artifact, not the old comms-plus-lane pair.

The DSH migration therefore adds **16 more deleted files and 1,504 deleted
lines** before rewriting the retained unified plugin. Combined with Section 2,
the signed deletion floor becomes **63 files and 13,767 lines**. The DSH tree
must remain net-negative after the kit is accounted separately.
