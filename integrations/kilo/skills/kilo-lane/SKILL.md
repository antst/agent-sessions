---
name: kilo-lane
description: Operate a daemon-owned Kilo lane with exact v2 prompt, steer, permission, recovery, and archive semantics.
---

# Kilo lane

Select product `kilo` through the structured Agent Sessions lane tool.

- Run doctor before first use. The pinned native version, stable routes, and
  v2 session routes must all be ready.
- Use `default` to relay native permission asks. Use `bypassPermissions` only
  when explicitly authorized; unknown modes fail.
- A lane uses an isolated authenticated `kilo serve` and the server-owned v2
  session prompt route. It is not a peer TUI and must never use `/tui/*`.
- Busy steer succeeds only when the v2 response corroborates the exact native
  message, session, and `steer` admission. Otherwise retain the durable input;
  never submit it twice.
- Resume only the exact returned `ses_*` identity. Wait and collect the exact
  turn, then interrupt or archive by exact lane identity.

The product-local protocol fixtures do not establish a real model turn. Do not
describe mock-backed checks as physical-product acceptance.
