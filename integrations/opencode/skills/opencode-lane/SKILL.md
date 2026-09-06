---
name: opencode-lane
description: Operate a daemon-owned OpenCode lane with exact receipt, permission, recovery, and archive semantics.
---

# OpenCode lane

Select product `opencode` through the structured sessionbus lane tool.

- Run doctor before first use. The pinned native version and all documented
  server routes must be ready.
- Use `default` to preserve native permission asks. Use
  `bypassPermissions` only when explicitly authorized; unknown modes fail.
- OpenCode admits an active delivery with native `steer`; an idle lane delivery
  uses its durable `queue` and runs when the caller starts the next turn.
- Resume only the exact returned `ses_*` identity. A replacement session is
  not recovery.
- Wait and collect the exact turn before reporting its result. Interrupt and
  archive by the exact lane identity.

The product-local protocol fixtures do not establish a real model turn. Do not
describe mock-backed checks as physical-product acceptance.
