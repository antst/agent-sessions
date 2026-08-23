# Native carrier and product-adapter protocol

These notes describe behavior observed in the installed Claude Code 2.1.226 Linux executable. This
is not a public Anthropic protocol specification.

## Native registry and Agent Sessions discovery

Claude enumerates JSON files in `$CLAUDE_CONFIG_DIR/sessions` or `~/.claude/sessions`. A messageable
record has a live `pid` and a connectable `messagingSocketPath`. The bridge corroborates process-start
identity through `/proc/<pid>/stat` on Linux and the kernel process table on macOS; an observation
failure is distinct from proof that a process is stale.

The host agent publishes exactly one service record using this compatible
subset. Product adapters register their real sockets with the host agent;
remote peers are never projected into Claude's registry. Ordinary Claude,
`claude-peer`, and Claude lane rows coexist in the configured shared profile:

Managed interactive Claude launches use a per-session socket below the host
agent's private runtime directory. The gated launch journal records that exact
path before native startup. If Claude creates its socket and socket-bound key
but stops before publishing the PID row, retirement still requires the key
fingerprinted under the exact live adapter; unrelated shared-profile sockets
are never swept by PID or liveness alone.

```json
{
  "pid": 12345,
  "sessionId": "agent-host-key",
  "cwd": "/agent-state",
  "startedAt": 1786000000000,
  "procStart": "123456789",
  "version": "agent-sessions/0.2.4",
  "peerProtocol": 1,
  "kind": "service",
  "entrypoint": "agent-sessions",
  "name": "agent-sessions--workstation",
  "nameSource": "agent",
  "status": "idle",
  "messagingSocketPath": "/run/user/1000/codex-claude-peer-1000/session-0123456789abcdef0123.sock"
}
```

Claude's `agents --json` command is the independent smoke test: a running host
agent adds exactly one service row regardless of native session or remote peer count.

Agent Sessions discovery is an AgentFrame request to that service, not a scan
of the Claude registry. It returns only peers sharing a group with the caller.
A visible peer can be addressed by name, display name, host/session ID, or
exact session ID. Hidden duplicate names do not participate in resolution.

Codex, Grok, and Qwen adapters own stable per-session UDS paths. A wrapped Claude
attachment instead registers Claude's native PID-bound socket; that socket can
rotate across exact resume while the shared transcript, Agent Sessions session
ID, catalog row, and groups remain stable.
Every newly published stable endpoint is a real `0600` Unix socket, never a
symlink to a PID-named backend. This is required because native senders may
correctly reject symlink reply targets. Legacy aliases are stale-artifact input
only: reconciliation removes one only after proving the exact dead owned
backend and never advertises or recreates it.
The host agent routes to that socket only after group admission. Claude's
native sender addresses the service row and puts the complete AgentFrame JSON
in the message body.

`nameSource` is `codex` when the name comes from Codex's session index, `explicit` when supplied by
`CLAUDE_PEER_SESSION_NAME`, `launch` for a prepared interactive wrapper, `lane` for a durable lane,
`manual` after the MCP rename tool is used, and `generated` for the startup fallback.

## Late-bound native resume contract

Some native products do not expose the selected transcript UUID until after
their interactive resume UI has started. A wrapper must preserve that native
behavior (including title matching and duplicate-title pickers) without
mistaking its launch-scoped identity for the selected transcript.

Every late-bound adapter therefore has two deliberately different identities:

- an unpredictable **attachment ID**, created before launch and used only for
  lifecycle journaling, socket ownership, process capabilities, and crash
  cleanup; and
- the native **session ID**, adopted only after an authoritative product-owned
  surface proves the selected live actor.

The first row or handshake is not necessarily the selection. Claude can
publish a transient boot UUID and replace it several seconds later after
`--resume TITLE` resolves. Grok resolves the target inside its private leader
and exposes the selected resident UUID and `title` through
`_x.ai/sessions/list`. An adapter must wait for the product-specific selection
proof, then atomically move catalog preferences and live registration to the
native UUID. A transient UUID must never enter the durable catalog or become
model-visible authority.

The attachment remains a narrowly scoped alias for descendants that inherited
environment before selection. The host agent may resolve that alias only to
the one live registration that published it, and only while its exact
PID/process-start/socket/lifecycle evidence remains valid. MCP attestation,
local/remote lane ownership, terminal notices, and cleanup must all return and
use the native UUID after resolution. Unknown, stale, duplicate, or colliding
aliases fail closed. Session ID is not made advisory and PID alone never grants
authority.

Names follow the same rule. Unless the caller supplied an explicit peer-name
override, exact-UUID and title resumes publish the native title from the same
authoritative surface and refresh it after a native rename. Cwd-derived or
launch-derived placeholders may exist before selection but must not overwrite
the native title afterward.

New product adapters with late-bound resume must test at least: a transient
boot ID followed by the selected ID; exact UUID and title resume; duplicate
title chooser behavior; explicit-name precedence; MCP discovery/send from a
descendant that still carries the attachment ID; local and remote lane launch
from that descendant; agent restart; normal exit and crash cleanup; and proof
that no provisional catalog row, peer, socket, or credential/config mutation
survives.

## New-adapter acceptance checklist

The historical design for a vendor is research input, not authority. Before a
new adapter ships, its current installed native version and supported surfaces
must be re-probed, then the adapter must satisfy all of these classes on Linux
and macOS:

1. **Native selection** — fresh, exact-UUID, native name/title, ambiguous
   chooser, rename, cwd, archive/unarchive, and ordinary↔managed transcript
   movement retain the product's normal behavior. Wrapper launch IDs never
   become transcript IDs.
2. **Attestation** — the selected native ID, name, PID plus strong start,
   ancestry, profile, canonical cwd, real socket, capability, and host catalog
   agree before messaging or lane ownership. A descendant carrying a
   provisional attachment ID must resolve to the adopted native ID.
3. **Permissions** — record what the native product can prove. Do not invent a
   mode, silently widen it, or add policy duct tape. Native mode changes remain
   product-owned unless the product exposes a supported immutable contract.
4. **Messaging** — managed and bare sessions are distinguished; discovery,
   direct send/reply, atomic multicast, named-group broadcast, idle/busy
   delivery, deduplication, and agent restart work in both directions. Stable
   endpoints are real sockets and no sender-side symlink workaround is needed.
5. **Lifecycle** — normal exit, Ctrl+C, SIGTERM, wrapper/worker/manager crash,
   pre-publication failure, PID/path reuse, partial cleanup, and retry debt
   remove only exact owned processes and artifacts. Ordinary sessions and
   unrelated profile state survive.
6. **Lanes and federation** — all parent→target combinations, immediate-parent
   anchors, explicit group inheritance, notices, collection, interrupt,
   archive/resume, disconnected/unready negatives, and destination residue are
   covered locally and in both federation directions.
7. **Install and release** — exact selected-profile install/upgrade/remove,
   readiness doctor, no-secret/nonmutation proof, prebuilt no-Go install,
   authoritative inventory, and identical Linux/macOS release gates pass.

If a native surface cannot prove one of these properties, fail closed or state
the narrower supported contract. Do not copy a prior adapter's workaround merely
because its process shape looks similar.

## Qwen dual-output and input contract

Managed interactive Qwen uses protocol-v2 `--json-file` for admission and
activity plus `--input-file` for queued submits. A fresh wrapper supplies the
same preallocated UUID through native `--session-id`; resume is allowed only
after the Agent Sessions selector resolves to one exact managed UUID. The first
`system/session_start` must corroborate that UUID, canonical cwd, Qwen version,
protocol version, and required event inventory before publication.

The Qwen plugin/MCP is authorized by an unguessable launch capability, exact
process ancestry and strong start, selected presence-sensitive profile, real
socket, and host registration. Native permission mode remains Qwen-owned and
mutable; the adapter records the launch request and observed mode or `unknown`
without converting either into Agent Sessions authority.

## Transport

- Unix stream socket on Linux or macOS
- one JSON object per line
- maximum frame size: 1 MiB
- socket file mode: `0600`
- containing runtime directory mode: `0700`

The observed user frame shape is:

```json
{
  "msgV": 1,
  "msg_id": "uuid",
  "type": "user",
  "message": {
    "role": "user",
    "content": "<cross-session-message from=\"uds:/path/to/sender.sock\" from-session=\"sender-id\" from-name=\"sender\" from-mode=\"prompting\">\n[codex-peer-metadata: {\"fromProduct\":\"codex\",\"messageId\":\"uuid\",\"sentAt\":\"2026-08-09T15:00:00.000Z\"}]\nmessage text\n</cross-session-message>"
  },
  "priority": "next",
  "from": "uds:/path/to/sender.sock"
}
```

The bridge also accepts a minimal `type: "user"` frame whose `message.content` is plain text. It
deduplicates recent `msg_id` values and honors `session_id` when a sender includes one.

Claude 2.1.226 strictly parses the recognized envelope attributes in grammar order: `from`,
`from-session`, `hop-chain`, `from-name`, and `from-mode`. Native peers do not normally emit
`from-session`, but controlled testing confirmed that it is grammar-recognized in this position.
Unknown attributes before the closing `>` cause the inbound security gate to discard
permission-mode attestation and hold the message for human approval. The host
agent therefore leaves the native attribute grammar unchanged and carries the
complete AgentFrame JSON in the body. Its `delivery` frame contains the
attested source, product, permission class, message ID, group, and content.
The App Server and hook receive paths show message ID, send/receive times, and sender type in
model-visible metadata. The parser remains backward-compatible with the short-lived attribute form
used by earlier bridge builds.

Closing `</cross-session-message` strings inside a message body are escaped before transport and
restored by the receiver. Peer names and session identifiers are constrained before interpolation
into envelope attributes.

## App Server wake path

`codex-peer` starts Codex's managed App Server and a bridge supervisor. The supervisor connects to
the standard `/rpc` WebSocket endpoint on the managed Unix socket, initializes one JSON-RPC client,
and reconciles `thread/loaded/list` with `thread/read`.

Only roots with an exact live interactive-owner record or a durable unarchived Codex lane capability
are advertised. Merely appearing in App Server discovery, notifications, hooks, or a wake journal
cannot mint that capability. Subagents and ordinary roots remain internal. The supervisor subscribes
to authorized persisted roots with `thread/resume` while their lifecycle is live. Exact owner-process
reconciliation removes discovery and unloads the App Server runtime after an attached-owner exit. A
zero-turn prepared-owner exit removes discovery without archive/unarchive and retains its exact stale
owner record as a one-use takeover proof. Either durable thread remains resumable but is not advertised
or wakeable until another `codex-peer` attachment.

Subscription uses `excludeTurns: true` and remains metadata-only. Codex's paginated history is a
derived SQLite projection of canonical rollout JSONL. Codex 0.147 does not repair an existing
projection cursor gap through `thread/read`, `thread/resume`, or `thread/turns/list`; the installer
therefore prevents the known cause by refusing to replace any running App Server.

The supervisor reports the cache-busted plugin version on its private status endpoint. `codex-peer`
replaces it when the installed version changes; this activates shim/protocol updates without a
separate daemon-management command on the next ordinary launch or resume. The launcher also stores
the last App-Server-loaded plugin version under the bridge state directory. A host-shell version
change requires App Server to have already been stopped from a host shell. An in-turn or running-
server update exits 75 without stopping anything. The updater starts a clean process only after
rechecking the stopped state under its cross-launch lock. Unchanged launches use `daemon start`
idempotently and keep the shared server.

Supervisor sockets and persistent control/lane state are keyed by the canonical `CODEX_HOME`.
Startup also verifies that a live supervisor reports the expected App Server socket. A replacement
must receive a successful stop acknowledgement and release its socket before a successor may bind;
an unresponsive live supervisor is never unlinked from underneath. The namespacing migration
explicitly stops the prior global `supervisor.sock` only after verifying its implementation and
App Server identity, preventing an old and new supervisor from coexisting after upgrade.

A fresh `codex-peer` launch is created through the shared App Server using the caller's canonical cwd.
An explicit UUID resume selects that thread. A session-name resume selects the newest usable exact-name
match, following native Codex ordering, then requires that resolved UUID to be unarchived. The
bridge persists the wrapper PID/process-start token against it, then replaces that process with a
remote TUI. Remote Codex 0.147 delays SessionStart until the first user turn, so both fresh and resumed
roots are publishable in a distinct prepared-pending phase only while that exact wrapper identity is
live. A fresh root is delete-on-abort until publication commits; after commit, definite wrapper death
unpublishes it while preserving the still-loaded zero-turn transcript and its exact stale owner as a
one-use takeover proof. The replacement resume consumes that proof without archive/unarchive.
SessionStart promotes either kind to attached. If a prepared owner is still pending at the first
`UserPromptSubmit`, recovery additionally requires the hook's local rollout `session_meta` to bind
its session-family id to the exact prepared thread, and requires the live PID/process-start argv to
contain the managed `--remote unix:// resume <thread>` tuple. No cwd/time heuristic participates.
The native launcher consumes only its peer-name
option and explicit resume selector, resolves the selector to one UUID, then prefixes the unchanged
remaining Codex argv with the managed remote/resume target and a missing cwd. This preserves relative
option order and cannot splice into a variadic option's values. Explicit `--yolo` is additionally
applied through the shared App Server lifecycle: `thread/start` for fresh peers, and `thread/resume`
followed by `thread/settings/update` for resumed peers before publication. This explicit update is
needed because a second attachment does not persist the requested policy uniformly on every supported
platform. The real Codex attachment still receives the unchanged native option. The resulting approval policy determines the prepared publication
class. Because App Server thread settings are durable, a resumed `--yolo` thread remains full-access
on later plain resumes until another settings update changes it. Picker/`--last`, fork, foreign remote endpoints, and loaded targets without the exact stale
zero-turn proof remain unsupported. Codex 0.147 retains the original thread cwd during resume, so an
explicit different `-C` is rejected before owner publication.

The version-change path keeps the old peer supervisor intact until clean App Server startup
succeeds, then replaces the supervisor by plugin version and exact executable SHA-256. Because it never replaces a live server,
there is no check-to-restart race with native clients and a failed server start does not
needlessly tear down peer supervision. The repository also includes an explicit recovery utility for the
single confirmed Codex 0.147 failure shape: a duplicate `thread_settings_applied` ordinal exactly
at the persisted projection cursor. It validates the adjacent ordinals, backs up SQLite, advances
only the derived byte cursor, and leaves canonical rollout JSONL unchanged. It never attempts a
generic or automatic rewrite for unknown history corruption.

### Process and socket lifecycle

The durable Codex thread, its attached TUI, and its discovery shim have deliberately different
lifetimes. Hook and App Server close events identify a thread but not the client attachment, so they
are inert teardown hints. Once the exact owner process is provably stale, the supervisor removes the
shim, briefly archives and immediately unarchives the root because App Server has no public unload
RPC, and thereby stops thread-scoped MCP children while leaving the transcript resumable with runtime
status `notLoaded`.
Archiving or deleting the thread explicitly retires its shim instead. Retirement is persisted as a
bridge tombstone before transport removal; a startup audit compares loaded threads with App
Server's non-archived thread list so archived-but-still-loaded threads are not republished.

Every supported `codex-peer` launch binds the App-Server-returned or resolved UUID to the wrapper's exact
PID/process-start identity before the wrapper becomes the TUI. The supervisor checks that owner on
its five-second reconciliation tick. Cleanup therefore still occurs if Codex skips `SessionEnd` or
the TUI dies with `SIGKILL`; a later process reusing the PID cannot match the start token. A session
started outside `codex-peer` has no owner record, cannot publish a peer transport, and cannot call
peer tools. The public protocols identify only the thread, not an individual attachment; a plain
client explicitly attached to an already-authorized peer thread is therefore inside that thread's
capability boundary. Supervised lanes have their own durable owner identity and cleanup policy.

A shim removes its own PID registry record, state record, backend socket, and stable alias on a
normal exit. It also watches the exact owner process identity and exits when that owner dies. If a
shim receives `SIGKILL`, no process can run an exit handler; the supervisor's startup and five-second
reconciliation sweep instead removes the dead transport and creates a replacement for every still
loaded root thread. A supervisor killed with `SIGKILL` is similarly recovered by the next launcher;
its shims notice the dead owner independently. Reboot removes the runtime socket directory, and the
next launch removes any remaining persistent stale records.

The supervisor retains a waiter for every child shim so abrupt exits are reaped instead of becoming
zombies. Linux liveness additionally treats `/proc` states `Z` and `X` as dead: `kill(pid, 0)` alone
is insufficient because it succeeds for a zombie and would make Claude display a dead duplicate.

Garbage collection is intentionally conservative. It deletes only records with the bridge's
entrypoint/version signature and exact expected PID, session hash, registry, backend, and stable
socket paths. A stable alias is removed only when it still points at that dead backend. Native
Claude registry entries and unknown files are not touched. Inbox files are persistent data and are
never part of transport cleanup, so an undelivered message survives shim replacement.

The supervisor and shim share one runtime-root calculation, including the short `/tmp/ccp-<uid>`
fallback needed for Unix-domain path limits. Abrupt-death tests execute real child shims, kill both
shim and owner processes, check cleanup artifacts, preserve inbox data, and verify that
native Claude records survive the sweep.

Before cleanup or bind, the runtime leaf is `lstat`-checked as a real directory owned by the current
effective uid and forced to `0700`; an attacker-precreated fallback directory is a hard startup
failure. On macOS, kernel process-table observations provide the owner identity used by authorization
and cleanup; unknown observations preserve state for a later retry.

On an inbound peer message:

1. The supervisor durably records the message id before any App Server operation. Retries read the
   same wake record; a timeout never creates a second delivery path. After supervisor replacement,
   an in-flight record is reconciled against persisted turn input before it can fall back to the
   hook inbox. A sender must not reuse one message id for different content: such a conflict is
   dropped rather than opening a second delivery path. Frames without a transport id receive a
   stable content-derived id from the bridge.
2. An idle loaded thread receives `turn/start` without policy overrides, inheriting the thread's
   existing approval and sandbox settings. A headless lane created with `never` remains `never`;
   an ordinary or read-only thread cannot be silently widened by a peer wake.
3. A bridge-started active turn receives `turn/steer` with its known active turn ID.
4. If direct delivery fails, the supervisor writes one deterministic fallback to the hook inbox.
   `Stop` or
   `UserPromptSubmit` injects only complete messages that fit its bounded context. Overflow remains
   queued for a later boundary or `agent_sessions.check_inbox`; a truncated message is never deleted.

Direct peer messages are pushed into an active recipient turn automatically. Orchestrators should
continue useful work rather than poll `check_inbox`, sleep, or block waiting for delivery;
`check_inbox` exists only to recover messages held past a delivery boundary.

## Grok private-leader wake path

`grok-peer` starts one private Grok leader and one persistent official ACP
stdio bridge, then replaces itself with the attached Grok TUI. Exact-UUID
resumes are known at launch; title and bare-picker resumes use the provisional
attachment until the private roster proves the one selected resident UUID.
The leader socket uses Grok's private protocol; Agent Sessions never speaks
it. Wake delivery uses ACP v1 over
`grok --leader-socket <private> agent --leader stdio`: initialize, authenticate
with the CLI's advertised `cached_token` method, observe the preselected
resident session through `_x.ai/sessions/list`, verify the exact local
`agent_sessions` stdio MCP through a direct read-only `_x.ai/mcp/call` to
`list_peers`, then submit Grok's official
`_x.ai/interject` extension. The observer never calls `session/load` or
`session/prompt`: a concurrent load can replace the TUI's still-starting actor
and reclaim its MCP process scope.

The peer is not published until the official FleetView extension
(`_x.ai/sessions/list` wrapping `x.ai/sessions/list`) returns exactly one
resident row and `agent_sessions.list_peers` succeeds through Grok's MCP call
extension. Grok 1.0.4's catalog omits plugin-only MCP clients, so catalog
presence is not treated as readiness. Unrelated MCP failures are ignored. The roster row's
boolean `yolo` is the authoritative live permission class; the bridge refreshes
it while the session is resident, so argv, user config, and in-TUI changes do
not leave stale sender or lane-owner metadata. Infrastructure-only leader and
waker processes use explicit neutral permission mode, while the TUI keeps the
user's native policy.
Incoming messages are durably journaled by message ID before the host accepts
ownership and serialized through the persistent bridge. Grok's `queued`
response is not an actor acknowledgement: Grok 1.0.4 can return it after a
closed mailbox send. Agent Sessions therefore waits for the matching
`x.ai/session/interjection` notification and records `actor_accepted` only
after the resident actor echoes the exact session and interjection ID. That
notification proves actor acceptance, not model completion. Grok does not
deduplicate repeated interjection IDs, so an ambiguous post-write timeout stays
durably `in_flight`, is logged, and is never replayed automatically. A dead observer is recreated,
reauthenticates, and re-observes roster plus direct MCP-call readiness before the next
queued wake; it never attaches as a second session owner. Roster, MCP, and
interjection requests supply no yolo/auto override, so a peer message cannot
widen the TUI's policy.

One `grok-peer` launch owns one session UUID. Its raw random launch token exists
only in the owner process tree and private control frames; disk records contain
only SHA-256. MCP and Codex/Claude lane ownership additionally require exact
owner, host, and leader process-start identities, the live bridge publication,
the inherited token, and ancestry inside that leader tree. On owner death the
host removes its discovery row and stops only its own leader and bridge process
groups. Native Grok clients must not concurrently open the same UUID.

Interactive Grok supports fresh sessions plus exact-UUID, title, and bare-picker
resume. Resolution remains a native-Grok concern rather than private-store
parsing; the adapter adopts the resulting live roster UUID/title. Separate
sole-owner headless ACP sessions implement local or federated Grok lanes.

Headless App Server turns can issue server-initiated `item/tool/call` JSON-RPC requests. The native
client handles bridge-owned `agent_sessions` tools directly only after the App Server-supplied `threadId`
matches an authorized peer thread; other dynamic MCP names continue through `mcpServer/tool/call`.
For stdio MCP calls, the MCP process must be an exact child (PID plus process-start identity) of the
App Server process corroborated over its Unix socket by the supervisor. Codex's host-owned
`_meta.threadId`, turn `session_id`, and turn `thread_id` must all be present and valid. The outer
and turn `thread_id` values must be identical, and that exact thread must carry an active owner/lane
capability. Codex may restore a distinct session-family `session_id`; it is context only and cannot
borrow another thread's capability.
The model-supplied `session_id` may only corroborate the attested caller; it never grants authority.
Even `list_peers` passes this gate. Because Codex activates plugin MCP inventory daemon-wide,
ordinary threads can see the tool names, but calls return a bounded inactive result before roster,
inbox, rename, or send access.
The plugin's default MCP approval mode lets calls reach this authorization boundary without a
daemon-wide pre-dispatch prompt; it grants no peer capability by itself.
Managed Claude uses the same public `agent_sessions` tool name through the Claude plugin, but a
different caller proof. The stdio MCP process must descend from both the exact live native Claude
adapter and its launcher lifecycle owner. The host agent must independently return the same
interactive Claude UUID, adapter/lifecycle process starts, and adapter socket; the native registry
row must repeat that UUID, PID, process start, `cli`/`interactive` classification, and socket, and
the socket must be live. Claude does not supply a Codex `session_id`; exact process ancestry is the
authority. The Claude inventory deliberately exposes only grouped discovery, send/multicast, and
broadcast. Bare Claude, a copied environment, a recycled PID, a different registered Claude peer,
or a mismatched row/socket receives no roster or send authority.

Interactive Claude resume selectors remain native Claude selectors. An exact
UUID is stable before launch; any other argument, including a title with
duplicates, is passed through so Claude can apply its own matching and chooser
semantics. The launcher must not resolve that title from the Agent Sessions
catalog. It prepares a provisional attachment, ignores Claude's transient boot
row, and promotes only after the native row plus validated transcript title
identify the selected UUID. The catalog is keyed by that selected UUID, while
the attachment survives only as the strongly attested descendant alias
described above.

The bridge accepts MCP approval elicitations only for the bridge-owned `agent_sessions`
server; foreign
MCP approvals and ordinary elicitations are not trusted.

## Shim control frames

The shim has private control actions for updating its name/status and shutting down. It also
accepts Claude `peer_message_status` control frames and exposes them through the same Codex inbox.
Observed delivery outcomes may include delivered, held, denied, or expired.

## Compatibility boundary

The public-facing Codex side uses supported plugin surfaces: lifecycle hooks, a local stdio MCP
server, and managed App Server. The Claude registry schema, envelope, and Unix-socket frame format
are reverse engineered. Protocol-specific code lives in `internal/bridge/runtime.go` and
`internal/bridge/mcp.go`; rerun the race suite and live bidirectional probes after upgrading Claude
Code.

Claude Code 2.1.226 hardcodes native peer presentation as another Claude session and resolves
targets through its own name/ref address book. The observed registry record has no alias or product
type field that changes those behaviors. The bridge therefore exposes product type in its own
Codex listing and message envelope, but cannot change native `ListAgents` labeling or make a raw
Codex session UUID a Claude-native target.

File transfer and Windows named pipes are not implemented. Remote-host grouped
messaging uses protocol-3 host agents plus the optional hub. Immediate
wake requires an App-Server-backed session; conventionally launched standalone Codex processes keep
the hook-inbox fallback behavior.
