---
name: qwen-lane
description: Orchestrate named, messageable local or remote Qwen Code worker lanes from Claude Code. Use for Qwen delegation, reviews, comparisons, follow-ups, and federated Qwen targets.
---

# Orchestrate Qwen lanes

Use the process-attested `sessionbus.lane` MCP tool with `product: "qwen"`.
The daemon owns a durable native Qwen transcript and exposes it as an Agent
Sessions peer. Qwen remains the authority for its approval mode. Do not execute
`qwen-peer-lane` through Bash.

Call `doctor` with arguments `["--json"]` and `list` with `["--all"]` first. Require
contract version 2 and `ready: true`; do not launch when version, ACP, archive,
trusted-cwd, profile, integration, or non-secret credential configuration evidence
is missing. Readiness is session-free and does not prove a live provider login.

For another host set `host` on the same structured lane calls after
confirming `qwen-lane` capability. Never use SSH or silently fall back locally.
Federation owns lifecycle, so omit `--persistent` and
`--no-auto-archive` remotely.

```json
{
  "product": "qwen",
  "command": "start",
  "arguments": ["--name", "qwen-review", "--no-yolo", "--auto-archive-after", "300", "-"],
  "input": "<briefing text>"
}
```

Never put a briefing on argv. `start` returns `lane.ready`, not the answer. Use one
collector, then select the final agent message matching the last `turn.completed`.
A timeout exits 124 without cancelling. Execute a delivered
`QWEN_LANE_TERMINAL` structured collection hint through `sessionbus.lane`, preserving
its product, command, lane identity, host, and timeout arguments. The notice
itself is not an answer.

Use `--yolo` only with explicit authority, `--no-yolo` for native initial
`default`, or a supported `--approval-mode MODE`. Qwen may change mode natively
after launch; status reports the last corroborated mode or `unknown`.

The lane is messageable through the `sessionbus` skill. Messages serialize as
collectable turns. Collect debt before follow-up:

Call `resume` with arguments `["qwen-review", "--timeout", "600", "-"]`
and the follow-up as `input`. Call `interrupt` and `archive` with
`["qwen-review"]`.

Resume preserves the exact native Qwen UUID. Interrupt returns 130. Archive is
idempotent and retires manager, worker, tool roots, helper, sockets, and grouped
publication. Default lanes belong to this exact Claude parent; persistent lanes
survive parent exit but retain the normal 60-second terminal auto-archive grace.
Pair `--persistent` with `--no-auto-archive` only for indefinite idle retention,
then archive explicitly. Use `--inherit-groups` only deliberately and repeat
`--group NAME` for child-specific groups.
