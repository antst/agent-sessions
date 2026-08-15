# Claude peer protocol notes

These notes describe behavior observed in the installed Claude Code 2.1.226 Linux executable. This
is not a public Anthropic protocol specification.

## Discovery

Claude enumerates JSON files in `$CLAUDE_CONFIG_DIR/sessions` or `~/.claude/sessions`. A messageable
record has a live `pid` and a connectable `messagingSocketPath`. The bridge corroborates process-start
identity through `/proc/<pid>/stat` on Linux and the kernel process table on macOS; an observation
failure is distinct from proof that a process is stale.

The bridge publishes this compatible subset. `entrypoint` is `codex` for an
App-Server-backed peer and `grok` for a private-leader-backed Grok peer:

```json
{
  "pid": 12345,
  "sessionId": "codex-session-id",
  "cwd": "/project",
  "startedAt": 1786000000000,
  "procStart": "123456789",
  "version": "codex-claude-peer/0.1.0",
  "peerProtocol": 1,
  "kind": "interactive",
  "entrypoint": "codex",
  "name": "codex-project-ab12cd34",
  "nameSource": "generated",
  "status": "idle",
  "messagingSocketPath": "/run/user/1000/codex-claude-peer-1000/session-0123456789abcdef0123.sock"
}
```

Claude's `agents --json` command was used as an independent smoke test and displayed the bridge
record as an interactive session.

The peer name is the primary durable address. The Codex MCP sender resolves it from fresh discovery
immediately before every send. A short ref is only a transient duplicate-name disambiguator and
must not be cached or persisted.

The advertised UDS path is a stable per-Codex-session symlink to the current shim's private
PID-named listener. Claude Code keys its conversation-local `name [ref]` identity by this path, so
the bridge avoids gratuitous ref rotation when a shim process is replaced. The Codex MCP sender
also accepts an exact full peer `sessionId` (or `session:<id>`), but Claude Code's native sender does
not resolve raw session IDs and the discovery record has no alias field.

`nameSource` is `codex` when the name comes from Codex's session index, `explicit` when supplied by
`CLAUDE_PEER_SESSION_NAME`, `launch` for a prepared interactive wrapper, `lane` for a durable lane,
`manual` after the MCP rename tool is used, and `generated` for the startup fallback.

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
permission-mode attestation and hold the message for human approval. Codex therefore puts extension
metadata on the first body line: transport `msg_id` as `messageId`, an ISO-8601 `sentAt`, and
`fromProduct: "codex"` or `"grok"` according to the attested sender.
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
SessionStart promotes either
kind to attached. No cwd/time heuristic participates. The native launcher consumes only its peer-name
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
   queued for a later boundary or `claude_peer.check_inbox`; a truncated message is never deleted.

Direct peer messages are pushed into an active recipient turn automatically. Orchestrators should
continue useful work rather than poll `check_inbox`, sleep, or block waiting for delivery;
`check_inbox` exists only to recover messages held past a delivery boundary.

## Grok private-leader wake path

`grok-peer` preselects an exact UUID, starts one private Grok leader and one
persistent official ACP stdio bridge, then replaces itself with the attached
Grok TUI. The leader socket uses Grok's private protocol; Agent Sessions never
speaks it. Wake delivery uses ACP v1 over
`grok --leader-socket <private> agent --leader stdio`: initialize, authenticate
with the CLI's advertised `cached_token` method, load the preselected session,
then submit `session/prompt`.

The peer is not published until ACP has loaded the exact session ID and cwd
and the official FleetView extension (`_x.ai/sessions/list` wrapping
`x.ai/sessions/list`) returns exactly one resident row for it. The row's
boolean `yolo` is the authoritative live permission class; the bridge refreshes
it while the session is resident, so argv, user config, and in-TUI changes do
not leave stale sender or lane-owner metadata. Infrastructure-only leader and
waker processes use explicit neutral permission mode, while the TUI keeps the
user's native policy.
Incoming messages are durably journaled by message ID before the host accepts
ownership, serialized through the persistent bridge, and never duplicated
after an ambiguous post-write timeout. A dead bridge is recreated and reloads
the same session before the next queued wake. Neither load nor prompt supplies
yolo/auto metadata; a peer message cannot widen the TUI's policy.

One `grok-peer` launch owns one session UUID. Its raw random launch token exists
only in the owner process tree and private control frames; disk records contain
only SHA-256. MCP and Codex/Claude lane ownership additionally require exact
owner, host, and leader process-start identities, the live bridge publication,
the inherited token, and ancestry inside that leader tree. On owner death the
host removes its discovery row and stops only its own leader and bridge process
groups. Native Grok clients must not concurrently open the same UUID.

The first release deliberately supports fresh sessions and exact-UUID resume.
Title resolution, a bare resume picker, native Grok lanes, and Grok federation
remain native-Grok or later-version concerns rather than private-store parsing.

Headless App Server turns can issue server-initiated `item/tool/call` JSON-RPC requests. The native
client handles bridge-owned `claude_peer` tools directly only after the App Server-supplied `threadId`
matches an authorized peer thread; other dynamic MCP names continue through `mcpServer/tool/call`.
For stdio MCP calls, the MCP process must be an exact child (PID plus process-start identity) of the
App Server process corroborated over its Unix socket by the supervisor. Codex's host-owned
`_meta.threadId`, turn `session_id`, and turn `thread_id` must then all be present and identical, and
that exact thread must carry an active owner/lane capability.
The model-supplied `session_id` may only corroborate the attested caller; it never grants authority.
Even `list_peers` passes this gate. Because Codex activates plugin MCP inventory daemon-wide,
ordinary threads can see the tool names, but calls return a bounded inactive result before roster,
inbox, rename, or send access.
The plugin's default MCP approval mode lets calls reach this authorization boundary without a
daemon-wide pre-dispatch prompt; it grants no peer capability by itself.
The bridge accepts MCP approval elicitations only for the bridge-owned `claude_peer`
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

File transfer, remote-host Claude messaging, and Windows named pipes are not implemented. Immediate
wake requires an App-Server-backed session; conventionally launched standalone Codex processes keep
the hook-inbox fallback behavior.
