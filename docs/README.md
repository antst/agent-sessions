# Agent Sessions documentation

- [Products](PRODUCTS.md) — supported peers and lanes, native selectors, and launch options.
- [Lanes](LANES.md) — worker lifecycle, results, messaging, archive, restart, and handover.
- [Groups](GROUPS.md) — visibility, private anchors, lane namespaces, and routing.
- [Installation](INSTALL.md) — host, product integration, hub, update, and removal.
- [Adapter architecture](ADAPTER-PROTOCOL.md) — how shipped adapters use the native protocol.
- [Native peer protocol](specs/NATIVE-PEER-PROTOCOL.md) — the publishable JSON-RPC v1 wire contract.
- [Federation](FEDERATION.md) and [wire protocol](federation/PROTOCOL.md) — multi-host operation.
- [Troubleshooting](TROUBLESHOOTING.md) — health checks and product-owned failure diagnosis.
- [Acceptance matrix](ACCEPTANCE-MATRIX.md) — credited real-product behavior and pending verification.

Internal requirements and doctrine:

- [Persistence and state](designs/PERSISTENCE-AND-STATE.md)
- [DSH TUI requirements](designs/DSH-TUI-REQUIREMENTS.md)

The product owns its sessions, names, histories, models, credentials, and defaults. Agent Sessions
provides one live presence and messaging plane, plus one durable table containing only offline-lane
discovery candidates.
