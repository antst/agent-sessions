---
name: opencode-lane
description: Operate a daemon-owned OpenCode lane with exact receipt, permission, recovery, and archive semantics.
---

# OpenCode lane

Select product `opencode` through the structured Agent Sessions lane tool.

- Run doctor before first use. The pinned native version and all documented
  server routes must be ready.
- Use `default` to preserve native permission asks. Use
  `bypassPermissions` only when explicitly authorized; unknown modes fail.
- OpenCode does not expose a proven mid-turn steer in this integration. Leave
  busy input in the daemon's durable queue and never submit a duplicate.
- Resume only the exact returned `ses_*` identity. A replacement session is
  not recovery.
- Wait and collect the exact turn before reporting its result. Interrupt and
  archive by the exact lane identity.

The product-local protocol fixtures do not establish a real model turn. Do not
describe mock-backed checks as physical-product acceptance.
