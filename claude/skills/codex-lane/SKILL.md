---
name: codex-lane
description: Orchestrate named, messageable local or remote Codex lanes — start a detached lane, collect its final answer, message or steer a running lane, resume the same transcript, and archive it. Use when asked to delegate work to Codex, run a Codex agent in the background, fan out parallel Codex workers, use a named federated host, check on or steer a running lane, or collect a lane result.
---

# Orchestrate Codex lanes

A lane is a named Codex thread on the local shared App Server. It registers with
the Agent Sessions host agent and is visible only to peers sharing one of its groups. Claude's
shared native registry contains ordinary/managed Claude rows and the single host-agent service,
not synthetic Codex rows. `--persistent` is an explicit opt-in for a lane that must survive its owner.

This skill is a pass-through to one CLI. It owns no policy: model, reasoning effort, sandbox,
approval, web access, config overlays, output schema, and worktree isolation are all decided by
whoever asked for the lane. See `references/policy.md`.

## Remote host

When the user requests another host, use federation instead of SSH. Run `peer-federator status`,
`peer-federator hosts`, and:

```
peer-federator lane --host HOST --product codex -- doctor --json
peer-federator lane --host HOST --product codex -- list --all
```

Require the local agent to be connected, the destination to advertise `codex-lane`, and remote
doctor contract 2 to be healthy. A destination advertises this capability only after its operator
explicitly enables remote lane execution. For the rest of this skill, replace `codex-peer-lane` with:

```
peer-federator lane --host HOST --product codex --
```

Do not run the local lane-preflight for a remote destination. Federation carries this live
Claude parent’s attested context and returns terminal notices through grouped routing; never pass
`--persistent`, `--notify`, `--no-notify`, or `--no-auto-archive` for those
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

Run `/agent-sessions:doctor`, or directly:

```
"${CLAUDE_PLUGIN_ROOT}/skills/codex-lane/scripts/lane-preflight"
```

Because the script is inside this skill, the same path relative to the skill directory works when
the skill is copied into `~/.claude/skills`. It reads the installed runtime marker and calls the
native binary directly, so it never bootstraps or changes host services. It reports whether the
runtime is reachable, the exact validated `invocation`, and
whether the runtime satisfies **contract version 2** (`list`, `doctor --json`, and
`contract_version` in `lane.ready`).

- Runtime missing: stop and print the user-facing commands from `references/install.md`. Never try
  to install, build, or start anything yourself.
- Runtime older than contract 2: say so and stop. Do not fall back to guessing the event shape.
- Compatible contract but `runtime_ready: false`: report which service is unreachable and stop.
  Host runtime recovery is not an orchestrator action.
- Use the report's exact `invocation` in place of `codex-peer-lane` in every command below. It
  names the same native binary that preflight validated; do not substitute a different launcher.

## The one constraint that shapes everything
A `Bash` call times out at 120s by default and 600s at most. A long lane can never be collected by
a blocking foreground command. Always `start` (which returns as soon as the lane is registered),
then collect separately.

Never use `run` for a detached lane, and never shell-background `run`.

## Workflow

### 1. Start
Write the briefing to a file first; never put a prompt on argv.

```
codex-peer-lane start \
  --name review-a \
  - < brief.md
```

Only `--name` is required. Add policy flags **only** when the caller specified them.

Pick a stable, role-based kebab-case name and check `codex-peer-lane list --all` plus an Agent
Sessions `discover` request from this session. The same name may exist in disjoint groups; a
messaging ambiguity matters only when it is visible to this sender. Parse stdout JSONL until
`{"type":"lane.ready"}`; retain its `thread_id` and confirm
`contract_version` is 2. Everything after `lane.ready` may be ignored for a detached lane.

Use `codex-peer-lane list --mine` when only lanes owned by this Claude orchestrator are relevant.
It matches process identity rather than the mutable Claude session ID and excludes persistent lanes;
the unfiltered list remains the authoritative view for host-local lifecycle state. Bare lifecycle
names can be ambiguous across otherwise group-isolated lanes, so retain and use exact IDs. `--mine` fails when
the caller is not a corroborated live Codex or Claude peer; it never guesses from a transient shell.

The runtime records this Claude process as owner and notifies it automatically. Use mode A when
`lane.ready.notify_target` is non-null and mode B otherwise. Pass `--no-notify` only when requested.
The lifecycle owner is this corroborated Claude session process, not the Bash subprocess executing
the command.
Pass `--persistent` only when the caller explicitly wants the lane to survive this orchestrator;
only then may `--notify PEER` be forwarded. Auto-archive is the runtime default: after the latest
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

**A. Notify-driven (preferred when `lane.ready.notify_target` is non-null).** Continue with other work. The lane's
terminal notice arrives as a peer message carrying `collection=required` and the exact `wait`
command. That message is a *pointer, never a result*. On arrival:

Peer delivery is push-based. Do **not** poll `agent_sessions.check_inbox`, sleep, or block the
orchestrator waiting for the notice; continue useful work and the message will be injected
automatically. `check_inbox` is only a recovery tool for content queued past a delivery boundary.

```
codex-peer-lane wait review-a --timeout 120 > lane.jsonl
```

It returns immediately because the turn is already terminal.

**B. Background collector.** Start a background `Bash` call and let it finish on its own:

```
codex-peer-lane wait review-a --timeout 540 > lane.jsonl
```

Here `wait --timeout` bounds only this collection call; it never interrupts or changes the lane.
On exit 124, start another bounded `wait` or inspect `status`.

**C. Polling.** `codex-peer-lane status review-a` returns one `lane.status` object. Use only when
neither of the above applies, and space the polls out.

Then `Read` `lane.jsonl` and extract the answer:

1. Take the **last** `turn.completed`; read its `turn_id`, `outcome`, `exit`.
2. Find the `item.completed` whose `turn_id` equals it, with `item.type == "agent_message"` and
   `item.phase == "final_answer"`.
3. The answer is that item's `text`.

Any earlier `final_answer` followed by a `turn.schema_retry` is a **rejected draft** — discard it.
Matching on the terminal `turn_id` is what makes this safe.

Exit codes: `0` completed, `124` timed out, `130` interrupted, `1` everything else.

### 3. Talk to a running lane

Use the `agent-sessions` skill and send a complete `AGENT_SESSIONS_FRAME `-prefixed AgentFrame body to the one host-agent
service. Address the lane by its current visible name or exact host-qualified identity. A message
wakes an idle lane or steers a turn already in flight. Message delivery does not return the result;
a turn started by an inbound message is collected by a later `wait`.

For a remote lane, use its current host-qualified identity from Agent Sessions discovery. The
destination-local lane name and session ID are valid behind `peer-federator lane` for lifecycle
commands; terminal notices include that remote collection command.

If Claude asks for confirmation with a current `name [ref]`, use that token only for the immediate
retry. Re-resolve immediately before every later send; short refs rotate and must never be stored.

### 4. Follow up, interrupt, retire

```
codex-peer-lane resume review-a - < followup.md   # same transcript, new turn
codex-peer-lane interrupt review-a                # stop the active turn
codex-peer-lane archive review-a                  # retire the peer; idempotent
```

`resume` also unarchives an archived lane, so an archived lane with a recorded outcome should be
resumed rather than replaced with a fresh one. It starts a new follow-up turn and cannot recover an
uncollected answer lost to archive. `resume` is itself a blocking collector; run its
`Bash` call in the background when the follow-up can exceed the tool limit, and do not start a
concurrent `wait`. Collect any outstanding turn before resume because resume begins a new
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
