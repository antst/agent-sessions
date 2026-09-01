---
description: "Dependency-ordered implementation tasks for six-product symmetric support"
---

# Tasks: Six-Product Symmetric Support

**Input**: Design documents in `specs/004-six-product-support/`

**Prerequisites**: Jointly converged [plan.md](plan.md), [spec.md](spec.md),
[research.md](research.md), [data-model.md](data-model.md), and `contracts/`.

**Testing rule**: Contract/regression tests are written first and must fail for
the missing behavior before implementation earns credit. Real native binaries
are required for truth/acceptance gates; a mock model provider is allowed.

**Ownership rule**: Tasks marked `[P]` have disjoint file ownership. No parallel
worker may edit the catalog, daemon catalog validation, composition root,
coordinator, federation, or install scripts unless its task explicitly owns
that central surface.

## Phase 1: Truth Gates and Planning Freeze

**Purpose**: Resolve the remaining native unknowns before runtime interfaces
freeze. These are evidence tasks, not production implementation.

- [x] T001 Verify isolated branch base/tree and record S0 evidence in `specs/004-six-product-support/evidence/phase0/S0-base.json`
- [x] T002 [P] Run the Kilo two-server/two-attach exact-routing and attach-parity spike in `scripts/spikes/six-product/kilo/` and record `specs/004-six-product-support/evidence/phase0/S1-kilo.json`
- [x] T003 [P] Run the exact DSH 0.1.2-alpha.3 tuple/Cordis idle-wake/busy-steer/parent-facade spike in `scripts/spikes/six-product/dsh/` and record `specs/004-six-product-support/evidence/phase0/S2-dsh.json`
- [x] T004 [P] Run the CodeBuddy managed-wrapper/registry/socket-to-PID daemon-restart, stale-row/port-reuse, cross-target, and no-peer-secret spike in `scripts/spikes/six-product/codebuddy/` and record `specs/004-six-product-support/evidence/phase0/S3-codebuddy.json`
- [x] T006 [P] Run static reachability/call inventory for live bridge/federator exports in `scripts/spikes/six-product/legacy/` and record the extract-and-freeze decision in `specs/004-six-product-support/evidence/phase0/S5-legacy.json`
- [x] T007 [P] Prototype deterministic staged-binary ten-product catalog/install projection in `scripts/spikes/six-product/catalog/` and record `specs/004-six-product-support/evidence/phase0/S6-catalog.json`
- [x] T008 Prove the historical protocol-3 decoder behavior and record the original opaque-capability/trusted-network decision in `specs/004-six-product-support/evidence/phase0/S0-federation.json` (superseded for the greenfield live protocol by T125)
- [x] T009 Reconcile all spike outcomes into `specs/004-six-product-support/{spec.md,research.md,data-model.md,plan.md,contracts/}` before source interfaces freeze
- [x] T010 Obtain visible `fable-architect` sign-off on S0-S6 evidence and reconciled planning artifacts in `specs/004-six-product-support/evidence/phase0/review.md`

**Checkpoint**: Every truth gate is green or the design has been updated and
re-reviewed. Production interface work may start.

---

## Phase 2: Foundational Shared Contracts and Engines

**Purpose**: Establish the sole product inventory, frozen runtime contracts,
durable input semantics, shared transports, live federation behavior, and test
fixtures before any new product is composed.

- [x] T011 Add nonbreaking typed `permissionmode.Mode`, the type-only platform-neutral `localtransport.PeerIdentity`, explicit `CapabilityParent`, shared bounded product/federation token validation, tested-version/support/install/transport/federation projection fields, and validation tests in `internal/{permissionmode,localtransport,productcatalog}/`
- [x] T012 Add deterministic `agent-sessions catalog --json` schema/projection implementation and tests in `internal/productcatalog/projection.go`, `internal/productcatalog/projection_test.go`, `internal/clihelp/{commands.go,commands_test.go}`, and `cmd/agent-sessions/{catalog.go,main.go,main_test.go}`
- [x] T013 Define frozen runtime records, driver interfaces, typed errors, redaction types, and permission contract in `internal/productruntime/{types.go,drivers.go,errors.go}`
- [x] T014 Implement explicit runtime registry validation against an injected inventory, synthetic-product extension proof, fake drivers, and no-init-registration tests in `internal/productruntime/{registry.go,registry_test.go,fakes_test.go}`
- [x] T015 Extend daemon catalog structs/validation/cloning/old-catalog normalization for lane inputs, leases, and component records in `internal/daemon/{state.go,state_test.go}`

### Central Interface Freeze

- [x] T016 Apply S5's extract-and-freeze decision, move live MCP relay/AgentFrame/instruction/name/help helpers into `internal/sessiontools/`, add an exact shrinking no-new-legacy-import baseline, preserve bridge compatibility wrappers, and update only pre-agreed imports in `cmd/agent-sessions/{connector.go,messaging.go,main.go}`
- [x] T017 Rewrite the normative unified daemon/runtime/component/ledger acceptance contract in `docs/ADAPTER-PROTOCOL.md`
- [x] T018 Break the catalog-test import cycle, collapse `internal/federator` product metadata onto a one-way `internal/productcatalog` compatibility projection, and add conformance/static tests banning duplicate product inventories and dispatch switches in `internal/{productcatalog,federator}/*_test.go` and `scripts/test`
- [x] T019 Gate Grok lanes to an explicitly requested `bypassPermissions` policy without mutation, reject every other mode before native invocation, and add fail-closed regression coverage in `internal/daemon/{adapter_grok_lane.go,adapter_grok_lane_test.go}`
- [x] T020 Run runtime/schema/static/original-four focused gates and obtain visible Fable sign-off on the frozen Phase-A interfaces in `specs/004-six-product-support/evidence/phase0/interface-freeze.md`

**Interface Freeze Checkpoint**: Runtime records and authority boundaries are
reviewed and frozen. Shared-engine workers may now start against those types.

### Shared Engine Tests and Implementation
- [x] T021 Write failing admission/dispatch/ambiguity/recovery/retirement tests for the lane-input contract in `internal/daemon/lane_input_test.go`
- [x] T022 Implement the private bounded no-follow spool and durable receipt engine in `internal/daemon/lane_input.go` and `internal/daemon/lane_input_spool.go`
- [x] T023 Replace volatile `laneActor.pending` acceptance with ledger-backed execution and recovery in `cmd/agent-sessions/lane.go` and `cmd/agent-sessions/lane_test.go`
- [x] T024 Implement exclusive native-session lease transitions and DSH-ready composite-key tests in `internal/daemon/{native_lease.go,native_lease_test.go}`
- [x] T025 [P] Write Linux/macOS peer-identity and bounded-frame tests for `internal/localtransport/` in `internal/localtransport/{transport_test.go,peer_linux_test.go,peer_darwin_test.go}`
- [x] T026 [P] Implement bounded AF_UNIX framing and exact Linux/macOS peer identity in `internal/localtransport/{transport.go,peer_linux.go,peer_darwin.go}`
- [x] T027 [P] Write component bootstrap/reconnect/replay/wrong-process/PID-reuse/heartbeat tests in `internal/component/{broker_test.go,protocol_test.go}`
- [x] T028 [P] Implement the component broker, bindings, protocol v1, and redaction in `internal/component/{broker.go,protocol.go,binding.go}`
- [x] T029 [P] Implement and test the inert-without-bootstrap shared component client in `integrations/shared/component/{client.js,protocol.js,client.test.js}`
- [x] T030 [P] Write process-group/framing/order/bounds/cancel/exit regression tests in `internal/structuredprocess/{process_test.go,framing_test.go}`
- [x] T031 [P] Implement exact owned structured child supervision in `internal/structuredprocess/{process.go,framing.go,process_unix.go}`
- [x] T032 [P] Write redirect/proxy/non-loopback/auth/body/SSE/reconnect hostile tests in `internal/productserver/{client_test.go,events_test.go,server_test.go}`
- [x] T033 [P] Implement literal-loopback HTTP, memory auth, bounded event streams, and owned-server supervision in `internal/productserver/{client.go,events.go,server.go}`
- [x] T034 [P] Implement the shared deterministic streaming/tool-call/slow-turn/cancel model fixture in `internal/testutil/mockprovider/{server.go,script.go,server_test.go}`
- [x] T035 Write failing opaque-capability/live-hub/generation/malformed-client tests in `internal/federation/{hub_test.go,host_test.go,protocol_fuzz_test.go}` and `cmd/agent-sessions-hub/main_test.go`
- [x] T036 Implement the original bounded opaque protocol-3 capability foundation, destination registry resolution hooks, live-hub integration, and macOS hub environment parity in `internal/federation/{protocol.go,hub.go,host.go}`, `cmd/agent-sessions/federation.go`, and `internal/servicecontrol/` (version/compatibility projection superseded by T125)
- [x] T037 Run original-four focused/full normal/race/vet/lint gates, capture results in `specs/004-six-product-support/evidence/phase0/foundation-gate.md`, and obtain Fable review of the frozen foundation

**Checkpoint**: Shared packages are green with no new product composed; original
four remain green; product owners may now work in parallel.

---

## Phase 3: User Story 1 - Managed Interactive Peers (Priority: P1)

**Goal**: Each new product's real interactive session is an exact managed peer
with visible idle wake, busy steer/queue, outbound messaging, rename, and resume.

**Independent Test**: Launch each `*-peer` in an isolated native profile,
exchange idle/busy messages with another grouped peer, rename, kill/resume, and
restart the daemon while preserving exact native identity.

### Tests for User Story 1

- [x] T038 [P] [US1] Write OpenCode/Kilo peer component, isolated full-attach exact-routing, `--mini` rejection, rename, resume, and reconnect tests in `internal/products/{opencode,kilocode,opencodefamily}/*_test.go`
- [x] T039 [P] [US1] Write Pi/OMP extension identity, idle-wake, busy-steer, rename, resume, and reconnect tests in `internal/products/{pi,omp,pifamily}/*_test.go`
- [x] T040 [P] [US1] Write CodeBuddy wrapper/registry/socket-to-PID correlation, reply-wake, CSRF, stale-row/port-reuse, cross-target, and daemon-restart tests in `internal/products/codebuddy/*_test.go`
- [x] T041 [P] [US1] Write DSH Cordis session binding, followup/steer, tuple, reconnect, and sandbox-socket tests in `internal/products/dsh/*_test.go`

### Implementation for User Story 1

- [x] T042 [P] [US1] Implement verified common OpenCode/Kilo component/server peer mechanics in `internal/products/opencodefamily/` and product-specific peer drivers in `internal/products/{opencode,kilocode}/`
- [x] T043 [P] [US1] Implement the shared Pi/OMP component extension core and peer drivers with a typed quirk table in `internal/products/pifamily/`, `internal/products/{pi,omp}/`, and `integrations/{pi,omp}/`
- [x] T044 [P] [US1] Implement the CodeBuddy typed product-owned peer endpoint client and wrapper-adopt registry/process/socket evidence in `internal/products/codebuddy/` and `integrations/codebuddy/`
- [x] T045 [P] [US1] Implement the exact-tuple DSH Cordis peer component and session binding in `internal/products/dsh/` and `integrations/dsh/`
**Checkpoint**: All six peer drivers and integration components pass their
product-owned contract suites and are ready for serialized host composition.

---

## Phase 4: User Story 2 - Durable Local and Federated Lanes (Priority: P1)

**Goal**: Each product supports the complete local/remote daemon-owned lane
lifecycle through MCP and CLI, including steer-or-queue and exact recovery.

**Independent Test**: Run start/wait/collect/interrupt/resume/archive locally
and remotely for each product, inject busy input, restart the daemon, and prove
exact identity or explicit ambiguity.

### Tests for User Story 2

- [x] T046 [P] [US2] Write OpenCode/Kilo lane HTTP/SSE/session/permission/recovery tests in `internal/products/{opencode,kilocode,opencodefamily}/*_lane_test.go`
- [x] T047 [P] [US2] Write Pi/OMP RPC ready/turn/steer/settled/abort/resume tests in `internal/products/{pi,omp,pifamily}/*_lane_test.go`
- [x] T048 [P] [US2] Write CodeBuddy jobs/reply/stream/stop/respawn/archive lane tests in `internal/products/codebuddy/*_lane_test.go`
- [x] T049 [P] [US2] Write DSH ACP new/resume/busy-queue/cancel-notification/stop/lease tests in `internal/products/dsh/*_lane_test.go`

### Implementation for User Story 2

- [x] T050 [P] [US2] Implement OpenCode and Kilo lane drivers above `internal/productserver` in `internal/products/{opencodefamily,opencode,kilocode}/`
- [x] T051 [P] [US2] Implement shared Pi/OMP JSONL RPC lane client and product drivers above `internal/structuredprocess` in `internal/products/{pifamily,pi,omp}/`
- [x] T052 [P] [US2] Implement CodeBuddy lane lifecycle above its typed HTTP/event client in `internal/products/codebuddy/`
- [x] T053 [P] [US2] Implement DSH ACP lane lifecycle, exact tuple handling, and lease use above `internal/structuredprocess` in `internal/products/dsh/`
- [x] T054 [US2] Add fail-closed product-specific permission mappers and unsupported-policy tests for all six in `internal/products/*/permission.go` and `internal/products/*/permission_test.go`

**Checkpoint**: All six lane drivers pass typed native lifecycle suites and are
ready for shared ledger/coordinator/federation composition.

---

## Phase 5: User Story 3 - New Products as Orchestrator Parents (Priority: P1)

**Goal**: Every new managed TUI can use an attested Agent Sessions tool to
message peers, launch lanes, collect results, and receive visible notices.

**Independent Test**: From each native TUI, list/send/start/wait/archive a child
lane and reject a forged native-session claim.

### Tests for User Story 3

- [x] T055 [P] [US3] Write OpenCode/Kilo registered-tool and exact parent-session attestation tests in `internal/products/{opencode,kilocode,opencodefamily}/*_parent_test.go`
- [x] T056 [P] [US3] Write Pi/OMP registered-tool, extension env, ancestry, and false-ID tests in `internal/products/{pi,omp,pifamily}/*_parent_test.go`
- [x] T057 [P] [US3] Write CodeBuddy per-session MCP/tool ancestry and terminal-notice tests in `internal/products/codebuddy/*_parent_test.go`
- [x] T058 [P] [US3] Write DSH native-tool/MCP, `DSH_SESSION_ID`, env-scrub, sandbox, and notice tests in `internal/products/dsh/*_parent_test.go`

### Implementation for User Story 3

- [x] T059 [P] [US3] Implement OpenCode/Kilo parent tools and exact component/session attesters in `internal/products/{opencodefamily,opencode,kilocode}/` and `integrations/{opencode,kilocode}/`
- [x] T060 [P] [US3] Implement Pi/OMP registered parent tools, commands, and exact session attesters in `internal/products/{pifamily,pi,omp}/` and `integrations/{pi,omp}/`
- [x] T061 [P] [US3] Implement CodeBuddy parent connector/tool injection and attester in `internal/products/codebuddy/` and `integrations/codebuddy/`
- [x] T062 [P] [US3] Implement DSH parent facade selected by S2 and exact attester in `internal/products/dsh/` and `integrations/dsh/`
**Checkpoint**: All six parent drivers/tools pass exact-identity suites and are
ready for serialized host/tool composition.

---

## Phase 6: Central Integration and P1 Story Acceptance

**Purpose**: Compose only fully implemented peer/lane/parent product packages,
replace central switches, add aliases and federation projection, and prove the
three P1 user stories end to end.

- [ ] T063 [US1] Add all six final descriptors and complete runtime products to the sole composition root and connect the pre-agreed component broker/coordinator hook in `internal/productcatalog/catalog.go` and `cmd/agent-sessions/{product_registry.go,codex_host.go}`
- [ ] T064 [US1] Replace product-name message delivery/rename dispatch with runtime registry drivers in `cmd/agent-sessions/{messaging.go,codex_host.go}` and add ten-product dispatch tests
- [ ] T065 [US2] Replace central product lane switches with `LaneDriver` dispatch, `Steer` fallback, and ledger execution in `cmd/agent-sessions/lane.go` and `cmd/agent-sessions/lane_test.go`
- [ ] T066 [US2] Implement six doctor probes' central support-state/federation readiness projection in `internal/diagnostics/` and `cmd/agent-sessions/admin.go`
- [ ] T067 [US1] Implement six managed peer aliases through catalog-driven launcher dispatch in `internal/launcher/{product.go,peer.go}` and add alias/help tests
- [ ] T068 [US2] Add six catalog-derived `*-peer-lane` aliases, stable help, closed-stdin doctor, and CLI contract tests in `internal/launcher/lane.go`, `internal/clihelp/`, and `internal/releaseevidence/`
- [ ] T069 [US2] Route six opaque `*-lane` capabilities through the destination registry and add local/remote unsupported-product tests in `cmd/agent-sessions/federation.go` and `internal/federation/`
- [ ] T070 [US3] Connect component `tool.call` and MCP relay to existing Agent Sessions operations through pre-agreed hunks in `cmd/agent-sessions/{connector.go,messaging.go}` and `internal/sessiontools/`
- [ ] T071 [US3] Add one common self-explanatory Agent Sessions skill/command surface plus thin product lane skills under `integrations/*/skills/` and catalog-derived package tests
- [ ] T072 [US3] Add cross-product forged-session/shared-MCP/subagent identity rejection tests in `cmd/agent-sessions/connector_test.go` and `internal/acceptance/`
- [ ] T073 [US1] Run the complete six-product peer matrix on Linux and record exact evidence in `specs/004-six-product-support/evidence/acceptance/linux-peers.json`
- [ ] T074 [US1] Run the complete six-product peer matrix on physical macOS and record exact evidence in `specs/004-six-product-support/evidence/acceptance/macos-peers.json`
- [ ] T075 [US1] Verify no pluginless/bare launch gains ambient management and close the US1 checkpoint in `specs/004-six-product-support/evidence/acceptance/peer-gate.md`
- [ ] T076 [US2] Run the six-product local lane lifecycle/crash/receipt matrix and record `specs/004-six-product-support/evidence/acceptance/local-lanes.json`
- [ ] T077 [US2] Run the six-product federated lane matrix across two hosts and record `specs/004-six-product-support/evidence/acceptance/remote-lanes.json`
- [ ] T078 [US2] Verify collection/archive/cleanup debt and zero owned residue for all ten products in `specs/004-six-product-support/evidence/acceptance/lane-gate.md`
- [ ] T079 [US3] Run each new product's local parent list/send/lane/notice matrix and record `specs/004-six-product-support/evidence/acceptance/local-parents.json`
- [ ] T080 [US3] Run same-product and cross-product local/federated child lanes from every new parent and record `specs/004-six-product-support/evidence/acceptance/parent-composition.json`
- [ ] T081 [US3] Close the parent authority/notice/skill checkpoint in `specs/004-six-product-support/evidence/acceptance/parent-gate.md`

**Checkpoint**: US1, US2, and US3 are independently demonstrable through the
real host, MCP, CLI, and federation surfaces. CodeBuddy's external account cell
is the only permitted pending model result.

---

## Phase 7: User Story 4 - Install, Diagnose, and Upgrade (Priority: P2)

**Goal**: One transactional catalog-derived install/release path manages all
ten optional integrations consistently on Linux and macOS.

**Independent Test**: Install, update, rollback, and remove against isolated
homes containing different product subsets; compare catalog, aliases, native
registrations, doctor, roster, and release evidence.

### Tests for User Story 4

- [ ] T082 [US4] Write install-strategy registry, ownership-receipt, drift, rollback, and user-modification tests in `internal/releaseinstall/{registry_test.go,projection_test.go,transaction_test.go}`
- [ ] T083 [P] [US4] Write OpenCode/Kilo native integration install/remove fixture tests in `internal/releaseinstall/opencodefamily_test.go`
- [ ] T084 [P] [US4] Write Pi/OMP global package/extension/skill install/remove fixture tests in `internal/releaseinstall/pifamily_test.go`
- [ ] T085 [P] [US4] Write CodeBuddy integration install/remove, no-peer-secret, and lane-server-secret non-persistence tests in `internal/releaseinstall/codebuddy_test.go`
- [ ] T086 [P] [US4] Write DSH pnpm/exact-profile/plugin ownership and rollback tests in `internal/releaseinstall/dsh_test.go`

### Implementation for User Story 4

- [ ] T087 [US4] Implement explicit catalog-derived release install registry and ownership receipts in `internal/releaseinstall/{registry.go,projection.go,ownership.go}`
- [ ] T088 [US4] Make `scripts/release-inventory`, `scripts/package-release`, Makefile aliases, and acceptance expansion consume the staged catalog projection with no product arrays
- [ ] T089 [US4] Refactor `scripts/install-host` and `scripts/remove-host` to execute typed projection plans transactionally and preserve exact prior integrations
- [ ] T090 [P] [US4] Implement OpenCode/Kilo install strategies and packaged assets in `internal/releaseinstall/opencodefamily.go` and `integrations/{opencode,kilocode}/`
- [ ] T091 [P] [US4] Implement Pi/OMP install strategies and packaged assets in `internal/releaseinstall/pifamily.go` and `integrations/{pi,omp}/`
- [ ] T092 [P] [US4] Implement CodeBuddy install strategy and experimental readiness projection in `internal/releaseinstall/codebuddy.go` and `integrations/codebuddy/`
- [ ] T093 [P] [US4] Implement DSH exact tuple/profile/pnpm install strategy in `internal/releaseinstall/dsh.go` and `integrations/dsh/`
- [ ] T094 [US4] Add ten-product doctor/roster/catalog JSON and secret-redaction operator tests in `internal/daemon/admin_test.go`, `internal/diagnostics/`, and `cmd/agent-sessions/admin_test.go`
- [ ] T095 [US4] Run isolated install/update/failure-rollback/remove transactions on Linux and macOS and record `specs/004-six-product-support/evidence/acceptance/install-transactions.json`
- [ ] T096 [US4] Close catalog/projection/archive/prebuilt-install drift gate in `specs/004-six-product-support/evidence/acceptance/install-gate.md`

**Checkpoint**: A release install is optional-product tolerant, transactional,
catalog-derived, and symmetric across systemd/launchd.

---

## Phase 8: User Story 5 - Crash and Partial-Write Recovery (Priority: P2)

**Goal**: Every shared/native boundary converges to exact recovery, explicit
ambiguity, or cleanup debt with no accepted-input loss or collateral mutation.

**Independent Test**: Inject deterministic crashes at each ledger, component,
process, server, lease, federation, and install transition and inspect exact
durable/native state after restart.

### Tests and Implementation for User Story 5

- [ ] T097 [US5] Add bounded deterministic fault points for ledger/component/process/server/lease/install transitions in `internal/testutil/faultpoint/` without enabling them in production builds
- [ ] T098 [US5] Run 100-iteration receipt/spool crash matrix and close every lost/duplicate/ambiguity path in `internal/daemon/lane_input_recovery_test.go`
- [ ] T099 [P] [US5] Run component daemon-restart, component-exit, PID-reuse, sequence-gap, and delivery-replay matrix in `internal/component/recovery_test.go`
- [ ] T100 [P] [US5] Run structured-child and product-server crash/reconnect/exit/redirect matrix in `internal/{structuredprocess,productserver}/*_recovery_test.go`
- [ ] T101 [P] [US5] Run CodeBuddy daemon/TUI/owned-lane-server crash, stale-registry, PID-reuse, and port-recycle permutations in `internal/products/codebuddy/recovery_test.go`
- [ ] T102 [P] [US5] Run DSH dual-owner/lease/crash/cancel/sandbox permutations and assert exact lease debt in `internal/products/dsh/recovery_test.go`
- [ ] T103 [US5] Add exact spool/component/server/process cleanup and unrelated-resource preservation tests in `internal/acceptance/six_product_cleanup_test.go`
- [ ] T104 [US5] Run interrupted install/update/remove recovery for all six strategies in `internal/releaseinstall/six_product_recovery_test.go`
- [ ] T105 [US5] Run hub/destination disconnect and retry/dedup recovery for every new lane capability in `internal/federation/six_product_recovery_test.go`
- [ ] T106 [US5] Expose receipt ambiguity, component unmanaged state, lease debt, and recovery diagnostics in `cmd/agent-sessions/{admin.go,lane.go}` with stable JSON tests
- [ ] T107 [US5] Record full Linux crash/recovery/nonmutation evidence in `specs/004-six-product-support/evidence/acceptance/linux-recovery.json`
- [ ] T108 [US5] Record full physical macOS crash/recovery/nonmutation evidence in `specs/004-six-product-support/evidence/acceptance/macos-recovery.json`

**Checkpoint**: Accepted work is never silently lost/replayed and every owned
resource converges or remains explicit debt.

---

## Phase 9: Documentation, Full Matrix, and Release Gate

**Purpose**: Make the feature usable by operators and bind all behavioral claims
to reproducible Linux/macOS evidence.

- [ ] T109 [P] Rewrite user-facing overview, install, systemd/launchd, hub, and first-run guidance for ten products in `README.md` and `docs/INSTALL.md`
- [ ] T110 [P] Add concise product setup/version/permission/failure guides in `docs/products/{opencode,kilocode,pi,omp,codebuddy,dsh}.md`
- [ ] T111 [P] Update cross-host, trusted-network security, operator roster/doctor, component, and lane CLI/MCP guidance in `docs/federation/`, `docs/SECURITY.md`, and `docs/OPERATIONS.md`
- [ ] T112 Expand the generated real-product acceptance runner/matrix for every declared capability in `scripts/realproducts/`, `scripts/test-real-products`, and `internal/releaseevidence/acceptance_products.go`
- [ ] T113 Extend release gate manifest/evidence schema with catalog digest, six native versions/tuples, component/ledger/federation cells, and explicit CodeBuddy pending credit in `scripts/release-gate-manifest` and `internal/releaseevidence/`
- [ ] T114 Run DRY/static audit proving no duplicate product arrays, unauthorized switches, init registration, new legacy imports, endpoint DSL, or secret-bearing durable refs; record `specs/004-six-product-support/evidence/acceptance/dry-audit.md`
- [ ] T115 Run repository-managed lint and complete normal suite, fixing root causes without weakening tests; record exact logs under `specs/004-six-product-support/evidence/acceptance/gates/`
- [ ] T116 Run race, vet, and all four supported builds; record exact logs and tool versions under `specs/004-six-product-support/evidence/acceptance/gates/`
- [ ] T117 Run full real-product/release/prebuilt-install gate on Linux and record commit/tree/native versions in `specs/004-six-product-support/evidence/acceptance/linux-final.json`
- [ ] T118 Run full real-product/release/prebuilt-install gate on physical macOS and record commit/tree/native versions in `specs/004-six-product-support/evidence/acceptance/macos-final.json`
- [ ] T119 Send the final diff/evidence to visible `fable-architect` for shared-contract, authority, DRY, permission, recovery, install, and matrix review; record sign-off in `specs/004-six-product-support/evidence/acceptance/fable-review.md`
- [ ] T120 Verify clean worktree, signed candidate lineage, zero unexplained debt/residue, and all non-account-gated cells green in `specs/004-six-product-support/evidence/acceptance/release-readiness.md`

---

## Frozen Contract Amendment Round 2

- [x] T121 Add product-neutral deferred native-session binding for create-on-first-turn lane drivers: explicit `LaneCapabilitySet.DeferredSessionBinding`, shared Open/StartTurn validation and exact-at-Open commit guard, atomic first-turn lane/receipt/turn binding, unbound restart/ambiguity/lease hostile tests, and the review-ready contract/evidence packet in `internal/{productruntime,daemon}/` and `specs/004-six-product-support/{contracts,evidence}/`

## Adopted Persistence-Audit Follow-ups (separate base-system work)

- [ ] T122 Record and schedule the product-neutral display-name projection cleanup: native title is the sole mutable writer, rename is write-through, and canonical peer addressing does not depend on a mutable display string
- [ ] T123 Record and schedule live on-demand peer presence/attestation so a healthy process is not muted solely by a daemon-generation re-adoption window
- [ ] T124 Record and schedule bounded garbage collection of detached attachment and terminal-delivery tombstones, with live ownership checks and no unrelated-state removal

## Post-Foundation Reviewed Amendments

- [x] T125 Replace released-binary protocol-3 compatibility machinery with the uniform protocol-4 handshake and one complete roster; delete the transport marker, per-client filtering, empty-capability inference, real-old binary test/scaffold/evidence; prove mismatch/N+1 rejection, roster equality, prospective amplification safety, normal/race/vet/fuzz/federation scripts; obtain isolated-commit Fable review and refreeze before federated-lane product credit
- [x] T126 Implement and freeze the bounded one-shot native launch handoff in `internal/{localtransport,launchhandoff}/` and `specs/004-six-product-support/contracts/launch-handoff.md`: exact live wrapper authority, memory-only sensitive command, full/zero/partial `go` outcome separation, reservation/capacity through finalization, shutdown convergence, truncated-frame refusal, syscall image replacement, normal/race/vet/Darwin compile cells, isolated independent security review, and Fable freeze before any secret-bearing peer launch credit

---

## Dependencies and Execution Order

### Phase dependencies

- Phase 1 truth gates block every source-interface task.
- Phase 2 freezes contracts and shared engines; it blocks all product work.
- US1, US2, and US3 product-owner work may proceed as four product streams once
  Phase 2 is green. Phase 6 serializes composition only after every stream has
  complete peer, lane, and parent drivers.
- US4 needs the frozen catalog/projection and product asset roots; it may overlap
  late product work only through disjoint product strategy files.
- US5 runs after native behavior exists and blocks final credit.
- Phase 9 blocks merge/release.

### Product stream dependencies

```text
T038/T042 -> T046/T050 -> T055/T059 -> T083/T090
T039/T043 -> T047/T051 -> T056/T060 -> T084/T091
T040/T044 -> T048/T052 -> T057/T061 -> T085/T092
T041/T045 -> T049/T053 -> T058/T062 -> T086/T093
```

Each product stream can progress independently against shared fakes until the
central tasks T063-T081 integrate the completed peer/lane/parent drivers.

### Shared foundation parallelism

After T013-T020 freeze shared types/state ownership and receive review:

```text
B1: T025-T029  component/local transport
B2: T030-T031  structured process
B3: T032-T033  product server
B4: T034       mock provider
```

T021-T024 and T035-T036 touch daemon-state/federation surfaces and are
serialized by the integrator.

## Parallel Fan-out Examples

### Truth gates

```text
Agent A: T002 Kilo spike
Agent B: T003 DSH spike
Agent C: T004 CodeBuddy spike
Agent E: T006 legacy audit
Agent F: T007 catalog projection spike
```

### Product implementation

```text
Agent A: OpenCode + Kilo stream (T038/T042/T046/T050/T055/T059)
Agent B: Pi + OMP stream (T039/T043/T047/T051/T056/T060)
Agent C: CodeBuddy stream (T040/T044/T048/T052/T057/T061)
Agent D: DSH stream (T041/T045/T049/T053/T058/T062)
Root integrator: central-only tasks after stream gates
```

## Implementation Strategy

1. Complete and jointly review truth gates; do not freeze around a red native
   assumption.
2. Build shared authority and transports with zero new product registered.
3. Fan out four product streams against fakes and typed contracts.
4. Complete peer, lane, and parent slices inside each product stream, then
   integrate them centrally and prove each story independently.
5. Derive install/release projection only after product asset contracts exist.
6. Run recovery matrices before broad real-product credit.
7. Merge only with physical Linux/macOS evidence and Fable/independent review.

## Task Format Validation

All 126 tasks use the required checkbox, sequential ID, optional `[P]`, required
story labels within story phases, and explicit file or evidence paths.
