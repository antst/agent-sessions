# DeepSeek Harness (DSH)

Agent Sessions supports the exact experimental tuple:

- `@deepseek-ai/dsh@0.1.2-alpha.3`
- `@deepseek-ai/dsh-acp-app@0.1.2-alpha.3`
- `@agent-sessions/dsh-plugin@0.1.2-alpha.3`
- the Agent Sessions profile at `0.1.2-alpha.3`
- `pnpm@10.28.1`

The managed peer profile boots DSH's native Web surface. The Cordis plugin
reports exactly the selected live native session over one
`presence.sock` connection. It does not enumerate sibling sessions into the
same identity. The report carries UUID, product title, launch groups, and
product `dsh`; a launch name is written through DSH's title service and later
title events update the same connection. Idle messages call
`followup`, busy messages call `steer`, and the registered parent tool uses the
same live session connection.

Local lanes run `dsh --profile acp` through the product's ACP surface. They
support new session, exact resume, prompt, wait, interrupt, and archive.
Session list/resume results must name the requested UUID at the requested cwd.
ACP busy rejection is returned truthfully rather than entering a daemon queue.
Cancel is an ACP notification. Projection-cache metadata is not liveness.

`default` maps to `workspace-write` plus `ask`; explicit
`bypassPermissions` maps to `danger-full-access` plus `never`. Unexpected ACP
permission requests are rejected rather than silently allowed.

Doctor checks the exact tuple and performs a live keyless ACP feature probe.
The product remains experimental; credentialed model-turn acceptance is a
separate real-product test.
