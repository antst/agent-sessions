# OpenCode integration

Agent Sessions targets OpenCode **1.18.25**.

The native plugin reports each live `ses_*` session over one `presence.sock`
connection. The first report is `{uuid,name,groups,product}`. Product title
events update it, deletion closes it, message delivery uses `prompt_async`, and
the registered parent tool uses the same connection. There is no component
socket, bootstrap, binding, or process-attestation path.

Lanes use OpenCode's documented HTTP API: session create/get/delete,
`prompt_async`, abort, event streaming, message listing, permission reply, and
exact session resume. An exact HTTP 204 is native prompt acceptance. The lane
does not claim queued prompts as steering.

`default` maps to native ask rules and `bypassPermissions` to allow rules.
Unknown modes fail. Doctor checks the exact CLI version, required live HTTP
routes, provider configuration, and the installed live-session plugin.
