# Pi integration

Agent Sessions targets `@earendil-works/pi-coding-agent` **0.84.4** and the
`pi` executable at that exact version.

The managed extension uses Pi's supported `--extension` option. For each live
Pi session it keeps one `presence.sock` connection and reports the product UUID,
native title, launch groups, and product `pi`. `session_info_changed` updates
the title on that same connection. EOF makes the session unavailable.

Live messages use `pi.sendUserMessage`: ordinary send while idle and native
steer/follow-up while busy. The registered `agent_sessions` tool sends parent
operations through the same session connection. Pi's UUID is the target; there
is no component binding or second socket.

Managed lanes use `pi --mode rpc`. Readiness is established with correlated
`get_state` because Pi emits no startup-ready frame. Turns use `prompt`,
`steer`, `abort`, `get_state`, and `get_last_assistant_text`; completion waits
for `agent_settled`. Exact resume is `--session <native-id>` and a different
returned ID is rejected.

`default` maps to Pi's read/search tool allowlist. `bypassPermissions` uses the
full native tool set. Unknown modes and caller arguments that override managed
mode/session/extension/tool settings are rejected.

Doctor checks the executable, exact version, required RPC/extension/resume/tool
options, the readable live-session extension, and a live integration check.
