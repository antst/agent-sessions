---
name: claude-lane
description: Orchestrate named, messageable local or remote Claude Code lanes from Claude Code. Use for self-delegation, parallel Claude reviews, follow-ups, lifecycle control, or a Claude lane on a federated host.
---

# Orchestrate Claude lanes

Use `claude-peer-lane`; the parent happens to be Claude, while the target
adapter is still the ordinary Claude lane runtime. Run `claude-peer-lane doctor
--json` and `list --all`, require contract version 1 and a logged-in Claude
runtime, then pipe prompts on stdin:

```sh
claude-peer-lane start --name review-a --permission-mode dontAsk - < brief.md
claude-peer-lane wait review-a --timeout 300 > result.jsonl
```

The child always joins its own private group and this immediate parent’s
private group. Add `--inherit-groups` only when deliberately propagating the
parent’s other groups; use repeatable `--group NAME` for child-specific groups.
`--no-inherit-groups` resets optional inheritance without removing the anchor.
Omitted resume flags restore the durable choice.

Discover and message the child through the `agent-sessions` skill: send the
complete `AGENT_SESSIONS_FRAME `-prefixed AgentFrame body to the single host-agent service projected into
the shared Claude profile. The lane is also a real native Claude registry row;
only AgentFrame discovery and routing are group-filtered.

For a remote target use `peer-federator lane --host HOST --product claude --`
after `status`, `hosts`, and remote doctor. Never fall back to SSH. Federation
supplies an attested parent context and grouped terminal notices.

`lane.ready` and `CLAUDE_LANE_TERMINAL` are pointers, not answers. Use one
collector, match terminal/result turn IDs, collect debt before `resume`, and
archive persistent/no-auto-archive lanes. `--persistent` changes lifecycle
ownership but never removes the parent communication anchor.
