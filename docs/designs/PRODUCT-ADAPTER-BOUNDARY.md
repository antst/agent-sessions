# Product adapter boundary

This 0.4.1 design note records the product-uniformity audit at parity WIP
`7bc62faf4e063c0bf59c01f1f140c73592fed320`. Line anchors describe that snapshot and must be
refreshed after the 0.4.0 landing series. This note states boundaries, not a delivery schedule.

## Target shape

Every per-product decision lives behind the product's adapter, descriptor, declared capability, or
registration factory. Shared code validates common grammar, resolves a registration, dispatches a
common interface, and maps common results and errors. It does not enumerate products, guess a
foreign product, or accumulate product-specific argument, identity, launch, or lifecycle branches.

The caller interface accepts any token-shaped `product` and never enumerates or resolves products.
The daemon is the sole resolver: first a registered descriptor selects its adapter; otherwise an
executable named `<product>-peer-lane` on the service `PATH` selects one generic Lane Worker wire
driver; if neither exists, the daemon returns `-32005 Product not launchable` with `{product}`. DSH
is a data row using this generic wire-worker driver, rather than the implementation that defines it.

The worker interface has a mandatory core: `Open`, `StartTurn`, `WaitTurn`, `Interrupt`, and
`Archive`. `Steer`, `DurableResume`, and `CallerSuppliedSessionID` are declared capabilities.
The caller interface universally exposes `doctor`, `list`, and `status` for every worker.
The daemon derives common cwd, permission, descriptor, and native-argument inputs before dispatch;
an adapter validates and translates only its product's native facts.

## Actionable audit families

Statuses distinguish the 0.4.1 boundary work from behavior corrections owned by the in-flight 0.4.0
parity/review-fix series. “MOVE” and “DELETE” below are the audit's canonical family labels.
The 0.4.1 implementation scope is families 2-6, 8-19, and 21; families 1, 7, 20, and 22 are listed
only to preserve the complete audit and their 0.4.0 closure ownership.

1. **DELETE caller enumeration** — `internal/sessiontools/mcp.go:95-100,119-122` builds the lane-tool product enum from today's catalog. **0.4.0 parity-owned closure; verify on the final parity tip:** accept token-shaped future products and let the daemon return `-32005 {product}`.
2. **MOVE product instructions** — `internal/sessiontools/mcp.go:17-22,48-56` hard-codes four caller products and advertises only Codex, Claude, Grok, and Qwen.
3. **MOVE product MCP policy** — `internal/sessiontools/mcp.go:24-46,59-92` selects allowed tools and session projection through a central product map; put caller-surface policy in its adapter or descriptor.
4. **MOVE peer launch dispatch** — `cmd/agent-sessions/main.go:85-99` is a four-product chain with a generic fallback; register launchers at the adapter boundary.
5. **MOVE executable resolution** — `internal/launcher/product.go:55-77` is a four-product switch with a synthesized fallback; the adapter or descriptor declares the exact environment key and resolver.
6. **MOVE lane bootstrap inputs** — `internal/launcher/lane.go:35-49,74-90` branches on Grok and Qwen workers to inject environment; a descriptor or capability declares required executable/runtime inputs.
7. **MOVE native flag arity** — `cmd/agent-sessions/lane.go:325-335` centrally hard-codes the union of product-native value-taking flags. **0.4.0 review-fix-owned closure; verify on its final tip:** consume descriptor native-argument rules before the uniform parser.
8. **MOVE Claude connector identity resolver** — `cmd/agent-sessions/connector.go:39-47,70-74`.
9. **MOVE Grok connector rename/default-name policy** — `cmd/agent-sessions/connector.go:54-67`.
10. **MOVE Qwen relay-source fallback** — `cmd/agent-sessions/connector.go:169-175`.
11. **MOVE Codex stdio source projection** — `cmd/agent-sessions/connector.go:224-235`.
12. **MOVE connector delivery registration/implementations** — `cmd/agent-sessions/connector.go:238-273` contains product-keyed lookup and three native deliverers; shared connector code retains generic adapter dispatch only.
13. **MOVE connector presence ownership** — `cmd/agent-sessions/connector.go:275-290` hard-codes Claude and Grok.
14. **MOVE connector native-ID env policy** — `cmd/agent-sessions/connector.go:381-410` uses a Grok-specific variable, a product map, and a cross-product fallback scan; the adapter declares the exact source and the scan is deleted.
15. **MOVE/DELETE automatic connector guessing** — `cmd/agent-sessions/connector.go:470-493` infers Grok or defaults Codex; discovery is adapter-declared and never guesses a foreign product.
16. **MOVE live title capability** — `cmd/agent-sessions/codex_host.go:110-164` and `cmd/agent-sessions/messaging.go:748-753` hold a central product-to-resolver map; the attachment adapter exposes an optional title resolver.
17. **MOVE candidate-name capability** — `cmd/agent-sessions/codex_host.go:449-509,555-565` holds a central product-to-resolver map; the lane adapter exposes durable native-name lookup.
18. **MOVE compile-time lane-driver population** — `cmd/agent-sessions/codex_host.go:175-320` constructs and registers nine products; product-owned factories driven by the catalog populate the generic registry.
19. **MOVE Codex-only attachment registration** — `cmd/agent-sessions/codex_host.go:334-365` returns a one-product adapter map; registration belongs to Codex composition authority.
20. **DELETE redundant product branch** — `cmd/agent-sessions/codex_host.go:910-926` is already the Codex native-event path. **0.4.0 review-fix-owned closure; verify on its final tip:** remove `report.Product == codex`; exact-thread ownership remains the guard.
21. **MOVE hook attestation** — `cmd/agent-sessions/hook.go:38-50` branches on `product != codex` in shared hook dispatch; Codex hook attestation belongs to its adapter.
22. **DELETE per-parent/data-gated invalid-cwd execution; run the negative exactly once outside the caller table** — `scripts/realproducts/matrix.go:362-381,504-506,669-710` is data-mediated: Claude alone sets `nativeLaneProduct`, which gates `runLaneCell`, whose invalid-cwd call is at line 696. **0.4.0 parity-owned closure; verify on the final parity tip.**

The related **opaque-product blocker** is at `cmd/agent-sessions/lane.go:85` and
`internal/launcher/managed_peer.go:288-292`: native-argument projection rejects an unknown catalog
ID before driver resolution. The 0.4.0 parity-owned closure resolves the driver first and returns the typed
`-32005 {product}` result; Go and JavaScript method validators remain token-grammar-only.

## Allowed shared data and dispatch

These are generic registries or test inventory, not product-policy leaks:

- `internal/daemon/attachment.go:97-131` generic `AttachmentAdapter` registry dispatch;
- `cmd/agent-sessions/lane.go:662,694,738,827,911,1195` generic `LaneDriver` registry lookup;
- Go/JavaScript live-presence token validation and response-product correlation;
- product assertions inside `internal/daemon/adapter_{codex,claude,grok,qwen}.go`; and
- `matrixProductInventory` plus native marker/selector rows as explicit harness data.

## Already-closed/carry families

- Item 4A C3 projects Pi `--resume` to `--session` through its descriptor.
- Item 4A C4 replaces four copied lane-help bodies with catalog-derived help.
- Item 4A C5 carries the optional opaque `native_stop_reason` in the common terminal result.
- Parity owns family 22 relocation, token-only Go/JavaScript params, typed `lane.start`/result
  authority, stored report cwd, and the cross-target cwd/future-product tests. Verify the last two
  acceptance pins on the final parity tip; they were absent from the audited WIP.

## Boundary checks

- Shared packages contain no product-name switch, allowlist, fallback scan, or native flag union.
- Adding a registered product requires a product-owned descriptor/factory and no shared dispatch edit.
- Adding an unregistered wire worker requires only an executable named `<product>-peer-lane` on the
  service `PATH`; missing registration and executable produce the same closed `-32005` result.
- Every worker implements the mandatory core; optional behavior is selected only by capabilities.
- Identical caller inputs derive identical shared cwd and permission state for every target product.
