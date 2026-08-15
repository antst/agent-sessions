# Claude Code lanes from Codex

`claude-peer-lane` is the symmetric companion to `codex-peer-lane`: it lets Codex and other local orchestrators start a named Claude Code worker, keep it messageable between turns, collect structured results, resume it, and retire it without orphan processes or discovery rows.

## Quick start

```bash
claude-peer-lane doctor --json

claude-peer-lane run \
  --name claude-review \
  --permission-mode dontAsk \
  --max-budget-usd 5 \
  --auto-archive-after 300 \
  - < review-brief.md
```

Detached mode returns `lane.ready` before the result:

```bash
claude-peer-lane start --name claude-review --max-budget-usd 5 - < review-brief.md
claude-peer-lane wait claude-review --timeout 300
claude-peer-lane status claude-review
claude-peer-lane archive claude-review
```

While idle, the lane appears in both Claude and Codex peer discovery and accepts ordinary peer messages. A message creates the next queued Claude turn; collect it with another `wait`.

## Remote hosts

With `peer-federator` protocol 2 connected on both hosts, the same native CLI can run on a named
destination without SSH:

```bash
peer-federator hosts
peer-federator lane --host workstation-b --product claude -- \
  start --name claude-review -C /srv/project --max-budget-usd 5 - < review-brief.md
peer-federator lane --host workstation-b --product claude -- \
  wait claude-review --timeout 300
```

Federation preserves JSONL, stderr, and exit status. It automatically adds `--persistent` and a
notify target back to the live originating peer for `run`, `start`, and `resume`; callers must not
pass `--persistent`, `--notify`, `--no-notify`, or `--no-auto-archive` themselves. The destination advertises
this capability only after its operator explicitly enables remote execution. A disconnected hub
rejects new commands, and there is no direct agent listener or SSH fallback. Remote lanes use plain destination `list` rather than
`--mine`, because they are persistent and have no destination lifecycle owner.
Pass `-C`/`--cd` on remote `run` or `start` whenever the cwd matters; otherwise the launcher
inherits the destination agent service's cwd. `resume` retains the established cwd. Remote stdin
is capped at 1 MiB; `--prompt-file` refers to an existing destination file and transfers nothing.
Remote auto-archive delays are capped at 86,400 seconds.
Message a remote lane through its qualified source-side discovery name (`name--host [ref]`) or an
incoming message's `from` UDS; its destination-local name and session ID are lifecycle addresses.

## Why there is a manager

Claude Code's `--print --input-format stream-json` process is itself the lane's sole discoverable
peer. Native peer messages start Claude turns directly; the bridge does not proxy, rewrite, or
queue them through another peer socket. The manager observes those stream turns and their
authoritative `result` frames so the same collection, notice, and accounting contract applies to
both launcher-submitted and peer-submitted work. A `run`/`start`/`resume --timeout` applies to that
launcher-submitted turn; native peer messages have no transport field for a durable execution
deadline.

The manager owns three resources as one lifecycle unit: durable lane state, the native Claude
stream worker, and its local control socket. Explicit archive, owner exit, auto-archive, manager
crash recovery, and install-time supervisor reconciliation converge on the same cleanup. The
worker socket is unavailable briefly while an archived or crashed lane is being resumed; messages
sent during that interval fail rather than being buffered by a proxy.

A native peer message sent while the worker is idle starts a new collectable lane turn. A message
accepted while Claude is already running a turn steers that active turn and shares its single
terminal result; it does not create a second `wait` result. The manager waits for Claude's replayed
local-user frame before marking a launcher-submitted turn active, so a simultaneous native message
and CLI `resume` are attributed in the order Claude actually begins them.
When no native turn is active, a 30-second acknowledgement watchdog fails and retires the worker
if Claude never replays a submitted launcher envelope. A native turn that legitimately runs first
refreshes that watchdog before the queued launcher turn begins. Native peer turns have no caller
deadline, so a still-active native turn can defer this check; owner exit and explicit archive remain
the lifecycle escape hatches.

## Lifecycle defaults

- A lane is owned by the launching Codex, Claude, or Antigravity session when that owner can be corroborated by process ancestry and live peer discovery. It is interrupted and archived after that owner exits.
- A caller without a corroborated agent owner must explicitly use `--persistent`; a transient shell is never guessed as the owner.
- Completed work auto-archives after 60 seconds. The exact deadline is exposed as `auto_archive_at`; cleanup occurs on the manager's sub-second maintenance loop.
- `--auto-archive-after S` changes the grace period. `--no-auto-archive` disables it and requires explicit archive.
- `--persistent` detaches lifecycle ownership. `--notify TARGET` is persistent-only; parent-owned lanes notify their inferred owner without a flag.
- Terminal notices are durably retried. A failed notice can delay automatic archive for up to 30 seconds so a short custom grace does not erase the only collection pointer.
- A terminal notice is an infrastructure pointer, not a conversational message. Collect it with the printed `wait` command rather than replying to its sender address; after a worker crash that address may no longer be live.

`claude-peer-lane list` returns all active Claude lanes in the profile and `--all` adds archived
lanes. `list --mine` selects only lanes owned by the current Codex, Claude, or Antigravity orchestrator, matching
the stable owner PID plus process-start identity rather than a mutable session ID. Persistent lanes
have no owner and are therefore excluded; use `--mine --all` to include owned archived lanes. The
query fails if no live Codex, Claude, or Antigravity owner can be corroborated rather than silently falling back
to a transient shell process.

## Claude-specific policy

Claude has no Codex sandbox axis. The launcher passes through Claude's `--permission-mode`, tool allow/deny lists, `--model`, `--effort`, `--json-schema`, and `--max-budget-usd` without pretending they are equivalent to Codex sandbox policy.

The default permission mode is `dontAsk`, because a headless worker cannot answer an interactive
approval. Workers start with `--settings '{"crossSessionInbound":"accept"}'` so native peer input
does not wait for an approval UI the lane does not have. When the launcher
corroborates a bypass-mode Codex, Claude, or Antigravity owner and the caller did not explicitly choose a mode,
the lane inherits `bypassPermissions`; an explicit `--permission-mode` always wins. The launcher
never trusts an inherited thread ID alone: it requires a live Codex-host ancestor as a launch-context
precondition plus a live matching session bridge PID, process-start identity, session ID, and
socket. Codex does not currently expose a per-thread host PID, so the bridge identity—not the
shared host process—is the lifecycle owner.

The worker enables Claude's Agent Teams tool set and always adds `SendMessage,ListAgents` to
`--allowedTools`. An explicit `--allowed-tools` list is preserved and extended rather than
replaced. This lets a lane discover and initiate messages to any reachable peer; ownership only
controls lifecycle and automatic terminal notices. Explicit `--tools` or `--disallowed-tools`
policy can still remove those capabilities. The SDK worker publishes and authors messages under
the lane name, owns the address reported by `lane.ready`, and accepts inbound peer turns directly.

For a federated notify target, the destination shadow carries the originating peer's source-asserted
permission class under the trusted-VLAN assumption. Infrastructure notices can therefore match
their remote owner without changing the Claude worker's actual permission mode.

Claude emits real cost data. Autonomous callers should set `--max-budget-usd`; trivial turns can
still be expensive because project instructions, plugins, skills, hooks, and MCP context load for
each turn. Claude 2.1.227 does not publish a native peer socket in `--bare` mode, so
`claude-peer-lane` rejects `--bare` rather than creating a lane that cannot receive messages.

## Collection contract

Stdout is JSONL. Diagnostics go to stderr. The normalized event sequence is:

```text
thread.started | thread.resumed
turn.started
lane.ready
item.completed (user_message)
item.completed (agent_message, phase=final_answer)
turn.completed
```

Use the final `turn.completed.turn_id` to select the matching final answer. Unknown native Claude stream frames are tolerated and never interpreted as completion. `wait --timeout` only stops waiting; it does not interrupt the lane.

Collect every outstanding turn before `resume`. Resume deliberately refuses to start a new turn while an older active, queued, or terminal turn is still owed, so its `turn.started` event can never be paired with another turn's result.

See the installed Codex skill `claude-lane` for the orchestration recipe and `claude-peer-lane --help` for the complete flag list.
