---
name: agent-lanes
description: Start, collect, message, resume, and archive durable Codex or Claude worker lanes from Grok. Use when the user asks Grok to delegate work to Codex or Claude, compare implementations, run parallel reviews, or keep a named worker available for follow-up messages.
---

# Agent lanes

Use Agent Sessions lane CLIs to delegate from Grok without attaching a second driver to this Grok
conversation. Choose `codex-peer-lane` for Codex work or `claude-peer-lane` for Claude Code work.

## Preflight

```sh
codex-peer-lane doctor --json
claude-peer-lane doctor --json
codex-peer-lane list --all
claude-peer-lane list --all
```

Codex lanes require contract version **2**. Claude lanes require contract version **1** and
`claude_logged_in: true`. Require each doctor to report its runtime/supervisor ready before using
that product. Fail closed: if a required field is false or missing, do not start that product and
report the lane cell as blocked; never "try anyway." Do not apply one product's contract version to
the other.

For a remote host, use `peer-federator lane --host HOST --product codex --` or `--product claude
--` only after the host advertises that capability. Never fall back to SSH. Remote lifecycle and
message delivery require the hub.

## Start and collect

Never place a briefing on argv. Pipe stdin:

```sh
codex-peer-lane start --name codex-review - < brief.md
claude-peer-lane start --name claude-review --permission-mode dontAsk - < brief.md
```

`lane.ready` means the worker is addressable; it is not the answer. Collect each lane with one
consumer:

```sh
codex-peer-lane wait codex-review --timeout 300 > codex.jsonl
claude-peer-lane wait claude-review --timeout 300 > claude.jsonl
```

Take the last `turn.completed`, then the `agent_message` final answer with the same `turn_id`.
Report outcome and exit. A wait timeout exits 124 without interrupting the worker.

## Message, follow up, and retire

Use the installed `agent_sessions` tools to send ordinary messages to a lane's current peer name or
session identity. A terminal pointer still requires `wait`; do not answer it conversationally.

```sh
codex-peer-lane resume codex-review - < follow-up.md
claude-peer-lane resume claude-review - < follow-up.md
codex-peer-lane interrupt codex-review
claude-peer-lane interrupt claude-review
codex-peer-lane archive codex-review
claude-peer-lane archive claude-review
```

Collect outstanding debt before resume. Resume preserves the same transcript. Default lanes belong
to this corroborated Grok peer and archive when it exits; use `--persistent` only when survival is
required. Always archive persistent or no-auto-archive lanes when finished.

Codex policy flags belong to the caller. Claude headless defaults to `dontAsk`; bypass only with
explicit authority. Read [references/contract.md](references/contract.md) for product-specific
events and lifecycle differences.
