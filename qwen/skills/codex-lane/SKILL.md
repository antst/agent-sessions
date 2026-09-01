---
name: codex-lane
description: Start, collect, message, resume, and archive durable local or remote Codex lanes from a managed Qwen parent. Use for Codex delegation, reviews, follow-ups, and federated targets.
---

# Codex lanes from Qwen

Use the attested `agent_sessions.lane` MCP tool; do not shell-execute a launcher.
Set `product` to `codex`, put the lifecycle verb in `command`, native arguments in
`arguments`, and the briefing in `input`. Add `host` only for an explicit target
with `codex-lane` capability; never fall back locally.

Run `doctor --json` and `list --all`; require contract version 2 and readiness.
Start with:

```json
{"product":"codex","command":"start","arguments":["--name","codex-review","-"],"input":"BRIEFING"}
```

Call `wait` exactly once and select the final agent message matching the last
`turn.completed`. A timeout exits 124 without cancellation; a terminal notice is
a collection pointer, not an answer. Send ordinary follow-ups through Agent
Sessions or call `resume` after collecting debt. Use `interrupt` and `archive`.
Default lanes belong to this exact Qwen parent. `--persistent` disables
parent-exit cleanup only; the normal 60-second terminal auto-archive grace still
applies. Pair it with `--no-auto-archive` only for indefinite idle retention,
then archive explicitly. Inherit groups only deliberately. Codex policy remains Codex-owned.
