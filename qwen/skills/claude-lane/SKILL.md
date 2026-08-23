---
name: claude-lane
description: Start, collect, message, resume, and archive durable local or remote Claude Code lanes from a managed Qwen parent. Use for Claude delegation, reviews, follow-ups, and federated targets.
---

# Claude lanes from Qwen

Use the attested `agent_sessions.lane` MCP tool. Set `product` to `claude`, the
lifecycle verb in `command`, native arguments in `arguments`, and the briefing in
`input`. Add `host` only for a target advertising `claude-lane`; never use SSH or
local fallback.

Run `doctor --json` and `list --all`; require contract version 1,
`claude_logged_in: true`, and profile/runtime readiness. Start with:

```json
{"product":"claude","command":"start","arguments":["--name","claude-review","--permission-mode","dontAsk","-"],"input":"BRIEFING"}
```

Use one `wait` collector and select the final message matching the last
`turn.completed`. A timeout exits 124 without cancelling; a terminal notice is
only a collection pointer. Collect debt before `resume`. Use `interrupt` and
idempotent `archive`; archive persistent lanes explicitly. Default lanes bind to
this exact Qwen parent. Inherit groups deliberately. Claude permission mode
remains Claude-owned; bypass requires explicit authority.
