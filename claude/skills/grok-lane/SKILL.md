---
name: grok-lane
description: Orchestrate named, messageable local or remote Grok Build worker lanes from Claude Code — start detached work, collect a final answer, send follow-ups, resume the exact conversation, interrupt it, and archive it. Use when asked to delegate work to Grok, compare a Grok result, or use a named federated host.
---

# Orchestrate Grok lanes

`grok-peer-lane` runs a headless Grok Build conversation as a durable Agent Sessions peer.

## Remote host

For another host, run `peer-federator status`, `peer-federator hosts`, then:

```sh
peer-federator lane --host HOST --product grok -- doctor --json
peer-federator lane --host HOST --product grok -- list --all
```

Require capability `grok-lane` and doctor contract 1. Replace every local invocation below with
`peer-federator lane --host HOST --product grok --`. Federation carries this Claude parent’s
attested context and returns terminal notices through grouped routing; never pass `--persistent`,
`--notify`, `--no-notify`, or `--no-auto-archive` remotely. Use a remote absolute `-C` when cwd
matters. Hub disconnect is a hard failure; never fall back to SSH or local execution.

## Preflight

Run the bundled read-only preflight:

```sh
"${CLAUDE_PLUGIN_ROOT}/skills/grok-lane/scripts/lane-preflight"
```

Require contract version 1, `runtime_ready: true`, `grok_available: true`, and no `grok_error`.
The nested `supervisor_reachable` field is diagnostic only because the Grok manager directly owns
ACP. Use the exact `invocation` reported by preflight. Check its `list --all` output plus current
Agent Sessions discovery, and retain the exact lane ID. Messaging names are group-scoped; bare
lifecycle names can still be ambiguous in host-local lane state.

Headless Grok cannot approve prompts. Every lane is explicit `always-approve`
(`bypassPermissions`). Start it only when the user's task authorizes autonomous execution in the
selected cwd.

## Start, collect, and message

Write prompts to a file or pipe stdin; never put them on argv:

```sh
grok-peer-lane start --name review-a --auto-archive-after 300 - < brief.md
grok-peer-lane wait review-a --timeout 300 > lane.jsonl
```

`start` returns at `lane.ready`, not at the answer. Use one collector. A durable
`GROK_LANE_TERMINAL` pointer tells the owner when to collect; it is not the answer. A wait timeout exits 124 but
does not interrupt the turn. From the collected JSONL, select the `agent_message` final answer with
the same `turn_id` as the last `turn.completed`, and report `outcome` and `exit`.

The lane is a grouped peer. Use the `agent-sessions` skill and send a complete AgentFrame JSON body
to Claude's one host-agent service; do not address a synthetic Grok row in Claude's registry. Then
collect the resulting serialized turn with `wait`. A message during active work is durably queued
for the next turn; it never creates a competing ACP driver.

For an explicit CLI-owned follow-up:

```sh
grok-peer-lane resume review-a --timeout 600 - < follow-up.md
```

Collect all debt first. Resume also unarchives and retains the exact UUID, transcript, cwd, name,
model, and reasoning policy.

## Retire safely

```sh
grok-peer-lane status review-a
grok-peer-lane interrupt review-a
grok-peer-lane archive review-a
```

Default lanes are owned by this corroborated Claude process and archive when it exits. Use
`--persistent` only when the lane must survive; use `--no-auto-archive` only with an explicit later
archive. Normal terminal grace is 60 seconds. Archive persistent lanes when finished.

Every child retains this immediate parent’s private group. Add `--inherit-groups` only when this
parent intentionally propagates its other groups; repeat `--group NAME` for child-specific groups.

Read [references/contract.md](references/contract.md) for the complete contract and
[references/install.md](references/install.md) when preflight reports a missing or incompatible
runtime.
