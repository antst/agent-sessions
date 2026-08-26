---
name: qwen-lane
description: Orchestrate named, messageable local or remote Qwen Code worker lanes from Claude Code. Use for Qwen delegation, reviews, comparisons, follow-ups, and federated Qwen targets.
---

# Orchestrate Qwen lanes

`qwen-peer-lane` owns a durable native Qwen transcript and exposes it as an Agent
Sessions peer. Qwen remains the authority for its approval mode.

Run `qwen-peer-lane doctor --json` and `qwen-peer-lane list --all` first. Require
contract version 1 and `ready: true`; do not launch when version, ACP, archive,
trusted-cwd, profile, integration, or non-secret credential configuration evidence
is missing. Readiness is session-free and does not prove a live provider login.

For another host use `peer-federator lane --host HOST --product qwen --` after
confirming `qwen-lane` capability. Never use SSH or silently fall back locally.
Federation owns lifecycle, so omit `--persistent`, `--notify`, `--no-notify`, and
`--no-auto-archive` remotely.

```sh
qwen-peer-lane start --name qwen-review --no-yolo --auto-archive-after 300 - < brief.md
qwen-peer-lane wait qwen-review --timeout 300
```

Never put a briefing on argv. `start` returns `lane.ready`, not the answer. Use one
collector, then select the final agent message matching the last `turn.completed`.
A timeout exits 124 without cancelling. Execute a delivered
`QWEN_LANE_TERMINAL` `Collect:` command verbatim; the notice itself is not an
answer.

Use `--yolo` only with explicit authority, `--no-yolo` for native initial
`default`, or a supported `--approval-mode MODE`. Qwen may change mode natively
after launch; status reports the last corroborated mode or `unknown`.

The lane is messageable through the `agent-sessions` skill. Messages serialize as
collectable turns. Collect debt before follow-up:

```sh
qwen-peer-lane resume qwen-review --timeout 600 - < follow-up.md
qwen-peer-lane interrupt qwen-review
qwen-peer-lane archive qwen-review
```

Resume preserves the exact native Qwen UUID. Interrupt returns 130. Archive is
idempotent and retires manager, worker, tool roots, helper, sockets, and grouped
publication. Default lanes belong to this exact Claude parent; persistent lanes
must be explicitly archived. Use `--inherit-groups` only deliberately and repeat
`--group NAME` for child-specific groups.
