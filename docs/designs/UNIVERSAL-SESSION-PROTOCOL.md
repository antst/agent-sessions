# Agentbus Universal Session Protocol

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
one in its independent ID space; gaps are allowed, and only range and
per-direction uniqueness matter. When JSON repeats a member, the last value
wins on both Go and JavaScript implementations. Notifications, batches,
unknown fields and explicit nulls are invalid. The first request is
`session.hello`. A worker sends it exactly once. A live peer may re-hello under
the identity-transition rule below. The daemon admits no other request from a spawned
worker until `session.open` has returned its session ID and that ID is durably
committed. Request admission captures the source identity and groups from the
exact current connection once; later identity changes cannot rewrite authority
for a request already admitted.

A request with a usable ID whose params or method are invalid receives its
correlated `invalid_frame` response, or `invalid_hello` for `session.hello`,
before the reader closes the connection. Without a usable ID the reader closes
without writing. The reader never dispatches a later frame after either case.
A response whose ID does not match an outstanding call is also an invalid frame:
the receiver closes the connection and fails its pending calls once.

The local Unix socket is trusted and plain by default. When the daemon's
optional `local_key` is configured, every peer and worker connection instead
uses TLS 1.3 on that same socket. The R1 key derivation and public-key pinning
apply with fixed labels `client` for the connecting side and `daemon` for the
listener; one shared key replaces federation's SNI lookup. A keyed daemon never
accepts plain frames. A kit without the required key reports `daemon requires
local key`; a keyed client connecting to a plain daemon reports the TLS
handshake failure. The key is never a wire field, argument, or log value.
Clients read the daemon Unix-socket path from `AGENTBUS_SOCKET`, falling
back to the documented default path when absent. Every spawned lane receives
that variable alongside `AGENTBUS_LAUNCH_TOKEN` and, when configured,
`AGENTBUS_LOCAL_KEY`.

A product is a binary; started with a launch token in its environment it is a
lane worker, with no mode argument. Lane-only workers such as non-AI tools are
never started without a token, so a worker flag would be dead syntax for them.
A flag plus a token would also create two sources of truth and require errors
for both disagreement cases; the token alone leaves one fact and zero
consistency checks. Per-spawn tokens are single-use and expiring, so an
ordinary human shell never contains one.

Every session has one canonical ID `id@host` and one canonical name
`name@host`; every output uses those forms on the session's own daemon and on
every federated host. The product owns the ID part because it owns the session
primitive that can address it; the daemon mints no session IDs. A caller may
use a bare ID or bare name as shorthand for its own daemon's host.
Resolution splits a qualified name on its last `@`. Name parts are 1–128
printable characters with no whitespace or control character; `/` and `@` are
allowed. The last `@` is always the canonical host boundary. Host parts match
`^[a-z0-9][a-z0-9-]{0,31}$`. For identity input, no `@` means a bare local part:
the daemon appends the caller's host, then tries exact ID before exact name. An
input containing `@` is always split at the last one, and an unknown right part
is `unknown_host` rather than a bare name. These grammars
are daemon checks because the shared schema deliberately has no `pattern`
keyword.
The wire has no generic tool frame: after hello, a peer or committed worker
originates the ordinary client-to-daemon methods in this section. Product-facing
start/wait/status/interrupt/list/send tools are caller-kit sugar over them.

Turn input and result strings, `message.send.message`, and
`message.deliver.body` are each limited to 262,144 decoded characters through
the closed generated types. The raw 1 MiB framing guard remains an earlier,
independent byte limit because UTF-8 width and JSON escaping are not character
counts. A worker kit truncates a longer native turn result to 262,144 characters
and sets `truncated:true`. Both
kits emit compact JSON with no insignificant whitespace.

The closed parameter and result shapes are authoritative in
`bus/internal/protocol/session.schema.json`, re-exported as bytes by each public
SDK. An implementation validates each frame against that schema with a small
interpreter and no validation library, then decodes it into the closed types.
Unknown fields, missing or null required fields, and out-of-range values are
rejected before product code runs. The shared fixture file tests the same
definitions in Go and JavaScript. The schema's
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
supplies its product-native bare `session_id` and an unqualified `name` part, plus
`groups` and `info`; the name part may itself contain `@` or `/`. The daemon
qualifies both parts with its effective host before installing the peer. A worker instead supplies a
one-use `launch_token`, its supported non-identity open fields, its ordered
extra-argument descriptions, and optionally the product version. The branches are mutually
exclusive: a request with both discriminants or neither is invalid. A worker
sends hello only after its product and plugin are app-ready, so hello success is
the sole readiness fact. A peer ID matching a durable lane row is invalid. A
worker's product must equal the product recorded by its launch-token
reservation. The result is `{}`.

A live peer may send another hello. With the same `session_id`, it updates the
product-owned name and `info` in place; `groups` must equal the original
declared slice exactly, including order, or the daemon returns `invalid_hello`
and closes the connection. With a
different `session_id`, identity replacement is atomic: the old transient
entry and private group are removed, its reply sinks are detached, and pending
inbound deliveries fail once before the new hello is acknowledged; the new
identity is then installed with its own private group and the new hello's
groups. Requests already admitted retain their captured old source. This
same-connection transition sends no `session.superseded`. A worker never sends
a second hello.

#### `session.superseded`

The daemon sends the displaced connection the ID-bearing request
`session.superseded` with `{}` when a new peer connection claims the same
canonical identity. The displaced client marks that identity terminal before its
best-effort `{}` response, closes, and never reconnects that identity. The new
connection is already current, so every request from the displaced connection,
including another hello, is rejected. After the directory replaces the exact
entry and releases its mutex, the old connection's owner writes the supersession request once with
`supersedeWriteBound = 1s`, closes the old local socket, and never waits for an
acknowledgement. A writable socket returns immediately; the bound only stops a
dead client from blocking this path. If a displaced client races one final reconnect, that new claim
may cause one more swap; because each displaced instance becomes terminal, the
race is bounded and cannot flap indefinitely.

#### `session.list`

A connected session sends `session.list` with an optional `session_id` filter.
The result contains the matching visible sessions, or all visible sessions when
the filter is absent. Each item reports canonical `id@host` and `name@host`, whether its one
connection is open, and whether one `turn.run` is outstanding. The host is already carried by both canonical identities, so no
separate summary host field exists. This single method
replaces peer listing, lane listing, and lane status. Its optional `hosts` array advertises product names by host. Whenever
the local optional non-empty product list is configured, the daemon's effective
local host identity is present; an empty list is treated as absent. Federated
hosts contribute their published lists.
Advertisement never gates launch: the service PATH remains authoritative.
Lane-row identity is immutable. Peers have no durable rows and follow the
re-hello and connection-supersession rules above. A session outside the caller's visibility is indistinguishable
from a missing session and yields `unknown_session`. Federation forwards
canonical identities unchanged; receiving daemons never relabel them. Host
qualification is a daemon invariant rather than a JSON-schema constraint.

#### `message.send`

A connected session sends `message.send` to exactly one `target`, one explicit
`targets` list, or one `group`. The daemon resolves recipients from current
connections, sends each one `message.deliver`, and returns one truthful receipt
per attempted delivery. The request retains one message body; explicit
multicast and group expansion do not create another protocol method.
Resolution validates the target name-part grammar, then tries exact canonical
session ID and canonical visible name. Bare input is first qualified with the
caller's own host. Each label yields its own receipt: an unknown session,
unknown host, or ambiguous name is rejected with that reason, while every
resolvable target is delivered. Resolved recipients are deduplicated by session
ID. Deliveries run concurrently, and one response returns their receipts in
label order after deduplication. There is no multicast timeout.

#### `message.deliver`

The daemon sends `message.deliver` to the target session with the message ID,
authoritative canonical `id@host` and `name@host` source identity, and body. Every product implements delivery while
idle and while a turn is running. Its result is exactly one closed receipt:
`injected`, `queued_for_next_turn`, or `rejected` with a nonempty reason. A
product may report `queued_for_next_turn` when its native surface cannot append
without starting a turn; any such queue is product- or wrapper-owned and
invisible to the daemon and to this wire. An interactive peer may instead start
a native turn on delivery and truthfully report `injected`; the bounded FIFO
rule in Section 4 applies to lane wrappers only.

#### `lane.describe`

A session sends `lane.describe` with a product token and optional `host`.
Absent `host`, or `host` equal to the local daemon, runs locally; another
connected host forwards the identical request one hop and performs the complete
probe there. The authoritative daemon starts `<product>` from its service PATH
with empty argv and a rowless one-use launch token in its environment, consumes its worker hello, returns the declared open
fields, extra arguments, and optional product version, then closes it without
sending `session.open`. The hello is emitted only after app-ready. Exit before
hello fails with the exit code and bounded trailing stderr; there is no
readiness object or readiness phase.

#### `lane.spawn`

A session sends `lane.spawn` either with a caller-chosen name leaf, product, open
options, optional `extra_groups`, and optional `host` for a new lane, or with
`resume_session_id` alone for a durable offline lane. A peer's private group is
`session:<id@host>`. A lane's private group is `<parent private group>/<leaf>`;
its default groups are exactly its parent's private group and that new private
group, plus `extra_groups`. The parent's other memberships are not inherited,
and the daemon permits any explicit extra group without checking the parent's
membership. The daemon composes the lane's canonical name as `<parent name
part>/<leaf>@<target host>`. Nested spawns extend both paths: peer `pA@pdev`
with ID `u@pdev` has private group `session:u@pdev`; after it spawns `pD` and
that lane spawns `pE`, the grandchild is named `pA/pD/pE@host` and has private
group `session:u@pdev/pD/pE`. Each level sees exactly one level down by
default; granting the grandparent's private group or a shared group through
`extra_groups` widens visibility explicitly. The leaf itself may contain `/` or
`@`, and the last `@` remains the host boundary. A composed name
part beyond 128 characters is invalid request data. Absent `host`, or `host` equal to the local daemon, spawns locally;
another connected host forwards the identical request one hop and performs the
entire transaction there. The row exists only on that authoritative target.
Resume never carries `host`: its session ID already selects the host. The
authoritative daemon resolves and starts the worker, waits for hello, sends
`session.open`, durably
commits the product-returned session ID, and only then returns `{session_id}`. The row
stores the original closed open-options object as one JSON value. Resume replays
that stored value unchanged, preserving `arguments` order, with
`resume_session_id`; this version permits no resume overrides. The one
spawn/open transaction timeout covers all of those steps.
Unsupported supplied
open fields, an invalid session ID, exit, or timeout fail truthfully and do not
publish a live session.

One lane owner processes the open result, exit, timeout, and shutdown in event
order, so exactly one outcome finishes the request. A fresh spawn has no ID, so
its composed name is reserved until the product returns an ID. The launch
token, not a speculative ID, keys the provisional worker until open returns.
Resume requires the returned ID
to equal `resume_session_id`. A fresh open returning an ID already held by an
existing row fails with `spawn_failed` and the exact text `session id already
exists`. The lane owner commits while handling the open response, before it
handles the next inbox frame. An exit drains the closed socket first, so an
already-written valid open response can commit; EOF first fails the spawn.

#### `session.open`

The daemon sends `session.open` only to a token-authenticated worker. It carries
always-present canonical composed `name` and `groups`, optional
`resume_session_id`, and one closed `open` object containing only the
non-identity options accepted by `lane.spawn`. Values of `permission_mode`,
`model`, and `reasoning_effort` are product-native strings passed through
verbatim: the daemon checks only shape and declared field support, while the
worker rejects an unsupported value as `spawn_failed` with
`stderr_tail:["unsupported value <field>=<value>"]`. `arguments` is an ordered
array passed verbatim to the product integration. Hello's `extra_arguments`
entries document those strings for callers; the daemon never interprets or
enforces them. The worker applies `name` as the product's session title wherever
the product exposes a title primitive, creates or resumes the native session,
and returns `{session_id}`. A successful result is
the commit point that turns the provisional worker connection into lane
presence. A probe never receives this method.

#### `turn.run`

Either a caller sends `turn.run` to the daemon or the daemon forwards the same
request to the addressed lane. Its parameters are exactly `{session_id,input}`.
The worker permits one outstanding call, invokes the native run primitive, and
answers only with the terminal outcome. The outstanding RPC is the running
fact; no start acknowledgement, wait method, status update, projection stream,
or collector exists. The daemon stores no turn result.

Bounded agent-tool behavior is caller-kit policy, not wire. The Go and JavaScript
caller kits keep `turn.run` outstanding and expose local `start`, `wait(timeout)`,
and `status` operations with a local-only turn ID. A wait timeout never cancels
the wire call. If the caller process disappears, the daemon drains and discards
the result. The caller kit reports `result unavailable, lane resumable`; the
caller resumes and asks the lane again or reruns a non-AI tool.
The worker kit is full-duplex: its reader never blocks on the native run, so
delivery, interrupt, and worker-originated session methods dispatch concurrently
with it. A terminal response may include `truncated:true` only when its result
was shortened to the wire limit.

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
the addressed lane. The request has optional `forget`, default false. The
worker asks the product to close and returns `{}` when
it does. One constant `closeBound = 10s`, measured from the daemon sending this
request, bounds the entire close path. A result before the bound makes the
daemon close the socket, send TERM, and reap; expiry makes it close the socket,
send KILL, and reap with no second waiting period. The spawn/open transaction
bound and `closeBound` are the only two lane-path timeouts.

If close arrives during a run, the worker kit interrupts once, awaits that same
run's terminal result, and then closes natively; the whole sequence must fit
inside `closeBound`. If the
bound forces a kill, worker EOF fails the outstanding caller exactly once; the daemon never
fabricates an interrupted result.

After orderly close or unrequested EOF, the durable row is offline and
resumable. `forget:true` deletes it only after the worker is stopped. There is no
closed row state and no daemon auto-archive policy. A future
protocol may add an idle-close option implemented wholly by a product kit, but
this version has none.

### 1.2 Edge rules

- A hello with both `session_id` and `launch_token`, with neither, or with fields
  from the other branch is invalid and closes the connection.
- A peer re-hello with the same ID updates name and info only; changed groups
  or group order are `invalid_hello`. A different-ID re-hello ends the old transient identity
  once, detaches its reply sinks, fails pending deliveries once, and installs
  the new peer, private group, and groups before acknowledging on the same
  connection, without a supersession frame. Already-admitted requests retain
  their captured source identity and groups. A worker re-hello is invalid.
- A peer hello whose ID belongs to a lane row is invalid. A worker hello whose
  product differs from its token reservation is invalid; the token lookup is
  the authority for both facts.
- A rowless describe token can authenticate hello but can never authorize
  `session.open`; EOF after describe is the worker's normal exit.
- Worker-originated session methods before the session ID commit are rejected.
  The lane owner commits while handling the successful open response, before
  handling the next inbox frame; commit failure closes the provisional
  connection. The kit adds no additional commit buffer, gate, or
  acknowledgement. After commit, the same connection is the lane's presence
  and uses those ordinary methods; there is no tool frame.
- A second `turn.run` while one is outstanding returns busy. There is one
  running boolean in a product kit and one pending RPC in the daemon.
- `lane.spawn` with `resume_session_id` naming a connected row returns
  `already_connected`; lanes never use supersession to manufacture a second
  worker connection.
- New `lane.spawn` requires a name not held by any row on that host; collision
  returns `name_taken`. Every retained row reserves that name until an explicit
  `session.close{forget:true}` deletes the row.
- A peer private group is `session:<id@host>`; a lane private group recursively
  appends `/<leaf>` to its parent's. A new lane receives exactly its parent's
  private group, its own private group, and explicit `extra_groups`; no other
  parent group is inherited. With no extra group, only its parent sees it by
  default. The trusted daemon accepts any explicit extra group.
- Composed lane names and recursive private groups record creation ancestry.
  A later peer or parent rename does not cascade into existing child names or
  group paths, and attached lane titles stay fixed.
- A lane row is claimed for resume, close, or forget until that operation's
  cleanup finishes. New run, interrupt, close, resume, or forget requests for a
  claimed row return `busy`; nothing waits on a row. Delivery to a claimed but
  still attached lane proceeds. Fresh spawn reserves its composed name until
  its product ID is known.
- Resume acquires product-side exclusive ownership before touching the existing
  native session and holds it through cleanup. Fresh open does the same before
  create when the wrapper chooses the ID; when only the product can allocate the
  ID, there is no pre-existing identified resource to fence, so it acquires the
  ID-keyed lock immediately after allocation and before every later mutation.
  Wrappers pass the wrapper host's flock file description into the native
  child, so abrupt wrapper death cannot free the lock while a surviving child
  writes; contention is `spawn_failed` with `session busy`. A native product's
  own mechanism qualifies only when it excludes competing processes.
- `turn.run`, `turn.interrupt`, or `session.close` addressed to a durable row
  without a connection returns `not_connected`. Resume is an explicit
  `lane.spawn`; a caller kit may compose that automatically without changing the
  wire.
- `turn.interrupt` while no run is outstanding returns not-running. An accepted
  interrupt does not promise that the native product has already stopped.
- Caller timeout or disappearance does not cancel the forwarded run. The daemon
  drains and discards its response. Result loss at an abandoned reply sink is
  accepted; the kit reports the lane as resumable and the caller re-asks or reruns.
- Worker EOF cancels every callback, never invokes interrupt afterward, calls
  native close once if open committed, fails pending calls exactly once, and
  leaves the durable row offline.
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
- Optional `host` on `lane.describe` or new `lane.spawn`, and canonical
  `id@host` identities on later operations, select the same one-hop daemon
  forwarder. An unconnected explicit or canonical host returns `unknown_host`; federation
  never creates a second request shape or retry path.

This protocol deliberately does not compensate for five losses. A daemon crash
before commit can orphan native files; workers exit on EOF and those files are
garbage, not state. A successful spawn reply can be lost with its caller; the
caller kit lists by name before spawning again. A caller can lose a completed
turn result with its reply sink; it re-asks the resumed AI lane or reruns the
tool. A wrapper can die after truthfully accepting a queued
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
| `lane.status` | `session.list` | A filtered list returns connection and running facts. |
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
| `-32600` | `invalid_frame` | Any method whose envelope, closed params, or daemon-checked identity grammar is invalid, but only when a valid request ID is recoverable. This includes a composed lane name beyond 128 characters. |
| `-32602` | `invalid_hello` | `session.hello` when its union, protocol, identity, or token is invalid. |
| `-32001` | `unknown_session` | `message.send`, resume `lane.spawn`, `turn.run`, `turn.interrupt`, or `session.close` when the named row or peer does not exist or is invisible to the caller. |
| `-32002` | `not_connected` | `turn.run`, `turn.interrupt`, or `session.close` when a durable row has no connection. |
| `-32003` | `busy` | `turn.run` when the target already has an outstanding run; a worker loop dequeuing a 257th unanswered call; a full 256-event connection inbox; or a new run, interrupt, close, resume, or forget for a claimed lane row. A delivery rejected by either bound has reason `busy`; delivery to a claimed but attached lane is still admitted. |
| `-32004` | `not_running` | `turn.interrupt` when the target has no outstanding run. |
| `-32005` | `already_connected` | Resume `lane.spawn` when the durable row already has its worker connection. |
| `-32007` | `unknown_product` | `lane.describe` or new `lane.spawn` when the product token is invalid or its binary is absent from the target host's service PATH. |
| `-32008` | `unsupported_open_field` | New or resumed `lane.spawn` when the supplied or stored `open` object contains a field absent from the new worker's hello declaration. |
| `-32009` | `spawn_failed` | `lane.describe` or `lane.spawn` when exec, hello, open, or native creation fails before commit. |
| `-32010` | `timeout` | `lane.describe` or `lane.spawn` when its one spawn/open transaction bound expires. |
| `-32011` | `not_committed` | Any worker-originated client-to-daemon session method received after hello but before product-session-ID commit. |
| `-32012` | `superseded` | Any request from a peer connection displaced by exact directory replacement. |
| `-32013` | `name_taken` | New `lane.spawn` when another row on that host already holds the requested composed name. |
| `-32014` | `unknown_host` | `lane.describe` or new `lane.spawn` naming an unfederated `host`, or any canonical identity input whose host part is neither local nor connected. |
| `-32015` | `forward_lost` | A one-hop federated request whose transport ends before its response; the request may or may not have been applied on the target host and is never retried. |
| `-32603` | `internal` | A durable row-file operation fails after directory cleanup, or a worker interrupt/close callback fails; it has no other use. |

## 2. Daemon

### 2.1 Stored and live data

The daemon is a router around one directory and a set of durable row files. It
does not know how any product creates a session, runs a turn, injects a message,
or closes. Native
products and wrappers expose those operations through the eleven methods in
Section 1. The daemon contains no product switch, lane actor, product driver,
capability interface, status projection, result collector, archive
transaction, or idle timer.

Only lanes have durable rows. A row has exactly these columns:

| Column | Meaning |
| --- | --- |
| `session_id` | Immutable canonical `id@host` and primary key; the product returns the ID part from `session.open`. |
| `product` | Immutable binary name executed with empty argv and a launch token in its environment. |
| `name` | Canonical `name@host`; for a lane the stored name part is `<parent name part>/<caller leaf>` and the host is where the lane runs. It is the resume recipe's mirror of the product title, set at open. |
| `groups` | Full resume-membership recipe containing the parent's private group, the recursively composed `<parent private group>/<leaf>`, and explicit `extra_groups`; no other parent membership is inherited. |
| `open` | The original validated `SessionOpenOptions` value, re-marshalled unchanged on resume with `arguments` order preserved. |
| `created_at` | Daemon timestamp assigned when the row commits. |

Each row is one JSON file named `<sha256(session_id)>.json`. A commit writes and
syncs a temporary file, renames it to that name, and syncs the containing
directory. Startup reads only committed `.json` names; an interrupted temporary
file is ignored. An unknown column, wrong digest name, duplicate session ID, or
duplicate canonical name makes the durable directory invalid.

There are no durable peer rows. The daemon qualifies a peer hello's product-
asserted ID and name parts with its effective host, then creates one transient
directory entry containing that canonical identity, groups, information, and
exact connection. A same-ID re-hello refreshes name and information only; a
different-ID re-hello ends the old identity on that connection and installs a
fresh entry. EOF removes only the exact installed entry. Lane EOF removes only
the live attachment; its row remains offline and resumable.
`SessionSummary.kind` is derived: a durable row is a lane and a transient entry
is a peer.

The in-memory directory indexes canonical IDs, canonical names, and launch
tokens. Each entry contains its row data, current attachment, claimed and
running facts, and an identity-lifetime `done` channel. One short directory
mutex protects those registry facts together. It is held only for lookup,
insert, replacement, removal, claim, and a copied list snapshot. No socket,
process, disk, channel wait, or callback runs while it is held, and it never
nests with another daemon mutex. An entry object is also the destination token:
a receiver compares that exact object before admitting a request or settling a
response, so a late event cannot attach to a replacement identity.

For a connected lane, live name, groups, attachment, and running status come
from its directory entry, exactly as for a peer. The durable `name`, `groups`,
and `open` columns are the product-title mirror and resume recipe used while
offline.

Every accepted connection has one reader goroutine, one writer goroutine, and
one owner loop. The reader posts frames in order into that connection's
256-element mixed-event inbox. The writer alone writes its bounded outbox. The
owner loop owns request admission, the pending-call map, child and launch
references, timers, and reply handling; it never shares those fields with
another loop. Foreign directory routing posts without blocking, returning
`busy` when the inbox is full. Owned helpers may post back to their own loop
while an owned count keeps that loop alive until every such helper has returned.

Each routed operation carries a capacity-one reply slot created by its sender.
The target loop is the sole code that removes the operation from its pending map
and answers that slot. One sender-owned helper waits for either that answer or
the sender identity's `done` channel and posts exactly one completion to the
sender loop. This leaves one place that settles a target operation and one place
that writes the caller response; it needs no reply lock, result collector, or
durable output.

Daemon start performs no recovery pass. Every durable row starts offline;
connections, pending calls, owner loops, and reservations start empty. Nothing
is replayed or reaped from a prior incarnation because its workers exited on
EOF. Restart is therefore a row-file load, not a lifecycle transition.

Daemon configuration consists of an optional federation hub address, the
daemon's federation secret when a hub is used, an optional host name, an
optional list of product names matching the product grammar, and an optional
`local_key`. With no hub the daemon is standalone and needs no federation
secret; every optional value may be absent. The effective host is the configured
host name, or the reserved `local` when none is configured. Configuring a hub
requires an explicit non-`local` host name. A hub rejects `local` and duplicate
host names during the authenticated handshake, so `@local` is never federated.
The hub's symmetric configuration is one map from host name to opaque secret,
with a unique secret for every host. The product list is an advertisement for
discovery, not a registry or allowlist: the service PATH alone decides whether
a lane can launch. The daemon validates each configured product name at load,
refuses duplicate names with one error, and treats an empty list as absent.
`agentbus secret` generates and prints 32 random bytes
encoded as base64; the daemon and hub reject any configured federation secret
or `local_key` shorter than 32 decoded bytes. Config files containing secrets
must be mode 0600.

No live fact, connection pointer, child process, deadline, caller ID, or
access-check result is stored in a row.

### 2.2 Opening and replacing connections

The daemon accepts a socket, completes the required local TLS handshake when
`local_key` is configured, enforces the framing limits in Section 1, and
requires `session.hello` first. Local TLS reuses the Section 2.5 derivation and
pinning code with `client` and `daemon` labels, one expected key, and no SNI
lookup. It rejects a peer hello unless `session_id` and `name` are valid
unqualified name parts, then appends its effective host to both.

For a first peer hello, the directory mutex covers one short replacement: check
the canonical ID against rows and peers, detach any old entry, and install the
new entry. The old entry becomes inadmissible before the mutex is released. Its
owner then writes `session.superseded` as the final frame with
`supersedeWriteBound = 1s` and closes the socket without waiting for an
acknowledgement. No lock is held for that write. A writable socket returns
immediately; the bound only limits a dead peer. The old owner settles its
pending calls once. No reconnect lease or grace timer exists, and a hello from
the displaced entry is rejected like every other request from it.

A second peer hello on that same live connection is handled in frame order. With
the same `session_id`, its declared groups slice must equal the original slice
exactly, including order; the daemon then refreshes name and information in
place. Different groups are `invalid_hello` and close the connection. With a different
`session_id`, the directory replaces the entry before acknowledgement. Each
admitted request already holds a copy of the old source identity and groups;
the old owner detaches its reply sinks and settles pending inbound deliveries
once. The new entry has its own private group and the new hello's groups. It
sends no `session.superseded` on this same socket. This adds one same-ID update branch
and one different-ID replacement branch. A worker connection never accepts a
second hello.

A worker hello resolves a one-use reservation created by `lane.describe` or
`lane.spawn`. The reservation binds token, product, transaction, and expiry.
Validation and token consumption are one directory operation. Wrong-product,
unknown, expired, or repeated hello is `invalid_hello`; cancellation removes an
unclaimed reservation. The connection stays provisional until open commits.
Before commit it may answer daemon calls but every worker-originated session
method returns `not_committed`. The lane owner processes the open response and
commits before it processes the next frame already waiting in its inbox. Commit
failure closes the provisional connection. This commit-before-dispatch ordering
needs no additional commit buffer, gate, or acknowledgement.

Local peers state their identity and groups, and a federated daemon states its
own summaries. The daemon adds no PID, peer-credential, descendant, signature,
or capability check.

### 2.3 Describe and spawn

The product token is exactly a binary name on the daemon's service PATH.
Native and wrapped products have one process contract: exec `<product>` with
empty argv and `AGENTBUS_LAUNCH_TOKEN` in the environment. Presence of
that variable selects worker mode; absence selects the product's ordinary
entry. Tokens are per-spawn, single-use, and expiring, so an ordinary human
shell never holds one accidentally. When configured, the daemon also supplies `AGENTBUS_LOCAL_KEY`;
the worker kit consumes and scrubs both variables before product code runs.
Callers name the product binary but never supply an executable path.
The daemon has no executable registry, fixed-argument table, or product
allowlist, and no product-specific configuration beyond the advertised names.
Its optional product list only advertises likely availability; an unlisted
executable works, while a listed name missing from PATH is `unknown_product`.
The hub address, effective host name, host secret, and local key configure
transport only and never participate in product resolution.

Wrapped products are our binaries named `claude-peer`, `codex-peer`,
`grok-peer`, `qwen-peer`, `opencode-peer`, `kilo-peer`, `pi-peer`, and
`omp-peer`. Invocation without a launch token is the interactive peer launch;
invocation with one is the resident lane wrapper. Tokens are honest binary
identities: `claude-peer` and `claude` are different products. When Claude
becomes native, `claude` begins honoring the same launch-token environment and
`claude-peer` is deleted; the daemon and protocol do not change. The daemon accepts a product token only when it matches
`^[a-z0-9][a-z0-9-]{0,31}$`; an invalid token is `unknown_product`, so no path
separator reaches process resolution. Failure to find an executable on PATH is
`unknown_product`; an executable that starts and then fails is `spawn_failed`.
The one-use token is present only in the child's environment.

`lane.describe` creates a rowless reservation, starts the process, and waits
for either a valid hello, process exit, shutdown, or the single spawn timeout.
Valid hello is the complete result. The daemon returns its declarations and
stops the supervised process without sending `session.open`. Describe creates no
row, native session, or product-specific readiness state.

New `lane.spawn` validates the full open object and composed name before exec.
It creates one directory reservation containing the composed name, product,
launch token, timer, and reply slot. The composed name is
`<parent name part>/<caller leaf>@<target host>`; the groups are the parent
private group, `<parent private group>/<leaf>`, and explicit `extra_groups`.
The prospective name stays reserved through final process cleanup. Siblings are
thereby unique, and two equal parent name parts with the same leaf collide; the
second spawn receives `name_taken`.

Resume looks up the durable row, checks visibility, and claims that exact entry
before exec. A resume claim has no live attachment until commit. Another run,
interrupt, close, resume, or forget returns `busy` immediately; nothing waits.
The owner reuses the row's product, composed name, groups, and stored open value.
New and resumed launches both start
`<product>` with empty argv and the token in the environment, consume hello,
check declared open-field support, and send one `session.open`. Resume includes
the row's ID part as `resume_session_id`; new open omits it.

The launch helper owns exec and the direct child. Before token claim it selects
among child exit, timeout, shutdown, and claim. After claim it posts process
events to the connection owner and waits for the child. The connection owner
serializes the open response, process exit, timeout, connection close, and
shutdown. A process-exit event records the exit while the reader drains frames
already written before EOF; one later event finishes the request. It records
TERM or KILL intent even when the process-start event is late, then applies that
exact intent when the child arrives. Final cleanup stops the process group and
joins the direct child, performs the final group KILL, then releases the
reservation. A child that exits before hello takes this same cleanup path, so
its descendants do not survive.

For a fresh open response, the owner validates the product-returned session ID,
reserves that canonical ID in the directory, writes the row file, then publishes
the ID, name, and attachment together. An existing peer or row with that ID is
`spawn_failed` with `session id already exists`. Resume requires exact equality
to `resume_session_id`. A disk failure returns `internal`; it removes the
unpublished ID and reservation and does not retry. The owner commits before it
handles the worker's next queued frame, so a request written immediately after
the open response observes the row. There is no additional commit buffer, gate,
or acknowledgement.

If the child exits during open, the reader still posts every frame it read
before its one connection-closed event. The owner records the process exit and
keeps the original spawn deadline while it drains those already-read frames. A
valid open response written before EOF may commit; EOF first fails spawn. No
compensating close is attempted for native files created before commit; that
accepted crash window does not justify another stored state.

Caller EOF never cancels describe, spawn, resume, or close. Only the caller's
reply helper ends; the launch owner continues to commit or final cleanup.
Describe still reaps its probe and a successful spawn still publishes its row.
A lost successful reply is recovered by listing visible lanes by name, not by
replaying spawn.

The bound is the single constant `spawnTransactionTimeout = 60s` for every
product and for both describe and spawn. It is not configuration and has no
per-product override.

### 2.4 Routing, turns, and close

Every request reads the caller from its exact current connection, qualifies
a bare target with the caller's own host, resolves the resulting canonical ID
or name among visible sessions, and then forwards the closed method without
translation. The target name part must satisfy the 1-128-character grammar
before lookup; malformed input is `invalid_frame`, while invisible and absent
targets are both `unknown_session`. The
daemon copies source identity and groups from its current peer entry or lane
entry when the request arrives and keeps that copy with the request;
caller-supplied identity does not cross the route and a later peer identity
replacement cannot alter an admitted call.

Destination-entry validation and the non-blocking post to its inbox are one
directory operation. A full 256-element inbox returns `busy` at that door. The
inbox holds mixed events, not 256 reserved call slots. A request already in that
queue may later receive `busy` when the target owner dequeues it. Multicast
resolution and deduplication happen first on a copied directory snapshot.

The target owner admits requests in inbox order. Its pending map contains at
most 256 unanswered worker calls; a dequeued 257th call returns `busy`. This is
a separate limit from the mixed-event inbox and must not be described as 512
calls. For `turn.run`, the owner also rejects a second outstanding run, records
the pending call, updates the directory's running projection, and sends the
worker frame before it handles a following interrupt. `turn.interrupt`,
`session.close`, and `message.deliver` use the same path. The worker kit owns the
single native interrupt invocation. A `session.close` operation retains its
pending slot through process cleanup.

Each pending operation retains the destination entry token and its capacity-one
reply slot. A worker result removes the operation and answers the slot in the
same owner-loop turn. Before settlement the owner compares the retained entry
with its current identity and directory entry; a response from a replaced
identity becomes `not_connected`, rendered as `no_receipt` for delivery. Worker
EOF settles every remaining operation once. A late response is unmatched and
dropped. Caller cancellation ends only its reply helper; it neither removes the
target's pending operation nor cancels product work. A later result is drained
and discarded, and the daemon stores no product-owned turn output.

A claimed row rejects every new run, interrupt, close, resume, or forget with
`busy`; it never waits. Delivery is not part of that exclusion: a delivery to a
claimed but still attached lane proceeds through the ordinary inbox and pending
limits. A row removed by a preceding `forget` is `unknown_session`.

On the first admitted `session.close`, the lane owner marks the row claimed and
sends exactly one worker close. The worker kit interrupts and awaits any current
run. A successful close response is held in the caller owner only while that
same target still has a run reply outstanding; a busy close is an admission
failure and is never held. When no same-target run reply remains pending, all
eligible held successful closes are released. The run terminal is written
first, then the close response. No dependency crosses targets.

The one `closeBound = 10s` deadline starts when the daemon sends
`session.close`. Close waits for the worker response, never for delivery of that
response to the caller. On ordinary completion the owner closes the socket and
records TERM. On expiry it closes the socket and records KILL. A process-start
event arriving later receives the recorded signal rather than reconstructing a
different one. Process cleanup kills the group, waits only for the direct child,
and sends one final group KILL. The stderr reader alone owns its read descriptor:
after child wait, an immediate read deadline wakes it, it drains bytes already
buffered without waiting for descendant EOF, closes the descriptor, and keeps a
bounded tail.

After cleanup the row remains offline and resumable. With `forget:true`, the
owner deletes its row file before removing the directory entry; later control or
resume is `unknown_session`. Forced cleanup settles the pending run once and
persists no fabricated result. Cleanup and claim release never wait for writing
the caller response.

Once close begins, the kit keeps the existing run slot occupied until process
exit even if the native run has already settled. A later `turn.run` is `busy`
without invoking product code, and delivery during close is rejected with
reason `closing`. This uses the existing run-slot fact; there is no closing
state.

The daemon has three named bounds: `spawnTransactionTimeout = 60s`,
`closeBound = 10s`, and `supersedeWriteBound = 1s`. The first two are lifecycle
clocks. The third bounds one final socket write and never delays a writable
connection. None is configuration or product-specific.

The daemon sends at most one `session.close` request on a worker connection.
A concurrent or later close while that row remains claimed receives `busy` and
the caller retries after cleanup. A second `session.close` frame on the
same worker connection is therefore a protocol violation and closes that
connection without a reply.

Every connection ending closes its file descriptor. The reader then posts the
one connection-closed event. On that event the owner detaches its exact
attachment, settles pending calls, and disposes the outbox. It continues
consuming its inbox until its owned helpers and child are finished and the inbox
is empty. Unrequested lane EOF also kills the process group and reaps the child
before releasing the claim. A resume attempt during that cleanup receives
`busy`. Peer EOF removes only the exact transient entry. A stale EOF cannot
clear a replacement entry.

Daemon shutdown closes every accepted connection, including a token-
authenticated worker that has not committed, prevents new reservations and
commits under the directory mutex, and broadcasts shutdown once. Each owner
disables that select case after receiving it, closes its socket, settles pending
work, and reaps its child. Connection owner loops, readers, writers, and launch
producers are registered before starting; reply helpers are covered transitively
by their owner's owned count. Shutdown returns after the accept loop and all
registered work have ended; nothing commits afterward.

### 2.5 Listing, messaging, and federation

`session.list` copies one coherent metadata snapshot under the directory mutex:
durable rows, transient peers, exact attachments, claims, and running
projections. It performs no disk read and waits on no owner loop. Filtering and
group visibility happen while selecting the snapshot; sorting and result
assembly happen after unlock. No roster cache or projection stream exists.

`message.send` first validates each target's name grammar. It then resolves and
deduplicates the whole visible recipient set before posting any delivery. Every
remaining label yields one receipt:
unresolvable labels are rejected with `unknown_session`, `unknown_host`, or
`ambiguous`, while every resolvable current target receives one
`message.deliver`. Resolution retains the exact destination entry. The target
compares that token when it dequeues the request, and response settlement checks
it again; a same-socket identity change therefore cannot attach an old delivery
or receipt to the new identity. Those deliveries run concurrently, and one response returns
the receipts in label order after session-ID deduplication. There is no delivery
timer. Offline lanes and failed calls produce rejected receipts. A full target
inbox or a target owner already holding 256 unanswered calls produces rejected
reason `busy`. A claimed but still attached lane remains deliverable. If EOF or
supersession ends the target connection before a receipt arrives, the sender
receives rejected reason `no_receipt`; whether product code acted is unknowable
and deliberately not promised. Canonical names are resolved only after
canonical IDs; an input containing `@` is always parsed at the last `@` and is
never retried as a local bare name.

Federation carries the same closed request and response objects between trusted
daemons. A daemon publishes only transient peers and durable lane summaries,
already containing canonical `id@host` and `name@host`; every receiver keeps
those values byte-identical and never persists a remote row. `session.list`
merges those summaries and `message.send` forwards a canonical delivery once. Federation snapshots
also carry each daemon's optional advertised product list; `session.list`
returns those lists without treating them as verified installation state.
Each host snapshot atomically replaces that host's prior contribution; ordered
live updates follow it. Disconnect removes the host's entire transient
contribution at once.

Hub-to-daemon transport is standard TLS 1.3 from Go's standard library. For a
configured secret and label, key derivation is exactly HKDF-SHA256 with empty
salt, info `agentbus/v1/<label>`, and 32 output bytes, passed as the seed
to `ed25519.NewKeyFromSeed`. Federation uses labels `host` for the daemon
identity and `hub` for the hub identity toward that host. Derived keypairs are
never stored or transmitted. The daemon presents a self-signed X.509
certificate over its derived host key and sends its effective host name as SNI.
SNI is cleartext by design because the host name is not secret. The hub selects
that name in `GetCertificate`, looks up its unique secret, derives the matching
hub key, and presents its self-signed certificate. Each side's
`VerifyPeerCertificate` pins only the peer public key to the independently
derived expected key; it intentionally applies no CA, certificate-expiry, or
certificate-hostname semantics.

The key match is the federation identity. The hub is the owner's trusted party:
it decrypts and routes federation traffic rather than acting as a byte tunnel.
An unknown, reserved `local`, or
already-connected SNI name, wrong secret, or name/key mismatch fails the TLS
handshake before any summary or request is accepted. A copied mode-0600 config
can impersonate that host because identity is the derived key, not a machine.
Secrets never leave configuration, TLS 1.3 supplies forward secrecy, and no
custom application handshake exists. There is deliberately no separate expiry
or revocation system: rotation is an edit to both configurations. The local daemon socket remains
the trusted transport described in Section 1. This transport is capped at
**120 production and 120 test logical lines** inside the proposed combined
daemon-and-connection budget; tests cover derivation, pinning, mismatch, and
rotation by configuration edit. Optional local encryption reuses that code and
may add at most **40 production and 60 test logical lines**; it does not create
a separate transport package or budget.

Visible remote lanes remain controllable without a second public wire. The one
forwarder accepts a canonical message recipient, a canonical session ID for
resume `lane.spawn`, `turn.run`, `turn.interrupt`, or `session.close`, or
explicit `host` on new `lane.spawn` and `lane.describe`. Absent `host`, or
`host` equal to the local daemon, stays local. Otherwise the connected target
daemon performs the entire reservation, process, row, visibility,
connection, and pending transaction locally and returns the identical response
or error. Its table is the only home of a spawned row; the origin later exposes
its already-canonical identities through a federation summary. An unconnected
explicit or canonical host returns `unknown_host` before product or session lookup.

A forwarded request is exactly
`{from:{session_id:<id@host>,groups:[...]},request:<unchanged public request>}`.
The originating host is the authenticated TLS connection and correlation is
the enclosing JSON-RPC ID. The target applies its ordinary visibility check
using the carried groups; the hub does not make that decision. A federated
turn is one outstanding RPC at each hop; caller loss has the same sink-only
effect, and the target daemon drains and discards a result whose origin
reply sink has disappeared. Transport loss before a forwarded response returns
`forward_lost`: the request may or may not have been applied and is never
retried.

`maxPendingForwardedPerHost = 256` is the one added limit. Overflow
closes that host connection and fails its pending forwarded calls as
`forward_lost`; the ordinary 1 MiB frame limit already bounds snapshots and
requests. All forwarding is one function of at most 60 logical lines: resolve
either form of host address, forward the identical request once, and return the
identical response or error. It stores no remote durable state and never
retries. Replay, durable hub rows, distributed locks, automatic host placement,
multi-hop routes, and capability negotiation that gates PATH launch are
explicitly outside the design.

### 2.6 Package boundary

The repository has two literal, split-ready top-level trees. `bus/` contains
the daemon and hub commands, `bus/internal/daemon`, generic connection and
process internals, `bus/internal/protocol` as the single schema/fixture authority,
conformance references, and the public `bus/sdk/go` and `bus/sdk/js` kits.
`wrappers/` contains every product-named launcher, resident wrapper, plugin,
and packaging projection. A future repository split can use `git subtree`
without moving a file across that boundary.

`bus/internal/daemon` owns the directory, session owner loops, row files,
reservations, routing, and federation. `bus/cmd/agentbus` only
parses CLI/MCP input, constructs the daemon with its optional federation,
discovery, and local-encryption configuration, and renders results. Product
selection never enters either request router.

`bus/internal/conn` is the daemon's product-agnostic socket pair. The session
creates the inbox and passes it in. One reader posts frames or one final
connection-closed event; one writer consumes the bounded outbox. Foreign posts
are non-blocking, while the reader and owner-confined helpers use their owned
paths. Closing the file descriptor is the only termination request. The reader
alone closes the connection `done` channel and reports the single termination
event; the owner handles it once and disposes the outbox. Graceful supersession
queues one final frame under its one-second deadline before the writer closes
the descriptor. Hard close, overflow, and shutdown close it immediately. No
product policy enters this package.

`bus/internal/rpc` is the separate full-duplex client used by the public kits.
Its one reader runs admission handlers in frame order; a handler decides state
and returns, while callbacks or waits run outside the reader. One complete-frame
write mutex and one small state mutex protect its client calls and close path.
It is not used to own daemon sessions and does not duplicate the directory or
owner-loop facts.

`bus/internal/structuredprocess` retains generic bounded TERM/KILL process
ownership and must not import a product or protocol state type. The public Go
and JavaScript worker and caller kits live only at `bus/sdk/go` and
`bus/sdk/js`; the JavaScript kit is published as `@agentbus/kit`, and wrappers
import bus code only through those paths. One Go module
contains bus, SDKs, and wrappers for now, so Go's `internal` rule enforces the
boundary mechanically while leaving each SDK a clean public import path.

An architecture test rejects every `bus/` import of `wrappers/`, every wrapper
import of `bus/internal`, and every real product-name token under `bus/` source
except opaque values in the optional products configuration and explanatory
documentation. Both SDKs consume the one authority in `bus/internal/protocol`
through their public schema-byte exports;
no schema or fixture copy exists.

`internal/productruntime` dies completely. Its driver interfaces, per-product
registry, environment carrier, native references, and daemon-facing errors are
the architectural seam this protocol removes. Wrapper-specific native code is
assessed in Sections 3 and 4; it may reuse product primitives, but it cannot
restore a daemon driver interface.

The implementation size contract is measured as final logical production
lines, not as additions hidden behind relocation accounting:

| Surface | Maximum | Constraint |
| --- | ---: | --- |
| `bus/internal/daemon` + `bus/internal/conn` | 1,400 | Directory, session owners, row files, launch, delivery, federation, and the daemon socket pair together. |
| Largest daemon router file | 450 | No product literal, argv parser, or product callback. |
| Durable row files | 120 | Load, write+rename, and delete for the six stored columns. |
| Connection admission | 150 | First hello, both peer re-hello branches, and supersession. |
| Spawn/describe | 200 | Name/token reservation, empty-argv exec, owner-loop decision, 60-second bound, and commit before the next frame. |
| Routing and visibility | 200 | Directory lookup/post, owner-loop admission, reply slots, group visibility, per-label receipts, and last-`@`/bare-name resolution. |
| Close, EOF, and forget | 100 | Claim, settle, optional row delete, and KILL at 10 seconds. |
| CLI, composition, and config | 100 | Optional hub, host, products, and local-key loading. |
| Cross-host forwarder | 60 | Host-qualified session ID or explicit spawn/describe host; one hop, no state, retry, or shape translation. |
| `bus/internal/conn` subcap | 150 | One reader, one writer, bounded inbox/outbox, and descriptor close. |
| `bus/internal/rpc` | 200 | Framing, one reader, pending calls, complete-frame writes, and close. |
| `bus/internal/structuredprocess` | 700 | Generic process ownership; current functionality may remain. |
| `bus/cmd/agentbus` daemon composition | 350 | Construction and rendering only; no protocol state. |

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
| 41 | `cmd/agent-sessions/dsh_lane.go` | Daemon-side DSH driver composition dies. | Universal PATH launch plus the native DSH plugin. |
| 348 | `cmd/agent-sessions/federation.go` | Product-aware federation router dies. | The generic one-hop forwarding function. |
| 444 | `cmd/agent-sessions/federation_test.go` | Tests the deleted router. | Daemon federation proofs in Section 5. |
| 1,628 | `cmd/agent-sessions/lane.go` | Lane actor, parsers, lifecycle, and product dispatch die. | Daemon table/router plus caller-kit composition. |
| 94 | `cmd/agent-sessions/lane_names.go` | Actor-derived name authority dies. | Durable-table name index and runtime-map identity. |
| 149 | `cmd/agent-sessions/lane_notice.go` | Terminal notice and collection machinery die. | Direct `turn.run` reply plus filtered `session.list`. |
| 1,245 | `cmd/agent-sessions/lane_test.go` | Tests the deleted lane machinery. | Daemon transaction tests and shared kit fixtures. |
| 746 | `cmd/agent-sessions/messaging.go` | Product-aware peer/lane routing dies. | Generic daemon resolution and delivery. |
| 662 | `cmd/agent-sessions/messaging_test.go` | Tests the deleted messaging router. | Daemon delivery and federation proofs in Section 5. |
| 434 | `cmd/agent-sessions/presence.go` | Report/projection presence server dies. | Universal connection admission in `internal/daemon`. |
| 1,257 | `cmd/agent-sessions/presence_test.go` | Tests the deleted presence server. | Universal admission, listing, EOF, and swap tests. |
| 68 | `cmd/agent-sessions/preparation.go` | Old host preparation composition dies. | Minimal command composition over the universal PATH launch. |
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
| 399 | `internal/daemon/control_unix.go` | Side control server dies. | One universal endpoint using `bus/internal/rpc`. |
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
| 31 | `internal/productruntime/lane_registry.go` | In-process product driver registry dies. | Direct `<product>` PATH resolution with launch-token worker selection in the daemon. |
| 31 | `internal/productruntime/lane_registry_test.go` | Tests the deleted registry. | Product grammar and PATH-resolution tests. |
| 110 | `internal/productruntime/registry.go` | Host dependency/product composition registry dies. | Command composition plus wrapper constructors. |
| 107 | `internal/productruntime/registry_test.go` | Tests the deleted registry. | Command composition and wrapper launch tests. |
| 251 | `internal/productruntime/types.go` | Daemon-facing native structs and capabilities die. | Shared schema types move to their connection, storage, process, or wrapper owners. |

One retained command path also changes ownership and is not counted as a
deletion:

| Retained file | Deleted dependency | Replacement / rehoming |
| --- | --- | --- |
| `cmd/agent-sessions/hook.go:38-72` | Its `internal/bridge` hook dispatcher dependency is separated from the Codex lane/App Server primitive. | Hook input and attestation dispatch stay in the command package and use the universal caller/control boundary; only Codex lane primitives move into the resident wrapper. |

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
| `open(cancel, request)` | Create or resume from the typed request, apply its composed name as the product title where supported, and return the exact product session ID. |
| `run(cancel, run, input)` | Start one native turn for the kit-owned `Run` token, observe it to a terminal result, and return that result. |
| `interrupt(cancel, run)` | Ask the native turn identified by that same `Run` token to stop. |
| `deliver(cancel, request)` | Receive the full closed `MessageDeliverRequest` `{message_id,from,body}`, inject now or queue for the next turn, and return the truthful closed receipt. |
| `close(cancel)` | Stop accepting work, close native state, and release product resources. |

These are primitives, not a daemon adapter interface. They live in the product
process, receive only closed wire values, and never expose a product type to the
daemon. A product can replace our kit with its own implementation by passing the
same conformance fixtures; no daemon or schema change follows.
`cancel` is a Go context or JavaScript `AbortSignal`; control EOF cancels every
callback, and close cancels remaining work after the terminal boundary.
App-ready means the product can accept `open()` and can serve its first `run()`;
the vendor decides how to establish that fact, and the kit sends hello only
afterward.

The product owns the session ID and title. A successful `open()` returns the ID
that the product itself uses, and resume must return the supplied
`resume_session_id` exactly. The composed `session.open.name` is applied as the
product title wherever a title primitive exists, so the bus and product expose
one name. A lane title is fixed for the lifetime of its worker connection:
native lane mode suppresses automatic retitling, and workers never re-hello. A
product that cannot suppress retitling declares an explicit R5 relaxation in
its ledger: the bus name remains authoritative and the native title may differ.

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

The worker kit reads `AGENTBUS_SOCKET`,
`AGENTBUS_LAUNCH_TOKEN`, and the optional `AGENTBUS_LOCAL_KEY`;
it removes both secrets from the process environment and connects to the
named daemon endpoint. The local key selects TLS and is retained only
in the kit's private connection material; neither secret is logged, returned,
or copied into product configuration. The kit sends the worker branch of
`session.hello` only after `hello()` succeeds. `session.open` is the only call
that invokes `open()`. Until that result is written, the kit has no public
session identity and the daemon rejects its worker-originated session methods.

The kit has only two live facts: the connection is open or closed, and a run is
present or absent. The product's opened session reference is data, not a
lifecycle state. There is no generation, projection, collector, archive phase,
deadline, or reconnect state machine.

The kit creates one `Run` token when it installs the run slot and passes that
same object, with the same per-run cancellation context, to `run()` and
`interrupt()`. `Run.Interrupted()` exposes the kit's coalesced interrupt mark;
the kit sets it before calling `interrupt()`. `Run.Native` is the product's one
product-synchronized slot for its native turn. The product publishes that slot
under its own handoff lock and then rechecks `Run.Interrupted()`; `interrupt()`
reads the slot under the same lock. Whichever side observes the other performs
the one native interrupt. Once `run()` returns, the kit issues no new native
interrupt, including while it writes the terminal result. A product or wrapper
keeps no second starting, active, or interrupt-requested lifecycle bits.
`Run.Done()` closes after the terminal response has been written and the run
slot has been cleared, or after the connection has entered its single close
path when no terminal can be written. It never closes before either boundary.

Before mutating an existing session, a native product must acquire exclusivity
that excludes any competing process and hold it through native cleanup. A
fresh product-minted ID is acquired immediately after allocation and before
subsequent mutation; no lock can truthfully precede the creation of an unknown
key. An in-process duplicate-session check alone is insufficient because it
cannot fence a writer surviving in another process. Products without such a
primitive use the wrapper-host inherited-flock rule in Section 4.1.

### 3.2 Full-duplex lifecycle

One reader validates inbound requests and runs each handler synchronously in
frame order. The handler decides state and returns at once; it dispatches every
product callback and response write outside the reader. This permits open,
deliver, interrupt, or close code to originate an ordinary session method and
receive its response. One writer
mutex preserves complete frames. Independent
request IDs correlate worker-originated session methods and inbound results, so
`deliver`, `interrupt`, `session.close`, and those methods all proceed while
`turn.run` is outstanding.

The `Run` token is installed before `run()` starts. Immediately when `run()`
returns, the kit cancels its per-run context under the slot mutex; from then on
interrupt returns `not_running` without product code, including during result
mapping, truncation, and the terminal write. Under that mutex, the run handler
then validates the terminal result, writes its response, and clears the slot. A
failed write closes the connection and leaves the slot occupied. A
second run receives `busy` while native work remains; one arriving during the
terminal write waits on the mutex and is admitted after the slot clears.
Interrupt marks the slot once and invokes `interrupt()` once; concurrent and
later interrupt requests for the same run return `{}` without a second native
call. After the terminal response and slot clear, interrupt returns
`not_running`. No kit timeout is involved. The run handler alone writes the run
response; close may await the slot's completion signal but never owns that
response.

The worker handles one `session.close`: it first claims an empty run slot or
joins the existing run, invokes the shared interrupt once without awaiting that
callback, awaits the run result when present, cancels every callback context,
calls `close()` once, writes one response, and closes the socket. A second close
frame is the protocol violation defined in Section 2.4. One `sync.Once`
arbitrates the product's `close()` call
between this orderly path and EOF; there are no close waiters or stored close
results. A hanging interrupt cannot hide an available terminal. This entire
interrupt/terminal/native-close sequence must fit within the daemon's single
`closeBound = 10s`; expiry kills the worker and the kit invents no result. From
the instant close owns the slot,
new runs are `busy`, delivery is rejected as `closing` before product code, and
interrupt returns `{}` without another product call.

The kit owns final process ordering: it calls `close()`, writes the close
response, closes its socket, and then resolves its `closed` signal. The product
awaits that signal before process exit; `closed` is a kit signal, not a seventh
callback. `Worker.Shutdown()` lets the product invoke the same single close path
when its native child dies; it is idempotent, closes the socket, and resolves
`closed` without adding another lifecycle state.

The three stop paths are distinct. `interrupt()` asks the current run to stop;
orderly `session.close` follows the preceding close sequence; control EOF first
cancels every callback context, never invokes `interrupt()` afterward, then
calls `close()` exactly once only if `open()` previously succeeded, and exits.
A describe probe whose connection closes before open therefore exits without a
`close()` call. Worker mode never reconnects with a consumed token.

A peer-mode connection behaves differently only at the connection boundary:
ordinary daemon EOF detaches the dead connection and retries the same asserted
peer identity every fixed `peerReconnectInterval = 2s`, with no backoff,
jitter, or attempt cap. A call made while disconnected fails `not_connected`
and is never replayed. `session.superseded` tombstones that identity instance
and stops retries permanently.
The peer kit keeps one JSON-round-trip snapshot as the desired identity. After
each hello response it compares what it sent with that desired value and sends
the current value immediately until they match; a change crossed with connect
or another re-hello therefore cannot leave the older value installed. Its
`rehello(name, info)` call preserves the product, session ID, and groups. While
disconnected it stores the new desired name and information and returns
`not_connected`.
Its `replace(ctx, identity)` call supplies a complete new identity for a
different-ID re-hello on the same connection. It settles outstanding operations
from the old identity as `not_connected`; reconnect uses the new desired
identity.
The product's current title is the peer name. A same-ID re-hello updates that
name and information in place only when the declared groups slice is exactly
equal, including order. A
different-ID re-hello ends the old transient identity and installs the new one
on the same socket; it is not a supersession. Worker mode never reconnects or
re-hellos. The closed types, pending calls, complete-frame writes,
delivery, and worker-originated session-method API are otherwise shared.

### 3.3 Go and JavaScript parity

The public SDK exposes `WorkerCallbacks`, the kit-owned `Run`, and the wire types
generated from the schema:
`HelloDescription`, `ExtraArgument`, `OpenOptions`, `OpenRequest`, `OpenResult`,
`TurnResult`, `DeliverySource`, `DeliveryRequest`, `DeliveryReceipt`, `Identity`,
`SessionSummary`, `HostProducts`, and `ProtocolError`; wrappers and products do
not hand-maintain protocol-shaped duplicates. The worker entry is
`serveWorker(callbacks, env)`, which returns the kit-owned `closed` signal. Peer
mode is `connectPeer(identity, deliver)`, `rehello(name, info)`, and
`replace(ctx, identity)`. A
connection-bound client supplies `list`, `send`, `describe`, `spawn`, `resume`,
`run`, `interrupt`, and `close(forget)` and is usable from every callback
without blocking the reader. Go also exports one thin no-hello client:
`Dial(socket)`, `Call(ctx, method, params)`, and `Close`; the caller kit and a
wrapper's private lane socket use that one framed implementation.
Go exports `NewCaller(call)` to place the same typed methods and caller
conveniences over that no-hello client's call function; workers and peers are
the other two uses of the same caller.

Go constructs one caller for each worker before `Serve` and binds it to the
worker's current connection through `Worker.Call`; repeated `Worker.Caller()`
calls return that same object, including when called before `Serve`.

`start`, `status`, and `wait` are caller conveniences for resident callers such
as plugins and the shared MCP server. They are not wire methods or
`agentbus-call` subcommands, and have one shared Go/JavaScript shape:

| Call | Result and local rule |
| --- | --- |
| `start({session_id,input})` | Starts one wire `turn.run` and returns `{turn_id:"t-<n>"}`, counting from 1 per client. A map holds outstanding runs; different target sessions may run concurrently, but a second uncollected run for the same target is locally `busy`. |
| `status({turn_id})` | Returns `{turn_id,session_id,state}` where state is `running`, `done`, or `unavailable`; `done` includes `result`. EOF uses reason `result unavailable, lane resumable`; another wire error uses `<code> <message>`. For an existing turn ID, status/wait return a state rather than an error. The first done/unavailable result collects the entry; a later lookup is `unknown_turn`. |
| `wait({turn_id,timeout_ms?})` | Returns the same object on completion or connection loss. A timeout returns state `running` without cancelling the wire call; absent `timeout_ms` waits without a local limit. Completion or loss frees the target immediately, while collection follows the same first-result rule as `status`. |

Go and JavaScript expose the
same cancellation, concurrency, error, truncation, and EOF behavior through
the shared fixtures; a later Python SDK is a translation of this surface, not a
new contract. `peerReconnectInterval = 2s` is the caller kit's one clock;
worker mode has none.

The Go host and JavaScript worker mode implement the preceding algorithm, not
two interpretations of it. Each uses a small interpreter over the published
schema with no external validation library. Both are tested against
`bus/internal/protocol/session.fixtures.json`; both caller kits also execute
`bus/internal/protocol/caller-sugar.fixtures.json`. Both run the same ordinary
table-driven lifecycle cases with fake product callbacks and a fake duplex
connection. The schema and fixtures are published for vendors to test their
kits. The lifecycle cases are:

1. app-ready hello, one open, and worker-originated session methods rejected before the open
   result but accepted on the same connection after it;
2. describe hello followed by EOF before its acknowledgement, proving open and
   close are never called;
3. completed, interrupted, and failed run results, including empty output and
   character-bounded truncation with the exact `truncated` flag;
4. a blocked run plus a second run rejected before product code;
5. concurrent interrupt requests for one run, with the kit mark visible on the
   shared token, exactly one native interrupt callback, and no native turn
   created when interruption wins the product handoff;
6. delivery and a worker-originated session method completing while run remains blocked;
7. close during run, with the terminal run response written before the close
   response;
8. control EOF during run, with all pending calls failed and product close
   invoked once;
9. peer EOF reconnect within one fixed interval with the same identity, a call
   while disconnected failing without replay, supersession stopping retries,
   same-ID re-hello updating name/info with identical ordered groups, changed-
   group rejection, and different-ID identity replacement on the same socket;
10. a daemon-to-worker `session.hello` rejected as a wrong-direction request
    before a product callback; worker-originated re-hello rejection belongs to
    daemon admission;
11. `run()` returning, terminal processing paused, and then interrupt, proving
    `not_running` and no native interrupt call before the terminal write;
12. delivery first and another delivery during close, proving cancellation of
    the first callback and `closing` before a second product call;
13. a non-run callback that originates a session method and receives its response;
    and
14. endpoint selection plus single launch-token/local-key reads and secret
    environment removal, followed by connect failure and process exit without
    reconnect;
15. `Run.Done()` remaining open until the terminal response is written and the
    run slot is cleared; and
16. product-requested `Worker.Shutdown()` resolving `closed`, with a second call
    doing nothing; and
17. a failed terminal write entering the connection close path, closing
    `Run.Done()`, and emitting no terminal frame.

There are no product names, product IDs, clocks, sleeps, or network sockets in
the fixture data. Tests control every callback and frame boundary
deterministically.

The size contract is final logical lines:

| Reference surface | Production | Tests |
| --- | ---: | ---: |
| Go transport (`bus/internal/rpc`) | 200 | 250 |
| Go worker host | 205 | 250 |
| JavaScript client plus worker mode | 200 | 250 |
| Go caller kit | 250 | 400 |
| Go no-hello client (`client.go`) | 30 | — |
| JavaScript caller kit | 150 | 150 |
| Reference caller | 200 | 200 |
| Reference worker | 250 | 150 |
| Shared wrapper host | 400 | 400 |
| Shared lifecycle fixture data | — | 220 |

Generic connection framing is counted in Section 2, not duplicated into either
worker host. The no-hello client's tests are counted in the Go caller-kit test
budget; it has no separate test allowance. A kit
exceeding these limits has grown product policy or a third lifecycle fact and
must be simplified.

### 3.4 DSH: the first native worker

DSH's product token and binary name are `dashi`. DSH uses one Agent Sessions
plugin and one connection per DSH root session. In
ordinary product mode the plugin sends peer hello without a launch token and
reconnects after daemon EOF. In lane mode it captures and scrubs the token,
waits for DSH app-ready, and sends worker hello on that same socket. It does not
also publish peer presence: successful `session.open` turns the worker
connection itself into the lane's presence, tool path, and delivery path.
Its read-only `/agent-sessions` command reports the current connection mode,
canonical identity, and attachment facts without mutating either product or
daemon state.

The DSH `agent-sessions` profile is exactly headless DSH core plus the unified
plugin configured `mode: lane`; it disables DSH automatic title changes after
open, and no TUI, second comms plugin, lane extension, relay, or local socket is
loaded. Its package profile is
`{"bundles":["@deepseek-ai/dsh-base","@deepseek-ai/dsh-headless"],"patchReload":"startup"}`
under `dsh.profile`, and its `cordis.patch.yml` inserts exactly
the `session-controller`, `workspace`, and
`{id: agent-sessions, name: '@agentbus/dsh', config: {mode: lane}}` rows,
plus the `workspace-write-noninteractive` permission-preset override, after
bundle patches and before any `--patch` overlays. A non-disabled plugin row is
load-mandatory by DSH boot semantics: import failure exits one with "plugin tree
failed to load". Phase 2 makes the `dashi` boot layer select this exact profile
and `mode: lane` plugin configuration whenever
`AGENTBUS_LAUNCH_TOKEN` is present; no command-line switch or alias is
involved. Without that variable, the normal DSH profile runs the same plugin in
peer mode. Forcing the plugin's lane mode without a launch token is a startup
error rather than an accidental peer.

The c5b280d integration already proves the DSH core primitives the unified
plugin needs:

- `appReady.onReady` and `appExit` gate hello and terminate headless mode;
- `sessionController.create` and `resolveAgent` create or resume an exact
  session; `rename` applies the composed product/bus title, and `selectModel`
  applies a `provider/model` value but currently also changes the deployment
  default (the upstream ask is a session-only selector);
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
  client-to-daemon session-method API carries its calls on the same socket.

Fresh open creates the DSH session only after the typed request arrives,
applies the composed name, and returns its exact ID. Resume resolves
`resume_session_id` and returns that same ID. DSH declares
`extra_arguments:[]`; a nonempty `open.arguments` fails with `spawn_failed`.
Run submits one user message, converts the observed DSH terminal reason to the
three wire outcomes, and carries the DSH reason kind as
`native_stop_reason`. Deliver never starts an unrequested DSH turn and
the native plugin contains no delivery queue. DSH reports `injected` because it
has append-without-run; `queued_for_next_turn` remains conforming for any
product that lacks that primitive. A running delivery is `injected` exactly when
`agent.steer` resolves; there is no receipt polling. In peer mode, a DSH rename
sends a same-ID re-hello with the new title and unchanged groups, updating the
bus name in place. Lane open applies the daemon-composed title and a lane never
re-hellos. The registered Agent Sessions tool exposes the
caller kit's start/wait/status/interrupt/spawn/describe/close/list/send surface
defined once in Sections 4 and 5. The close callback cancels if needed, waits
on the currently unbounded `agent.whenIdle`, flushes the session, and returns;
`closeBound` therefore exposes a deliberate supervisor-KILL path until DSH
offers a bounded primitive. A separate outer plugin task awaits the kit's
`closed` signal and then calls `appExit(0)`. After open, control EOF follows the
same product cleanup and exit path; a describe EOF has no native close.

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

`integrations/dsh/comms` remains as the source location but its package identity
becomes `@agentbus/dsh`; `dsh-comms` is a deprecated compatibility alias
only during packaging migration. Its plugin and tests are rewritten
around the shared JavaScript kit; its Cordis patch installs peer mode, while the
headless `agent-sessions` profile pins lane mode. Package/install inventory must
contain one DSH integration artifact, not the old comms-plus-lane pair.

DSH also applies `$DSH_HOME/cordis.patch.yml` (default
`~/.dsh/cordis.patch.yml`) to every profile above per-profile patches and below
`--patch` overlays. The installer writes one `@agentbus/dsh` insert there
and places the package where every profile's module walk resolves it, for
example `$DSH_HOME/profiles/node_modules/@agentbus/dsh`. DSH currently
heals `profiles/node_modules` only for its own dependency closure; teaching it
to preserve this external package is the upstream packaging ask. The `dashi`
launcher retains its exact DSH version pin and, for peer launches, maps its
`-g` values to the JSON `AGENTBUS_GROUPS` environment.

The DSH migration therefore adds **16 more deleted files and 1,504 deleted
lines** before rewriting the retained unified plugin. Combined with Section 2,
the signed deletion floor becomes **63 files and 13,767 lines**. The DSH tree
must remain net-negative after the kit is accounted separately.

## 4. Non-native resident wrappers

### 4.1 Shared wrapper boundary

| Ledger item | Decision | Reason |
| --- | --- | --- |
| Process and connection | Lane mode starts one resident wrapper, which owns one product session and the one daemon connection. Peer mode has no wrapper around the interactive TUI: the product-spawned stdio MCP server or JavaScript plugin owns the direct peer connection. | The connection holder is the integration process the product already supervises. Wrapping an interactive TUI would add terminal, signal, resize, and hand-started-session failure modes without improving the protocol. |
| Installed entry forms | One installed integration image is named `<native>-peer` and exposes two entry forms: invocation without a launch token starts the interactive product whose MCP/plugin holds a direct peer connection; invocation with a launch token holds the worker connection and owns a headless child. | The environment is the mode discriminator for native and wrapped products alike; no product-specific worker flag enters the daemon. |
| Peer identity and name | Identity is fixed before peer hello from the exact environment table below when present; otherwise Claude resolves its parent through `claude agents --json` (`cmd/agent-sessions/connector.go:449-487`), while DSH/OpenCode/Kilo/Pi/OMP read their in-process session ID; Codex defers hello until the first tool call supplies `_meta.threadId` (`connector.go:247-251`). Fresh Grok and Qwen peer launchers mint one product-compatible UUID and pass it both through native `--session-id` and `AGENTBUS_SESSION_ID`; a hand-started Grok or Qwen process without either source serves every Agent Sessions tool with an error naming the required launcher and never sends hello. The product's current title is the hello name; a supported retitle sends a same-ID, same-groups re-hello. | Identity and title are launcher or product facts, never guessed process-global values. Re-hello mirrors one product name on the bus without adding an update method. |
| Product boundary | The wrapper exposes the six Section 3 callbacks locally and contains every product import, argument translation, native protocol, and delivery compromise. | Deleting one wrapper when a vendor adopts the native kit must require no daemon, schema, or caller-kit change. |
| Child launch and title | The lane wrapper connects and sends worker hello before starting a native child. It receives and validates `session.open`, then spawns the child with the stored cwd, model, reasoning, permission, ordered argument values, and composed name as the product title wherever the product supports one. It never observes or publishes later native retitles. If a product cannot keep that title fixed, its ledger names the R5 relaxation and the bus name remains authoritative. | Process-level flags and titles are ordinary open fields for wrappers because the product does not exist until open. Native products start before open and therefore need session-level primitives instead. A fixed lane identity cannot silently follow product auto-title churn. |
| Child lifetime | If the native child dies while idle, the wrapper reaps it and exits immediately; worker EOF makes the row offline and explicitly resumable. | A live worker connection must never advertise a dead product or synthesize an internal restart policy. |
| Native-session exclusivity | Before touching a resumed session, or before fresh creation when the wrapper chooses its ID, the wrapper opens `dirname(AGENTBUS_SOCKET)/locks/<product>/<session_id>` with `O_CREAT` and takes an exclusive flock. It never treats file existence as ownership: a stale file is harmless and only a live flock blocks. When only the product can allocate a fresh ID, the wrapper locks immediately after allocation and before every later mutation. It passes that same open file description to the native child as an inherited descriptor and holds it through cleanup. The OS releases the lock only after every holder exits; contention fails open as `spawn_failed` with `session busy`. A native product may replace this only with its own cross-process exclusion of competing writers. | An ordinary wrapper death must not release ownership while its child can still write. The inherited flock provides a death-safe process boundary without PID state, a reap registry, or daemon product knowledge; locking an unknown not-yet-minted ID would be fictitious. |
| Tool ingress | In lane mode the wrapper owns the private Unix socket `dirname(AGENTBUS_SOCKET)/lanes/<session_id>.sock`, unlinks it on exit, and passes that path as `AGENTBUS_LANE_SOCKET` to the product-spawned `<product>-peer mcp` helper. A product that cannot spawn a stdio helper may use another private endpoint, but none of Claude, Codex, Grok, or Qwen needs one. In peer mode that same MCP entry or plugin connects directly to the daemon through the peer kit. | Each mode still has exactly one daemon connection: wrapper-owned for a lane, product-integration-owned for a peer. A per-session Unix path cannot be reused by a stray child as a different lane's endpoint. Private lane helpers never become presence. |
| Shared MCP entry | `wrappers/mcp` is one stdio MCP server for the caller-kit tool surface. Its peer backend uses the caller kit plus `connectPeer`; its lane-local backend uses the wrapper's per-session Unix socket. Each product adds only `<product>-peer mcp` dispatch and identity resolution. | One MCP implementation and two small backends prevent every wrapper from rebuilding tool JSON and framed client code. Budget: **200 production / 200 test logical lines**. |
| Local encryption handoff | Thin peer launchers place `AGENTBUS_LOCAL_KEY` in the product environment when configured, and the product-spawned MCP entry or JavaScript plugin consumes and scrubs it. A lane wrapper consumes the key for its daemon connection and never passes it to its private MCP/plugin endpoint or native child. | Only connections to the daemon use optional local TLS; private wrapper hops are not daemon connections and carry no daemon key. |
| Wrapper-only queue and run handoff | A wrapper that lacks native append/injection owns one in-memory FIFO capped at 64 deliveries and 1 MiB total after rendering and newline separators. The wrapper host's own renderer, fixed by a golden fixture, emits the `[agentbus-metadata: ...]` carrier line, preserves arrival order, joins rendered entries with newlines, and prepends the result before caller input. FIFO extraction, native-turn creation, and interrupt are serialized under one boundary: interrupt before native creation aborts creation and returns terminal `interrupted` without a native call; after creation it calls native cancel. `injected` is returned only when the product callback confirms an actually active native turn; otherwise the message joins the next input. At run start the host atomically swaps the FIFO; overflow is rejected as `queue_full`. Shared fixtures cover interrupt at creation, delivery racing the first turn, and delivery racing terminal completion. | One renderer and one handoff boundary prevent wrappers from changing sender metadata, losing a boundary delivery, creating an unstoppable turn, or claiming injection into a turn that did not exist. `queued_for_next_turn` remains truthful; loss with wrapper exit is the accepted loss in Section 1.2. |
| Interactive delivery | A peer integration may let an interactive product start a turn in response to delivery and report `injected`; the wrapper FIFO rule applies only to a lane, where delivery must not start an unrequested turn. | Peer interaction is already user-owned product work; lane control remains explicit through `turn.run`. |
| Caller tool surface | Every product exposes the same caller-kit start/wait/status/interrupt/spawn/describe/close/list/send operations; product plugins do not invent wire methods. | Tool presentation is kit sugar over the eleven methods and is identical for native and wrapped products. |
| Shared size cap | Wrapper host, private MCP/plugin endpoint, and bounded FIFO together: **400 production / 400 test logical lines**. | Product-independent scaffolding larger than the daemon router would be a second protocol implementation. |
| Shared deletion | Delete the 16 shared files in `internal/launcher`, including `lane_grok_test.go` exactly once (2,199 lines), and `cmd/agent-sessions/connector_refresh.go` plus its test (333 lines). Rewrite `connector.go`/test as the peer-mode MCP entry that owns a direct peer connection, and rewrite `native_peer.go` as thin exec-time product configuration; lane-wrapper composition is separate. | The old launcher package still dies: CLI parsing and thin peer exec plans move to `cmd`, lane process ownership to the generic bus supervisor, and lane recipes to wrappers. Connector self-exec/release refresh is unnecessary when the installed MCP entry already is the peer connection holder. |

The launcher/daemon environment contract is exact:

| Variable | Value | Producer |
| --- | --- | --- |
| `AGENTBUS_SESSION_ID` | Bare product session ID part. | Peer launcher only. |
| `AGENTBUS_SESSION_NAME` | Bare, unqualified name part. | Peer launcher only. |
| `AGENTBUS_GROUPS` | JSON array string containing the asserted groups. | Peer launcher only. |
| `AGENTBUS_SOCKET` | Named daemon Unix-socket endpoint. | Peer launcher and daemon lane spawn. |
| `AGENTBUS_LOCAL_KEY` | Optional local-TLS secret. | Peer launcher and daemon lane spawn when configured. |
| `AGENTBUS_LAUNCH_TOKEN` | One-use expiring worker reservation token. | Daemon lane spawn only. |
| `AGENTBUS_LANE_SOCKET` | Wrapper's per-session Unix socket for the stdio MCP helper. | Lane wrapper only. |

Peer launchers pass ID, name, groups, socket, and optional local key unchanged
through the product to its MCP/plugin child. Lane launches contain only socket,
optional local key, and launch token; canonical lane identity arrives later in
`session.open`.

### 4.2 Claude Code

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `claude-peer` with a launch token owns one long-lived `claude -p --input-format stream-json --output-format stream-json --verbose --replay-user-messages` child. Without a token, `claude-peer` launches interactive Claude; Claude's product-spawned Agent Sessions stdio MCP server owns the peer connection. | The lane stream is proven at c5b280d `internal/products/claude/lane.go:112-145`. Peer presence needs no TUI wrapper and continues to work for hand-started Claude sessions. |
| Open and resume | Fresh mints a product-compatible UUID, passes its bare part as `--session-id`, applies the composed title with `--name`, and returns that ID; resume uses `--resume <resume_session_id>` and reapplies the stored open object. Claude supports all five open fields: `cwd`, `permission_mode`, `model`, `reasoning_effort`, and `arguments`; model maps to `--model`, effort to `--effort`, and the protocol's default permission maps deliberately to `--permission-mode dontAsk`. | c5b280d maps the existing stream flags at `lane.go:112-141`; Claude's product CLI help exposes `--session-id`, `--name`, `--model`, `--effort`, and permission mode. The wrapper, not the daemon, owns ID minting and the single interpretive permission mapping. |
| Readiness and projection | The wrapper uses `InitGated` readiness by default: worker hello follows Claude's `system/init`. `ExitWatch2s` is used only if the `umka-dev1` timing probe proves init cannot gate startup. Interactive projection lands with the `claude-peer mcp` entry, not the lane wrapper. | One installed-product probe chooses between two named observations; no daemon readiness state or product timer is added. |
| Run | Write one stream-json user frame, keep the run callback pending, and convert the exact result frame to the terminal result. | `lane.go:173-225` already proves the single stream write plus terminal observation. |
| Tools | In lane mode `claude/.mcp.json` starts `claude-peer mcp` against the wrapper's private Unix endpoint. In peer mode the same stdio entry takes launcher environment first and otherwise confirms its parent session through `claude agents --json` before hello. After Claude `/clear`, the next tool call identifies the new product session and causes a different-ID re-hello on the same connection. | The two entry forms share the caller kit but never coexist for one session; `/clear` ends the old transient peer identity instead of creating an `inactive` side state. |
| Deliver | While a run is active, write the same user frame and report `injected`; while idle, use the bounded wrapper FIFO and report `queued_for_next_turn`. | Active stream injection is proven by `lane.go:224-249`. The c5 idle `SendMessage` also writes a user frame (`lane.go:251-274`) and would start unrequested work, so it cannot be called idle under the universal contract. |
| Interrupt and close | Interrupt writes the native `control_request` subtype `interrupt`; close ends the stream and reaps the exact child. | `lane.go:277-321` proves both operations and their native acknowledgements. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded delivery FIFO. | Every open value has a native process flag or stream mapping; the delivery compromise remains isolated behind `deliver` and the daemon contains no Claude branch. |
| Size cap | **360 production / 400 test logical lines**, including stream framing and Claude argument translation but excluding the shared wrapper host. | The current 578-line actor combines generic lifecycle with product translation; the generic kit removes that duplication. |
| Deletion inventory | Delete all `internal/products/claude` (3 files / 1,108 lines), `internal/launcher/{claude_peer.go,claude_peer_test.go}` (2 / 183), `internal/bridge/claude_title*.go` (2 / 99), and the orphan `internal/bridge/claude_sdk_socket_*.go` family (4 / 178). Rewrite `claude/.mcp.json`; retain product docs/skills. Total: **11 files / 1,568 lines**. | The wrapper owns title and stream/socket integration directly; the daemon driver, bridge title observer, orphan SDK socket split, and old launcher disappear. No compatibility adapter remains. |

### 4.3 Codex

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `codex-peer` with a launch token owns one session-specific App Server client/subscription. Without a token, `codex-peer` launches interactive Codex; Codex's product-spawned Agent Sessions stdio MCP server owns the peer connection. | The lane surface is already App Server RPC (`internal/bridge/codex_native.go`). Peer presence does not require the lane wrapper or a host-global coordinator. |
| Open and resume | Fresh performs `thread/start`, returns the product thread ID, applies the composed name through `thread/name/set`, and materializes its rollout; resume uses `resume_session_id` and reapplies the stored open object. It supports all five open fields. Permission mapping is exact: default means approval `never` with the configured sandbox, while bypass means approval `never` plus `danger-full-access`. | `CodexStartRequest` and `CodexLaneTurnRequest` at `codex_native.go:51-70` expose cwd, model, reasoning effort, permissions, and arguments. The wrapper preserves the product-owned thread ID and name instead of inventing daemon aliases. |
| Run | Send one `turn/start`, await the matching `turn/completed`, and extract the final agent message. | `internal/products/codex/lane.go:75-122` and `codex_native.go:528-656` prove the end-to-end primitive. |
| Tools | In lane mode the wrapper supplies the private `codex-peer mcp` Unix endpoint in that lane's App Server `thread/start` `mcp_servers` configuration. In peer mode Codex starts the same stdio MCP entry, which defers its direct peer hello until `_meta.threadId` arrives. After Codex `/clear`, the first tool call carries the new thread ID and product title and causes a different-ID re-hello on that same connection. | MCP configuration is per thread; `/clear` removes the old transient identity and installs the new one without `inactive`, reconnect, or host-global coordination. |
| Deliver | Active thread uses `turn/steer` and reports `injected`; idle delivery enters the bounded wrapper FIFO instead of invoking c5's `turn/start`. | `codex_native.go:479-527` explicitly distinguishes active steer from idle start; universal delivery forbids the latter. |
| Interrupt and close | Interrupt calls `turn/interrupt` for the exact active turn. Close archives the thread and unsubscribes before the wrapper exits. | `internal/products/codex/lane.go:124-158` and `codex_native.go:758-780` are the existing native boundaries. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded delivery FIFO. | App Server exposes every typed open value; idle delivery is the only missing primitive and stays wrapper-local. |
| Size cap | **700 production / 700 test logical lines**, including App Server framing and session code but excluding the shared wrapper host. | Native protocol code must be counted with the product that requires it; host-global coordination is forbidden. |
| Deletion inventory | Delete all `internal/products/codex` (2 files / 331 lines) and `internal/launcher/{codex_peer.go,codex_peer_test.go}` (2 / 786). Total: **4 files / 1,117 lines**. | The App Server lane primitive is rehomed into wrapper-owned code; the daemon driver dies and the peer exec plan moves into thin `cmd` composition. |

### 4.4 Grok

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `grok-peer` with a launch token owns one private leader, one authenticated ACP primary, and one observer for the exact lane session. Without a token, `grok-peer` execs `grok --leader --session-id <uuid>`; it does not create a private leader. Grok spawns our stdio MCP server, which owns the peer connection and reaches the product through Grok's default leader socket. A hand-started Grok with no launcher identity never hellos. | Grok Build 1.0.13 exposes the ACP agent through `grok agent stdio` / `leader` and acts as an MCP client. The default leader socket is a product-internal hop, not another bus connection. |
| Open and resume | The resident wrapper receives `session.open` before it starts the private leader. Fresh mints a UUID, starts Grok with `--session-id <id>`, applies the composed title through the observer rename primitive, attaches with ACP `session/load`, and returns that product ID; resume starts with `--resume <resume_session_id>` and loads the same ID. It never calls `session/new`. It puts `--permission-mode`, `--reasoning-effort`, `-m`, and ordered `arguments` on that command line, with `cwd` as the child working directory. | Grok rejects a fresh `--session-id` that already exists, preserving product-owned uniqueness. ACP `_meta` exposes only `yoloMode` / `autoMode`; it is not an open-field transport. All five fields and the title are applied after open. The existing 15-second startup hold remains inside `spawnTransactionTimeout = 60s`. |
| Run | Call ACP `session/prompt`, consume matching update notifications, and return its stop reason and accumulated output. | `grok_native_session.go:270-308` is the resident prompt primitive. |
| Tools | In lane mode the wrapper publishes a private endpoint and `grok/scripts/native-entry` is a local stdio MCP relay to it. In peer mode Grok, an MCP client, spawns the installed stdio MCP entry; that entry owns the direct peer connection and delivers through the default leader socket. Without a live leader, delivery is rejected as `no_leader`. | ACP is the wrapper-to-Grok control protocol; MCP is the product-facing Agent Sessions tool boundary. The two must not be conflated. |
| Deliver | While running, observer interjection reports `injected` only after the actor acknowledges it. While idle, Grok acknowledges retention on its native held-prompt queue and the wrapper reports `queued_for_next_turn`. | Both dispositions describe acknowledged product-owned behavior; the wrapper adds no FIFO and the daemon learns no Grok condition. |
| Interrupt and close | Interrupt sends one ACP `session/cancel` notification; `{}` means the notification was sent, not that the run has stopped. Close sends cancel and awaits a terminal for at most 6 seconds, then closes observer, primary, and leader concurrently for at most 3 seconds; at 9 seconds it kills the wrapper-owned process group. | The product-specific 6+3-second schedule fits inside the daemon's single 10-second `closeBound`, leaves one second for wrapper EOF/reap, and cannot create another daemon clock. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only queue: **none**; Grok's idle prompt queue is native state. | Grok exposes running interjection, native idle queuing, and all typed open controls; no limitation leaks outward. |
| Size cap | **750 production / 700 test logical lines**, including ACP framing and leader bootstrap but excluding the shared wrapper host. | Grok's private leader is product-specific and must not escape its ledger or recreate daemon attachment/generation state. |
| Deletion inventory | Delete all `internal/products/grok` (2 files / 637 lines), `internal/launcher/{grok_peer.go,grok_peer_test.go}` (2 / 1,183), `cmd/agent-sessions/grok_peer.go` (1 / 213), and all 11 `internal/bridge/grok*.go` files (1,883). `internal/launcher/lane_grok_test.go` is counted once in the shared row. Rewrite `grok/.mcp.json` and `grok/scripts/native-entry` as dual-entry peer/direct or lane/local assets. Total: **16 files / 3,916 lines**. | The wrapper receives copied, product-owned ACP/leader/observer slices rather than retaining a cross-product bridge package; the thin peer exec plan is rehomed without wrapping the TUI. |

### 4.5 Qwen Code

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `qwen-peer` with a launch token owns one `qwen --acp` child and one ACP client for the lane session. Without a token, `qwen-peer` mints a Qwen-compatible v4 UUID, passes it to Qwen as both `--session-id` and `AGENTBUS_SESSION_ID`, then execs interactive Qwen; its spawned stdio MCP server owns the peer connection. | `internal/products/qwen/lane.go:102-181` proves the headless ACP lifetime. A hand-started peer lacking this identity returns the launcher error and never hellos; obsolete file-observer presence dies. |
| Open and resume | Initialize ACP v1, mint a v4 ID for fresh open and pass it in `_meta["qwen-code/sessionId"]` to `session/new`, or use capability-checked `session/resume` with `resume_session_id`; verify and return the exact product ID, then rename fresh sessions to the composed title. Supported open fields are `cwd`, `permission_mode`, `model`, and `arguments`; model maps to `-m`. Default permission uses Qwen's ordinary mode; bypass adds `--yolo` and verifies the returned mode. Arguments may not claim `--acp`, approval/yolo, resume/continue/session-id, prompt/input/output, or name controls. | Qwen Code 0.23.0 accepts the session ID metadata, resume, and `-m`; it exposes no reasoning-effort flag or ACP field. The wrapper mints only because Qwen requires the caller-provided UUID and fails closed on reserved controls. |
| Run | Start `session/prompt`, accumulate session updates, and resolve the matching future to a terminal result. | `lane.go:199-263` and `client.go` prove the one ACP request/future. |
| Tools | In lane mode ACP `mcpServers` starts `qwen-peer mcp` against the wrapper's private Unix endpoint. In peer mode Qwen starts the same stdio MCP entry, which owns the direct peer connection. | c5 already injects an MCP server during `session/new` (`lane.go:134`); lane mode changes only its destination, while peer mode preserves the product-spawned connector pattern. |
| Deliver | Idle delivery enters the shared bounded FIFO and is prepended to the next run. During a run, `craft/drainMidTurnQueue` hands queued entries to Qwen and an acknowledged drain reports `injected`. At terminal, every undrained entry—including aborts and runs with no tool round—moves back to the shared FIFO for the next run. | Qwen Code 0.23.0 owns this native mid-turn drain: each call is bounded at 2 seconds, three strikes disable it, `-32601` disables it immediately, and a 30-second late-drain recovery preserves entries. No delivery is dropped between the native queue and wrapper FIFO. |
| Interrupt and close | Interrupt calls `craft/cancelPendingPrompt`; close cancels the ACP lifetime and reaps the child. | `lane.go:265-310` proves both calls. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open field: `reasoning_effort`. Wrapper-only state: the bounded idle and recovery FIFO; native mid-turn drain state remains Qwen-owned. | Product help exposes model but no effort selector; permission vocabulary and reserved arguments stay wrapper data, and no Qwen condition enters the daemon. |
| Size cap | **520 production / 600 test logical lines**, including ACP framing but excluding the shared wrapper host. | The current driver/client split contains generic actor state that disappears; all retained Qwen protocol code remains charged here. |
| Deletion inventory | Delete all `internal/products/qwen` (4 files / 1,031 lines), `internal/launcher/{qwen_peer.go,qwen_peer_test.go,qwen_test_helpers_test.go}` (3 / 1,412), `cmd/agent-sessions/{qwen_peer.go,qwen_peer_test.go}` (2 / 234), and the obsolete 11-line `qwen/scripts/native-entry`; replace it with the installed dual-entry MCP image and rewrite `qwen/mcp.json`. Total: **10 files / 2,688 lines**. | ACP becomes lane-wrapper-owned; event-file identity and old peer launcher state die, while a thin peer exec plan and product-spawned MCP entry replace them. |

### 4.6 OpenCode and Kilo

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Worker entry | Token-selected `opencode-peer` or `kilo-peer` is only a boot shim for `<product> serve --hostname 127.0.0.1 --port 0`; the in-process JavaScript plugin links the native worker kit and owns the one daemon connection. Without a token the entry launches the interactive product and the same plugin runs peer mode. The shim remains only until upstream product boot checks the token directly. | The product SDK already owns session creation, prompt, events, abort, title, tools, and directory. Keeping the universal lifecycle in the plugin makes this the DSH-native shape rather than a Go HTTP adapter. |
| Open and resume | After a v2 SDK capability probe and app-ready, create or fetch the exact product session, return its ID, apply the composed title and permission rules, and retain model/agent/variant defaults. Both products support all five open fields; ordered arguments allow only the documented `--agent` selector. | `opencodefamily/lane.go:69-187` proves the product primitives. The deployed pdev plugin/SDK is 1.2.10 while the CLI is 1.18.28, so the v2 probe must succeed before hello rather than trusting the CLI version. |
| Run | Call `session.promptAsync`, follow the exact event stream, then fetch the matching assistant result. | `lane.go:168-337` and `client.go` contain the existing bounded HTTP/SSE primitive. |
| Tools | The plugin's peer and worker modes use the same JS caller/worker kit and register the same product tool; there is no lane-local bridge endpoint. | The current plugins already own SDK tool registration in `integrations/{opencode,kilo}/agent-sessions.mjs`; the token changes hello mode, not transport shape. |
| Deliver | Without a session-level append or active injection primitive, the plugin reports `queued_for_next_turn` using product-native pending input if available; otherwise this is the named upstream blocker and the worker cell cannot pass. | Starting `promptAsync` would create unrequested work, while the old Go driver's unsupported steer is not a native contract. The conformance probe, not adapter history, decides readiness. |
| Interrupt and close | Interrupt calls the SDK abort endpoint and cancels event wait; close disposes the exact product session and lets the kit close the socket. | The plugin owns both session and connection, so no private server supervisor state enters the bus. |
| Exception ledger | Section 1 code exceptions: **0** for both products. Declared unsupported open fields: **none**. Product lifecycle exceptions: **0** once the v2 SDK and delivery probes pass. | Dialect differences remain product SDK data; they never select a wire method or daemon branch. |
| Size cap | Shared native plugin **750 production / 700 test**, plus **60 / 80** per boot-shim/dialect leaf, excluding the shared JS kit. | OpenCode and Kilo differ only in SDK dialect, permission mapping, and packaged entrypoint; product transport remains charged to this family. |
| Deletion inventory | Delete all `internal/products/opencodefamily` (10 files / 2,776 lines), `internal/products/opencode` (2 / 93), and `internal/products/kilocode` (2 / 98): **14 files / 2,967 lines**. Rewrite, but do not duplicate or delete, the two retained integration packages and tests. | Server pooling, daemon driver maps, doctor probes, and Go leaf drivers are replaced by one native JS plugin family plus two boot shims. |

### 4.7 Pi

| Ledger item | Decision | Source-backed reason |
| --- | --- | --- |
| Resident wrapper | `pi-peer` with a launch token owns one exact `pi --extension <managed-plugin> --mode rpc ...` JSONL child for product `pi`. Without a token, `pi-peer` launches interactive Pi, whose JavaScript extension owns the direct peer connection through the JS kit. | `internal/products/pifamily/lane.go:119-164` is the proven lane RPC launch; interactive Pi already owns the extension lifecycle. |
| Open and resume | Fresh mints a Pi-compatible ID, passes `--session-id <id>` and the composed product title, then returns the product state ID; resume passes `--session <resume_session_id>` and verifies the returned state. Pi supports all five open fields: model maps to `--model` and reasoning effort to independent `--thinking` before child spawn. | `pifamily/lane.go:76-173` and `quirks.go:113-134` prove the transaction; Pi product help exposes the exact session, model, and thinking flags. |
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
| Resident wrapper | `omp-peer` with a launch token owns the exact `omp --extension=<managed-plugin> --mode=rpc ...` JSONL child for product `omp`; resume is spelled `--resume=<id>`. Without a token, `omp-peer` launches interactive OMP, whose JavaScript plugin owns the direct peer connection through the JS kit. | `internal/products/pifamily/quirks.go` and `rpc_lane_test.go:388-437` prove the equals-style lane dialect; interactive OMP already supervises its plugin. |
| Open and resume | Create or resume the exact OMP session, call `set_session_name` at fresh open so the product title equals the composed bus name, and apply mapped permissions. OMP supports all five typed open fields: cwd maps to `--cwd=`, model to `--model=`, and reasoning effort to `--thinking`. Its three documented extra arguments are `--tools`, `--exclude-tools`, and `--approval-mode`; conflicts with typed permission fail before spawn. | OMP product help exposes the typed flags, while its ready/RPC surface proves the title call. The default permission path fails closed when RPC approval mediation is unavailable; bypass maps explicitly to the product's noninteractive mode. |
| Run | Send RPC `prompt`, accept OMP's declared terminal event, and read final assistant text through the family implementation. | `pifamily/rpc.go` contains the closed event decoder; OMP selects the terminal quirk rather than a second lifecycle. |
| Tools | The retained OMP entrypoint loads the Pi-family plugin in peer/direct or lane/local mode and registers the same caller tool. | `integrations/omp/agent-sessions.mjs` is already a three-line family entrypoint; the shared plugin owns mode selection. |
| Deliver | Running uses RPC `steer` and reports `injected`; lane-idle delivery uses the bounded wrapper FIFO. In peer mode, the product's native `nextTurn` queue retains the message and may report `queued_for_next_turn`. | OMP's native steer does its own framing (`pifamily/lane.go:301-326`); the interactive plugin's next-turn queue is product state, not wrapper or daemon state. |
| Interrupt and close | RPC `abort` and exact process cleanup are identical to Pi. | No OMP-specific lifecycle callback is justified. |
| Exception ledger | Section 1 code exceptions: **0**. Declared unsupported open fields: **none**. Wrapper-only state: the bounded idle-delivery FIFO. | OMP exposes every open value as a process flag; its dialect remains launch/result data only and never reaches the daemon or wire. |
| Size cap | OMP leaf **60 production / 100 test** in addition to the shared Pi-family cap. | The leaf may declare quirks and permissions only. |
| Deletion inventory | Delete all `internal/products/omp` (3 files / 135 lines); shared `pifamily` deletion is counted under Pi. Rewrite the retained OMP entrypoint/test/manifest around the local wrapper plugin. Total: **3 files / 135 lines**. | OMP becomes one immutable family-wrapper registration, not a driver package. |

### 4.9 Wrapper migration ledger

| Ledger | Files | Deleted lines | Decision |
| --- | ---: | ---: | --- |
| Product-specific rows in Sections 4.2-4.8 | 69 | 14,743 | Delete every non-native daemon driver, bridge-owned product slice, and product-specific depart-on-exec launcher. |
| Shared wrapper boundary in Section 4.1 | 18 | 2,532 | Delete the remaining launcher package and connector self-refresh; retain only rewritten CLI/wrapper entrypoints. |
| Section 4 subtotal | **87** | **17,275** | Replacement wrapper and test lines are reported separately against the stated caps. |
| Cumulative Sections 2-4 floor | **150** | **31,042** | This is the minimum physical deletion from c5b280d before Section 5 migration/conformance cleanup. |

## 5. Migration and conformance

### 5.1 State boundary

The universal daemon reads only a new, initially empty table file; it never
reads, migrates, backs up, or deletes the c5 catalog file, which remains wholly
owner-controlled.

### 5.2 Ordered implementation and runtime proof

| Phase | Source deliverable | Gate | Runtime rule |
| ---: | --- | --- | --- |
| 0 | Signed document, `bus/internal/protocol` schema and shared fixtures re-exported by the public SDKs, generated protocol, architecture boundaries, and deletion ledger. | Schema fixtures pass in Go and JavaScript; method/error tables are byte-identical; deletion counts reproduce from c5b280d; `bus/` has no wrapper import or product token. | No installed daemon or product runtime. |
| 1 | `bus/sdk/go` worker kit, universal daemon, durable lane table, Go caller kit, reference caller, and token-selected `bus/cmd/example-peer` reference worker. | Daemon caps hold; unit/race/vet/build green; an in-process restart with durable rows proves every row loads offline with empty maps/reservations and no spawn; old actor/driver/control packages are absent; contract learned nothing from adapters: **yes**. | Installed-daemon integration runs only on `umka-dev1`, against an empty universal table. |
| 2 | JavaScript worker kit, JavaScript caller kit, then the unified DSH plugin/profile. | All 17 shared lifecycle fixtures pass in both worker kits; DSH passes both cells in Section 5.5. | DSH installed-product proof only on `umka-dev1`; no other product is enabled. |
| 3 | `wrappers/` products in order: Claude, Codex, Grok, Qwen, OpenCode, Kilo, Pi, OMP; each product lands lane mode first and its shared-MCP entry second. | Each product meets its size/exception ledger and passes its two conformance cells before the next product is enabled. | Product runtime proof only on `umka-dev1`; failures do not enable a compatibility path. |
| 4 | CLI rendering, package projections, install/remove inventory, documentation, and federation. | Full unit/race/vet/build/package gates; federation assertions follow Section 5.7; all 18 cells pass in one clean candidate run. | The sole full runtime matrix runs on `umka-dev1`; no install elsewhere. |
| 5 | Release candidate. | Universal state starts empty; no old protocol endpoint, actor, driver, launcher process, socket, or compatibility package remains; the c5 catalog file's SHA-256 is recorded before and after the install and full run on `umka-dev1` and must be identical. | Production installation requires owner authorization after the clean `umka-dev1` evidence is sealed. |

Source compilation and unit/race tests, including in-process daemons and workers
on temporary sockets, may run in any isolated clone. Installing or running the
daemon, a wrapper, a product, a TUI, or a conformance cell as a service or
against installed products is permitted only on `umka-dev1`.

### 5.3 Reference sides

| Reference | Closed behavior |
| --- | --- |
| Reference worker | PATH-resolved `example-peer` starts with the one-use launch token in its environment and empty argv. Its hello declares all five open fields. Ordered `open.arguments` entries are `key=value`; `session_id=<id>` selects the returned product session ID and its absence makes the worker mint one. A plain turn input is echoed; `block` waits only for the run cancellation and returns `interrupted`; `call <method> <params-json>` performs that worker-originated session method and returns the response JSON; `fail <code>` returns that error. An idle delivery is `queued_for_next_turn` and prepended to the next input; a delivery during a run is `injected` and appended to the echoed result. The close callback returns immediately. The worker has no configuration, clock, or product import. |
| Reference caller | `bus/cmd/agentbus-call [-name <name>] [-g a,b] [-socket <path>] <method> [<params-json>]` opens one peer connection, sends exactly one raw client-to-daemon request, prints its result or error object on stdout, and exits 0 for a result, 1 for an error, or 2 for usage. `turn.run` waits for its terminal; deliveries during the call are JSON lines on stderr and receive `injected`. Separate invocations supply a second peer or abandon a reply sink. The binary has no configuration file, stdin protocol, product condition, or local turn registry. |
| Worker invocation | The worker suite accepts only a product token and invokes that exact PATH binary with empty argv and a launch token in its environment; every vendor and `example-peer` run the identical trace. |
| Caller invocation | The caller suite invokes the product's installed Agent Sessions tool, not a private test API; every product runs the identical trace against the reference worker. |

The caller/reference size contract is final logical lines:

| Reference surface | Production | Tests |
| --- | ---: | ---: |
| Go caller kit | 250 | 400 |
| Go no-hello client (`client.go`) | 30 | — |
| JavaScript caller kit, additional to the 200-line JavaScript client/worker cap | 150 | 150 |
| Reference caller | 200 | 200 |
| Reference worker | 250 | 150 |

A caller kit over its cap has become a second router and is a design finding,
not a reason to move lines into a product integration.

### 5.4 Vendor acceptance traces

| Trace | Caller against reference worker |
| --- | --- |
| C1 | Two peer connections prove exact session identity, visibility-filtered list, and the full tool schema; the second visible peer can run, interrupt, and close the lane. |
| C2 | Describe without open or residue; two-level spawn composes matching name/private-group paths, gives each lane only its parent and own private groups plus `extra_groups`, and proves explicit ancestor-group widening; all declared open fields and ordered arguments survive. Discovery advertises configured product names without gating an unlisted executable and a listed-but-missing name returns `unknown_product`; explicit `host` naming the other daemon in a two-daemon fixture describes and creates the row only there, while an unfederated host returns `unknown_host`. |
| C3 | Local-kit start returns a local ID while one wire `turn.run` remains outstanding; status is running and bounded wait timeout does not cancel it. |
| C4 | Wait returns the terminal, including exact result truncation metadata; abandoning the caller sink makes the kit report `result unavailable, lane resumable`, while the daemon drains and persists nothing. |
| C5 | Send resolves ID/name/group, deduplicates, and returns dispositions `injected`, `queued_for_next_turn`, or `rejected`, including exact rejected reasons `ambiguous` and `no_receipt`; an invisible peer receives `unknown_session`. |
| C6 | Concurrent interrupts coalesce to one worker interrupt and idle interrupt maps `not_running`. |
| C7 | Explicit close during a run orders a terminal observed within the bound before the close response; the one 10-second `closeBound` covers request through reap, and expiry KILL invents no terminal or second wait. The row remains resumable unless `forget:true`. |
| C8 | Peer EOF reconnects; a new connection with the same canonical ID supersedes the displaced identity terminally. Same-ID re-hello refreshes name/info with fixed groups. Different-ID re-hello atomically detaches the old reply sinks, fails its pending deliveries once, removes its private group, and installs the new identity/group before acknowledgement; requests admitted before the switch retain the old source. |
| C9 | The caller drives the remote row by canonical ID; offline resume replays the stored open value unchanged with argument order preserved; `name_taken`, `already_connected`, `not_connected`, forgotten-row `unknown_session`, disconnected-host `unknown_host`, and ambiguous one-hop loss `forward_lost` match Section 1.4; cleanup leaves no connection, process, token, or pending call. |

| Trace | Worker against reference caller |
| --- | --- |
| W1 | Describe launch reads the endpoint, scrubs the token and optional local key, sends one valid hello after app-ready, never opens or closes natively, and exits on EOF without reconnect. |
| W2 | Fresh and resumed open return the exact product session ID; resume mismatch, unsupported field or value, typed-field/argument conflict, duplicate session ID, exit, and timeout fail truthfully. |
| W3 | Worker-originated client-to-daemon session methods are `not_committed` before open commit; after a successful open response, already-read later frames are withheld from dispatch until the durable commit and then succeed without a kit-side wait. Commit failure closes the provisional connection. |
| W4 | Completed, interrupted, failed, empty-output, and over-limit native results produce the exact terminal and `truncated` shapes; a second run is busy. |
| W5 | Reader remains full-duplex: delivery and a worker-originated session method complete while run is blocked; delivery receipts are exact. |
| W6 | Concurrent interrupt and close invoke one native interrupt; terminal-before-interrupt invokes none. |
| W7 | Close claims the slot before product code, rejects later delivery as closing, writes the one run response from its run owner, and completes interrupt/terminal/close-response/socket-close/`closed` ordering within the daemon's one 10-second `closeBound`; an unresponsive callback is killed at that bound. |
| W8 | Control EOF cancels every callback, calls native close once, fails pending calls once, and exits; later explicit resume creates one new worker process. |
| W9 | Malformed, unknown, oversized-frame, overlength-payload, duplicate-member, and out-of-range-ID inputs follow Section 1.1; product callbacks never see rejected frames and cleanup leaves zero residue. |

### 5.5 Eighteen-cell matrix

| Product | Caller cell against token-selected `example-peer` | Worker cell against reference caller |
| --- | --- | --- |
| DSH | `C-DSH`: C1-C9 through the unified DSH plugin's registered tool. | `W-DSH`: W1-W9 against token-selected `dashi`. |
| Claude | `C-Claude`: C1-C9 through the product-spawned peer MCP entry. | `W-Claude`: W1-W9 against token-selected `claude-peer`. |
| Codex | `C-Codex`: C1-C9 through the product-spawned peer MCP entry. | `W-Codex`: W1-W9 against token-selected `codex-peer`. |
| Grok | `C-Grok`: C1-C9 through the product-spawned peer MCP entry. | `W-Grok`: W1-W9 against token-selected `grok-peer`. |
| Qwen | `C-Qwen`: C1-C9 through the product-spawned peer MCP entry. | `W-Qwen`: W1-W9 against token-selected `qwen-peer`. |
| OpenCode | `C-OpenCode`: C1-C9 through the JavaScript plugin in peer mode. | `W-OpenCode`: W1-W9 against token-selected `opencode-peer`. |
| Kilo | `C-Kilo`: C1-C9 through the JavaScript plugin in peer mode. | `W-Kilo`: W1-W9 against token-selected `kilo-peer`. |
| Pi | `C-Pi`: C1-C9 through the Pi-family plugin in peer mode. | `W-Pi`: W1-W9 against token-selected `pi-peer`. |
| OMP | `C-OMP`: C1-C9 through the Pi-family plugin in peer mode. | `W-OMP`: W1-W9 against token-selected `omp-peer`. |

All eighteen cells are required. A failure may change only the failing product
wrapper/plugin or a genuinely universal kit defect reproduced by token-selected
`example-peer`; it may not add a product branch, capability exception, or
alternate daemon path. Every cell records **contract learned nothing from the
adapter: yes/no**, and any `no` fails the candidate.

### 5.6 Upstream native-worker feasibility

The target for OpenCode, Kilo, Pi, and OMP is a plugin-native worker inside the
product's own headless start, with no Go wrapper. The deciding current product
facts are:

| Product | Session-level cwd/model fact |
| --- | --- |
| OpenCode | **Present:** its plugin SDK accepts `directory` on session operations and a model object on prompt operations, so both values can arrive after the process and plugin start. |
| Kilo | **Present:** its plugin SDK accepts `directory` on session operations and carries the active prompt model through its session APIs, so process start need not fix either value. |
| Pi | **Absent today:** the extension context exposes the process cwd and session manager, but not the proposed upstream `session.configure({cwd,model})` primitive; both values remain process flags. |
| OMP | **Absent today:** the shared extension context likewise lacks the proposed upstream `session.configure({cwd,model})` primitive; both values remain process flags. |

These facts affect only when a vendor can replace its wrapper with the native
kit. They do not change Section 1, add daemon capabilities, or excuse a wrapper
from the same conformance matrix.

Before its conformance cells, each product has a named `umka-dev1` probe:

Every product probe also kills a running wrapper or native worker abruptly and
attempts immediate resume. Resume must fail as `spawn_failed` with `session
busy` until the still-running native writer exits, then succeed. This proves
cross-process native-session exclusivity rather than an in-memory duplicate
check.

| Product | Required installed-product probe |
| --- | --- |
| DSH | Profile composition, global Cordis patch/package resolution, app-ready timing, one connection per root, title/append/steer/terminal reason, and bounded supervisor exposure around `whenIdle`. |
| Claude | `system/init` timing relative to the first stream input, `--mcp-config` precedence, session-ID/title flags, permission `dontAsk`, active injection, private Unix MCP, and `/clear` followed by same-socket different-ID re-hello with only the new titled peer visible. |
| Codex | Deferred `_meta.threadId` peer identity, per-thread `mcp_servers.<id>.url`, thread naming, approval/sandbox mappings, steer, interrupt, resume, and `/clear` followed by first-call different-ID re-hello with only the new titled peer visible and no `inactive` state. |
| Grok | Private leader startup/load without `session/new`, observer rename/interjection/held queue acknowledgements, default-leader peer delivery, and the 6+3-second close schedule. |
| Qwen | ACP resume and `_meta["qwen-code/sessionId"]`, rename, permission vocabulary, `craft/drainMidTurnQueue` including undrained recovery and its 2-second/three-strike/`-32601`/30-second bounds, and private Unix MCP. |
| OpenCode | v2 SDK directory/model/title/prompt/abort/delivery primitives before hello, plus the installed SDK/CLI version pair. |
| Kilo | The same v2 SDK probe as OpenCode; runtime support remains explicitly unproven until Kilo is installed on `umka-dev1`. |
| Pi | Exact `pi --extension <path> --mode rpc` launch, product ID, fresh/resume ID, title, model/thinking flags, steer, abort, and terminal event. |
| OMP | Exact equals-style child invocation including `--resume=`, product ID, `set_session_name`, fail-closed permission behavior, three extra arguments, native `nextTurn` peer queue, steer, abort, and terminal event. |

### 5.7 Federation gate

| Gate | Assertion |
| --- | --- |
| Identity | Every local and remote summary emits the same canonical `id@host` and `name@host`; no separate host field or receiving-side relabeling exists. A standalone daemon uses `local`, and a federated daemon requires a configured non-`local` unique host name. |
| Visibility | The authoritative daemon filters by groups; the receiving daemon trusts that assertion and never persists a remote row. |
| Messaging | One canonical remote message is forwarded once, produces one receipt, and is never retried or duplicated after federation reconnect. Bare input selects the caller's own host; qualified input is split only at the last `@`. |
| Control and creation | The capped function forwards canonical remote resume/run/interrupt/close and explicit-host spawn/describe exactly one hop as `{from:{session_id:id@host,groups},request}`; the TLS connection identifies the origin and the JSON-RPC ID correlates it. The target applies ordinary visibility, caller loss removes only the reply sink, and transport loss returns `forward_lost` without retry. |
| Federation authentication | `agentbus secret` produces 32 random bytes in base64; either side rejects a decoded secret shorter than 32 bytes. A correct host/secret pair federates; a wrong secret, unknown or reserved `local` name, duplicate host, or name/key mismatch fails the TLS handshake. Changing one side's secret disconnects that host until both configurations match; no separate expiry or revocation exists, and secret-bearing config is mode 0600. |
| Optional local encryption | A keyed daemon rejects a keyless client; a keyed client fails truthfully against a plain daemon; matching keys connect; and a spawned lane receives and scrubs both `AGENTBUS_LAUNCH_TOKEN` and `AGENTBUS_LOCAL_KEY`. |
| Reconnect | A per-host snapshot replaces its predecessor atomically and is followed by ordered live updates. `maxPendingForwardedPerHost = 256` is the sole flow cap and the existing 1 MiB frame limit bounds snapshots; overflow disconnects that host. Disconnect removes its entire transient contribution and fails pending one-hop calls once as `forward_lost`; reconnect publishes a fresh snapshot and never replays a request. |
| Refused federation machinery | The hub has no replay, durable rows, distributed locks, automatic placement, multi-hop routing, or capability negotiation that gates PATH launch. |

### 5.8 Test rehoming

| Deleted test family | Universal owner |
| --- | --- |
| Daemon lane actors, registries, projections, collectors, archives, timers, product dispatch, and argv reparse | Router/table tests drive the eleven methods over a real connection and assert only rows, current pointers, pending calls, and supervisor ownership. |
| Presence, messaging, federation, roster, names, and notices | Daemon visibility/resolution tests plus the federation gate; no test constructs a private actor or product driver. |
| Product lane drivers and peer launchers | Each Section 4 wrapper test drives its six callbacks and exact native transcript; the shared wrapper-host unit suite proves the FIFO cap of 64 deliveries / 1 MiB rendered bytes, overflow `queue_full`, stale lock files do not block, a live inherited flock survives wrapper death until the child exits, interrupt at native-turn creation, first-turn/terminal delivery handoff races, and child death with a non-empty FIFO invents no receipt while leaving the row resumable; peer exec-plan tests stop at product config and never claim socket ownership. |
| Go/JavaScript lifecycle duplication | The one 17-row fixture table runs unchanged through both native kits and the reference worker. |
| Connector and plugin tool tests | Caller-kit conformance C1-C9 through the installed peer MCP/plugin entry, with product-private transport tested only at its local boundary. |
| Packaging and release projections | Package tests assert one schema/kit projection, correct peer and lane entry forms, no deleted compatibility artifact, and byte-identical installed assets. |
| Protocol and design documentation | Generate `bus/docs/PROTOCOL.md` from Sections 1 and 3.1 verbatim, with this document as the sole source and `bus/internal/protocol` as the sole embedded schema/fixture authority re-exported by the public SDKs. Delete every superseded lane-convergence, presence-supersession, adapter-boundary, and DSH-adapter note under `docs/designs`; do not retain archived or paraphrased protocol authorities. |

Rehoming is mandatory proof, not permission to preserve a deleted abstraction
under a new test helper. Every deleted test is named in the implementation
ledger beside its replacement row.
