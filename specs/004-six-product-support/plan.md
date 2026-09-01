# Implementation Plan: Six-Product Symmetric Support

**Branch**: `feature/six-product-support` | **Date**: 2026-09-01 | **Spec**: [spec.md](spec.md)

**Input**: Fully symmetric, maximally DRY support for OpenCode, KiloCode, Pi,
OMP, CodeBuddy, and DSH on the released v0.3.0 unified-daemon base.

## Summary

Add six products without creating six more lifecycle frameworks. The feature
first removes closed-four-product assumptions and strengthens durable busy-input
handling. It then builds three shared transport mechanisms—a managed component
stream, a supervised structured child, and a safe authenticated loopback
client—with typed product protocols above them. Product behavior is selected
through one data catalog and one explicit runtime registry. Existing and new
products continue to use the same daemon attachment, delivery, lane, turn,
group, cleanup, and federation authorities.

The implementation is evidence-gated. Five native truth spikes plus base,
federation, and catalog checks run before interfaces freeze. Product work fans
out only after shared contracts and foundations pass original-four regression
gates.

## Technical Context

**Language/Version**: Go 1.22 host/runtime; Bash 3.2-compatible install/release
scripts; product integration assets in plain JavaScript/TypeScript supported by
the native product loaders

**Primary Dependencies**: Go standard library, `golang.org/x/sys`, existing
JSON schema/canonicalization packages; native documented OpenCode/Kilo HTTP+SSE,
Pi/OMP JSONL RPC, CodeBuddy OpenAPI/HTTP, and DSH ACP/Cordis APIs

**Storage**: Existing bounded atomic daemon JSON catalog plus a new private
bounded `0700`/`0600` lane-input content spool; native products retain their own
session stores

**Testing**: Go unit/integration/race/fuzz tests, repository script gates,
shared deterministic OpenAI-compatible mock provider, isolated real native
products, systemd and launchd install/acceptance, physical Linux/macOS evidence

**Target Platform**: Linux and macOS, amd64 and arm64 builds; runtime acceptance
on real Linux and physical macOS

**Project Type**: Multi-product local/federated daemon, CLI aliases, native
plugins/extensions, installer, hub, and release system

**Performance Goals**: No polling or process per delivery beyond native
requirements; bounded component/stdio/HTTP frames; prompt admission fsync cost
is explicit; idle component connections have bounded heartbeat/resource use;
roster and existing-product latency remain within current gates

**Constraints**: Exact identity, no TTY scraping, no credential persistence,
no permission widening, durable accepted input, fail-closed recovery,
transactional install/remove, trusted-network uniform protocol-4 federation, same-UID
local trust, CodeBuddy model GA cell account-gated, DSH exact alpha tuple

**Scale/Scope**: Ten supported products, six new peer aliases, six lane aliases,
six local/federated lane capabilities, five in-process component shapes,
three shared transport engines, one catalog and one runtime composition root

## Constitution Check

### Pre-design gate

| Principle | Plan evidence | Result |
|---|---|---|
| Shared contracts / DRY | One catalog, capability registry, component broker, structured process, product server, ledger, shared family clients | PASS |
| Exact identity / fail closed | bootstrap + peercred/process evidence, product-native corroboration, destination registry, exact native refs/leases | PASS |
| Root-cause first | phase-0 truth spikes block interface freeze; red gate changes design | PASS |
| Evidence-driven testing | crash matrix, hostile decoders, mock-provider real product tests, physical OS matrices | PASS |
| Linux/macOS parity | shared socket/path helpers, service-env task, physical two-OS release gate | PASS |
| Transactional lifecycle | spool/catalog ordering, ambiguity state, exact lease, install ownership receipts | PASS |
| Explicit protocols/docs | versioned component/ledger/catalog/federation contracts and ADAPTER-PROTOCOL rewrite | PASS |

### Post-design gate

The Phase-1 contracts preserve the same results. The one deliberate complexity
increase—three transport engines—is justified because local component streams,
structured stdio children, and authenticated HTTP/event servers have genuinely
different ownership and failure semantics. Product-specific typed clients stay
above those shared mechanics.

## Project Structure

### Documentation

```text
specs/004-six-product-support/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── tasks.md
├── contracts/
│   ├── acceptance-matrix.md
│   ├── catalog-projection.md
│   ├── component-protocol.md
│   ├── federation-v4.md
│   ├── lane-input-ledger.md
│   └── runtime-product.md
└── evidence/
    ├── phase0/
    └── acceptance/
```

### Source Code

```text
cmd/agent-sessions/
├── product_registry.go       # sole runtime composition root
└── ...                       # thin pre-agreed integration hunks only

internal/
├── productcatalog/           # data-only sole product inventory
├── productruntime/           # interfaces, records, registry validation, fakes
├── localtransport/           # bounded AF_UNIX framing + peer identity
├── launchhandoff/            # one-shot secret-safe native exec handoff
├── component/                # broker, bindings, component protocol
├── sessiontools/             # extracted AgentFrame/MCP/tool/instruction helpers
├── structuredprocess/        # exact owned structured child/framed stdio
├── productserver/            # literal-loopback HTTP/auth/event/supervision
├── products/
│   ├── opencodefamily/       # verified common OpenCode/Kilo client mechanics
│   ├── pifamily/             # verified common Pi/OMP RPC/extension mechanics
│   ├── opencode/
│   ├── kilocode/
│   ├── pi/
│   ├── omp/
│   ├── codebuddy/
│   └── dsh/
├── federation/               # live uniform protocol-4 opaque-capability hub/host
└── releaseinstall/           # catalog-derived transactional install registry

integrations/
├── shared/component/         # shared JS/TS component client
├── opencode/
├── kilocode/
├── pi/
├── omp/
├── codebuddy/
└── dsh/

internal/testutil/
└── mockprovider/             # shared streaming/tool-call provider fixture
```

**Structure Decision**: New product semantics live in `internal/products` and
integration assets. Shared transport/lifecycle code contains no product-name
switch. Central host integration is deliberately confined to one registry file
and small pre-agreed command hooks.

## Architecture and Authority

```text
native TUI/plugin/extension ── component.sock ─┐
native HTTP/SSE server ─── productserver ───┼─ product driver ─┐
native RPC/ACP child ─ structuredprocess ───┘                  │
                                                               v
caller/MCP/CLI -> host control -> daemon engines -> runtime registry
                                     │
                                     ├─ catalog + input spool + leases
                                     └─ federation v4 -> destination registry
```

Authority rules:

1. `internal/daemon` alone commits attachment/delivery/lane/turn/receipt/lease
   lifecycle.
2. Drivers perform native I/O and return native evidence; they do not commit
   lifecycle state directly.
3. `productcatalog` authors stable data; `productruntime` validates executable
   capability composition.
4. The hub routes opaque advertised capability strings; destination daemon and
   runtime registry authorize actual product execution.
5. Product components are inert without managed wrapper bootstrap.
6. Native credentials remain in native clients; generated credentials for
   Agent Sessions-owned servers remain memory-only.

## Phase 0: Truth Gates Before Interface Freeze

No broad implementation begins until all gates are green or the design is
updated and re-reviewed.

### S0 — Base and federation decoding

- Prove worktree is exact `679fe9d` plus planning artifacts.
- Prove current federation decoder accepts additive unknown JSON fields; record
  that no new wire field is required.
- Freeze uniform protocol 4, exact-version rejection, and trusted-network scope.

### S1 — Kilo exact routing and attach parity

- Run two isolated servers and two `kilo attach <url>` TUIs.
- Inject idle and busy input; cross-delivery count must be zero.
- Prove attach mode supports `/tui/*`, background process attribution, MCP,
  rename, resume, completion events, and parent tool context.
- If exact routing fails, choose one isolated server per peer and update the
  family contract before freeze.
- Recorded result: **PASS** with one authenticated `kilo serve` plus one full
  `kilo attach` TUI per peer. Two pairs proved exact/zero-cross idle and busy
  routing, completion events, MCP, background-process session attribution,
  rename, and resume. `attach --mini` does not consume `/tui/*` and is therefore
  explicitly unsupported for managed peer messaging.

### S2 — DSH exact tuple and Cordis facade

- Materialize exact CLI/ACP/plugin/profile tuple with pnpm.
- In a real supported profile, enumerate native sessions, wake idle through
  followup, steer busy, and observe completion.
- Choose and prove native registered tool versus MCP parent facade.
- Prove HOME/XDG component socket visibility under sandbox.
- Recorded result: **PASS**, selecting the native Cordis registered tool as the
  primary parent facade. The exact tuple proved idle wake, busy native steer,
  turn completion, sandbox-visible HOME socket, matching `DSH_SESSION_ID`,
  ACP busy rejection, cancel-as-notification, and projcache non-liveness.

### S3 — CodeBuddy peer registry and endpoint ownership

- Correlate one exact interactive registry session/PID/URL with the managed
  wrapper child and prove the endpoint uses constant CSRF-header gating, not a
  peer credential.
- Verify the literal-loopback listening socket belongs to the attested TUI PID,
  then corroborate executable, start identity, and ancestry before delivery.
- Prove idle wake/busy reply, daemon-restart re-discovery, cross-target
  isolation, stale-row/PID-reuse/port-recycle rejection, and zero peer-secret
  persistence.
- Keep the Agent Sessions-owned authenticated lane server explicitly separate;
  its generated secret is an ephemeral product-server runtime value.
- Recorded result: **RED reconciled**. The interactive worker exposes no
  password and needs no component or sidecar; wrapper Adopt/Refresh re-attests
  registry claim -> socket owner PID -> executable/start/ancestry. The distinct
  Agent Sessions-owned lane server retains memory-only password auth. Linux
  stale-row/PID-reuse/port-recycle/cross-target cells passed; physical macOS
  socket-owner proof remains a Phase-E acceptance cell.

### S4 — Shared component native identity

- For OpenCode, Kilo, Pi, and OMP, prove the product-native registered
  tool/`shell.env`/extension context carries the exact native session ID.
- Demonstrate the shared component client can represent bootstrap, reconnect,
  rename, state, delivery, terminal event, and tool calls for all four without
  weakening evidence.
- Recorded result: **PASS**. OpenCode/Kilo `shell.env.sessionID` and Pi/OMP
  extension/RPC/registered-tool context each matched the authoritative native
  session. One component-v1 frame vocabulary covered all four; capability ID
  alone stayed inert, the ephemeral launch secret activated the component, and
  raw secret material was absent from evidence.

### S5 — Legacy reachability decision

- Use static analysis plus source call inventory to identify live bridge exports
  and exact unreachable file set on 679fe9d.
- Recorded result: **extract-and-freeze**. The legacy entrypoints are
  unreachable, but no production file is independently safe to delete because
  live helpers and legacy tests remain entangled.
- Extract the 32 live bridge exports into focused packages, collapse the
  duplicate catalog, enforce a shrinking no-new-legacy-import baseline, and
  leave full deletion to a separately gated follow-up.
- `docs/ADAPTER-PROTOCOL.md` rewrite is mandatory either way.

### S6 — Catalog/install projection

- Prototype deterministic staged-binary ten-product JSON projection.
- Prove aliases, payloads, install strategies, federation capabilities, and
  acceptance cells derive without shell product arrays.
- Recorded result: **PASS**, including explicit parent capability, one bounded
  token grammar, exact DSH tuple/pnpm policy, separate CodeBuddy peer/lane
  surfaces, 110 Linux/macOS capability cells, and drift rejection. Prototype
  digest: `9a0b001b5b92d1c0123d7ea1c62dee3ead70f545c07117cc95f1096a7fea1702`.

### Phase-0 gate

- Structured evidence exists for S0–S6.
- No unresolved red assertion.
- Contract changes caused by a spike are reconciled with Fable and reflected in
  spec/research/data/contracts before Phase A.

## Phase A: Central Contract and State Freeze

One central owner performs this phase; no parallel source edits.

1. Extend the data catalog, add explicit `CapabilityParent`, one shared token
   validator for product/federation capability namespaces, and a deterministic
   projection schema.
2. Add `productruntime` interfaces, records, stable error taxonomy, validation,
   and fakes, including optional `Steer` and explicit driver recovery.
3. Add catalog schemas/validation for lane input receipts, native leases, and
   component records; no product native I/O yet.
4. Collapse the duplicate federator product descriptor onto productcatalog.
5. Apply S5's extract-and-freeze result and enforce a shrinking no-new-legacy
   import boundary.
6. Rewrite `docs/ADAPTER-PROTOCOL.md` for the unified daemon and new contracts.
7. Add static/conformance tests prohibiting product switches and duplicate
   product lists outside allowed packages/composition.
8. Fix or explicitly gate the existing Grok unconditional permission widening.

### Phase-A gate

- Original four pass focused/full normal/race/vet/lint gates.
- A synthetic runtime product proves registry validation and optional
  capabilities.
- No consumer authors a second product inventory.
- Fable signs off the frozen interfaces and records.

## Phase B: Shared Foundations

The following streams may run in parallel after Phase A. Each owns only its
listed packages and tests; central command/catalog/state/federation/install
files are off limits.

### Stream B1 — Local transport, component broker, shared client

- `internal/localtransport`
- `internal/launchhandoff`
- `internal/component`
- `integrations/shared/component`
- protocol, bootstrap, reconnect, replay, redaction, heartbeat, wrong-process,
  PID-reuse, malformed-frame, full/zero/partial launch commit, secret absence,
  exec-image-replacement, and Linux/macOS socket tests

The central integrator later owns the small `cmd/agent-sessions` broker startup
hook.

### Stream B2 — Structured process

- `internal/structuredprocess`
- exact process group ownership, framed read/write, order, size bounds,
  cancellation, exit evidence, restart hooks, Linux/macOS process tests

### Stream B3 — Product server

- `internal/productserver`
- literal loopback validation, redirect/proxy refusal, memory auth, bounded
  HTTP/decompression/SSE, reconnect/dedup, owned-server supervision, test server

### Stream B4 — Session tools and live-helper extraction

- `internal/sessiontools`
- shared AgentFrame wrapping, product instructions/tool descriptions, connector
  entrypoint validation, MCP relay helpers selected by S5
- unknown entrypoints fail loudly; no Claude default

The central integrator owns the pre-agreed import/call-site hunks.

### Stream B5 — Durable ledger/spool

- `internal/daemon/lane_input.go` and focused statestore/spool tests
- admission/dispatch/ambiguity/recovery/retirement state machine
- migration hook replacing volatile pending authority is central-integrator work

This stream is serialized with any other daemon-state change.

### Stream B6 — Federation and hub debt

- live `internal/federation` opaque capabilities
- port command integration tests from legacy hub
- hostile decoder fuzz and generation/group/destination tests
- macOS hub service environment parity

This stream is serialized with central federation/service files.

### Stream B7 — Shared mock provider

- deterministic OpenAI-compatible streaming/tool-call/slow-turn/cancel fixture
- reusable native-product harness API and credential-redaction checks

### Phase-B gate

- All shared packages pass unit/race/fuzz/integration tests with no new product
  registered.
- Ledger crash matrix is green.
- Component reconnect and local-server hostile tests are green on Linux/macOS.
- Original four remain green after live-helper and pending-queue migration.

## Phase C: Product Implementations

Four product owners may run in parallel after Phase B. Each owns its product
and family directories, integration assets, focused fixtures, and product docs.
They do not edit catalog, composition, coordinator, federation, daemon state,
install scripts, or shared component core.

### C1 — OpenCode and KiloCode

- typed `opencodefamily` common client/quirks;
- OpenCode plugin peer, product server lane, parent tool, permissions, doctor;
- Kilo exact S1-selected peer topology, `/tui/*` routing, server lane, parent
  identity, permissions, doctor;
- rename/resume/reconnect/event-stream tests and product assets.

### C2 — Pi and OMP

- shared `pifamily` JSONL RPC driver and extension adapter;
- Pi ready negotiation, `agent_settled`, steer, abort, resume, restricted tools;
- OMP quirk table, interjection framing, approval modes, spawn behavior;
- peer/parent extension assets, skills/commands, doctor and mock-provider cells.

### C3 — CodeBuddy

- typed workers/jobs/reply/event client;
- wrapper-adopt registry/process/socket peer evidence plus restart/stale-row
  semantics; no component sidecar;
- separately supervised authenticated lane server with memory-only credential;
- peer/lane/parent/permission/doctor/assets;
- offline OpenAPI and mock-provider evidence; support remains experimental until
  the external Tencent account cell passes.

### C4 — DSH

- typed ACP client above structured process;
- exact tuple installer metadata and durable native lease behavior;
- Cordis peer/parent component, busy queue, cancel notification, Stop/ACP
  completion, sandbox socket/env handling, doctor/assets;
- exact tuple and real credentialed evidence.

### Phase-C gate

Each owner demonstrates its product/family PEER × LANE × PARENT × doctor
contract in isolation against the real pinned binary. No product is composed
into the main host before its focused gate is green.

## Phase D: Central Integration

One integrator owns all shared central edits:

1. add six descriptors and six runtime constructors to the sole composition
   root;
2. replace coordinator/launcher/messaging/lane switches with registry dispatch;
3. connect component broker startup and session-tool imports through pre-agreed
   hunks;
4. replace `laneActor.pending` with the durable ledger executor;
5. connect federation readiness/capability projection;
6. connect catalog-derived release/install strategy registry and scripts;
7. generate aliases, payloads, help, skills, acceptance matrices, and docs;
8. migrate current four products behind runtime contracts/wrappers and prove no
   behavior loss;
9. make CodeBuddy support-state gating and DSH tuple gating visible in doctor,
   roster, and federation.

### Phase-D gate

- Ten-product catalog/registry conformance.
- No product dispatch switch outside permitted product/composition locations.
- No shell-authored product array.
- Existing four and six new focused matrices green, except the declared
  CodeBuddy account-gated cell.

## Phase E: Acceptance, Review, and Release

1. Run focused, normal, race, vet, lint, four-build, install/remove, federation,
   and real-product gates.
2. Run every declared capability cell on real Linux and physical macOS at exact
   product versions.
3. Run cross-host representative matrix for all six new lane capabilities.
4. Inspect exact cleanup, component/server/process residue, catalog/spool/lease
   debt, and unrelated profile nonmutation.
5. Generate evidence manifest bound to commit/tree/native versions.
6. Fable reviews shared-contract adherence, identity, recovery, permissions,
   installer projection, and matrix completeness while available; final review
   also uses independent implementation agents.
7. Merge only after every non-account-gated cell is green. CodeBuddy remains
   explicitly experimental until Tencent model-turn evidence is available.

## Dependency Graph

```text
joint plan
  └─ Phase 0 S0..S6
       └─ Phase A contracts/state/catalog/legacy gate
            ├─ B1 component/local transport
            ├─ B2 structured process
            ├─ B3 product server
            ├─ B4 session tools
            ├─ B5 durable ledger
            ├─ B6 federation/hub
            └─ B7 mock provider
                 └─ Phase-B gate
                      ├─ C1 OpenCode + Kilo
                      ├─ C2 Pi + OMP
                      ├─ C3 CodeBuddy
                      └─ C4 DSH
                           └─ Phase-D central integration
                                └─ Phase-E Linux/macOS/federation/release
```

## Parallel Ownership Matrix

| Stream | May edit | Must not edit |
|---|---|---|
| Phase 0 spikes | isolated spike/evidence fixtures only | production contracts before reconciliation |
| Central Phase A/D | catalog, runtime interfaces, daemon state, composition, coordinator, install/federation | product-owner uncommitted files |
| B1 | `internal/localtransport`, `internal/component`, `integrations/shared/component` | `cmd`, catalog, daemon catalog, product packages |
| B2 | `internal/structuredprocess` | coordinator/product packages |
| B3 | `internal/productserver` | product packages/coordinator |
| B4 | `internal/sessiontools` | central call sites (integrator applies hunks) |
| B5 | lane-input implementation/tests only | other daemon state/federation |
| B6 | live federation/hub/service-env files | product packages/catalog |
| B7 | shared mock-provider testutil | product implementations |
| C1 | opencode family/products/assets/docs/fixtures | shared core, catalog, composition, install |
| C2 | pi family/products/assets/docs/fixtures | shared core, catalog, composition, install |
| C3 | codebuddy product/assets/docs/fixtures | shared core, catalog, composition, install |
| C4 | dsh product/assets/docs/fixtures | shared core, catalog, composition, install |

No parallel worker edits `cmd/agent-sessions`, the product catalog, composition
root, daemon catalog/state validation, federation protocol, or install scripts.
Those merges are serialized through the central integrator.

## Risk Controls

| Risk | Control |
|---|---|
| Kilo route targets wrong TUI | S1 blocks interface/topology choice; isolated server fallback |
| DSH alpha/API churn | exact tuple, pnpm, capability doctor, per-profile ownership, pinned acceptance |
| CodeBuddy stale registry or recycled port | wrapper child plus registry/socket-to-PID/executable/ancestry re-attestation; peer and authenticated lane endpoints remain distinct |
| Duplicate native prompt after crash | dispatch-intent state + ambiguity/no-replay |
| Shared abstraction hides product semantics | three mechanical engines; typed family/product clients; no endpoint DSL |
| Permission widening | typed mapper, unsupported policy error, Grok migration audit |
| Product-list drift | catalog projection and CI; no shell arrays |
| Legacy code expands | S5 decision and enforced no-new-import rule |
| Federation attack exposure | explicit trusted-network assumption; hostile parser tests; auth tracked separately |
| Parallel edit collision | ownership matrix and central-only files |

## Complexity Tracking

| Added complexity | Why needed | Simpler alternative rejected because |
|---|---|---|
| Separate component, structured-process, and product-server engines | Ownership/framing/auth/recovery differ materially | One universal transport would hide security and lifecycle differences in untyped configuration |
| Private content spool beside catalog | Accepted input must survive restart without placing bodies in bounded metadata catalog | Volatile queue loses work; catalog bodies change privacy/size semantics |
| Product runtime registry in addition to data catalog | Stable metadata must remain importable while executable capabilities need dependencies | Callbacks in catalog create cycles; switches drift; init registration hides composition |
| Native session lease | DSH permits dual owners | Trusting native locking would allow concurrent writers and transcript corruption |

## Deliverables

- Six complete product integrations and aliases.
- Shared component/local transport, structured process, product server, runtime
  registry, lane input ledger/spool, native lease, and session tools.
- Catalog-derived install/release/acceptance projection.
- Uniform protocol-4 opaque-capability live hub with closed test debt.
- Updated adapter protocol, README/install/product/cross-host documentation.
- Linux and physical macOS evidence for every non-account-gated capability.
