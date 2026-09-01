---
description: "Parity-first implementation tasks for the unified user daemon"
---

# Tasks: Unified User Daemon

**Input**: Design documents from `specs/002-unified-user-daemon/`

**Prerequisites**: `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/`, the closed
202-cell acceptance inventory, and the working `c056fbc5015d4ab0a673f66cac5404206f7bcee6` source tree

**Tests**: Mandatory and test-first. All four products' mapped regressions are frozen before shared
production extraction. Real installed product evidence precedes deletion. Stop on the first genuine
RED and complete RCA before changing implementation.

**Organization**: Setup freezes machine-readable traceability and the complete baseline. Foundation
then extracts only behavior proven shared by all applicable products and produces a runnable minimal
daemon before any product cutover. Product cutover remains sequential: Codex → Claude → Grok → Qwen.

## Format: `[ID] [P?] [Story] Description with exact path`

- **[P]**: File-disjoint work that does not bypass an earlier gate
- **[Story]**: Required only in user-story phases
- A completed checkbox requires the named per-cell evidence, never compilation or an aggregate pass

## Phase 1: Setup — Freeze Traceability and the 202-Cell Baseline

**Purpose**: Make omissions, aggregate credit, and premature deletion mechanically impossible.

- [X] T001 Add a failing parser/cardinality/uniqueness/platform-applicability/exact-section-and-cell-locator/prerequisite-DAG/topology-delta-ledger test for exactly 202 fully expanded acceptance cells in `internal/releaseevidence/acceptance_matrix_test.go`
- [X] T002 Pin `gopkg.in/yaml.v3` and implement the checked acceptance-manifest loader used by tests and evidence tooling in `go.mod` and `internal/releaseevidence/acceptance_matrix.go`
- [X] T003 Add failing schema, current-status-only cumulative-predicate, monotonic-revision, referential-integrity, complete-202-cell-union, implementation-gate, and deletion-gate tests for the baseline port map in `internal/releaseevidence/baseline_port_map_test.go`
- [X] T004 Implement the checked port-map loader and stage validator in `internal/releaseevidence/baseline_port_map.go`
- [X] T005 Expand `SH-CLI`, `SH-IDENTITY`, `SH-MSG`, `L-SHARED`, `L-PRODUCT`, `FED`, and `INSTALL` to exact baseline function/test symbols, complete acceptance-cell sets, and deletion paths in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T006 Expand `C-LAUNCH` and `C-SUP` to exact Codex function/test symbols, complete acceptance-cell sets, and deletion paths in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T007 Expand `CL-LAUNCH` and `CL-MCP` to exact Claude function/test symbols, complete acceptance-cell sets, and deletion paths in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T008 Expand `G-LAUNCH` and `G-HOST` to exact Grok function/test symbols, complete acceptance-cell sets, and deletion paths in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T009 Expand `Q-LAUNCH` and `Q-HOST` to exact Qwen function/test symbols, complete acceptance-cell sets, and deletion paths in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T010 Record in `specs/002-unified-user-daemon/evidence/baseline-functional-cells.md` every ID in `specs/002-unified-user-daemon/contracts/acceptance-matrix.yml` with its exact baseline tests, scripts, installed commands, expected artifacts, cleanup assertions, and—where the cell asserts restart or reconnect convergence—the exact existing state predicate and existing deadline from the authoritative baseline test or script; for every permitted Agent Sessions topology substitution also record and review the baseline observation, target observation, and preserved functional/native invariant
- [X] T011 Run the unmodified baseline normal/race/vet/lint, mapped tests, and manifest validators on Linux and record exact pre-existing failures in `specs/002-unified-user-daemon/evidence/baseline-linux.md`
- [ ] T012 Run the identical unmodified baseline and manifest gates on real macOS and record exact pre-existing failures in `specs/002-unified-user-daemon/evidence/baseline-macos.md`

**Checkpoint**: The manifests expand to 202 acceptance cells; every implementation entry has exact old
symbols and tests; no production extraction has started.

---

## Phase 2: Regression Freeze — All Four Products Before Shared Code

**Purpose**: Turn the working product behavior into executable documentation before designing shared
production mechanisms.

- [X] T013 [P] Transplant all mapped Codex launch, App Server, owner, hook/MCP, messaging, resume, history, archive, fault, and cleanup assertions through a behavior-preserving seam in `internal/bridge/codex_daemon_parity_test.go`
- [X] T014 [P] Transplant all mapped Claude profile, settings, gate, registry/socket, selection, permission, MCP, rollback, resume, fault, and cleanup assertions in `internal/bridge/claude_daemon_parity_test.go`
- [X] T015 [P] Transplant all mapped Grok executable, token, TUI/host/leader/MCP ancestry, ACP, roster, interjection, permission, resume, fault, and cleanup assertions in `internal/bridge/grok_daemon_parity_test.go`
- [X] T016 [P] Transplant all mapped Qwen profile/readiness, capability, dual-output, daemon/ACP ancestry, event/input, permission, resume, archive, rollback, fault, and cleanup assertions in `internal/bridge/qwen_daemon_parity_test.go`
- [X] T017 Add event-specific bare and managed SessionStart, UserPromptSubmit, Stop, permission-refresh, stale-generation, and rollback assertions for every applicable product in `internal/bridge/hook_daemon_parity_test.go`
- [X] T018 Add a failing evidence-runner contract test that consumes the manifest prerequisite DAG and rejects fake-vendor product credit, unknown/missing/duplicate cell IDs, aggregate-only output, missing prerequisite results, prerequisite-red omission, missing verdict-conditional fields, and ambiguous reruns without an explicit same-cell/candidate/platform supersession link in `internal/acceptance/result_contract_test.go`
- [X] T019 Run all four transplanted product families together without production extraction, record exact named results, populate replacement tests/evidence, and advance `C-LAUNCH`, `C-SUP`, `CL-LAUNCH`, `CL-MCP`, `G-LAUNCH`, `G-HOST`, `Q-LAUNCH`, and `Q-HOST` to `transplanted` in `specs/002-unified-user-daemon/evidence/transplanted-regressions.md` and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`

**Checkpoint**: All four native contracts can disprove a proposed shared mechanism. No daemon adapter or
shared attachment implementation exists yet.

---

## Phase 3: Foundation — Shared Invariants and a Runnable Minimal Daemon

**Purpose**: Extract only proven shared behavior, then provide the real daemon/control/client substrate
required by product acceptance.

- [X] T020 [P] Add failing product/help/plugin/lane inventory parity tests in `internal/productcatalog/catalog_test.go`
- [X] T021 Implement one authoritative four-product capability and command inventory in `internal/productcatalog/catalog.go`
- [X] T022 Add table-driven byte/order/delimiter/repeated-option parity tests across all four wrappers, then record passing replacement tests and advance `SH-CLI` to `transplanted` in `internal/launcher/options_test.go` and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T023 Extract one wrapper option scanner with explicit product option tables in `internal/launcher/options.go`
- [X] T024 Add cross-platform exact PID/start/strong-start/ancestry and no-follow filesystem identity regressions, then record passing replacement tests and advance `SH-IDENTITY` to `transplanted` in `internal/procinfo/identity_test.go`, `internal/pathidentity/identity_test.go`, and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [X] T025 Consolidate the proven process and filesystem identity primitives in `internal/procinfo/identity.go` and `internal/pathidentity/identity.go`
- [X] T026 Add failing bounded-atomic-write, crash, stale-writer, lifecycle-transition, and cleanup-debt tests in `internal/statestore/store_test.go` and `internal/daemon/state_test.go`
- [X] T027 Implement bounded atomic HostRuntime, ManagedAttachment, NativeEvidence, Delivery, Lane, Turn, and CleanupDebt storage in `internal/statestore/store.go` and `internal/daemon/state.go`
- [X] T028 Add failing fixed-endpoint, same-user, framing, launcher/hook/connector/admin role, generation, idempotency, duplicate-daemon, bare-inactive, and no-lifecycle-over-socket tests in `internal/daemon/control_test.go` and `internal/daemon/control_unix_test.go`
- [X] T029 Implement the one private local control endpoint and framed role-scoped protocol in `internal/daemon/control.go` and `internal/daemon/control_unix.go`
- [X] T030 Add failing prepare/adopt/refresh/detach/rollback tests over all four distinct NativeEvidence variants in `internal/daemon/attachment_test.go`
- [X] T031 Implement one shared attachment transaction with explicit product callbacks in `internal/daemon/attachment.go`
- [X] T032 Add credential/profile metadata-only and prompt/result/transcript/log content canaries across hooks, connectors, diagnostics inputs, and test evidence in `internal/securityboundary/content_canary_test.go`
- [X] T033 Add failing connector framing, reconnect, stale-generation, exact-attestation, canonical bare-inactivity, and content-canary tests in `internal/bridge/mcp_relay_test.go`
- [X] T034 Implement the stateless evidence-preserving MCP relay in `internal/bridge/mcp_relay.go`
- [X] T035 Implement event-specific hook dispatch that is silent for bare sessions and exact-evidence gated for managed sessions in `internal/bridge/hook_dispatch.go`
- [X] T036 Add failing daemon single-authority, explicit-stop, crash-recovery, component-cancellation, and workflow-command-does-not-bootstrap tests in `internal/daemon/runtime_test.go`
- [X] T037 Compose the minimal attachment/control/relay/state runtime into one cancellable authority in `internal/daemon/runtime.go`
- [X] T038 Add failing multi-call dispatch, alias, help, passthrough, daemon-unavailable, and no-lifetime-management tests in `internal/clihelp/commands_test.go` and `cmd/agent-sessions/main_test.go`
- [X] T039 Implement the minimal `agent-sessions` daemon/admin/peer/lane/hook/connector multi-call image in `cmd/agent-sessions/main.go` and `internal/clihelp/commands.go`
- [X] T040 Implement structured per-cell result validation, verdict-conditional evidence, prerequisite-RED propagation, and explicit same-cell/candidate/platform rerun supersession in `internal/acceptance/result.go`
- [X] T041 Add the real-product runner that invokes authenticated vendor binaries, consumes exact requested cell IDs, and performs exact test-owned cleanup in `scripts/test-real-products`

**Checkpoint**: Shared code is derived from four frozen contracts. A real daemon process and short-lived
clients exist before the first product adapter is accepted.

---

## Phase 4: User Story 1 — Preserve Interactive Peers (Priority: P1) 🎯 MVP

**Goal**: Move interactive Agent Sessions ownership product-by-product without changing native behavior.

**Independent Test**: All `C-*`, `CL-*`, `G-*`, and `Q-*` cells pass individually on authenticated
Linux and macOS installations, with exact native history, process, profile, identity, and cleanup evidence.

### Codex

- [ ] T042 [US1] Refactor the mapped lazy App Server, launch/resume, owner, hook, delivery, history, archive, and cleanup operations into callable functions while keeping the legacy caller green in `internal/bridge/appserver.go`, `internal/bridge/launch.go`, `internal/bridge/supervisor.go`, and `internal/bridge/hook.go`
- [ ] T043 [US1] Add failing daemon-backed Codex adapter tests for all mapped evidence variants and `C-01..C-18` prerequisites in `internal/daemon/adapter_codex_test.go`
- [ ] T044 [US1] Connect daemon attachment ownership to the transplanted Codex operations, including lazy App Server startup, in `internal/daemon/adapter_codex.go`
- [ ] T045 [US1] Run and record `C-01..C-18` individually on an installed authenticated Linux Codex in `specs/002-unified-user-daemon/evidence/codex-linux.md`
- [ ] T046 [US1] Run and record `C-01..C-18` individually on an installed authenticated macOS Codex in `specs/002-unified-user-daemon/evidence/codex-macos.md`

### Claude

- [ ] T047 [US1] Refactor the mapped profile/settings/gate/row/socket/selection/delivery/permission/rollback/cleanup operations into callable functions while keeping the legacy caller green in `internal/launcher/claude_peer.go` and `internal/bridge/mcp.go`
- [ ] T048 [US1] Add failing daemon-backed Claude adapter tests for every mapped evidence variant and `CL-01..CL-11` prerequisite in `internal/daemon/adapter_claude_test.go`
- [ ] T049 [US1] Connect daemon attachment ownership to the transplanted Claude operations and presence-sensitive profile evidence in `internal/daemon/adapter_claude.go`
- [ ] T050 [US1] Run and record `CL-01..CL-11` individually on an installed authenticated Linux Claude in `specs/002-unified-user-daemon/evidence/claude-linux.md`
- [ ] T051 [US1] Run and record `CL-01..CL-11` individually on an installed authenticated macOS Claude in `specs/002-unified-user-daemon/evidence/claude-macos.md`

### Grok

- [ ] T052 [US1] Refactor the mapped executable/token/TUI-host-leader-MCP ancestry/ACP/roster/wake/interjection/selection/cleanup operations into callable functions while keeping the legacy caller green in `internal/launcher/grok_peer.go` and `internal/bridge/grok.go`
- [ ] T053 [US1] Add failing daemon-backed Grok adapter tests for every mapped evidence variant and `G-01..G-21` prerequisite in `internal/daemon/adapter_grok_test.go`
- [ ] T054 [US1] Connect daemon attachment ownership to transplanted Grok operations without inventing daemon-child native ancestry in `internal/daemon/adapter_grok.go`
- [ ] T055 [US1] Run and record `G-01..G-21` individually on an installed authenticated Linux Grok in `specs/002-unified-user-daemon/evidence/grok-linux.md`
- [ ] T056 [US1] Run and record `G-01..G-21` individually on an installed authenticated macOS Grok in `specs/002-unified-user-daemon/evidence/grok-macos.md`

### Qwen

- [ ] T057 [US1] Refactor the mapped profile/readiness/capability/dual-output/native-ancestry/artifact/delivery/archive/rollback/cleanup operations into callable functions while keeping the legacy caller green in `internal/launcher/qwen_peer.go`, `internal/bridge/qwen.go`, and `internal/bridge/qwen_host.go`
- [ ] T058 [US1] Add failing daemon-backed Qwen adapter tests for every mapped evidence variant and `Q-01..Q-10` prerequisite in `internal/daemon/adapter_qwen_test.go`
- [ ] T059 [US1] Connect daemon attachment ownership to the transplanted Qwen operations in `internal/daemon/adapter_qwen.go`
- [ ] T060 [US1] Run and record `Q-01..Q-10` individually on an installed authenticated Linux Qwen in `specs/002-unified-user-daemon/evidence/qwen-linux.md`
- [ ] T061 [US1] Run and record `Q-01..Q-10` individually on an installed authenticated macOS Qwen in `specs/002-unified-user-daemon/evidence/qwen-macos.md`

### Cross-product interactive gate

- [ ] T062 [US1] Run four simultaneous real peers through idle/busy daemon restart, bare hook/MCP coexistence, exact rediscovery, accepted direct messaging, and cleanup in `scripts/test-real-products`
- [ ] T063 [US1] Record the cross-product results and every prerequisite cell verdict without aggregate substitution in `specs/002-unified-user-daemon/evidence/interactive-cross-product.md`

**Checkpoint**: All four real interactive products work through daemon-owned attachments; no old path is deleted.

---

## Phase 5: User Story 2 — Preserve Lanes and Collaboration (Priority: P1)

**Goal**: Move local collaboration and lane ownership once while retaining every product-native lane rule.

**Independent Test**: `L-01..L-30`, all sixteen `P-*`, all sixty-four `M-*`, and all four `A-*` cells
pass individually with group, permission, lifecycle, restart, archive/resume, and cleanup evidence.

- [x] T064 [US2] Transplant mapped AgentFrame, global-group, discovery, direct/multicast/broadcast/reply, acknowledgement, authorization, and local delivery regressions, then record passing replacement tests and advance `SH-MSG` to `transplanted` in `internal/federation/frame_test.go`, `internal/federation/groups_test.go`, `internal/daemon/delivery_test.go`, and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [x] T065 [US2] Move AgentFrame, global groups, local routing, and delivery acceptance exactly once into `internal/federation/frame.go`, `internal/federation/groups.go`, and `internal/daemon/delivery.go`
- [x] T066 [US2] Transplant shared lane turn/notice/collection/archive/restart/cleanup regressions and all product-specific native differences, then record passing replacement tests and advance `L-SHARED` and `L-PRODUCT` to `transplanted` in `internal/daemon/lane_test.go` and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [x] T067 [US2] Implement the durable shared lane, turn, notice, collection, and cleanup-debt engine in `internal/daemon/lane.go` and `internal/daemon/turn.go`
- [x] T068 [US2] Add failing Codex native lane callback tests in `internal/daemon/adapter_codex_lane_test.go`
- [x] T069 [US2] Connect Codex App Server lane launch/resume/permission/archive/interrupt operations in `internal/daemon/adapter_codex_lane.go`
- [x] T070 [US2] Add failing Claude native lane callback tests in `internal/daemon/adapter_claude_lane_test.go`
- [x] T071 [US2] Connect Claude stream worker/permission/transcript/archive/interrupt operations in `internal/daemon/adapter_claude_lane.go`
- [x] T072 [US2] Add failing Grok native lane callback and sole-ACP-driver tests in `internal/daemon/adapter_grok_lane_test.go`
- [x] T073 [US2] Connect Grok ACP session/roster/permission/archive/interrupt operations in `internal/daemon/adapter_grok_lane.go`
- [x] T074 [US2] Add failing Qwen native lane callback and sole-ACP-client tests in `internal/daemon/adapter_qwen_lane_test.go`
- [x] T075 [US2] Connect Qwen ACP/event/input/mode/permission/archive/interrupt operations in `internal/daemon/adapter_qwen_lane.go`
- [X] T076 [US2] Add missing/duplicate/unknown-ID and destination-evidence checks for the lane and messaging runners in `internal/acceptance/matrix_runner_test.go`
- [ ] T077 [US2] Run and record `L-01..L-30` for every applicable real target/parent/platform scope in `specs/002-unified-user-daemon/evidence/lane-lifecycle.md`
- [ ] T078 [US2] Run and record all sixteen explicit `P-*` cells on Linux and macOS in `specs/002-unified-user-daemon/evidence/lane-composition.md`
- [ ] T079 [US2] Run and record all sixty-four explicit `M-*` cells with idle/busy destination acknowledgement and group isolation on Linux and macOS in `specs/002-unified-user-daemon/evidence/messaging-matrix.md`
- [ ] T080 [US2] Run and record `A-C`, `A-CL`, `A-G`, and `A-Q` with exact archive/unarchive counts or explicit native `N/A` in `specs/002-unified-user-daemon/evidence/archive-matrix.md`
- [ ] T081 [US2] Restart the daemon during one accepted turn for each target product and prove baseline continuation or one collectable interruption without redispatch in `specs/002-unified-user-daemon/evidence/lane-restart.md`

**Checkpoint**: Existing local collaboration and lanes are green under one daemon-owned implementation.

---

## Phase 6: User Story 3 — Operate One Host Authority (Priority: P1)

**Goal**: Install and operate one standard user service without taking ownership of vendor processes.

**Independent Test**: Service lifecycle cells pass with four live products, one endpoint, metadata-only
diagnostics, no mixed version, and no native-session termination.

- [x] T082 [US3] Add failing systemd user enable/start/stop/restart/upgrade/failed-update-preservation/explicit-stop/unrelated-service tests in `internal/servicecontrol/service_linux_test.go`
- [x] T083 [US3] Add failing launchd bootstrap/bootout/kickstart/upgrade/failed-update-preservation/explicit-stop/sleep-wake/unrelated-agent tests in `internal/servicecontrol/service_darwin_test.go`
- [x] T084 [US3] Add failing status/doctor/readiness schema, truthful projection, bounded-output, and credential/message/transcript content-canary tests in `internal/diagnostics/report_test.go` and `internal/daemon/admin_test.go`
- [x] T085 [US3] Add failing four-peer idle/busy restart, crash recovery, generation replacement, no-redispatch, and exact residue tests in `internal/daemon/runtime_recovery_test.go`
- [x] T086 [US3] Implement systemd user service control and assets in `internal/servicecontrol/service_linux.go` and `deploy/agent-sessions/systemd/user/agent-sessions.service`
- [x] T087 [US3] Implement launchd user-agent control and assets in `internal/servicecontrol/service_darwin.go` and `deploy/agent-sessions/launchd/net.antst.agent-sessions.plist`
- [x] T088 [US3] Implement metadata-only diagnostic projection in `internal/diagnostics/report.go` and `internal/daemon/admin.go`
- [x] T089 [US3] Expose explicit start/stop/restart through systemd or launchd and status/doctor through the running daemon, without granting lifecycle authority to workflow commands, in `cmd/agent-sessions/main.go`
- [ ] T090 [US3] Run four-real-peer and four-active-lane restart/crash/upgrade recovery in `scripts/test-daemon-restart`
- [ ] T091 [US3] Run Linux systemd login/start/stop/crash/restart/upgrade/failed-update-preservation/removal/reinstall acceptance in `specs/002-unified-user-daemon/evidence/service-linux.md`
- [ ] T092 [US3] Run macOS launchd login/start/stop/crash/restart/sleep-wake/upgrade/failed-update-preservation/removal/reinstall acceptance in `specs/002-unified-user-daemon/evidence/service-macos.md`

**Checkpoint**: One service-managed daemon and endpoint own Agent Sessions state; vendor clients remain external.

---

## Phase 7: User Story 4 — Preserve Multi-Host Federation (Priority: P2)

**Goal**: Embed the outbound host agent and preserve the independent hub and existing global space.

**Independent Test**: `X-01..X-08` and the remote scopes of `P-*`, `M-*`, and `L-*` pass across Linux
and macOS with equal-protocol unrelated builds and mismatch refusal.

- [x] T093 [US4] Transplant only network host-registration/reconnect/remote-delivery/remote-lane/hub regressions, then record passing replacement tests and advance `FED` to `transplanted` in `internal/federation/host_test.go`, `internal/federation/hub_test.go`, and `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [x] T094 [US4] Add exact protocol-version, malformed-frame, duplicate-delivery, reconnect-generation, and unrelated-build compatibility tests in `internal/federation/protocol_test.go`
- [x] T095 [US4] Embed outbound host registration, reconnect, remote delivery, and remote lane transport over the already-moved local frame/groups implementation in `internal/daemon/federation.go` and `internal/federation/host.go`
- [x] T096 [US4] Implement the independent hub runtime and command in `internal/federation/hub.go` and `cmd/agent-sessions-hub/main.go`
- [ ] T097 [US4] Run and record `X-01..X-08` plus remote `P-*`, `M-*`, and `L-*` scopes across Linux and macOS in `specs/002-unified-user-daemon/evidence/federation.md`
- [ ] T098 [US4] Prove unrelated-build equal-protocol interoperability, independent host/hub upgrades, and pre-registration mismatch refusal in `scripts/federation/binary_pair_test.py` and `specs/002-unified-user-daemon/evidence/protocol-compatibility.md`

**Checkpoint**: The existing one-hub/multiple-host space works without a second host authority.

---

## Phase 8: User Story 5 — Install Across a Greenfield Boundary (Priority: P2)

**Goal**: Ship version 0.3 without compatibility machinery for unreleased Agent Sessions state.

**Independent Test**: After operator-controlled quiescence, `U-01..U-12` pass while vendor profiles,
authentication, settings, transcripts, and histories remain untouched and usable.

- [x] T099 [US5] Add failing greenfield-boundary tests proving active standard install, `install-all`, update, remove, service, package, help, plugin, and executable dependency surfaces contain no legacy Agent Sessions process/state names or inventory/adopt/drain/retire/migrate/cleanup paths and treat operator-provided old-stack quiescence solely as an external harness precondition; explicitly exclude historical evidence/tests, the repository-only cleanup contract, the direct cleanup script, and fixture-isolated tests from that active-surface name scan while proving no Makefile names either cleanup artifact or defines a cleanup target and no production dependency can invoke the script in `internal/releaseinstall/greenfield_test.go`
- [x] T100 [US5] Add failing staged-validation/partial-commit/prior-install-preservation/service-restart/removal tests in `internal/releaseinstall/transaction_test.go`
- [x] T101 [US5] Add failing optional-product/native-installer/exact-source/rollback/removal/no-credential-read tests for all four connectors in `internal/releaseinstall/connectors_test.go`
- [x] T102 [US5] Add failing two-binary/four-platform/payload/checksum/reproducibility/prebuilt-no-Go archive tests in `internal/releasepkg/inventory_test.go`
- [x] T103 [US5] Implement staged host/hub install, prior-install-preserving update, one service restart, and removal in `internal/releaseinstall/transaction.go`
- [x] T104 [US5] Implement supported Codex, Claude, Grok, and Qwen connector transactions without credential access in `internal/releaseinstall/connectors.go`
- [x] T105 [US5] Implement host/hub install/remove targets and four-platform packaging in `Makefile` and `scripts/package-release`, then run T099-T105, record passing replacement tests, and advance `INSTALL` to `transplanted` in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [x] T106 [US5] Derive and review the complete Linux/macOS legacy target inventory from `c056fbc`, encode its sole authoritative stable-ID/platform/exact-path-or-bounded-root/expected-type/owner/legacy-source/removal-scope/metadata-revision contract in `specs/002-unified-user-daemon/contracts/pre-unification-cleanup.yml`, and add failing direct-script tests for no-glob/no-discovery enforcement, default plan-only behavior with a deterministic non-content plan revision, separate explicit `--apply <plan-revision>`, zero mutation when the recomputed complete plan differs, changed-revision refusal between apply-time enumeration and deletion, opaque deletion without content reads or hashes, vendor transcript/history/credential/non-Agent-Sessions-settings/ordinary-file canary preservation, unrelated plugin/process preservation, repeat-safe second execution, fixture-root confinement, and absence from release archives in `internal/releaseevidence/pre_unification_cleanup_test.go`
- [x] T107 [US5] Implement `scripts/cleanup-pre-unification` as the sole operational consumer of `specs/002-unified-user-daemon/contracts/pre-unification-cleanup.yml`: no-argument mode emits the ordered metadata-only plan and deterministic revision, `--apply <plan-revision>` recomputes the complete plan and mutates nothing unless the revision matches, and each removal revalidates its current metadata tuple; add no operational Make target, installer/updater/remover/service call, release-package entry, or production-code dependency, and permit generic automated tests to invoke it only with an explicit fixture-owned root
- [ ] T108 [US5] Prove the old stack is quiescent through an external metadata-only process census, invoke `scripts/cleanup-pre-unification` with no arguments and record its mutation-free reviewed plan and plan revision, invoke it separately with `--apply <plan-revision>`, then record metadata-only before/after/second-plan evidence on each of the three controlled hosts; prove a changed complete plan causes zero mutation, changed per-target revisions fail closed, every contracted legacy target is absent, vendor profiles and histories still authenticate and resume, vendor ordinary files remain intact, and record exact boundaries in `specs/002-unified-user-daemon/evidence/pre-unification-cleanup.md`
- [ ] T109 [US5] Run and record `U-01..U-12` during controlled Linux/macOS greenfield transitions and prove vendor authentication/history resume in `specs/002-unified-user-daemon/evidence/greenfield-install.md`

**Checkpoint**: Version 0.3 installs repeatably and contains no prototype compatibility subsystem; the
one-time controlled-host cleanup remains a direct repository utility outside every standard product
lifecycle and release artifact.

---

## Phase 9: Cutover, Deletion, and Release Proof

**Purpose**: Remove old topology only after every mapped replacement and all 202 cells are accounted for.

- [ ] T110 Complete every machine port-map status, new-symbol, replacement-test, acceptance-cell, evidence, and deletion field and validate status `removable` in `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`
- [ ] T111 Remove only the exact `deletion_paths` authorized by completed manifest entries and preserve mapped native operations, recording the deletion diff in `specs/002-unified-user-daemon/evidence/deletion-trace.md`
- [x] T112 Prove installed aliases resolve to `agent-sessions`, the hub is `agent-sessions-hub`, and all four archives contain exactly those two executable basenames and exclude both `scripts/cleanup-pre-unification` and `specs/002-unified-user-daemon/contracts/pre-unification-cleanup.yml` in `internal/releaseevidence/release_inventory_test.go`
- [ ] T113 [P] Update final behavior and operations, project only the reviewed topology-delta ledger into target-topology acceptance wording, document the direct controlled-host cleanup boundary without exposing it as a standard installation command, and verify every non-topology functional/native assertion remains unchanged in `README.md`, `docs/INSTALL.md`, `docs/ADAPTER-PROTOCOL.md`, `docs/GROUPS.md`, `docs/FEDERATION.md`, `docs/ACCEPTANCE-MATRIX.md`, and `docs/TROUBLESHOOTING.md`
- [ ] T114 Run normal/race/vet/lint/Bash-3.2/four-build/package/install/content-canary gates; validate the reviewed topology-delta ledger; and validate one schema-complete authoritative result, including prerequisite and supersession rules, for every applicable one of the 202 cells in `specs/002-unified-user-daemon/evidence/final-matrix.md`
- [ ] T115 Run the complete quickstart independently on real Linux and record exact identities, toolchains, product versions, failures, preserved state, and residue in `specs/002-unified-user-daemon/evidence/final-linux.md`
- [ ] T116 Run the complete quickstart independently on real macOS and record exact identities, toolchains, product versions, failures, preserved state, and residue in `specs/002-unified-user-daemon/evidence/final-macos.md`
- [ ] T117 Perform final constitution, per-cell, credential/content, platform-parity, process-census, controlled-host-cleanup boundary, and 100%-deletion-traceability review in `specs/002-unified-user-daemon/evidence/final-review.md`

---

## Dependencies & Execution Order

### Phase dependencies

- Setup blocks every other phase.
- Regression Freeze depends on reviewed machine manifests and baseline evidence.
- Foundation depends on all four transplanted product families being green together.
- US1 depends on the runnable daemon/control/client foundation and proceeds sequentially Codex → Claude → Grok → Qwen.
- US2 depends on all four real interactive adapters.
- US3 depends on interactive and lane ownership working in a daemon process.
- US4 depends on the single local frame/groups/delivery implementation and daemon lane callbacks.
- US5 depends on final host/hub roles and service contracts.
- T109 depends on T106-T108 establishing and proving the external greenfield cleanup precondition on the
  controlled hosts; T106-T108 never become dependencies of standard product lifecycle code.
- Cutover depends on all stories, every required per-cell result, and port-map status `removable`.

### Dependency graph

```text
Traceability + 202 IDs
  -> all four regression families
  -> shared extraction + runnable daemon
  -> US1(Codex -> Claude -> Grok -> Qwen)
  -> US2
  -> US3
  -> US4
  -> US5 direct controlled-host cleanup + standard install
  -> Cutover
```

### Within each product

1. Exact port-map rows and frozen old tests already exist.
2. Refactor only mapped old native operations into callable functions while the legacy caller remains green.
3. Add the failing daemon-adapter test.
4. Connect daemon ownership.
5. Run focused and full source gates.
6. Run every exact installed Linux cell ID.
7. Run every exact installed macOS cell ID.
8. Stop and complete RCA on any genuine RED before touching the next product.

### Parallel opportunities

- T013–T016 are file-disjoint after the manifest and baseline gates.
- T020 may proceed independently after T019; T022 and T024 are sequential manifest commits because both
  update `specs/002-unified-user-daemon/contracts/baseline-port-map.yml`.
- Linux and macOS execution of one already-built candidate may run concurrently when neither mutates shared infrastructure.
- T113 documentation may proceed beside the final validation preparation after T110 completes.
- Product cutover tasks are deliberately sequential and never parallel.

## Implementation Strategy

### MVP: Four Preserved Interactive Products

The first user-visible increment is not one abstract adapter. It is the runnable daemon plus all four
interactive products passing their complete installed cells. A single product may be an internal
checkpoint, but it does not satisfy User Story 1 or justify deleting shared baseline code.

### Incremental delivery

Keep the legacy path runnable while each product is cut over. Move local messaging and lanes once only
after all four interactive products work. Move network host/hub responsibilities after local
collaboration. Remove obsolete topology only at the final manifest-authorized cutover.

## Notes

- `[P]` never permits bypassing a parity checkpoint.
- Fake vendor helpers test protocol primitives and injected failures only.
- A green aggregate command does not mark unnamed cells complete.
- Do not weaken functional or native-product assertions to fit new topology; update only Agent Sessions process/service/package observations listed in the reviewed topology-delta ledger.
