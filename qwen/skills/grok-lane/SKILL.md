---
name: grok-lane
description: Start, collect, message, resume, and archive durable local or remote Grok Build lanes from a managed Qwen parent. Use for Grok delegation, reviews, follow-ups, and federated targets.
---

# Grok lanes from Qwen

Use the attested `sessionbus.lane` MCP tool. Set `product` to `grok`, the
lifecycle verb in `command`, native arguments in `arguments`, and the briefing in
`input`. Add `host` only for a target advertising `grok-lane`; never use SSH or
local fallback.

Run `doctor --json` and `list --all`; require contract version 2,
`grok_available: true`, and no `grok_error`. Headless Grok uses
`bypassPermissions`, so launch only with autonomous execution authority. Start:

```json
{"product":"grok","command":"start","arguments":["--name","grok-review","-"],"input":"BRIEFING"}
```

Use one `wait` collector and match the final answer to the last terminal turn.
Collect debt before `resume`; use `interrupt` and idempotent `archive`. Default
lanes bind to this exact Qwen parent. `--persistent` disables parent-exit cleanup
only; the normal 60-second terminal auto-archive grace still applies. Pair it
with `--no-auto-archive` only for indefinite idle retention, then archive
explicitly. Inherit groups only deliberately.
