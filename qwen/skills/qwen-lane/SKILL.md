---
name: qwen-lane
description: Start, collect, message, resume, and archive durable local or remote Qwen Code lanes from a managed Qwen parent. Use for Qwen delegation, reviews, follow-ups, and federated targets.
---

# Qwen lanes from Qwen

Use the attested `sessionbus.lane` MCP tool. Set `product` to `qwen`, the
lifecycle verb in `command`, native arguments in `arguments`, and the briefing in
`input`. Add `host` only for a target advertising `qwen-lane`; federation owns
lifecycle flags and never falls back locally.

Run `doctor --json` and `list --all`; require contract version 2 and `ready: true`
with Qwen >= 0.21.15, ACP session/MCP capabilities, native archive, trusted cwd,
selected-profile identity, and installed integration. Start with:

```json
{"product":"qwen","command":"start","arguments":["--name","qwen-review","--no-yolo","-"],"input":"BRIEFING"}
```

Use `--yolo` only with explicit authority or a supported native
`--approval-mode`; Qwen may change mode later. Use one `wait` collector and match
the final answer to the last terminal turn. Collect debt before `resume`, which
preserves the exact native Qwen UUID. Use `interrupt` for exit 130 and idempotent
`archive` for process/socket/tool-root/helper cleanup. Default lanes bind to this
exact Qwen parent. `--persistent` disables parent-exit cleanup only; the normal
60-second terminal auto-archive grace still applies. Pair it with
`--no-auto-archive` only for indefinite idle retention, then archive explicitly.
Inherit groups only deliberately.
