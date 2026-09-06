---
name: claude-lane
description: Orchestrate named, messageable local or remote Claude Code lanes from Claude Code. Use for self-delegation, parallel Claude reviews, follow-ups, lifecycle control, or a Claude lane on a federated host.
---

# Orchestrate Claude lanes

Use the process-attested `sessionbus.lane` MCP tool. The parent happens to
be Claude, while the target adapter is the ordinary Sessionbus Claude lane
runtime. This is intentionally separate from Claude-native subagents and makes
the same orchestration instructions portable to Codex, Grok, and Qwen parents.

Call `lane` with `product: "claude"`, `command: "doctor"`, and arguments
`["--json"]`; then call `list` with `["--all"]`. Require contract version 2,
`ready: true`, and a logged-in Claude runtime.

Start detached work with one structured call:

```json
{
  "product": "claude",
  "command": "start",
  "arguments": ["--name", "review-a", "-"],
  "input": "<briefing text>"
}
```

Do not execute `claude-peer-lane` through Bash and do not shell-background a
foreground command. `start` returns after registration; the Sessionbus
daemon owns the background worker. Add model, effort, permission, budget,
schema, worktree, or tool flags only when the caller supplied that policy.

The child always joins its own private group and this immediate parent’s
private group. Add `--inherit-groups` only when deliberately propagating the
parent’s other groups; use repeatable `--group NAME` for child-specific groups.
`--no-inherit-groups` resets optional inheritance without removing the anchor.
Omitted resume flags restore the durable choice.

Discover and message the child through the `sessionbus` skill using
`sessionbus.list_peers` and `sessionbus.send_message`. The lane is also
a real native Claude registry row, but native Claude messaging is not an Agent
Sessions fallback; report a structured-tool failure instead of switching
channels. Only Sessionbus discovery and routing are group-filtered.

For a remote target set `host` on the same `sessionbus.lane` call after
status, host discovery, and remote doctor. Never fall back to SSH. Federation
supplies an attested parent context and grouped terminal notices.

`lane.ready` and `CLAUDE_LANE_TERMINAL` are pointers, not answers. Use exactly
one collector: after the terminal pointer, call `lane` with command `wait` and
arguments `["review-a", "--timeout", "300"]`, then parse its returned stdout.
Match terminal/result turn IDs, collect debt before `resume`, and call `archive`
when done. `--persistent` changes lifecycle ownership but never removes the
parent communication anchor or disables the normal 60-second terminal
auto-archive grace. Pair it with `--no-auto-archive` only for indefinite idle
retention, then archive explicitly.
