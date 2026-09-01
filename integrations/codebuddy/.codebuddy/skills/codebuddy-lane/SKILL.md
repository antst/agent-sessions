---
name: codebuddy-lane
description: Operate an Agent Sessions-owned CodeBuddy lane with exact wait, steer-or-queue, recovery, and archive semantics.
---

# CodeBuddy lane

Select product `codebuddy` through the structured Agent Sessions lane tool.
CodeBuddy lanes use an Agent Sessions-owned, literal-loopback `--serve`
process with password authentication. That surface is distinct from the
credential-free interactive TUI peer endpoint.

- Run doctor before first use and respect experimental/unavailable results.
- Treat accepted busy input as delivered when CodeBuddy reports immediate
  delivery or when its already-saved pending reply is consumed by an exact
  native respawn. A failure after the save is ambiguous: do not requeue,
  replay, or submit a duplicate.
- Resume only the exact returned native session. Never accept a replacement
  job or session on recovery.
- A terminal notice is status metadata, not the child answer. When it requires
  collection, wait and collect once for the exact lane and turn. Interrupt and
  archive by exact lane identity.
- Default permission mode preserves native prompts. Full bypass is unavailable
  unless the caller explicitly authorized the sandbox-only bypass mode.

Offline protocol success is not evidence that a Tencent-authenticated model
turn completed. That acceptance cell remains pending while support is
experimental.
