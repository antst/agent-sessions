# Agent Sessions documentation

This index separates the shipped product contracts and operator guides from future design work.
Documents under `designs/` are proposals only; they are not installed capabilities.

## Install and operate

- [Installation](INSTALL.md) — native runtime plus Codex, Claude, and Grok integrations.
- [Claude-side installation](CLAUDE-INSTALL.md) — the Claude plugin and isolated-profile setup.
- [Groups](GROUPS.md) — discovery, addressing, inheritance, and isolation.
- [Federation](FEDERATION.md) — host agents, hubs, and remote lanes.
- [Federation operations](federation/OPERATIONS.md) — deployment and operational checks.
- [Codex-side installation](CODEX-INSTALL.md) — runtime, plugin, and interactive-peer setup.
- [Grok-side installation](GROK-INSTALL.md) — plugin setup and managed Grok sessions.

## Product contracts

- [Codex adapter](CODEX-ADAPTER.md)
- [Codex-side installation](CODEX-INSTALL.md)
- [Codex worker lanes](CODEX-LANES.md)
- [Claude adapter](CLAUDE-ADAPTER.md)
- [Claude-side installation](CLAUDE-INSTALL.md)
- [Claude worker lanes](CLAUDE-LANES.md)
- [Grok adapter](GROK-ADAPTER.md)
- [Grok-side installation](GROK-INSTALL.md)
- [Grok worker lanes](GROK-LANES.md)

Codex interactive-peer behavior is described by the shared
[native carrier and product-adapter protocol](ADAPTER-PROTOCOL.md) and the launcher sections of
[Installation](INSTALL.md). The protocol document is intentionally not product-prefixed because it
also defines the Claude carrier, Grok wake path, common frames, and compatibility boundary.

## Protocol and verification

- [Native carrier and product-adapter protocol](ADAPTER-PROTOCOL.md)
- [Federation wire protocol](federation/PROTOCOL.md)
- [Acceptance matrix](ACCEPTANCE-MATRIX.md)

## Future designs

- [Qwen Code adapter](designs/QWEN-ADAPTER.md) — unimplemented design work, not a shipped product.
