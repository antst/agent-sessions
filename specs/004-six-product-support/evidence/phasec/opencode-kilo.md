# Phase C OpenCode/Kilo Product-Slice Audit

Status: **GREEN for the product-local T038/T042/T046/T050/T054/T055/T059 clauses**

Audited at `2026-09-01T16:17:33Z` on Linux. This report audits only the
product-owned OpenCode/Kilo slice for T038, T042, T046, T050, the
OpenCode/Kilo portion of T054, T055, and T059. It awards no credit for physical
platform acceptance, central composition, federation, install/release work, or
the Phase 6 acceptance matrices.

## Commit ancestry and audited tree

- Branch: `feature/six-product-support`
- Audited HEAD: `93209ca7e7a083293c3c7bac9c4ab1eb9f537233`
  (`Record frozen transport gates and product evidence`), plus the scoped
  uncommitted product-test and evidence diff described below.
- Upstream base: `origin/main` at
  `679fe9d3068b6362df867f8d78ce6708c4ce1342`
- Product-slice commit:
  `52a843ad2c71e2ce52493ca23df52a1c8218a26f`
  (`Add OpenCode and Kilo runtime integrations`), whose exact parent is
  `077e4015d7e2285d8d3698bddb27ffde341976e0`.
- Product test follow-up:
  `6dbca287835c23c5c3598e561c3aed4463ab4c11`
  (`Align component lookup identity semantics`), whose exact parent is the
  product-slice commit `52a843a...`.
- Both `52a843a...` and `6dbca28...` are ancestors of the audited HEAD
  (`git merge-base --is-ancestor` returned 0 for each).
- The only committed relevant-path change after `52a843a...` is the
  `6dbca28...` addition to
  `internal/products/opencodefamily/peer_parent_test.go`. A scoped
  `git diff --quiet 6dbca28..HEAD` over the production and integration paths
  `internal/products/{opencodefamily,opencode,kilocode}` and
  `integrations/{opencode,kilo}` returned 0, so the audited product slice is
  byte-unchanged after that follow-up. The current uncommitted product diff is
  test-only: one Kilo-dialect recovery regression and one Go-side registered
  parent-tool contract regression in `opencodefamily`.

The implementation uses catalog product ID `kilo` and integration directory
`integrations/kilo`; the Go package remains
`internal/products/kilocode`. This is a naming/path divergence from the
literal `integrations/kilocode` spelling in T059, not an observed runtime
dispatch divergence.

## Focused verification

Toolchain: `go version go1.26.5 linux/amd64`; Node `v25.9.0`.

```text
/usr/local/go/bin/go test ./internal/products/opencodefamily ./internal/products/opencode ./internal/products/kilocode -count=1
```

Result: PASS, exit 0.

```text
ok github.com/antst/agent-sessions/internal/products/opencodefamily 0.020s
ok github.com/antst/agent-sessions/internal/products/opencode       0.004s
ok github.com/antst/agent-sessions/internal/products/kilocode       0.030s
```

```text
/usr/local/go/bin/go test -race ./internal/products/opencodefamily ./internal/products/opencode ./internal/products/kilocode -count=1
```

Result: PASS, exit 0.

```text
ok github.com/antst/agent-sessions/internal/products/opencodefamily 1.067s
ok github.com/antst/agent-sessions/internal/products/opencode       1.013s
ok github.com/antst/agent-sessions/internal/products/kilocode       1.053s
```

```text
node --test integrations/opencode/agent-sessions.test.mjs integrations/kilo/agent-sessions.test.mjs
```

Result: PASS, exit 0: 10 tests, 10 passed, 0 failed, 0 skipped,
duration 138.58 ms. This includes the async delivery-deadline and serialized
rename race cases in both plugins.

```text
for iteration in $(seq 1 20); do
  node --test integrations/opencode/agent-sessions.test.mjs integrations/kilo/agent-sessions.test.mjs >/dev/null
done
```

Result: PASS, exit 0: 20/20 repeated runs, 200 aggregate test executions.
Node has no Go-equivalent `-race` detector; the repeated run is async stress,
not a claim of JavaScript data-race instrumentation.

## Clause audit

### T038 — implemented

Implemented clauses:

- common component bootstrap, live binding lookup, exact native acceptance,
  reconnect-to-current-binding, and exact rename tests in
  `opencodefamily/peer_parent_test.go`;
- OpenCode managed plugin-TUI launch and unsafe topology/`--mini` rejection in
  `opencode/peer_test.go`;
- two isolated Kilo full-attach pairs, exact concurrent routing, zero
  cross-delivery, full attach override/`--mini` rejection, background-process
  attribution, and MCP registration in `kilocode/peer_client_test.go`;
- Kilo exact resume, missing-resume fail-closed behavior, fresh-session cleanup,
  detach preservation, and rollback convergence in
  `kilocode/peer_lifecycle_test.go`;
- exact component delivery/rename behavior and parent-bound tool execution in
  both integration JavaScript suites.

The formerly stale `evidence/phase0/component-contract-refreeze.md` now records
the already-granted round-1 sign-off accurately: the reviewed worktree at
`1dd16eb...` was materialized as `039d250...`, both adversarial and Fable cells
are GREEN, and `agent-sessions.component.v1-r1` is frozen. This does not invent
product acceptance: T038's product-local rename credit comes from the exact
OpenCode/Kilo Go and JavaScript regressions above; central and physical gates
remain separate.

### T042 — implemented

- `opencodefamily` supplies the verified component peer, typed authenticated
  HTTP client, owned isolated server manager, exact component/session lookup,
  transient bootstrap inputs, message delivery, reconnect, and native rename
  mechanics.
- `opencode` supplies the product-specific managed TUI launcher and runtime
  wiring.
- `kilocode` supplies the product-specific isolated `kilo serve` plus exact
  full `kilo attach --session ...` lifecycle, selection, ownership, rollback,
  delivery attribution, and runtime wiring.
- The corresponding OpenCode and Kilo plugins are inert without component
  activation, bind to one native session, emit session/turn frames, and require
  exact native acceptance before acknowledging delivery or rename.

### T046 — implemented

Implemented clauses:

- typed OpenCode and Kilo HTTP routes, exact session create/get/delete,
  deterministic prompt acceptance, Kilo v2 queue/steer/permission/interrupt,
  bounded messages, SSE terminal handling, missed-fast-terminal
  reconciliation, receipt integrity, concurrency, cancellation, recovery, and
  cleanup-debt tests exist in `opencodefamily`;
- product-specific permission tests exist in both product packages.

`TestKiloLaneRecoveryRequiresExactNativeSession` now exercises the common lane
driver with `DialectKilo` directly. It proves exact recovery to the prior Kilo
session and current generation, proves the recovery path (not Open) was used,
and rejects a substituted product-native session as both unsupported recovery
and ambiguous while closing the provisional server. Together with the existing
OpenCode success/unsupported-recovery coverage, the literal two-dialect
recovery matrix is explicit.

### T050 — implemented

- `opencode.NewLaneDriver` and `kilocode.NewLaneDriver` configure the common
  `opencodefamily.LaneDriver` with product ID, exact dialect, generation,
  durable receipt reader, server manager, recovery permission source, and
  permission decision callback.
- The driver implements open/recover/start/wait/steer/interrupt/archive above
  the typed `productserver`-backed client and owned server manager, including
  exact native IDs and cleanup-debt retention.

### T054 (OpenCode/Kilo portion) — implemented

- `opencode.MapPermission` and `kilocode.MapPermission` are product-owned
  exports over the shared native rule representation.
- `default` maps exactly to `ask`; `bypassPermissions` maps exactly to `allow`;
  unknown/unsupported modes return `productruntime.ErrUnsupportedPolicy`
  before native I/O.
- Both product-specific unsupported-policy suites pass under normal and race
  runs.

### T055 — implemented

- `opencodefamily/peer_parent_test.go` verifies the exact component binding,
  claimed native session, kernel peer PID/process identity agreement, durable
  attachment, and rejection of false session claims and foreign processes.
- Both JavaScript integration suites verify registration of the
  `agent_sessions` native tool, rejection of a foreign/unbound tool context,
  overwrite of a forged native-session argument with the exact bound native
  session, and routing through `component.callTool`.
- `TestRegisteredParentToolAssetsMatchExactAttestationContract` in
  `internal/products/opencodefamily/peer_parent_test.go` now pins both product
  assets to that registered-tool contract from the product-internal test
  surface: native tool registration, exact bound-session check, caller-ID
  overwrite, component relay, cancellation, and exact `shell.env` projection.
  The same table creates each product's real shared `ParentAttester`, accepts
  the exact binding, and rejects a foreign product. It reads the product assets
  instead of copying their implementation; the JavaScript suites remain the
  behavioral execution layer.

### T059 — implemented at the product boundary

- Both product runtime constructors install an
  `opencodefamily.ParentAttester` parameterized by the exact product ID.
- The shared attester delegates to the host-owned exact verifier and requires
  component binding, claimed session, live peer credential/process identity,
  attachment, and generation agreement.
- Both plugins register bounded Agent Sessions operations, bind calls to the
  component-confirmed native session, propagate cancellation, and prevent the
  model from substituting a native-session argument.

This is product-boundary credit only. No credit is assigned here for the
central connector/tool relay composition required later by T070 or for any
end-to-end parent acceptance matrix.

## Real-product spike evidence inspected

`S1-kilo.json` is PASS evidence produced on base
`679fe9d3068b6362df867f8d78ce6708c4ce1342` with real pinned
`@kilocode/cli` 7.5.6 and product protocol unmocked. It records two isolated
authenticated server/full-attach pairs, distinct sessions, exact routing,
zero cross-delivery, idle wake, busy queue, background-process attribution,
connected MCP, visible rename, same-ID resume/history, `--mini` rejection, and
no persisted raw server password. Only the model provider is a deterministic
local fixture.

`S4-component.json` is PASS evidence on the same base with real pinned OpenCode
1.18.25 and Kilo 7.5.6. For both, `shell.env.sessionID` matches the authoritative
native session, the component is inert without the bootstrap secret and active
with it, and the raw secret is absent from artifacts. S4 explicitly says it is
a native-identity/frame-expressiveness spike, not an implemented daemon broker;
only `session.announce` was observed for the OpenCode/Kilo runs while the full
frame vocabulary was a contract mapping.

These spikes validate the selected product mechanics but predate the product
implementation commit and do not execute the current Go/JavaScript slice
end-to-end. They are not physical-macOS evidence and are not credited as
central composition or Phase 6 acceptance.

## Honest remaining gaps and verdict

Every product-local clause assigned by T038, T042, T046, T050, the
OpenCode/Kilo portion of T054, T055, and T059 is **GREEN**: product code,
focused normal tests, Go race tests, JavaScript behavior tests, and 20 repeated
JavaScript stress runs pass. The three audit gaps are closed without changing
product implementation or shared contracts.

This is deliberately only the Phase-C product-slice gate. It proves no
physical macOS cell, central composition/dispatch, federation, install/release,
or real-product end-to-end execution of the current committed slice. Those
later gates remain uncredited.
