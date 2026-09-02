# Oh My Pi integration

Agent Sessions targets `@oh-my-pi/pi-coding-agent` and `omp` **18.0.11**.
OMP shares Pi-family mechanics only where the native protocols really match.

The extension reports each live OMP session over its one `presence.sock`
connection as `{uuid,name,groups,product}` and uses that connection for message
delivery and parent tool calls. Native title events update the live report.
There is no component handshake, binding, or second socket.

Lanes run `omp --mode=rpc`. OMP's protocol-1 startup `ready` object is required,
then `get_state` confirms the native ID. Exact resume uses `--session
<native-id>`. Continuing `agent_end` events are not terminal.

OMP approval prompts have no local user relay. `default` is therefore
unsupported; only explicit `bypassPermissions` maps to
`--approval-mode=yolo`. Unknown modes and arguments that can inject input or
override managed session/extension/policy settings are rejected.

Doctor checks the executable, exact version, required RPC/extension/resume and
approval options, the readable live-session extension, and a live integration
check.
