# Contract: Baseline-Grounded Product Adapters

## Shared daemon boundary

The daemon owns Agent Sessions preparation/adoption revisions, groups, routing, durable delivery, lane
turns, collection, notices, cleanup debt, and federation. Product adapters own only calls into observed
native contracts. Adapter methods are derived from the port map and may be optional where a product
does not expose the same lifecycle stage.

Shared operations are probe, validate/prepare, observe/adopt, corroborate, deliver, start/reconnect/
interrupt/collect/archive/resume a turn, reconcile, and clean. These names do not impose one launch
ordering or process tree. Each product section controls its evidence and ordering.

## Codex contract

- Preserve `codex_peer.go` parsing and native argv placement.
- On first managed use, connect to the selected profile's App Server and use the existing supported
  daemon-start path when its socket is absent; callers never start Agent Sessions itself.
- Preserve fresh preparation, name/UUID resume, loaded-owner handling, cwd validation, history
  readiness, `--remote unix://` invocation, hook/MCP ancestry, delivery, permissions, archive, and cleanup.

## Claude contract

- Preserve absent versus explicit profile environment state, including secure-storage namespace.
- Preserve gated launch, caller settings merge without numeric rewriting, managed permission
  constraints, native registry/socket publication, late title/UUID adoption, delivery, permission
  refresh, key sidecars, rollback, and exact cleanup.
- Daemon ownership replaces the wrapper's Agent Sessions registry, not Claude's native registry or
  socket contract.

## Grok contract

- Preserve executable selection and native argument/permission semantics.
- Preserve a raw launch capability whose hash is stored durably and exact TUI, host-equivalent,
  private-leader, and MCP ancestry established by the baseline.
- ACP roster, wake, interjection, selection, resume, and archive use the existing Grok protocol.
- A daemon coordinator may replace the state-owning Grok host, but not invent a different process
  relationship. Any vendor-boundary helper is stateless and owns no Agent Sessions listener.

## Qwen contract

- Preserve Qwen profile/runtime selection and readiness.
- Preserve native args, permission mode, launch capability, dual-output admission, daemon/ACP ancestry,
  event/input artifact evidence, delivery, resume, archive, rollback, and cleanup.
- A daemon observer/input component replaces the Agent Sessions host only after native evidence remains
  identical.

## MCP relay contract

The relay serves vendor-required stdio JSON-RPC, reconnects to the fixed daemon endpoint, forwards
bounded MCP methods, and exits with its native host or stdin EOF. It owns no durable registry or local
listener. It forwards product-specific launch and ancestry evidence.

For bare sessions, initialization/tool discovery may remain visible, but tool calls return the canonical
inactive result and hooks exit zero without output or mutation.
