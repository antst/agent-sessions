---
name: grok-lane
description: Spawn, supervise, message, collect, resume, and archive durable local or remote Grok Build worker lanes from Codex. Use when the user asks Codex to delegate work to Grok, compare a Grok implementation or review, keep a named Grok worker available for follow-up messages, orchestrate parallel Grok work, or run a Grok lane on a named federated host.
---

# Grok Lane

Use `grok-peer-lane` to run a named, headless Grok Build conversation. A lane remains a normal
Agent Sessions peer between turns, accepts ordinary peer messages, and emits machine-readable
JSONL.

## Managed Codex execution boundary

From a managed Codex peer, run every lifecycle operation through the attested
`claude_peer.lane` MCP tool. Do not invoke `grok-peer-lane` from a shell tool:
the Codex OS sandbox is expected to deny the App Server, supervisor, and host-agent
Unix sockets even when their directories are writable. The MCP tool retains this
session as the exact parent and returns `exit`, `stdout`, and `stderr`.

Set `product` to `grok`, put the lifecycle verb in `command`, and pass only the
arguments after that verb in `arguments`. Pass the briefing as `input`; do not use
shell redirection. Supply the current session ID injected by SessionStart. Example:

```json
{"product":"grok","command":"start","arguments":["--name","review-api","-"],"input":"Review the API and return a concise finding.","session_id":"CURRENT_SESSION_ID"}
```

For federation, add `"host":"HOST"` to the same call. The CLI examples below
define native arguments for host-shell use; translate them to this MCP shape when
operating as a Codex peer.

## Remote host

When the user requests another host, use federation instead of SSH:

```bash
peer-federator status
peer-federator hosts
peer-federator lane --host HOST --product grok -- doctor --json
peer-federator lane --host HOST --product grok -- list --all
```

Require a connected hub, destination capability `grok-lane`, and healthy contract version 1. Then
replace each `grok-peer-lane` command below with
`peer-federator lane --host HOST --product grok --`. Federation carries the attested parent context
and returns terminal notices through grouped routing; do not pass `--persistent`, `--notify`,
`--no-notify`, or `--no-auto-archive`. Pass `-C /absolute/remote/path` when cwd matters. Remote stdin
is capped at 1 MiB; `--prompt-file` refers to a destination-local file. On disconnect fail closed;
never use SSH or silently run locally.

## Preflight

Run once before starting work:

```bash
grok-peer-lane doctor --json
grok-peer-lane list --all
```

Require `contract_version: 1`, `grok_available: true`, and no `grok_error`.
`supervisor_reachable` is diagnostic only: the Grok manager directly owns ACP and supports
persistent lanes from a plain host shell without the shared Codex App Server. The launcher validates
the exact Grok Build executable and rejects a chat-only Grok product or macOS application helper.
Pick a unique descriptive name after checking the unfiltered list and current peer discovery.

Headless Grok cannot answer approval prompts. Every lane uses explicit `always-approve`
(`bypassPermissions`). Start one only when the user's request authorizes autonomous execution in
the selected cwd; there is no prompting-mode fallback.

## Start and collect

For a foreground task:

```bash
grok-peer-lane run --name review-api --timeout 600 - < brief.md
```

For detached work:

```bash
grok-peer-lane start --name review-api --auto-archive-after 300 - < brief.md
grok-peer-lane wait review-api --timeout 300 > lane.jsonl
```

Never put the briefing on argv. Use stdin or `--prompt-file`. `start` returns after `lane.ready`;
it does not return the answer. Use exactly one `wait` collector. A wait timeout exits 124 and does
not cancel the Grok turn, so collect again later.

Read the last `turn.completed`, then select the `item.completed` agent message with the same
`turn_id` and `phase: "final_answer"`. Report its `outcome` and `exit` with the answer. A
`lane.ready` event is only a startup pointer, never a result. The owner also receives a
`GROK_LANE_TERMINAL` pointer; continue useful work until it arrives, then run its exact `wait`
command. Do not treat the pointer as the answer.

## Messages and follow-up

The live lane is an ordinary peer. Send messages by its current name or session identity through
Agent Sessions. An idle message creates a serialized collectable turn; a message arriving during a
turn is durably queued for the next turn rather than opening a second ACP writer. Collect each turn
with `wait`.

For an explicit follow-up:

```bash
grok-peer-lane resume review-api --timeout 600 - < follow-up.md
```

Collect all outstanding results before resume. `resume` also unarchives the exact Grok session and
uses `session/load`; it preserves the UUID, transcript, cwd, name, model, and reasoning policy.

## Lifecycle

```bash
grok-peer-lane status review-api
grok-peer-lane interrupt review-api
grok-peer-lane archive review-api
```

- Default lanes belong to the launching Codex, Claude, or Grok peer and archive after that exact owner
  exits. A plain shell must use `--persistent`.
- Every lane keeps its immediate parent anchor. Parent groups propagate only after explicit
  `--inherit-groups`; repeat `--group NAME` for child-specific groups.
- Completed lanes auto-archive after 60 seconds. Extend it with `--auto-archive-after`; use
  `--no-auto-archive` only when you will explicitly archive.
- `interrupt` sends ACP `session/cancel`; collect the interrupted terminal event afterward.
- Archive is bridge-owned and idempotent: it withdraws the peer, worker, MCP children, sockets,
  launch record, and registry state while retaining the native transcript for exact `session/load` resume.
- Always archive persistent or no-auto-archive lanes when orchestration ends.

Read [references/contract.md](references/contract.md) for event fields, outcomes, collection debt,
and the Grok-specific ACP ownership boundary.
