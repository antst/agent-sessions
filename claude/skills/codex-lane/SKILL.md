---
name: codex-lane
description: Orchestrate named, messageable local or remote Codex lanes — start a detached lane, collect its final answer, message or steer a running lane, resume the same transcript, and archive it. Use when asked to delegate work to Codex, run a Codex agent in the background, fan out parallel Codex workers, use a named federated host, check on or steer a running lane, or collect a lane result.
---

# Orchestrate Codex lanes

A lane is a named Codex thread on the local shared App Server. It registers with
the Agent Sessions host agent and is visible only to peers sharing one of its groups. Claude's
shared native registry contains ordinary/managed Claude rows and the single host-agent service,
not synthetic Codex rows. `--persistent` is an explicit opt-in for a lane that must survive its owner.

This skill is a pass-through to the process-attested `agent_sessions.lane` MCP
tool. It owns no policy: model, reasoning effort, sandbox, approval, web access,
config overlays, output schema, and worktree isolation are all decided by whoever
asked for the lane. See `references/policy.md`.

## Required lifecycle transport

Use `agent_sessions.lane` for every lifecycle operation with `product: "codex"`.
Supply one exact `command`, its native arguments after the command, optional
briefing `input`, and optional federated `host`. Never execute `codex-peer-lane`
through Bash. CLI spellings below document the argument contract only; translate
them to the structured tool. `start` returns after registration while the Agent
Sessions daemon owns the detached background worker.

## Remote host

When the user requests another host, use federation instead of SSH. Set `host`
on the structured call, then use command `doctor` with arguments `["--json"]`
and command `list` with arguments `["--all"]`.

Require the local agent to be connected, the destination to advertise `codex-lane`, and remote
doctor contract 2 to be healthy. A destination advertises this capability only after its operator
explicitly enables remote lane execution. Keep `host` on every later lifecycle call.

Do not run the local lane-preflight for a remote destination. Federation carries this live
Claude parent’s attested context and returns terminal notices through grouped routing; never pass
`--persistent` or `--no-auto-archive` for those
commands. Remote lanes have no lifecycle owner and are excluded from `--mine`; use the remote plain `list`, names, or IDs.
The JSONL contract, one-consumer cursor, terminal notice, deadlines, and auto-archive grace remain
native Codex lane behavior.

Pass `-C /absolute/remote/path` on remote `run` and `start` whenever the working directory matters;
otherwise the native launcher inherits the destination agent service's cwd. `resume` retains the
lane's established cwd; a replacement `-C` is ignored. Send remote briefings on stdin (maximum
1 MiB). `--prompt-file` names an already-existing destination file; federation does not transfer
it. Remote auto-archive delay is capped at 86,400 seconds.

Every remote operation requires the hub. On disconnect, stop and report the failure; never fall back to SSH
or silently execute locally. Notifications are push-delivered through federation, so
do not poll.

## Preflight (once per session)

Call `agent_sessions.lane` with command `doctor` and arguments `["--json"]`, then
command `list` and arguments `["--all"]`. Require **contract version 2**, a
reachable runtime, and `ready: true`.

- Runtime missing: stop and print the user-facing commands from `references/install.md`. Never try
  to install, build, or start anything yourself.
- Runtime older than contract 2: say so and stop. Do not fall back to guessing the event shape.
- Compatible contract but `runtime_ready: false`: report which service is unreachable and stop.
  Host runtime recovery is not an orchestrator action.
- The MCP tool binds the packaged runtime and attested parent. Do not substitute a shell-resolved
  launcher from another installation.

## The one constraint that shapes everything
Always `start` detached work, which returns as soon as the lane is registered,
then collect separately. Do not hold an orchestration turn open with `run` when
the work can outlive the call.

Never use `run` for a detached lane, and never shell-background `run`.

## Workflow

### 1. Start
Pass the briefing as MCP `input`; never put a prompt in `arguments`.

```json
{
  "product": "codex",
  "command": "start",
  "arguments": ["--name", "review-a", "-"],
  "input": "<briefing text>"
}
```

Only `--name` is required. Add policy flags **only** when the caller specified them.

Pick a stable, role-based kebab-case name and check structured `list --all` plus an Agent
Sessions `discover` request from this session. The same name may exist in disjoint groups; a
messaging ambiguity matters only when it is visible to this sender. Parse stdout JSONL until
`{"type":"lane.ready"}`; retain its `session_id` and confirm
`contract_version` is 2. Everything after `lane.ready` may be ignored for a detached lane.

Use structured `list` with arguments `["--mine"]` when only lanes owned by this Claude orchestrator are relevant.
It matches process identity rather than the mutable Claude session ID and excludes persistent lanes;
the unfiltered list remains the authoritative view for host-local lifecycle state. Bare lifecycle
names can be ambiguous across otherwise group-isolated lanes, so retain and use exact IDs. `--mine` fails when
the caller is not a corroborated live Codex or Claude peer; it never guesses from a transient shell.

The runtime records this Claude Agent Sessions attachment as the immediate parent and notifies it
automatically. `lane.ready.owner_session_id` identifies that relationship; it is not necessarily
Claude's native transcript UUID. The unified daemon does not require `notify_target`, and
`--notify`/`--no-notify` are not lifecycle inputs. Their absence never selects a different
collection mode. The lifecycle owner is the corroborated Claude attachment, not the MCP connector
handling the call.
Pass `--persistent` only when the caller explicitly wants the lane to survive this orchestrator.
Auto-archive is the runtime default: after the latest
terminal turn it leaves a one-minute collection/message grace period. Pass
`--auto-archive-after SECONDS` only when the caller requests a different grace; the minimum and
persisted granularity are `0.001` seconds. Pass
`--no-auto-archive` only when the caller explicitly wants indefinite retention; combine it with
`--persistent` for a permanent lane. Never combine the two auto-archive flags.

Every lane keeps this immediate parent’s private group. Add `--inherit-groups` only when this
parent deliberately propagates its other groups; use repeatable `--group NAME` for child-specific
membership. Persistence does not remove the communication anchor.

### 2. Collect
Pick one mode. Modes A and B are both correct; C is a fallback.

**A. Push-notice driven.** Continue with other work. The lane's
terminal notice arrives as a peer message. If it carries `collection=required`,
follow its structured `agent_sessions.lane` `wait` hint. If it carries
`collection=none`, another collector already consumed the turn. The
message is status metadata, never a result.

Peer delivery is push-based. Do **not** poll `agent_sessions.check_inbox`, sleep, or block the
orchestrator waiting for the notice; continue useful work and the message will be injected
automatically. `check_inbox` is only a recovery tool for content queued past a delivery boundary.

Call `lane` with command `wait` and arguments
`["review-a", "--timeout", "120"]`.

It returns immediately because the turn is already terminal.

**B. Bounded collector.** When immediate collection is required, call `wait`
once with a bounded timeout. The timeout bounds only this collection call; it
never interrupts or changes the lane. On exit 124, make another bounded `wait`
call or inspect `status`.

**C. Polling.** Structured `status` with arguments `["review-a"]` returns one `lane.status` object. Use only when
neither of the above applies, and space the polls out.

Then parse the tool result's captured stdout and extract the answer:

1. Take the **last** `turn.completed`; read its `turn_id`, `outcome`, `exit`.
2. Find the `item.completed` whose `turn_id` equals it, with `item.type == "agent_message"` and
   `item.phase == "final_answer"`.
3. The answer is that item's `text`.

Any earlier `final_answer` followed by a `turn.schema_retry` is a **rejected draft** — discard it.
Matching on the terminal `turn_id` is what makes this safe.

Exit codes: `0` completed, `124` timed out, `130` interrupted, `1` everything else.

### 3. Talk to a running lane

Use the `agent-sessions` skill and `agent_sessions.send_message`. Address the
lane by its current visible name or exact host-qualified identity. Do not fall
back to Claude's native messaging when the structured tool fails. A message
wakes an idle lane or steers a turn already in flight. Message delivery does
not return the result; a turn started by an inbound message is collected by a
later `wait`.

For a remote lane, use its current host-qualified identity from Agent Sessions discovery. The
destination-local lane name and session ID are valid behind the product lane launcher with `--host` for lifecycle
commands; terminal notices include that remote collection command.

If Claude asks for confirmation with a current `name [ref]`, use that token only for the immediate
retry. Re-resolve immediately before every later send; short refs rotate and must never be stored.

### 4. Follow up, interrupt, retire

Call `resume` with arguments `["review-a", "-"]` and follow-up `input`, or call
`interrupt`/`archive` with arguments `["review-a"]`.

`resume` also unarchives an archived lane, so an archived lane with a recorded outcome should be
resumed rather than replaced with a fresh one. It starts a new follow-up turn and cannot recover an
uncollected answer lost to archive. `resume` is itself a blocking collector;
prefer a message-triggered follow-up or a bounded call when it may run long, and
do not start a concurrent `wait`. Collect any outstanding turn before resume because resume begins a new
collection cursor. If that collector is killed, a later `wait` during the terminal grace period
safely recovers the follow-up turn.
Always `archive` when the orchestration is done.

When Claude exits, ordinary lanes are interrupted if active and then archived automatically. A
`--persistent` lane survives its owner, but the default one-minute terminal grace still applies.
Use `--persistent --no-auto-archive` only when the lane must remain available indefinitely.

## Rules

- Exactly one `wait` consumer per lane. Never run two concurrently, never mix `wait` with another
  collector, and never use `wait` against an interactive `codex-peer` session.
- `wait` exiting `124` means the collection call timed out, **not** that the lane failed. Re-`wait`.
- A killed collector or broken pipe leaves the cursor unacknowledged; re-`wait` returns the same
  turn safely during the terminal grace period.
- Report `outcome` and `exit` to the caller alongside the answer. Do not present a `failed`,
  `interrupted`, or `timed_out` lane as a successful result.
- Never invent policy flags, and never retry a failed `start` blindly — read the `error` message.

## References

- `references/events.md` and `references/failures.md` — event fields and recovery taxonomy.
- `references/install.md` and `references/policy.md` — portable installation and caller-owned flags.
