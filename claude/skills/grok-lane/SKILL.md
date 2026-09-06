---
name: grok-lane
description: Orchestrate named, messageable local or remote Grok Build worker lanes from Claude Code — start detached work, collect a final answer, send follow-ups, resume the exact conversation, interrupt it, and archive it. Use when asked to delegate work to Grok, compare a Grok result, or use a named federated host.
---

# Orchestrate Grok lanes

Use the process-attested `sessionbus.lane` MCP tool with `product: "grok"`.
It runs a headless Grok Build conversation as a daemon-owned durable Agent
Sessions peer. Do not execute `grok-peer-lane` through Bash.

## Remote host

For another host, use the same structured calls with `host` set. First call
`doctor` with arguments `["--json"]` and `list` with `["--all"]`.

Require capability `grok-lane` and doctor contract 2. Federation carries this Claude parent’s
attested context and returns terminal notices through grouped routing; never pass `--persistent`
or `--no-auto-archive` remotely. Use a remote absolute `-C` when cwd
matters. Hub disconnect is a hard failure; never fall back to SSH or local execution.

## Preflight

Call `doctor` with arguments `["--json"]`. Require contract version 2,
`runtime_ready: true`, and a ready native Grok adapter.
The nested `supervisor_reachable` field is diagnostic only because the Grok manager directly owns
ACP. Check `list --all` output plus current Sessionbus discovery, and retain the exact lane ID.
Messaging names are group-scoped; bare
lifecycle names can still be ambiguous in host-local lane state.

Headless Grok cannot approve prompts. Every lane is explicit `always-approve`
(`bypassPermissions`). Start it only when the user's task authorizes autonomous execution in the
selected cwd.

## Start, collect, and message

Pass the briefing as MCP `input`, never in `arguments`:

```json
{
  "product": "grok",
  "command": "start",
  "arguments": ["--name", "review-a", "--auto-archive-after", "300", "-"],
  "input": "<briefing text>"
}
```

`start` returns at `lane.ready`, not at the answer. Use one collector. A durable
`GROK_LANE_TERMINAL` pointer tells the owner when to collect; it is not the answer. A wait timeout exits 124 but
does not interrupt the turn. From the collected JSONL, select the `agent_message` final answer with
the same `turn_id` as the last `turn.completed`, and report `outcome` and `exit`.

The lane is a grouped peer. Use the `sessionbus` skill and
`sessionbus.send_message`; do not fall back to Claude's native messaging or
address a synthetic Grok row in Claude's registry. Then collect the resulting
serialized turn with `wait`. A message during active work is durably queued for
the next turn; it never creates a competing ACP driver.

For an explicit follow-up, call `resume` with arguments
`["review-a", "--timeout", "600", "-"]` and the follow-up as `input`.

Collect all debt first. Resume also unarchives and retains the exact UUID, transcript, cwd, name,
model, and reasoning policy.

## Retire safely

Call `status`, `interrupt`, and finally `archive` through the same lane tool,
passing `["review-a"]` as their arguments.

Default lanes are owned by this corroborated Claude process and archive when it exits. Use
`--persistent` only when the lane must survive its owner. It disables parent-exit cleanup only;
the normal 60-second terminal auto-archive grace still applies. Pair it with
`--no-auto-archive` only for indefinite idle retention, then archive explicitly.

Every child retains this immediate parent’s private group. Add `--inherit-groups` only when this
parent intentionally propagates its other groups; repeat `--group NAME` for child-specific groups.

Read [references/contract.md](references/contract.md) for the complete contract and
[references/install.md](references/install.md) when preflight reports a missing or incompatible
runtime.
