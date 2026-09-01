# Codex worker lanes

`codex-peer-lane` is the integration surface for any local orchestrator. It creates and controls a
named persistent Codex App Server thread and publishes that thread in Claude's native peer registry.
It owns no project packet format, briefing, model, reasoning, sandbox, approval, web, artifact, or
postflight policy.

Managed Codex peers invoke local and federated lane lifecycle operations through
the attested `agent_sessions.lane` MCP tool. Codex shell tools run inside an OS sandbox
that may correctly deny the App Server, supervisor, and host-agent Unix sockets;
granting broader shell access is not a prerequisite for launching a lane. The MCP
tool executes the exact packaged runtime outside that shell sandbox, binds the live
registered parent session, and returns the native exit code, stdout, and stderr.
Codex, Claude, Grok, and Qwen orchestrators use this same tool for both self-product
and cross-product lanes. Product lane CLIs remain the human/automation surface, not
the model orchestration transport.

The MCP transport permits one blocking `run`, `resume`, or `wait` call for at most
24 hours. A positive native `--timeout` below that ceiling receives an additional
one-minute transport margin, so documented long waits such as 2700 seconds remain
valid. An omitted or zero timeout uses the 24-hour transport bound; if that bound is
reached the tool returns exit 124 plus captured tail output and does not claim a turn
was collected. Use `start` followed by bounded `wait` calls for longer work. Output
caps retain the tail, where lane JSONL emits the final answer and `turn.completed`,
and report truncation explicitly.

## Foreground compatibility

`run` accepts the prompt on stdin and emits JSONL compatible with the useful `codex exec --json`
event vocabulary:

```bash
codex-peer-lane run \
  --name linux-review-a \
  --cd /work/project \
  --model gpt-5.6-sol \
  --effort xhigh \
  --sandbox read-only \
  --approval-policy never \
  --web \
  --timeout 1800 \
  - < briefing.md
```

`thread.started`, `turn.started`, and `lane.ready` are emitted before model output. `lane.ready`
means the briefing turn has already started, durable lane state exists, and the peer is registered;
there is no interval in which a discoverable peer has no turn to receive a message. The events
expose `contract_version: 2`, the thread/session ID, turn ID, selected name, and exact reply address. App Server
`agentMessage` items are normalized to `agent_message` for existing exec parsers.

`run` owns the synchronous wait. SIGINT or SIGTERM asks App Server to interrupt its turn. Do not
shell-background `run` as a detached lane; use `start` followed by `wait`.

## Detached native-agent style

`start` returns once the peer is registered and the initial turn has started. The shared App Server
continues the work after that launcher command exits, while the launching orchestrator remains the
lane's lifecycle owner:

```bash
codex-peer-lane start \
  --name implementation-b \
  --cd /work/tree \
  --sandbox workspace-write \
  --approval-policy never \
  - < briefing.md

codex-peer-lane status implementation-b
codex-peer-lane wait implementation-b --timeout 2700
codex-peer-lane interrupt implementation-b
codex-peer-lane archive implementation-b
codex-peer-lane list
codex-peer-lane list --mine
codex-peer-lane list --all
codex-peer-lane doctor --json
```

A parent-owned lane must be launched beneath a live Codex or Claude orchestrator whose process
identity can be corroborated. A plain shell, cron job, or CI runner has no such lifecycle owner;
use `--persistent` there or the command fails closed with an actionable owner error.

## Remote hosts

The unified daemon can proxy this unchanged native contract through a protocol-4 hub:

```bash
codex-peer-lane --host workstation-b \
  start --name implementation-b -C /srv/project - < briefing.md
codex-peer-lane --host workstation-b wait implementation-b --timeout 2700
```

The proxy preserves stdout JSONL, stderr, and the native exit code. The source agent supplies an
attested parent context; the destination always gives the child the source-host parent anchor and
copies other parent groups only after explicit `--inherit-groups`. Remote lanes are persistent on
the destination and terminal pointers return as ordinary grouped Agent Sessions frames. Every
operation routes local daemon → hub → destination daemon. Hub loss fails closed and cancels a
still-blocking proxy; there is no direct network, shadow-socket, or SSH fallback.
Pass `-C`/`--cd` on remote `run` or `start` whenever the cwd matters; otherwise the launcher
inherits the destination agent service's cwd. `resume` retains the established cwd. Remote stdin
is capped at 1 MiB; `--prompt-file` refers to an existing destination file and transfers nothing.
Remote auto-archive delays are capped at 86,400 seconds.
The destination advertises its lane capability only while its native product adapter is ready.
Message a remote lane through the same group-filtered Agent Sessions protocol used locally; its
destination-local name and thread ID remain lifecycle addresses.

Parent-owned lanes automatically create a durable supervisor-owned terminal job for their owning
Codex or Claude session. On completed, failed, or interrupted outcomes—or a bridge-enforced timeout—the
owner receives a small pointer
containing lane name, thread ID, turn ID, raw App Server status, normalized outcome, wrapper exit
code, and the current collection state. `collection=required` includes a structured
`agent_sessions.lane` `wait` hint; `collection=not_required` means another collector already
consumed it. These fields describe the App Server turn. The job survives launcher exit and supervisor
replacement: startup audits the stored turn, so a completion notification missed by a crashed or
killed supervisor is reconstructed. Killing a discovery shim does not kill its App Server lane;
the supervisor replaces the shim and the terminal notice is still emitted when the turn finishes.
Shim termination is crash recovery, not a cleanup operation: use `archive`, or let a recorded lane
owner exit, to retire discovery permanently. Lanes created by older runtimes have no owner metadata
and are never adopted heuristically; archive those legacy lanes explicitly.
An explicit `archive` makes one last delivery attempt, then cancels any still-undeliverable notice
and reports `notices_dropped` instead of allowing a hint to block authoritative cleanup.
For a remote lane, the structured hint includes `host=HOST`, so collection uses
the same daemon-owned federation path.

The default lifecycle is parent-scoped. A direct Claude caller is identified only when its
environment hints, current process ancestry, PID, available process-start identity, session ID,
and live registry socket agree. In that case the corroborated Claude session process is the owner,
not the short-lived shell or adapter subprocess invoking `codex-peer-lane`. Other launchers are
owned by their direct parent process. When the
owner exits, the supervisor interrupts a running turn and archives the lane after it reaches a
terminal state, removing its discovery shim while retaining normal archived Codex history. The
unified daemon always routes the terminal collection pointer to the immediate Agent Sessions
parent; this routing is not a model-policy option.
Terminal pointers are supervisor-generated infrastructure messages. At delivery they use the live
Claude target's permission class so Claude does not hold an ordinary completion pointer for a
prompting/bypass mismatch. Ordinary peer messages from a parent-owned lane to that exact parent use
the same established relationship; messages to every other peer retain the lane's actual permission
class. Neither behavior changes the lane's Codex approval or sandbox policy.
If the live target cannot be resolved and classified, delivery fails closed and the durable notice
remains queued for retry rather than being recorded as sent.
Federated origin, product, and permission metadata live in the inner Agent Sessions frame and the
attested hub roster. The Claude outer carrier describes the local host agent and never upgrades
the source's permission class.

`--persistent` is the explicit opt-out from parent cleanup. A persistent lane survives its launcher
or orchestrator while retaining its immediate Agent Sessions parent anchor. `lane.ready` reports
`persistent`, `auto_archive`, and `owner_session_id`. The unified lifecycle has no
`notify_target`, `--notify`, or `--no-notify` input.
`resume` preserves whether the lane is persistent or parent-owned. Passing `--persistent` explicitly
promotes a parent-owned lane; omitting it never silently demotes an existing persistent lane. A
parent-owned resume records the new launcher as its lifecycle owner.

Auto-archive is enabled by default. After the latest turn reaches its final terminal state (including
any schema correction), the lane remains idle and messageable for one minute, then the supervisor
archives it. `--auto-archive-after SECONDS` selects a different grace period with millisecond
granularity (minimum `0.001`). A newer turn
cancels the pending timer. Collect or message the lane during that grace period; after archive the
transcript remains resumable, but the detached `wait` result is no longer available.
`--no-auto-archive` disables the timer and conflicts with `--auto-archive-after`. A lane intended to remain idle indefinitely must
use both `--persistent` and `--no-auto-archive`. The setting works with parent-owned and persistent
lanes and is re-evaluated by each `resume`; re-pass a custom grace or the default returns to 60 seconds.
`auto_archive_at` is the exact not-before deadline. Cleanup runs on the supervisor's reconciliation
tick, so archiving normally occurs within five seconds after that deadline rather than at an exact
wall-clock instant.
If auto-archive occurs before collection, the prior detached answer is no longer recoverable through
`wait`; `resume` starts a new turn and does not recover that answer.

`--timeout SECONDS` defaults to zero (disabled). When positive, it is a durable per-turn deadline for `run`, `start`, and `resume`. The deadline is
stored with the lane and enforced by the supervisor after a detached launcher exits or a supervisor
is replaced. App Server's raw terminal status for that interrupt is normally `interrupted`; bridge
events and notices preserve the more precise `outcome: "timed_out"` and exit code `124`. Enforcement
is asynchronous, so the deadline is not a precise wall-clock kill fence. On `wait`, the same
spelling only bounds the collection call. A `wait` timeout never interrupts or changes the lane;
call `wait` again or inspect `status`.

This `start` + `wait` pair is the recommended generic adapter for Claude orchestrators. Parse the
returned `lane.ready` record, retain its thread ID, and use the ID for lifecycle operations. Use
Claude's standard messaging for agent interaction while the lane exists. A completed lane remains
idle and messageable during its configured grace period. Owner exit can archive a parent-owned lane
sooner; `--no-auto-archive` retains it until that owner exits. A persistent lane with
`--no-auto-archive` remains until explicit archive. SIGINT or SIGTERM sent to a detached `wait` or `resume` collector exits only that
collector; the unacknowledged turn remains running or recoverable by a later `wait`.

`list` reads the profile-scoped lane registry without scraping Claude's peer listing. Its
`lane.list` event includes active lanes by default and archived-but-resumable lanes with `--all`.
`--mine` filters by the current orchestrator's corroborated process identity, not its mutable
session ID; persistent lanes are intentionally excluded because they have no lifecycle owner.
Combine `--mine --all` to include that orchestrator's archived lanes. If no live Codex or Claude
owner can be corroborated, `--mine` fails instead of returning a misleading empty list; unlike
lifecycle ownership, this cross-invocation query never falls back to a transient shell process.
`doctor --json` emits one `lane.doctor` event containing `contract_version`, runtime version/path,
App Server and supervisor reachability, `CODEX_HOME`, and the profile state root. Both discovery
events use contract version 2. A generic adapter should require a supported contract major before
launching work.

The repository also ships a self-contained Claude Code plugin, `agent-sessions`, which teaches this
contract to any Claude orchestrator without copying a project-specific adapter. It adds no runtime,
hooks, policy defaults, or global permission mutation. Managed Claude launchers grant only the exact
Agent Sessions MCP tools in their lifecycle-owned settings/argv. Its process-attested MCP declaration
provides grouped messaging and the same daemon-backed lane control used by the other products.

`wait` is a single-consumer cursor over the lane's persisted turn history. Its first successful
call collects the initial turn; each later call blocks for and returns the oldest uncollected turn,
including a turn started by an inbound peer message. It never replays the last completed turn.
The durable pending queue survives additional wake turns before collection; a schema correction
replaces its rejected draft in the same queue position instead of skipping neighboring results.
Consumers should identify the result by `item.completed` with `type: "agent_message"` and
`phase: "final_answer"`; that phase label is part of the adapter contract. Do not run concurrent
`wait` calls for one lane or combine `wait` with another lifecycle collector. Interactive
`codex-peer` sessions do not use this cursor—the attached TUI owns their App Server event stream.
On reconnect, `wait`, `status`, and `interrupt` resubscribe through a metadata-only App Server
resume. Installation requires App Server to be stopped and never replaces a live process, because
Codex 0.147's projection-backed history APIs do not repair a cursor gap left by an interrupted
server.

`archive` synchronously writes a retirement tombstone and removes the live peer transport. It keeps
the small local lane record so the archived thread remains addressable by lane name for an explicit
follow-up resume. On supervisor startup, loaded threads are compared with the non-archived thread
list so a legacy archived-but-loaded thread cannot be republished.

If initial authentication or another authoritative App Server rejection occurs before Codex creates
the first rollout, there is no transcript to preserve or resume. In that narrow case, `archive`
accepts an App Server `no rollout found` response only when the local failed record contains no turn
ID, pending turn, collected turn, or terminal-turn evidence. It deletes that unmaterialized thread
and writes the ordinary retirement tombstone. A transport error or any local turn evidence remains
ambiguous and is refused rather than guessed away.

## Follow-up turns on the same transcript

Use `resume` to question or correct a completed lane without re-briefing a new model context:

```bash
codex-peer-lane resume implementation-b - < follow-up.md
```

If the lane was archived, `resume` unarchives it, resumes the same App Server thread, preserves its
full transcript, starts one new turn, and republishes the same lane name. It waits like `run` and
emits `thread.resumed`, `turn.started`, `lane.ready`, item events, and the terminal event. Collect
any outstanding turn first: explicit resume starts a new collection cursor for the follow-up even
though the older content remains visible in the transcript.

## Structured output, isolation, and accounting

`--schema FILE` passes a caller-owned JSON Schema to App Server as `outputSchema` and independently
validates the final `final_answer`. A detached `start` stores the schema for `wait`; validation is
therefore identical for foreground and detached lanes. The supervisor owns correction: invalid
JSON or a schema violation starts at most two constrained correction turns on the same transcript,
emitting `turn.schema_retry`. This serialization prevents a waiter and terminal notification from
starting duplicate retries. A streaming consumer can see the rejected turn's `final_answer` before
its subsequent `turn.schema_retry`; the authoritative structured result is the `final_answer` whose
`turn_id` matches the eventual successful `turn.completed` event.

`--worktree` creates a detached Git worktree from the repository's current `HEAD` beneath the
profile-scoped bridge state and uses it as the lane cwd. The path is returned in lane state and is
retained on archive so an orchestrator can inspect, merge, or remove it deliberately; the bridge
does not discard a possibly dirty worktree.

Every `turn.completed` event contains `accounting`: App Server duration/start/completion fields and
normalized token counters. The supervisor persists token updates so a collector attached after a
detached turn still receives them. `cost` is explicitly `null` with `cost_available: false`; the
bridge does not guess pricing without authoritative model/rate data.

The terminal event also carries raw `status`, normalized `outcome`, and `exit`. Cursor
acknowledgement occurs only after persisted terminal items and the terminal event have been emitted;
an interrupted collector or broken output pipe never marks unseen content as collected. The
supervisor durably spools lane `item/completed` notifications until acknowledgement, so a collector
can recover a final answer even while App Server's history projection exposes the terminal row
before its items. Terminal status is also corroborated by the supervisor's durable completion
observation (or a complete persisted historical row), because App Server can briefly project a newly
started turn as interrupted. A successful turn is not acknowledged until that persisted
`final_answer` exists.
`status` exposes the same durable `outcome` and `exit`, including `timed_out`/`124` after the
terminal notice has passed.

Use stable role-based names, but do not treat them as ownership keys. Agent
Sessions discovery and messaging resolve names only among peers visible through
the sender's groups, so the same name may exist in disjoint groups. Lane CLI
lifecycle commands (`wait`, `status`, `interrupt`, `archive`, and `resume`) read
host-local lane state; a bare name can therefore be ambiguous even when the
lanes are group-isolated. Persist and use exact lane IDs for lifecycle work.
Agent Sessions discovery and messaging go through the host service; native
Claude registry refs and direct UDS replies are not the cross-product routing
contract.

## Configuration inheritance

All policy flags are optional. Omitted values inherit the normal Codex user and `CODEX_HOME`
configuration. Explicit values are overlays:

- `-m`, `--model`
- `--effort`
- `--sandbox read-only|workspace-write|danger-full-access`
- `--approval-policy`
- `--web` / `--no-web`
- repeated `-c KEY=VALUE` with dotted keys and JSON/TOML-like scalar values

The wrapper does not replace or disable user configuration. It internally overlays only
`features.code_mode_host=false`, which is headless execution plumbing rather than agent policy:
detached lanes have no attached TUI to act as an external code-mode host. Shell/patch tools remain
available in App Server, and the supervisor dispatches nested MCP calls such as `agent_sessions`.

An autonomous lane cannot ask a human through a TUI. Orchestrators that intend unattended tool use
should normally pass `--approval-policy never`; the wrapper deliberately does not choose this for
them. Approval and sandboxing are independent: `agent_sessions` messaging is client-side and works for
a `read-only` model sandbox. The supervisor auto-accepts only MCP approval elicitations for its own
`agent_sessions` server, reflecting the bridge's trusted-isolated-peer contract; it does not approve
foreign MCP servers or ordinary elicitations.

Prompts should use stdin or `--prompt-file`; large briefings are never placed on argv.

## Adapter contract

A project-specific launcher normally needs only to:

1. Run `codex-peer-lane doctor --json` and require a supported `contract_version`.
2. Assemble its own prompt and policy flags.
3. Invoke `codex-peer-lane start --name ... -` with that prompt on stdin.
4. Parse JSONL until `lane.ready` and retain the returned thread ID.
5. Optionally send/receive ordinary Claude peer messages while the lane runs.
6. Invoke the single-consumer `wait THREAD_ID`, extract the `phase: "final_answer"`
   `item.completed` agent message for the terminal event's `turn_id`, and apply its own postflight
   checks. Invoke `wait` again only when deliberately collecting a later conversational or
   peer-wake turn.
7. Optionally use `resume THREAD_ID` for a transcript-preserving follow-up.
8. Invoke `archive THREAD_ID` when the peer should disappear before its owner exits or grace timer.
   Parent exit archives ordinary lanes automatically; default lanes auto-archive one minute after
   their latest terminal turn. Only `--persistent --no-auto-archive` requires explicit archive.

Do not copy the bridge implementation into an orchestrator. Keep the adapter thin so runtime,
protocol, wake, and portability fixes remain centralized in this repository.
