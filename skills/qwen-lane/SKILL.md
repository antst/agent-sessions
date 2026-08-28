---
name: qwen-lane
description: Spawn, supervise, message, collect, resume, and archive durable local or remote Qwen Code worker lanes from Codex. Use for delegation to Qwen, Qwen review or comparison work, persistent follow-ups, and named federated Qwen targets.
---

# Qwen lane

Use `qwen-peer-lane` for one named Qwen Code transcript exposed as a durable,
messageable Agent Sessions peer. Qwen owns its native approval mode: the managed
lane records requested and observed modes but does not prevent native mode changes.

From a managed Codex peer, execute every lifecycle operation through the attested
`agent_sessions.lane` MCP tool. Set `product` to `qwen`, put the lifecycle verb in
`command`, pass arguments after the verb in `arguments`, and pass the briefing as
`input`. Supply the current SessionStart session ID. Do not shell-execute the lane
launcher from a sandboxed Codex peer.

```json
{"product":"qwen","command":"start","arguments":["--name","qwen-review","--no-yolo","-"],"input":"Review the change and return a concise finding.","session_id":"CURRENT_SESSION_ID"}
```

For a remote host add `"host":"HOST"`; require a connected hub and advertised
`qwen-lane`. Remote doctor must return `ready: true`, authority `remote-daemon`,
the exact requested host, and product `qwen`. Remote `--mine` matches the exact
source-proxy parent and host. Federation owns lifecycle flags, so do not pass
`--persistent`, `--notify`, `--no-notify`, or `--no-auto-archive` remotely. Send
prompts through bounded `input`; remote `--prompt-file` is unsupported. Never
fall back to SSH or local execution.

Run `doctor --json` and `list --all` before starting. Require `ready: true`,
`authority: "daemon"`, and product `qwen`. The daemon's Qwen adapter validates the selected
profile and native prerequisites; readiness is session-free and does not claim live provider
authentication.

Start detached work and collect it exactly once:

```sh
qwen-peer-lane start --name qwen-review --no-yolo --auto-archive-after 300 - < brief.md
qwen-peer-lane wait qwen-review --timeout 300
```

Use `--yolo` only with explicit authority, or pass a supported native
`--approval-mode MODE`; omission preserves Qwen's native default. `start` returns
at `lane.ready`, not at the answer. Select the final `agent_message` matching the
last `turn.completed`, and report its outcome and exit. A wait timeout exits 124
without cancelling work. A `QWEN_LANE_TERMINAL` notice is a collection pointer,
not the answer; execute its `Collect:` line verbatim.

The live lane accepts Agent Sessions messages. An idle message creates one durable
serialized turn; messages arriving during active work queue behind it. Collect all
terminal debt before an explicit follow-up:

```sh
qwen-peer-lane resume qwen-review --timeout 600 - < follow-up.md
```

Resume preserves the Agent Sessions lane ID and exact native Qwen session UUID,
unarchives the native session, and uses ACP `session/resume`.

```sh
qwen-peer-lane status qwen-review
qwen-peer-lane interrupt qwen-review
qwen-peer-lane archive qwen-review
```

Interrupt maps ACP cancel to outcome `interrupted`, exit 130. Archive is daemon-owned and
idempotent, retires the grouped publication and adapter-owned native resources, while retaining
the native transcript for resume. Default lanes belong to the exact parent and archive
when it exits; use `--persistent` only when survival is intended. Child groups are
private by default; add `--inherit-groups` deliberately and repeat `--group NAME`
for child-specific membership.
