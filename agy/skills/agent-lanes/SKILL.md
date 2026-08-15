---
name: agent-lanes
description: Start, collect, resume, message, interrupt, and archive durable Codex or Claude Code worker lanes with the native Agent Sessions CLIs. Use when the user asks Antigravity to delegate work to a Codex or Claude worker, run an agent in the background, compare implementations or reviews, keep a named worker available for follow-up, or operate a lane on a federated host.
---

# Agent lanes

Use the existing `codex-peer-lane` and `claude-peer-lane` contracts. They own the
worker lifecycle; do not recreate it with raw `codex`, `claude`, shell jobs, or
custom transcript parsing.

Only start a lane when the user asked for delegation or a lane. Never contact,
resume, or repurpose an unrelated existing peer. Use a new role-based name and
operate only lanes created for the current task unless the user identifies an
existing lane explicitly.

## Choose the product

- Use `codex-peer-lane` when the requested worker is Codex.
- Use `claude-peer-lane` when the requested worker is Claude Code.
- For a remote host, use
  `peer-federator lane --host HOST --product codex --source-session SESSION_ID --`
  or the same command with product `claude` in place of the local binary. Use
  this Agy conversation's exact session ID from the startup context or call
  `agent_sessions.identity` without `session_id` to recover it from the attested
  MCP host. Automatic source inference is Codex/Claude-only.
  Never fall back to SSH or silently run locally.

Run `<binary> doctor --json` before the first local lane of each product. Require
`contract_version: 2` and a reachable runtime. For a remote lane, check
`peer-federator status`, `peer-federator hosts`, and the remote `doctor --json`.
Report a failed preflight; do not install, restart, or repair host services unless
the user separately asks.

## Start and collect

Write the briefing to a file or pipe it on stdin. Never place a potentially large
or sensitive briefing in argv.

For short work that can remain in the foreground:

```bash
codex-peer-lane run --name review-codex - < briefing.md
claude-peer-lane run --name review-claude - < briefing.md
```

For background work, always use `start` and collect separately; never shell-background
`run`:

```bash
codex-peer-lane start --name review-codex - < briefing.md
codex-peer-lane wait review-codex --timeout 300 > result.jsonl
```

Parse stdout as JSONL. Retain `lane.ready.thread_id` and require
`lane.ready.contract_version == 2`. To collect the answer:

1. Read the last `turn.completed` and its `turn_id`, `outcome`, and `exit`.
2. Select the `item.completed` with that `turn_id`, `type == "agent_message"`,
   and `phase == "final_answer"`.
3. Discard an earlier final answer followed by `turn.schema_retry`.
4. Report the outcome and exit code with the answer. Never present failed,
   interrupted, or timed-out work as success.

Exit `124` from `wait` means only that this collection call timed out. It does not
interrupt the lane; issue another bounded `wait` or inspect `status`. Never run two
collectors for one lane.

## Follow up and retire

```bash
codex-peer-lane status review-codex
codex-peer-lane resume review-codex - < followup.md
codex-peer-lane interrupt review-codex
codex-peer-lane archive review-codex
```

The same commands apply to Claude lanes. Collect any outstanding result before
`resume`, because resume begins a new turn. Archive lanes when the delegated work
is done.

An ordinary lane belongs to this live Antigravity conversation and is cleaned up
when it exits. Add `--persistent` only when the user explicitly wants the lane to
survive this conversation. Add `--no-auto-archive` only for deliberate indefinite
retention; otherwise collect within the reported terminal grace period.

Do not invent model, reasoning, sandbox, approval, permission, tool, web, budget,
schema, worktree, persistence, or notification flags. Pass them only when the
user requested the policy. Headless Codex cannot answer approval prompts; use
`--approval-policy never` only when autonomous permission was requested. Claude's
default is `dontAsk`; use `--permission-mode` only when requested.

## Messaging a lane

Once `lane.ready` is emitted, the lane is a normal Agent Sessions peer. Use the
`agent_sessions` MCP tools and the lane's exact name or session ID to steer it or
wake an idle lane. Re-resolve ambiguous names immediately before sending. A peer
message does not collect the result; use the lane's `wait` cursor afterward.
