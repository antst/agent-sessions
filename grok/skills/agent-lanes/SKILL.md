---
name: agent-lanes
description: Start, collect, message, resume, and archive durable Codex, Claude, Grok, or Qwen worker lanes from Grok. Use for delegation to any supported target product, local or remote parallel work, and follow-ups.
---

# Agent lanes

Use the attested `sessionbus.lane` MCP tool to delegate from Grok without attaching a second
driver to this Grok conversation. It executes the exact packaged runtime, retains this Grok peer as
the lifecycle and communication parent, and returns `exit`, `stdout`, and `stderr`. Set `product` to
`codex`, `claude`, `grok`, or `qwen`, put the lifecycle verb in `command`, pass native trailing arguments in
`arguments`, and pass briefings as `input`. Add `host` for federation. Do not shell-execute a lane
launcher when the MCP tool is available; every lifecycle example below is an `sessionbus.lane`
argument object.

Example:

```json
{"product":"claude","command":"start","arguments":["--name","claude-review","--permission-mode","dontAsk","-"],"input":"Review the requested change and report the result."}
```

## Preflight

```json
{"product":"codex","command":"doctor","arguments":["--json"]}
{"product":"claude","command":"doctor","arguments":["--json"]}
{"product":"grok","command":"doctor","arguments":["--json"]}
{"product":"qwen","command":"doctor","arguments":["--json"]}
{"product":"codex","command":"list","arguments":["--all"]}
{"product":"claude","command":"list","arguments":["--all"]}
{"product":"grok","command":"list","arguments":["--all"]}
{"product":"qwen","command":"list","arguments":["--all"]}
```

All four unified daemon lanes require contract version **2**;
Claude also requires `claude_logged_in: true`, while Grok requires `grok_available: true` and no
`grok_error`. Qwen requires `ready: true`, including ACP/archive/profile integration evidence.
Require each doctor to report its target-specific readiness before using
that product. Fail closed: if a required field is false or missing, do not start that product and
report the lane cell as blocked; never "try anyway." Do not apply one product's contract version to
the other.

For a remote host, add `"host":"HOST"` to the same tool call only after the host advertises the
matching capability. Never fall back to SSH. Remote lifecycle and message delivery require the hub.

Every child always joins its own private group and this Grok parent’s private group. Do not copy
other parent groups unless the launch deliberately includes `--inherit-groups`. Use repeatable
`--group NAME` for child-specific membership. `--no-inherit-groups` resets optional inheritance
without removing the parent anchor; omission on resume restores the durable choice.

## Start and collect

Never place a briefing on argv. Put it in `input`:

```json
{"product":"codex","command":"start","arguments":["--name","codex-review","-"],"input":"BRIEFING"}
{"product":"claude","command":"start","arguments":["--name","claude-review","--permission-mode","dontAsk","-"],"input":"BRIEFING"}
{"product":"grok","command":"start","arguments":["--name","grok-review","-"],"input":"BRIEFING"}
{"product":"qwen","command":"start","arguments":["--name","qwen-review","--no-yolo","-"],"input":"BRIEFING"}
```

`lane.ready` means the worker is addressable; it is not the answer. Collect each lane with one
consumer:

```json
{"product":"codex","command":"wait","arguments":["codex-review","--timeout","300"]}
{"product":"claude","command":"wait","arguments":["claude-review","--timeout","300"]}
{"product":"grok","command":"wait","arguments":["grok-review","--timeout","300"]}
{"product":"qwen","command":"wait","arguments":["qwen-review","--timeout","300"]}
```

Take the last `turn.completed`, then the `agent_message` final answer with the same `turn_id`.
Report outcome and exit. A wait timeout exits 124 without interrupting the worker.

## Message, follow up, and retire

Use the installed `sessionbus` tools to send ordinary messages to a lane's current peer name or
session identity. A terminal pointer still requires `wait`; do not answer it conversationally.

```json
{"product":"codex","command":"resume","arguments":["codex-review","-"],"input":"FOLLOW-UP"}
{"product":"claude","command":"resume","arguments":["claude-review","-"],"input":"FOLLOW-UP"}
{"product":"grok","command":"resume","arguments":["grok-review","-"],"input":"FOLLOW-UP"}
{"product":"qwen","command":"resume","arguments":["qwen-review","-"],"input":"FOLLOW-UP"}
{"product":"codex","command":"interrupt","arguments":["codex-review"]}
{"product":"claude","command":"interrupt","arguments":["claude-review"]}
{"product":"grok","command":"interrupt","arguments":["grok-review"]}
{"product":"qwen","command":"interrupt","arguments":["qwen-review"]}
{"product":"codex","command":"archive","arguments":["codex-review"]}
{"product":"claude","command":"archive","arguments":["claude-review"]}
{"product":"grok","command":"archive","arguments":["grok-review"]}
{"product":"qwen","command":"archive","arguments":["qwen-review"]}
```

Collect outstanding debt before resume. Resume preserves the same transcript. Default lanes belong
to this corroborated Grok peer and archive when it exits; use `--persistent` only when survival is
required. Persistence disables parent-exit cleanup only; the normal 60-second terminal auto-archive
grace still applies. Pair `--persistent` with `--no-auto-archive` only for indefinite idle retention,
then archive explicitly.

Codex policy flags belong to the caller. Claude headless defaults to `dontAsk`; bypass only with
explicit authority. Qwen approval remains native and mutable; use `--yolo`, `--no-yolo`, or
`--approval-mode` only as authorized launch preferences. Read [references/contract.md](references/contract.md) for product-specific
events and lifecycle differences.
