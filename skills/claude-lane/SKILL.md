---
name: claude-lane
description: Spawn, supervise, message, collect, resume, and archive durable local or remote Claude Code worker lanes from Codex. Use when the user asks Codex to delegate work to Claude, compare a Claude implementation or review, keep a named Claude worker available for follow-up messages, orchestrate parallel Claude work, or run a Claude lane on a named federated host.
---

# Claude Lane

Use `claude-peer-lane` to run Claude Code as a named peer with durable lifecycle state. The lane remains addressable between turns, accepts ordinary peer messages, and returns machine-readable JSONL.

## Remote host

When the user requests another host, use federation instead of SSH. First run:

```bash
peer-federator status
peer-federator hosts
peer-federator lane --host HOST --product claude -- doctor --json
peer-federator lane --host HOST --product claude -- list --all
```

Require the local agent to be connected, the host to advertise `claude-lane`, and remote doctor
contract 1 to be healthy. A destination advertises this capability only after its operator has
explicitly enabled remote lane execution. Then replace every `claude-peer-lane` invocation below with:

```bash
peer-federator lane --host HOST --product claude --
```

For remote `run`, `start`, and `resume`, federation carries this live parent’s attested context and
returns terminal notices through grouped routing. Do not pass `--persistent`, `--notify`, `--no-notify`, or
`--no-auto-archive` for those commands. Remote lanes therefore have no lifecycle owner and are
excluded from `--mine`; use the remote plain `list`, names, or IDs. Their native JSONL, exit codes, collection rules, grace
timer, and archive behavior are otherwise unchanged.

Pass `-C /absolute/remote/path` on remote `run` and `start` whenever the working directory matters;
otherwise the native launcher inherits the destination agent service's cwd. `resume` retains the
lane's established cwd and rejects a replacement. Send remote briefings on stdin (maximum 1 MiB).
`--prompt-file` names an already-existing destination file; federation does not transfer it.
Remote auto-archive delay is capped at 86,400 seconds.

Every remote operation requires the hub. If it is disconnected, fail closed and report it; never fall back to SSH
or silently run locally. Peer messages and terminal pointers are pushed through
the hub, so continue useful work rather than polling.

## Local preflight

For a local lane, run:

```bash
claude-peer-lane doctor --json
claude-peer-lane list
```

Use `claude-peer-lane list --mine` to show only lanes owned by this orchestrator. Ownership uses
the stable process identity rather than a mutable session ID; persistent lanes are excluded. Keep
the unfiltered list for host-local lifecycle state. `--mine` fails rather than guessing when the
caller is not a corroborated live Codex or Claude peer.

Require `contract_version: 1`, `claude_available: true`, `claude_logged_in: true`, and
`supervisor_reachable: true`. This is a local credential-state preflight; require the first real
turn to succeed before claiming end-to-end authentication. Pick a descriptive name and retain the
exact lane ID: messaging names are group-scoped, while bare lifecycle names can be host-ambiguous.

## Start and collect

For a single foreground task:

```bash
claude-peer-lane run \
  --name review-api \
  --permission-mode dontAsk \
  --max-budget-usd 5 \
  --auto-archive-after 300 \
  - < brief.md
```

For detached parallel work:

```bash
claude-peer-lane start \
  --name review-api \
  --permission-mode dontAsk \
  --max-budget-usd 5 \
  --auto-archive-after 300 \
  - < brief.md
```

The owner receives a `CLAUDE_LANE_TERMINAL` pointer. It is not the answer. Collect immediately:

```bash
claude-peer-lane wait review-api --timeout 300
```

Peer messages are pushed into the active Codex turn automatically. Do **not** poll
`claude_peer.check_inbox`, sleep, or block waiting for a terminal pointer; continue other useful
work and collect when the pointer arrives. `check_inbox` is only for recovery of content that was
queued past a delivery boundary.

Use only one collector for a lane. Select the `agent_message` with `phase: "final_answer"` whose `turn_id` matches the final `turn.completed`. Branch on `turn.completed.outcome` and `exit`, not on the presence of prose.

## Follow-up and messages

An idle local lane is a normal Claude-visible peer. Send it a peer message by name or session ID,
then collect the new turn with `wait`. For a remote lane, use the same group-filtered Agent
Sessions discovery and send tools; no source-side shadow is created.
The destination-local name and session ID are valid only for `peer-federator lane` lifecycle commands.

A peer message accepted while a lane is already working steers that active turn and shares its one
result; it does not create another collection cursor. Only an idle-lane message starts a new turn.
Never reply conversationally to a `CLAUDE_LANE_TERMINAL` pointer—run its printed `wait` command.

Claude workers also receive `ListAgents` and `SendMessage` by default. They may discover and send
ordinary messages to any reachable local or federated peer, not only their lifecycle owner. An
explicit `--allowed-tools` list is extended with these tools; explicit `--tools` or
`--disallowed-tools` policy can still remove them.

Use explicit resume when the follow-up must be a CLI-owned turn:

```bash
claude-peer-lane resume review-api --timeout 600 - < follow-up.md
```

Collect every outstanding turn first. `resume` refuses collection debt rather than returning an older turn under the new turn's event sequence.

## Lifecycle

- Default lanes belong to the launching Codex/Claude session and are interrupted and archived when that owner exits.
- Completed lanes auto-archive after 60 seconds by default. Pass `--auto-archive-after S` when the orchestrator needs a larger collection window.
- Pass `--no-auto-archive` only when you will explicitly archive the lane.
- Pass `--persistent` to survive the owner. Explicit `--notify` is allowed only with `--persistent`.
- Every lane keeps the immediate parent anchor. Add `--inherit-groups` only when the parent
  deliberately propagates its other groups; use repeatable `--group NAME` for child-only groups.
- `wait --timeout` bounds collection only and never interrupts Claude. `run/start/resume --timeout` is a durable turn deadline.
- Launcher submission acknowledgement is bounded at 30 seconds whenever no native turn is active; a missing replay frame then fails and retires the worker. Native peer turns have no caller-supplied execution deadline and can defer that check until they finish.
- Always archive a persistent or no-auto-archive lane when the task ends:

```bash
claude-peer-lane archive review-api
```

## Policy and cost

Claude headless workers cannot answer approval prompts. The default is `--permission-mode dontAsk`.
Workers explicitly set `crossSessionInbound` to `accept` so native peer messages do not wait for a
headless approval UI.
If a corroborated owner is already in bypass mode and no explicit mode was supplied, its Claude
lane inherits `bypassPermissions` so its outbound native messages remain within the owner's
permission class.
Otherwise use bypass only when the user granted that authority. There is no fake Codex sandbox
mapping.

Claude lane turns load Claude context and can be materially more expensive than Codex lanes. Set
`--max-budget-usd` for autonomous work. Claude 2.1.227 does not publish a native peer socket in
`--bare` mode, so the lane CLI rejects `--bare` rather than exposing a non-messageable worker.

Read [references/contract.md](references/contract.md) for event fields, failure handling, and Claude-specific divergences.
