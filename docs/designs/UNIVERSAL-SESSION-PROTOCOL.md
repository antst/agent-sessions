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
| `-32603` | `internal` | A durable-table write fails after in-memory cleanup, or a worker interrupt/close callback fails; it has no other use. |

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
If EOF or supersession ends the target connection before a receipt arrives,
the sender receives rejected reason `no_receipt`; whether product code acted is
unknowable and is deliberately not promised.
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
| `hello(cancel)` | Return fixed product, version, supported open fields, and ordered extra-argument declarations after app-ready. |
| `open(cancel, request)` | Create or resume from the typed request and return the exact native ID. |
| `run(cancel, input)` | Start one native turn, observe it to a terminal result, and return that result. |
| `interrupt(cancel)` | Ask the one current native turn to stop. |
| `deliver(cancel, message)` | Inject now or queue for the next turn and return the truthful closed receipt. |
| `close(cancel)` | Stop accepting work, close native state, and release product resources. |

These are primitives, not a daemon adapter interface. They live in the product
process, receive only closed wire values, and never expose a product type to the
daemon. A product can replace our kit with its own implementation by passing the
same conformance fixtures; no daemon, registry, or schema change follows.
`cancel` is a Go context or JavaScript `AbortSignal`; control EOF cancels every
callback, and close cancels remaining work after the terminal boundary.

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
session identity and the daemon rejects its outbound tools.

The kit has only two live facts: the connection is open or closed, and a run is
present or absent. The product's opened native reference is data, not a
lifecycle state. There is no generation, projection, collector, archive phase,
deadline, or reconnect state machine.

### 3.2 Full-duplex lifecycle

One reader continuously validates and dispatches inbound requests; it never
awaits any product callback inline. This permits open, deliver, interrupt, or
close code to make an outbound tool call and receive its response. One writer
mutex preserves complete frames. Independent
request IDs correlate outbound tools and inbound results, so `deliver`,
`interrupt`, `session.close`, and product-originated tools all proceed while
`turn.run` is outstanding.

The run slot is installed before `run()` starts and cleared only after its
validated terminal response is written. A second run receives `busy` without
calling product code. Interrupt atomically marks that slot once and invokes
`interrupt()` once; concurrent and later interrupt requests for the same run
return `{}` without a second native call. No kit timeout is involved.

If `run()` has returned but its response has not yet been written, interrupt
returns `{}` without invoking native code. The run handler alone writes the run
response; close may await the slot's completion signal but never owns that
response.

Close is idempotent and first claims the empty run slot or joins the existing
one. With no run, it calls `close()` once and responds. With a run, it invokes
the shared interrupt once without awaiting that callback, awaits the run
result, lets the run handler write its response, then calls `close()` and
responds. A hanging interrupt cannot hide an available terminal. If product
code does not settle, the daemon's two-second process supervisor terminates the
whole worker; the kit invents no result. From the instant close owns the slot,
new runs are `busy`, and delivery is rejected as `closing` before product code.

The kit owns final process ordering: it calls `close()`, writes the close
response, closes its socket, and then resolves its `closed` signal. The product
awaits that signal before process exit; `closed` is a kit signal, not a seventh
callback. An ordinary worker EOF cancels product contexts, invokes close once,
and exits rather than reconnecting with a consumed token.

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
9. peer EOF reconnect versus supersession terminality;
10. malformed, unknown, oversized, and out-of-range-ID frames rejected before
    a callback;
11. a terminal returned before interrupt is decoded, proving no native
    interrupt call;
12. idle close followed by delivery, proving `closing` and zero delivery calls;
13. a non-run callback that invokes an outbound tool and receives its response;
    and
14. single token read plus environment removal, followed by connect failure and
    process exit without reconnect.

There are no product names, native IDs, clocks, sleeps, or network sockets in
the fixture data. Tests control every callback and frame boundary
deterministically.

The size contract is final logical lines:

| Reference surface | Production | Tests |
| --- | ---: | ---: |
| Go worker host | 280 | 300 |
| JavaScript client plus worker mode | 260 | 260 |
| Shared lifecycle fixture data | — | 220 |

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
non-native wrappers only. A running delivery is `injected` exactly when
`agent.steer` resolves; there is no receipt polling. A DSH rename is a fresh
peer hello on a new connection that supersedes the old connection, never a
status update. The registered Agent Sessions tool exposes the caller kit's
start/wait/status/spawn/describe/close/list/send surface defined once in
Sections 4 and 5. Close cancels if needed, flushes the session, waits for the
kit's `closed` signal, and calls `appExit(0)`; control EOF follows the same
product cleanup and exit path.

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

## 4. Non-native resident wrappers

### 4.1 Shared wrapper boundary

| Ledger item | Decision | Reason |
| --- | --- | --- |
| Process and connection | Lane mode starts one resident wrapper, which owns one product session and the one daemon connection. Peer mode has no wrapper around the interactive TUI: the product-spawned stdio MCP server or JavaScript plugin owns the direct peer connection. | The connection holder is the integration process the product already supervises. Wrapping an interactive TUI would add terminal, signal, resize, and hand-started-session failure modes without improving the protocol. |
| Installed entry forms | One installed integration image exposes two entry forms: peer MCP/plugin mode holds a direct peer connection; lane-wrapper mode holds a worker connection and owns a headless child. | Sharing code and kits is required; sharing process topology would violate the one-connection rule in one of the two modes. |
| Product boundary | The wrapper exposes the six Section 3 callbacks locally and contains every product import, executable name, argument translation, native protocol, and delivery compromise. | Deleting one wrapper when a vendor adopts the native kit must require no daemon, schema, caller-kit, or registry change. |
| Child launch | The lane wrapper connects and sends worker hello before starting a native child. It receives and validates `session.open`, then spawns the child with the stored cwd, model, reasoning, permission, and ordered argument values. | Process-level flags are ordinary open fields for wrappers because the product does not exist until open. Native products start before open and therefore need session-level primitives instead. |
| Tool ingress | In lane mode the wrapper serves loopback HTTP MCP when the product accepts HTTP; otherwise a packaged stdio helper or JavaScript plugin connects only to a private wrapper endpoint. In peer mode that same MCP entry or plugin connects directly to the daemon through the peer kit. | Each mode still has exactly one daemon connection: wrapper-owned for a lane, product-integration-owned for a peer. Private lane helpers never become presence. |
| Wrapper-only queue | A wrapper that lacks native append/injection owns one in-memory FIFO capped at 64 deliveries and 1 MiB total. At run start it atomically swaps and canonically renders that FIFO before the caller input; overflow is rejected as `queue_full`. | `queued_for_next_turn` must be truthful and bounded. Loss with wrapper exit is the accepted loss in Section 1.2, not durable daemon state. |
| Caller tool surface | Every product exposes the same caller-kit start/wait/status/spawn/describe/close/list/send operations; product plugins do not invent wire methods. | Tool presentation is kit sugar over the eleven methods and is identical for native and wrapped products. |
| Shared size cap | Wrapper host, local HTTP MCP, stdio/plugin bridge protocol, and bounded FIFO together: **400 production / 400 test logical lines**. | Product-independent scaffolding larger than the daemon router would be a second protocol implementation. |
| Shared deletion | Delete the 16 non-product-specific files in `internal/launcher` (2,199 lines) and `cmd/agent-sessions/connector_refresh.go` plus its test (333 lines). Rewrite `connector.go`/test as the peer-mode MCP entry that owns a direct peer connection, and rewrite `native_peer.go` as thin exec-time product configuration; lane-wrapper composition is separate. | The old launcher package still dies: CLI parsing and thin peer exec plans move to `cmd`, lane process ownership to `structuredprocess`, and lane recipes to wrappers. Connector self-exec/release refresh is unnecessary when the installed MCP entry already is the peer connection holder. |

### 4.2 Claude Code

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-claude` owns one long-lived `claude -p --input-format stream-json --output-format stream-json --verbose --replay-user-messages` child. The peer launcher still execs interactive Claude; Claude's product-spawned Agent Sessions stdio MCP server owns the peer connection. | The lane stream is proven at c5b280d `internal/products/claude/lane.go:112-145`. Peer presence needs no TUI wrapper and continues to work for hand-started Claude sessions. |
| Open and resume | Fresh adds `--session-id <daemon session_id> --name <row name>`; resume uses `--resume <native_id>`. Claude supports all five open fields: `cwd`, `permission_mode`, `model`, `reasoning_effort`, and `arguments`; the wrapper maps model to `--model` and effort to `--effort` when it starts the child after open. | c5b280d maps the other fields at `lane.go:112-141`; Claude's product CLI help exposes both `--model <model>` and `--effort <level>`. Process flags are available because no child exists before open. |
| Run | Write one stream-json user frame, keep the run callback pending, and convert the exact result frame to the terminal result. | `lane.go:173-225` already proves the single stream write plus terminal observation. |
| Tools | In lane mode the wrapper serves loopback HTTP MCP and supplies that endpoint in Claude's launch configuration. In peer mode Claude starts the stdio MCP entry, which owns the direct peer connection. | The two entry forms share the caller kit but never coexist for one session; lane mode has wrapper presence and peer mode has MCP-server presence. |
| Deliver | While a run is active, write the same user frame and report `injected`; while idle, use the bounded wrapper FIFO and report `queued_for_next_turn`. | Active stream injection is proven by `lane.go:224-249`. The c5 idle `SendMessage` also writes a user frame (`lane.go:251-274`) and would start unrequested work, so it cannot be called idle under the universal contract. |
| Interrupt and close | Interrupt writes the native `control_request` subtype `interrupt`; close ends the stream and reaps the exact child. | `lane.go:277-321` proves both operations and their native acknowledgements. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded delivery FIFO. | Every open value has a native process flag or stream mapping; the delivery compromise remains isolated behind `deliver` and the daemon contains no Claude branch. |
| Size cap | **360 production / 400 test logical lines**, including stream framing and Claude argument translation but excluding the shared wrapper host. | The current 578-line actor combines generic lifecycle with product translation; the generic kit removes that duplication. |
| Deletion inventory | Delete all `internal/products/claude` (3 files / 1,108 lines) and `internal/launcher/{claude_peer.go,claude_peer_test.go}` (2 / 183). Rewrite `claude/.mcp.json`; retain product docs/skills. Total: **5 files / 1,291 lines**. | The lane wrapper replaces the daemon driver; the peer launcher is rehomed as a thin `cmd` exec plan and the MCP entry remains the peer connection holder. No compatibility adapter remains. |

### 4.3 Codex

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-codex` owns one session-specific App Server client/subscription. The peer launcher execs interactive Codex; Codex's product-spawned Agent Sessions stdio MCP server owns the peer connection. | The lane surface is already App Server RPC (`internal/bridge/codex_native.go`). Peer presence does not require the lane wrapper or a host-global coordinator. |
| Open and resume | Fresh performs `thread/start`, names the thread, and materializes its rollout; resume performs the exact prepare/resume. It supports all five open fields: `cwd`, `permission_mode`, `model`, `reasoning_effort`, and `arguments`, stored once and applied to every run. | `CodexStartRequest` and `CodexLaneTurnRequest` at `codex_native.go:51-70` expose exactly these settings; c5's split between open and per-turn inputs is collapsed into stored open data. |
| Run | Send one `turn/start`, await the matching `turn/completed`, and extract the final agent message. | `internal/products/codex/lane.go:75-122` and `codex_native.go:528-656` prove the end-to-end primitive. |
| Tools | In lane mode the wrapper serves loopback HTTP MCP through App Server thread configuration. In peer mode Codex starts the stdio MCP entry, which owns the direct peer connection. | App Server already owns MCP configuration/reload (`codex_native.go:184-201`); each mode has one connection and no daemon-side product coordinator. |
| Deliver | Active thread uses `turn/steer` and reports `injected`; idle delivery enters the bounded wrapper FIFO instead of invoking c5's `turn/start`. | `codex_native.go:479-527` explicitly distinguishes active steer from idle start; universal delivery forbids the latter. |
| Interrupt and close | Interrupt calls `turn/interrupt` for the exact active turn. Close archives the thread and unsubscribes before the wrapper exits. | `internal/products/codex/lane.go:124-158` and `codex_native.go:758-780` are the existing native boundaries. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded delivery FIFO. | App Server exposes every typed open value; idle delivery is the only missing primitive and stays wrapper-local. |
| Size cap | **700 production / 700 test logical lines**, including App Server framing and session code but excluding the shared wrapper host. | Native protocol code must be counted with the product that requires it; host-global coordination is forbidden. |
| Deletion inventory | Delete all `internal/products/codex` (2 files / 331 lines) and `internal/launcher/{codex_peer.go,codex_peer_test.go}` (2 / 786). Total: **4 files / 1,117 lines**. | The App Server lane primitive is rehomed into wrapper-owned code; the daemon driver dies and the peer exec plan moves into thin `cmd` composition. |

### 4.4 Grok

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-grok` owns one private leader, one authenticated ACP primary, and one observer for the exact lane session. The peer launcher execs interactive Grok; Grok's product-spawned Agent Sessions stdio MCP server owns the peer connection. | `internal/bridge/grok_native_session.go:16-210` proves the lane process tree. Peer presence remains product-spawned and does not wrap the interactive TUI. |
| Open and resume | Start the private leader, open or resume the ACP session, then apply model and mode. It supports all five open fields; `model` and `reasoning_effort` are promoted from c5 argument/mode translation into typed open data. | `internal/products/grok/lane.go:98-170` and `grok_native_session.go:248-268` prove native model and mode setters plus cwd, permission, and argument handling. |
| Run | Call ACP `session/prompt`, consume matching update notifications, and return its stop reason and accumulated output. | `grok_native_session.go:270-308` is the resident prompt primitive. |
| Tools | In lane mode the wrapper publishes a private endpoint and `grok/scripts/native-entry` is a local stdio relay to it. In peer mode the same installed MCP entry owns the direct peer connection instead. | Grok's product interface is stdio MCP in both cases; an explicit entry mode selects exactly one destination and one presence owner. |
| Deliver | Observer interjection is used idle or running and reports `injected` only after the native request succeeds. | `internal/products/grok/lane.go:251-260` already sends delivery through the observer without starting a prompt. |
| Interrupt and close | Interrupt sends ACP cancel once. Close tears down observer, primary, leader, and its private socket directory in that order. | `lane.go:263-285` and `cmd/agent-sessions/grok_peer.go:151-183` prove the exact cleanup ownership. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only queue: **none**. | Grok exposes native interjection and all typed open controls; no limitation leaks outward. |
| Size cap | **750 production / 700 test logical lines**, including ACP framing and leader bootstrap but excluding the shared wrapper host. | Grok's private leader is product-specific and must not escape its ledger or recreate daemon attachment/generation state. |
| Deletion inventory | Delete all `internal/products/grok` (2 files / 637 lines), `internal/launcher/{grok_peer.go,grok_peer_test.go}` (2 / 1,183), and `cmd/agent-sessions/grok_peer.go` (1 / 213). Rewrite `grok/.mcp.json` and `grok/scripts/native-entry` as dual-entry peer/direct or lane/local assets. Total: **5 files / 2,033 lines**. | One lane wrapper replaces the driver/leader composition; the thin peer exec plan is rehomed without wrapping the TUI. |

### 4.5 Qwen Code

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-qwen` owns one `qwen --acp` child and one ACP client for the lane session. The peer launcher execs interactive Qwen; Qwen's product-spawned Agent Sessions stdio MCP server owns the peer connection. | `internal/products/qwen/lane.go:102-181` proves the headless ACP lifetime. The obsolete file-observer peer path dies without inserting a wrapper around the TUI. |
| Open and resume | Initialize ACP v1, call `session/new` or capability-checked `session/resume`, verify the exact ID, then rename fresh sessions. Supported open fields are `cwd`, `permission_mode`, `model`, and `arguments`; model maps to Qwen's `-m` process flag. | `lane.go:117-178` contains the native transaction, and Qwen product help exposes `-m, --model`. `reasoning_effort` remains unsupported because Qwen exposes no corresponding flag or ACP field. |
| Run | Start `session/prompt`, accumulate session updates, and resolve the matching future to a terminal result. | `lane.go:199-263` and `client.go` prove the one ACP request/future. |
| Tools | In lane mode the wrapper serves loopback HTTP MCP through ACP `mcpServers`. In peer mode Qwen starts the stdio MCP entry, which owns the direct peer connection. | c5 already injects an MCP server during `session/new` (`lane.go:134`); lane mode changes only its destination, while peer mode preserves the product-spawned connector pattern. |
| Deliver | Always use the bounded wrapper FIFO and report `queued_for_next_turn`; the next run prepends the deliveries. | c5 declares `Steer` unsupported and exposes only ACP prompt plus cancel (`lane.go:265-279`), so claiming live injection would be false. |
| Interrupt and close | Interrupt calls `craft/cancelPendingPrompt`; close cancels the ACP lifetime and reaps the child. | `lane.go:265-310` proves both calls. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open field: `reasoning_effort`. Wrapper-only state: the bounded delivery FIFO. | Product help exposes model but no effort selector; the one missing control is declared at hello and no Qwen condition enters the daemon. |
| Size cap | **520 production / 600 test logical lines**, including ACP framing but excluding the shared wrapper host. | The current driver/client split contains generic actor state that disappears; all retained Qwen protocol code remains charged here. |
| Deletion inventory | Delete all `internal/products/qwen` (4 files / 1,031 lines), `internal/launcher/{qwen_peer.go,qwen_peer_test.go,qwen_test_helpers_test.go}` (3 / 1,412), `cmd/agent-sessions/{qwen_peer.go,qwen_peer_test.go}` (2 / 234), and the obsolete 11-line `qwen/scripts/native-entry`; replace it with the installed dual-entry MCP image and rewrite `qwen/mcp.json`. Total: **10 files / 2,688 lines**. | ACP becomes lane-wrapper-owned; event-file identity and old peer launcher state die, while a thin peer exec plan and product-spawned MCP entry replace them. |

### 4.6 OpenCode and Kilo

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-opencode` or `asl-lane-kilo` owns one private authenticated `<product> serve` child and one HTTP/SSE client for exactly one lane session. In peer mode the product's JavaScript plugin owns the direct peer connection through the JS kit. | `internal/products/opencodefamily/server.go:74-148` already constructs the private lane server. An interactive product already supervises its plugin, so peer mode needs no Go wrapper. |
| Open and resume | Create or fetch the exact session, apply title and permission rules, and retain model/agent/variant defaults. Both products support all five open fields. | `opencodefamily/lane.go:69-157` and `:168-187` prove cwd, permission, arguments, parsed model, and reasoning variant. |
| Run | Call `session.promptAsync`, follow the exact event stream, then fetch the matching assistant result. | `lane.go:168-337` and `client.go` contain the existing bounded HTTP/SSE primitive. |
| Tools | The retained OpenCode/Kilo JavaScript plugin has two explicit modes: peer mode opens the one direct daemon connection through the JS kit; lane-local mode connects only to the wrapper's private endpoint and registers the same caller tool. | The current plugins already own product SDK registration in `integrations/{opencode,kilo}/agent-sessions.mjs`; an environment-selected destination changes, not the tool surface. |
| Deliver | Always use the bounded wrapper FIFO and report `queued_for_next_turn`. | The native driver explicitly returns unsupported steer (`lane.go:259-261`), while the plugins' `promptAsync` delivery starts a native prompt (`integrations/opencode/agent-sessions.mjs:139-151`); neither is valid injection into the current or idle session. |
| Interrupt and close | Interrupt calls the product abort endpoint and cancels event wait; close stops the private server process. | `lane.go:338-379` proves both product operations. |
| Exception ledger | Section 1 code exceptions: **0** for both products. Declared unsupported open fields: **none**. Wrapper-only state: the bounded delivery FIFO. | Dialect differences remain local URL/status decoding; they never select a wire method. |
| Size cap | Shared family wrapper **750 production / 700 test**, plus **60 / 80** per dialect leaf; includes HTTP/SSE and excludes only shared wrapper-host code. | OpenCode and Kilo differ only in dialect, permission mapping, and packaged plugin entrypoint; their native transport stays charged to the family. |
| Deletion inventory | Delete all `internal/products/opencodefamily` (10 files / 2,776 lines), `internal/products/opencode` (2 / 93), and `internal/products/kilocode` (2 / 98): **14 files / 2,967 lines**. Rewrite, but do not duplicate or delete, the two retained integration packages and tests. | Server pooling, daemon driver maps, doctor probes, and leaf driver composition are replaced by two thin wrapper registrations over one family implementation. |

### 4.7 Pi

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-pi` owns one `pi --mode rpc --extension <managed plugin>` JSONL child for exactly one lane session. In peer mode Pi's JavaScript extension owns the direct peer connection through the JS kit. | `internal/products/pifamily/lane.go:119-164` is the proven lane RPC launch; interactive Pi already owns the extension lifecycle. |
| Open and resume | Apply the Pi argument/permission mapping, create with the daemon ID, or pass `--session <native_id>` and verify the returned state. Pi supports all five open fields: model maps to `--model` and reasoning effort to the independent `--thinking` flag before child spawn. | `pifamily/lane.go:76-173` and `quirks.go:113-134` prove the transaction; Pi product help exposes `--model <pattern>` and `--thinking <level>`. |
| Run | Send RPC `prompt`, observe terminal JSONL events, and read the final assistant text. | `pifamily/lane.go:179-300` and `rpc.go` are the existing primitive. |
| Tools | The retained Pi extension runs in peer mode with a direct JS-kit daemon connection, or in lane-local mode against the wrapper's private endpoint; both register the same caller tool. | `integrations/pi/pifamily.mjs:106-151` already owns product tool registration; only the selected connection mode changes. |
| Deliver | Running uses RPC `steer` and reports `injected`; idle uses the bounded wrapper FIFO. | `pifamily/lane.go:301-329` proves active steer. The current extension's idle `sendUserMessage` (`pifamily.mjs:157-174`) starts product work and therefore cannot implement idle injection. |
| Interrupt and close | Interrupt sends RPC `abort`; close reaps the exact RPC process while leaving its transcript durable. | `pifamily/lane.go:331-379` proves both operations. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded idle-delivery FIFO. | Model and thinking are process flags applied after open; only idle append is missing and it stays wrapper-local. |
| Size cap | Shared Pi-family wrapper **650 production / 700 test**, plus Pi leaf **60 / 80**; includes JSONL framing and excludes only shared wrapper-host code. | Product quirks are fixed launch/terminal data, not lifecycle branches; native framing is not an uncounted utility. |
| Deletion inventory | Delete all `internal/products/pifamily` (8 files / 2,242 lines) and `internal/products/pi` (3 / 110): **11 files / 2,352 lines**. Rewrite `integrations/pi/pifamily.mjs` and its test as a local wrapper plugin; retain the package entrypoint/manifest. | The common driver and doctor disappear; the family wrapper owns the same native RPC with no daemon-facing interface. |

### 4.8 OMP

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `asl-lane-omp` uses the same resident Pi-family JSONL wrapper with OMP's fixed equals-style extension/mode/session arguments and terminal quirks. In peer mode OMP's JavaScript plugin owns the direct peer connection through the JS kit. | `internal/products/pifamily/quirks.go` and `rpc_lane_test.go:388-437` prove the lane dialect; interactive OMP already supervises its plugin. |
| Open and resume | Create or resume the exact OMP session and apply mapped permissions. OMP supports all five open fields: cwd maps to `--cwd=`, model to `--model=`, and reasoning effort to `--thinking` before child spawn. | OMP product help exposes `--cwd=<value>`, `--model=<value>`, and `--thinking <level>`; the family wrapper owns their equals-style projection. |
| Run | Send RPC `prompt`, accept OMP's declared terminal event, and read final assistant text through the family implementation. | `pifamily/rpc.go` contains the closed event decoder; OMP selects the terminal quirk rather than a second lifecycle. |
| Tools | The retained OMP entrypoint loads the Pi-family plugin in peer/direct or lane/local mode and registers the same caller tool. | `integrations/omp/agent-sessions.mjs` is already a three-line family entrypoint; the shared plugin owns mode selection. |
| Deliver | Running uses RPC `steer` and reports `injected`; idle uses the bounded wrapper FIFO. | The same native distinction as Pi applies; OMP's native steer does its own framing, documented at `pifamily/lane.go:301-326`. |
| Interrupt and close | RPC `abort` and exact process cleanup are identical to Pi. | No OMP-specific lifecycle callback is justified. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded idle-delivery FIFO. | OMP exposes every open value as a process flag; its dialect remains launch/result data only and never reaches the daemon or wire. |
| Size cap | OMP leaf **60 production / 100 test** in addition to the shared Pi-family cap. | The leaf may declare quirks and permissions only. |
| Deletion inventory | Delete all `internal/products/omp` (3 files / 135 lines); shared `pifamily` deletion is counted under Pi. Rewrite the retained OMP entrypoint/test/manifest around the local wrapper plugin. Total: **3 files / 135 lines**. | OMP becomes one immutable family-wrapper registration, not a driver package. |

### 4.9 Wrapper migration ledger

| Ledger | Files | Deleted lines | Decision |
| --- | ---: | ---: | --- |
| Product-specific rows in Sections 4.2-4.8 | 52 | 12,583 | Delete every non-native daemon driver and product-specific depart-on-exec launcher. |
| Shared wrapper boundary in Section 4.1 | 18 | 2,532 | Delete the remaining launcher package and connector self-refresh; retain only rewritten CLI/wrapper entrypoints. |
| Section 4 subtotal | **70** | **15,115** | Replacement wrapper and test lines are reported separately against the stated caps. |
| Cumulative Sections 2-4 floor | **133** | **28,882** | This is the minimum physical deletion from c5b280d before Section 5 migration/conformance cleanup. |
