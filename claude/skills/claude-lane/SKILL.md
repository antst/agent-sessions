---
name: claude-lane
description: Orchestrate named, messageable local or remote Claude Code lanes from Claude Code. Use for self-delegation, parallel Claude reviews, follow-ups, lifecycle control, or a Claude lane on a federated host.
---

# Orchestrate Claude lanes

Use `claude-peer-lane`; the parent happens to be Claude, while the target
adapter is still the ordinary Claude lane runtime. Run `claude-peer-lane doctor
--json` and `list --all`, require `ready: true`, `authority: "daemon"`, and
product `claude`, then pipe prompts on stdin:

```sh
claude-peer-lane start --name review-a --permission-mode dontAsk - < brief.md
claude-peer-lane wait review-a --timeout 300 > result.jsonl
```

The child always joins its own private group and this immediate parent’s
private group. Add `--inherit-groups` only when deliberately propagating the
parent’s other groups; use repeatable `--group NAME` for child-specific groups.
`--no-inherit-groups` resets optional inheritance without removing the anchor.
Omitted resume flags restore the durable choice.

Discover and message the child through the `agent-sessions` skill using
`agent_sessions.list_peers` and `agent_sessions.send_message`. The lane is also
a real native Claude registry row, but native Claude messaging is not an Agent
Sessions fallback; report a structured-tool failure instead of switching
channels. Only Agent Sessions discovery and routing are group-filtered.

For a remote target use `agent-sessions lane --host HOST --product claude --`
after `agent-sessions status --json` and remote doctor. Require remote `ready: true`, authority
`remote-daemon`, the exact host, product `claude`, and advertised `claude-lane`. Remote `--mine`
matches this source-proxy parent and host; prompts use bounded stdin and remote `--prompt-file` is
unsupported. Never fall back to SSH. Federation supplies an attested parent context and grouped
terminal notices.

`lane.ready` and `CLAUDE_LANE_TERMINAL` are pointers, not answers. Use one
collector, match terminal/result turn IDs, collect debt before `resume`, and
archive persistent/no-auto-archive lanes. `--persistent` changes lifecycle
ownership but never removes the parent communication anchor.
