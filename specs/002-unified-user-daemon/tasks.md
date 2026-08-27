---

description: "Dependency-ordered implementation tasks for the unified user daemon"
---

# Tasks: Unified User Daemon

**Input**: Design documents from `/specs/002-unified-user-daemon/`

**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`

**Tests**: Required by the feature specification and project constitution. In every story, create the
listed failing tests before implementing the behavior they cover. Stop on the first genuine RED and
establish RCA before changing the design or weakening a gate.

**Organization**: Tasks are grouped by user story. The implementation converges existing behavior only:
one `agent-sessions` daemon/image per OS user-host, one separately deployed `agent-sessions-hub` binary
for the network's central hub, multiple embedded host agents, existing global groups, independently
built same-repository hub/host deployments interoperating by exact hub-protocol equality, and no live
handoff during first migration.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: May run in parallel after its phase prerequisites because it changes distinct files.
- **[Story]**: Maps the task to the corresponding user story in `spec.md`.
- Every task names the files it must create or modify.

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Establish the new package and evidence layout without changing runtime behavior.

- [X] T001 Record the authoritative pre-unification process, socket, command, state-root, service, and release inventory plus a closed, individually named inventory of every existing peer, lane, group, host-suffix, permission, archive, collection, cleanup, resume, composition, and federation acceptance cell in specs/002-unified-user-daemon/evidence/baseline-runtime-inventory.md and specs/002-unified-user-daemon/evidence/baseline-functional-cells.md
- [X] T002 Create the canonical host command/daemon skeleton and distinct hub command skeleton in cmd/agent-sessions/main.go, cmd/agent-sessions-hub/main.go, and internal/daemon/doc.go
- [X] T003 [P] Add in-process daemon test fixtures that never start a second user daemon in internal/testutil/daemon.go and internal/testutil/daemon_test.go
- [X] T004 [P] Create host, hub, and acceptance asset directories; seed deploy/agent-sessions/VERSION from the current declared source-release value without yet removing the legacy version file; and document independent host/hub ownership in deploy/agent-sessions/README.md, deploy/agent-sessions-hub/README.md, and scripts/README-unified-daemon.md

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Implement the one-host-binary, one-host-endpoint durable-authority substrate and the shared
two-binary command/protocol inventories required by every user story.

**Critical**: No user-story implementation starts until this phase is green.

### Foundational tests

- [X] T005 [P] Add failing DRY and completeness tests for one authoritative four-product/two-binary/release/hub-protocol inventory plus canonical mode, host alias, parser/help, exact environment, JSON-schema, numeric-exit, checked-documentation parity, and exact federation protocol descriptor/feature-contract/docs parity in internal/productcatalog/catalog_test.go, internal/bridge/product_test.go, internal/federation/protocol_contract_test.go, and internal/clihelp/contract_test.go
- [X] T006 [P] Add failing role-neutral atomic-state, revision-CAS, corruption, symlink, changed-type, disk-full, file-descriptor-exhaustion, and crash-recovery tests in internal/statestore/store_test.go plus host-schema tests in internal/daemon/state_test.go
- [X] T007 [P] Add failing NDJSON hello/request/response, frame-bound, generation, idempotency, and role-operation contract tests in internal/daemon/control_test.go
- [X] T008 [P] Add failing fixed-endpoint, duplicate-daemon lock, stale-socket identity, AF_UNIX budget, and TMPDIR-independence tests in internal/daemon/control_unix_test.go and internal/daemon/control_darwin_test.go
- [X] T009 [P] Add the test-owned foundational host-core observability manifest API and tests in internal/testutil/observability_manifest.go and internal/testutil/observability_manifest_test.go plus failing shared metadata-only envelope/redaction/bounded-error canaries covering normal, debug, error, crash-report, metric, and trace output and host logs/status/doctor in internal/diagnostics/output_test.go and internal/daemon/diagnostics_test.go; fail if any host-core sink listed in that manifest bypasses the content canary, without introducing a production sink registry or depending on later service-manager or hub boundaries

### Foundational implementation

- [X] T010 Implement the shared product, capability, host-command alias, host/hub binary-role, connector, and hub-protocol inventory in internal/productcatalog/catalog.go; declare the initial unified wire contract as ProtocolVersion 3 while making exact protocol equality the sole software-version interoperability input; retain the four existing lane capability tokens as operation availability only; replace duplicate descriptor tables in internal/bridge/product.go, internal/federator/product.go, and internal/launcher/product.go; establish internal/federation as the logical package, move the shared AgentFrame and group contract plus tests from internal/federator/frame.go and internal/federator/groups.go into internal/federation/frame.go and internal/federation/groups.go, and update every existing federator consumer to import the moved contracts so Foundation stays green before later stories consume them; and update docs/FEDERATION.md from the authoritative descriptor so T005's feature-contract/document parity gate becomes green during Foundation
- [X] T011 Implement canonical non-secret configuration, state-root, and platform runtime-root resolution in internal/daemon/config.go, internal/daemon/paths.go, internal/daemon/paths_linux.go, and internal/daemon/paths_darwin.go
- [X] T012 Implement shared atomic revisioned records/journals in internal/statestore/store.go and compose schema-versioned host runtime, attachment, delivery, lane, federation, transaction, and debt records over it in internal/daemon/state.go
- [X] T013 Implement the single process composition root, generation lifecycle, admission states, and ordered recovery skeleton in internal/daemon/runtime.go
- [X] T014 Implement the one private Unix listener, NDJSON framing, request correlation, frame bounds, subscriptions, and generation checks in internal/daemon/control.go
- [X] T015 Implement same-user peer credentials and exact PID/start corroboration for Linux and Darwin in internal/daemon/peercred_linux.go and internal/daemon/peercred_darwin.go
- [X] T016 Implement role-scoped local dispatch, daemon-unavailable errors, idempotency-key replay, and prohibition of model-facing admin operations in internal/daemon/dispatch.go and internal/daemon/errors.go
- [X] T017 Implement shared metadata-only diagnostic envelopes/redaction in internal/diagnostics/output.go and compose ordered host state recovery, retryable lifecycle debt, status, and doctor projections in internal/daemon/recovery.go and internal/daemon/diagnostics.go
- [X] T018 Implement one typed CLI descriptor inventory for both binaries, exact environment/JSON/numeric-exit contracts, generated help and checked docs/CLI.md in internal/clihelp/catalog.go, and canonical host daemon/admin/launcher/connector/lane multi-call/argv0 dispatch in cmd/agent-sessions/main.go without building additional host-side thin executables; reserve hub dispatch exclusively for cmd/agent-sessions-hub/main.go

**Checkpoint**: In-process tests prove one host runtime authority, one host endpoint, durable revisions,
one host command image, and a distinct declared hub binary/protocol boundary; no product behavior or hub
implementation has moved yet.

---

## Phase 3: User Story 1 — Operate One Runtime Authority (Priority: P1) — MVP

**Goal**: Install, run, inspect, restart, upgrade, stop, remove, and purge one service-managed daemon
without mixed versions or launcher-driven lifecycle management.

**Independent Test**: On a clean Linux or macOS user, install with an arbitrary subset of native
products, prove one service/process/socket, restart and upgrade it transactionally, explicitly stop it
without resurrection, and verify workflow commands fail without starting it.

### Tests for User Story 1

- [X] T019 [P] [US1] Add failing single-authority, generation, crash-restart, explicit-stop, workflow-does-not-start-service, and daemon-admission disk/memory/file-descriptor/process exhaustion tests in internal/daemon/runtime_lifecycle_test.go
- [X] T020 [P] [US1] Extend the test-owned observability manifest through the file-disjoint internal/testutil/observability_linux.go fragment with Linux service-manager boundaries and add failing descriptor-driven systemd user enable/start/restart/stop/disable and login-start contract tests shared by host and hub roles, including captured journal/stdout/stderr content canaries for normal, debug, failure, and crash exits, in internal/servicecontrol/service_linux_test.go
- [X] T021 [P] [US1] Extend the test-owned observability manifest through the file-disjoint internal/testutil/observability_darwin.go fragment with macOS service-manager boundaries and add failing descriptor-driven launchd bootstrap/KeepAlive/bootout/explicit-stop and sleep-wake contract tests shared by host and hub roles, including captured launchd stdout/stderr content canaries for normal, debug, failure, and crash exits, in internal/servicecontrol/service_darwin_test.go
- [X] T022 [P] [US1] Add failing role-neutral immutable-release, same-version identity, disjoint host/hub release-root/selection/lock ownership, pointer commit, readiness-hook, crash-journal, removal, purge, and exact prior-state restoration tests in internal/releaseinstall/transaction_test.go plus host-role all-four-product connector preparation/commit/rollback/removal tests in internal/daemon/install_hooks_test.go
- [X] T023 [P] [US1] Add failing status/doctor schema, exact identity, adapter isolation, admin completeness, canonical parsed-option/help/environment/JSON/exit parity, and MCP admin-absence tests in internal/daemon/admin_test.go and cmd/agent-sessions/help_test.go
- [X] T024 [P] [US1] Add failing zero/one/several/all-product host install, sole deploy/agent-sessions/VERSION authority with no legacy version file, exact two-binary archive inventory, and host-install hub exclusion tests in internal/releasepkg/unified_archive_test.go and internal/releaseevidence/unified_inventory_test.go; obsolete command/service asset absence remains the T072/T087 gate after those assets are actually retired
- [X] T025 [P] [US1] Add failing active-blocker refusal, normal state-preserving removal, offline purge-plan revision, interrupted purge, and vendor exclusion tests in internal/daemon/remove_test.go

### Implementation for User Story 1

- [X] T026 [US1] Implement descriptor-driven service-manager status/start/stop/restart/enable/disable operations once for both deployment roles in internal/servicecontrol/service.go, internal/servicecontrol/service_linux.go, and internal/servicecontrol/service_darwin.go; add only the host role descriptor in internal/daemon/service_role.go
- [X] T027 [P] [US1] Add the standard foreground user services in deploy/agent-sessions/systemd/user/agent-sessions.service and deploy/agent-sessions/launchd/net.antst.agent-sessions.plist
- [X] T028 [US1] Implement descriptor-generated human/JSON status, doctor, service diagnostics, stable semantic exit classes, cause-specific next actions, and admin CLI/help dispatch in internal/daemon/admin.go, internal/clihelp/catalog.go, and cmd/agent-sessions/main.go
- [X] T029 [US1] Implement immutable same-filesystem release staging, disjoint role-owned release roots, install locks, transaction journals and current selections, role readiness hooks, removal/purge planning, and exact rollback once for both deployment roles in internal/releaseinstall/transaction.go and internal/releaseinstall/store.go; implement only host connector, migration, service descriptor, and readiness hooks in internal/daemon/install.go
- [X] T030 [US1] Implement recoverable optional connector prepare/commit/rollback orchestration for Codex marketplace, Claude marketplace, Grok plugin, and Qwen extension without credential reads in internal/daemon/connectors.go, internal/daemon/connector_codex.go, internal/daemon/connector_claude.go, internal/daemon/connector_grok.go, and internal/daemon/connector_qwen.go; refactor Makefile install-codex/install-claude and internal/bridge/grok_plugin.go and internal/bridge/qwen_plugin.go to use it
- [X] T031 [US1] Replace in-place aggregate host installation with a deploy/agent-sessions/VERSION-driven role invocation of internal/releaseinstall, host aliases, optional-product probing, four-product connector transactions, host-service enablement, one validated host restart, removal of deploy/peer-federator/VERSION only after T024 proves the new sole-version contract RED, and an explicit prohibition on hub lifecycle mutation in Makefile, scripts/release-inventory, and scripts/package-release
- [X] T032 [US1] Implement host-specific fail-closed four-product connector removal hooks and invoke shared revision-bound removal/purge planning with descriptor-backed CLI/help/exit behavior while preserving Agent Sessions metadata and all vendor state in internal/daemon/remove.go, internal/daemon/connectors.go, internal/releaseinstall/transaction.go, internal/clihelp/catalog.go, and Makefile
- [X] T033 [US1] Remove launcher/App-Server supervisor lazy bootstrap and return cause-specific daemon-unavailable diagnostics in internal/launcher/bootstrap.go, internal/launcher/peer.go, and internal/launcher/lane.go
- [ ] T034 [US1] Add and run the installed Linux/macOS service, optional-product, all-four-connector rollback, CLI/help/documentation completeness, upgrade-rollback, explicit-stop, removal, purge, and real service-manager/log/crash/metric/trace content-canary acceptance flow in scripts/test-unified-service and scripts/test

**Checkpoint**: User Story 1 is independently usable as a service/runtime MVP even when every native
product is absent.

---

## Phase 4: User Story 2 — Keep Managed Peers Available Through Restart (Priority: P1)

**Goal**: Run Codex, Claude, Grok, and Qwen interactive peers as daemon-owned attachments with existing
group messaging and no supervisor, shim, product host, or per-session Agent Sessions listener.

**Independent Test**: Launch one peer per product in shared and isolated groups, exercise identity,
discovery, direct send, multicast, broadcast, and reply, restart the daemon, and repeat with unchanged
native identities and no duplicate accepted message.

### Tests for User Story 2

- [X] T035 [P] [US2] Add failing attachment prepare/select/adopt/refresh/detach, connector reconnect, exact-evidence, ambiguous-name, bare-session, and active-peer upgrade-generation preservation tests in internal/daemon/attachment_test.go
- [X] T036 [P] [US2] Add failing group discovery, direct send, all-target multicast admission, broadcast authorization, durable acceptance, retry, at-most-once delivery, message-content log canary, and disk/memory/file-descriptor/process pre-acceptance tests in internal/daemon/delivery_test.go
- [X] T037 [P] [US2] Add failing Codex App Server attachment, history-readiness, delivery, and daemon-restart/upgrade-generation reconstruction tests in internal/bridge/codex_daemon_adapter_test.go
- [X] T038 [P] [US2] Add failing Claude UUID/name adoption, native-socket delivery, attestation, synthetic-service, and restart/upgrade-generation reconstruction tests in internal/bridge/claude_daemon_adapter_test.go
- [X] T039 [P] [US2] Add failing Grok leader/ACP roster, interjection, exact-owner, delivery, and restart/upgrade-generation reconstruction tests in internal/bridge/grok_daemon_adapter_test.go
- [X] T040 [P] [US2] Add failing Qwen readiness, dual-output admission, native input/event, ancestry, delivery, and restart/upgrade-generation reconstruction tests in internal/bridge/qwen_daemon_adapter_test.go

### Implementation for User Story 2

- [X] T041 [US2] Implement the daemon-owned attachment registry, launch capabilities, session preferences, exact actor evidence, publication, and restart reconciliation in internal/daemon/attachment.go
- [X] T042 [US2] Implement durable AgentFrame admission, global-group recipient resolution, local delivery state, retries, and at-most-once destination outcomes in internal/daemon/delivery.go using shared internal/federation/groups.go and internal/federation/frame.go
- [ ] T043 [US2] Convert peer launch/resume commands into short-lived prepare/adopt clients that exec or hand off to the native vendor process and never supervise the daemon in internal/launcher/codex_peer.go, internal/launcher/claude_peer.go, internal/launcher/grok_peer.go, and internal/launcher/qwen_peer.go
- [ ] T044 [US2] Convert the MCP implementation into a stateless stdio relay whose tools and decisions come from the daemon in internal/bridge/mcp.go and internal/bridge/dynamic_tools.go
- [ ] T045 [P] [US2] Refactor Codex App Server coordination, hooks, attachment verification, and delivery into a callable daemon adapter in internal/bridge/appserver.go, internal/bridge/hook.go, and internal/bridge/supervisor.go without spawning supervisors or shims
- [ ] T046 [P] [US2] Refactor Claude launch observation, registry/socket attestation, synthetic service projection, and delivery into a callable daemon adapter in internal/bridge/claude_adapter.go
- [ ] T047 [P] [US2] Refactor Grok owner/leader/ACP observation, selection, wake, and delivery into a callable daemon adapter in internal/bridge/grok.go without a grok-host listener/process
- [ ] T048 [P] [US2] Refactor Qwen readiness, selection, event observation, native input, and delivery into a callable daemon adapter in internal/bridge/qwen.go and internal/bridge/qwen_host.go without a qwen-host listener/process
- [ ] T049 [US2] Point Codex, Claude, Grok, and Qwen connector payloads at canonical stateless relay modes and one `agent_sessions` tool namespace in .mcp.json, claude/.mcp.json, grok/.mcp.json, qwen/mcp.json, and scripts/native-entry
- [ ] T050 [US2] Add and run the four-product peer, group-isolation, bare-session, daemon-restart, accepted-message, and exact-process-census acceptance flow plus an installer-driven upgrade while all four peers remain active, proving unchanged native process/session identities and reconstructed messaging, in scripts/test-unified-peers and scripts/test

**Checkpoint**: Interactive peers preserve existing messaging semantics across daemon restart with no
separate Agent Sessions peer runtime.

---

## Phase 5: User Story 3 — Run Durable Lanes Under One Authority (Priority: P1)

**Goal**: Run all four lane products as daemon goroutines with one durable lifecycle and the existing
product-specific permission, transcript, resume, archive, and cancellation behavior.

**Independent Test**: Exercise start, follow-up, wait/collect, interrupt, resume, archive, failures, and
daemon restart for every target product and all 16 parent-target combinations, proving one dispatch and
one terminal result with no lane-manager process/socket.

### Tests for User Story 3

- [ ] T051 [P] [US3] Add failing lane/turn acceptance, exact parent/groups/permissions, duplicate-dispatch, concurrent-collection, notice, archive, cleanup-debt, lane-result content-log canary, and disk/memory/file-descriptor/process pre-acceptance tests in internal/daemon/lanes_test.go
- [ ] T052 [P] [US3] Add failing Codex lane App Server start/reconnect/interrupt/collect/archive and restart tests in internal/bridge/codex_daemon_lane_test.go
- [ ] T053 [P] [US3] Add failing Claude stream-worker lifecycle, permission, terminal, cleanup, and evidence-gated restart-outcome tests in internal/bridge/claude_daemon_lane_test.go
- [ ] T054 [P] [US3] Add failing Grok ACP start/load/interject/interrupt/collect/archive and evidence-gated restart-outcome tests in internal/bridge/grok_daemon_lane_test.go
- [ ] T055 [P] [US3] Add failing Qwen ACP/event/archive/interrupt/collect/cleanup and evidence-gated restart-outcome tests in internal/bridge/qwen_daemon_lane_test.go
- [ ] T056 [P] [US3] Add native restart discriminators that prove reconnectability or the single interrupted exception per product in scripts/test-unified-lane-restart
- [ ] T057 [P] [US3] Add a failing 16-cell parent-target composition that accepts one active turn in each cell, restarts the daemon during that exact turn, verifies the adapter-specific continued-or-evidence-approved-interrupted outcome without redispatch, and performs the obsolete-process/socket census in scripts/test-unified-lane-composition

### Implementation for User Story 3

- [ ] T058 [US3] Implement daemon-owned lanes, accepted turns, native dispatch identity, notices, collection cursors, archive revisions, recovery, and cleanup debt in internal/daemon/lanes.go
- [ ] T059 [P] [US3] Refactor Codex lane lifecycle into the daemon adapter and remove per-lane shims in internal/bridge/lane.go and internal/bridge/lane_contract.go
- [ ] T060 [P] [US3] Refactor Claude lane-manager state and worker coordination into a daemon-owned actor in internal/bridge/claude_lane.go
- [ ] T061 [P] [US3] Refactor Grok lane-manager ACP/session logic into a daemon-owned actor in internal/bridge/grok_lane_manager.go and internal/bridge/grok_lane.go
- [ ] T062 [P] [US3] Refactor Qwen lane-manager ACP/event logic into a daemon-owned actor in internal/bridge/qwen_lane_manager.go and internal/bridge/qwen_lane.go
- [ ] T063 [US3] Route every lane CLI and model-facing lane operation through the unified daemon contract in internal/launcher/lane.go, internal/bridge/mcp_lane.go, and internal/daemon/dispatch.go
- [ ] T064 [US3] Remove supervisor, shim, lane-manager, host, and lane-watch runtime dispatch from internal/bridge/runtime.go after all adapter actors are daemon-owned
- [ ] T065 [US3] Run and record the complete local lane lifecycle, crash, cleanup, and 16-cell composition acceptance with an active-turn daemon restart individually evidenced in every parent-target cell in scripts/test and specs/002-unified-user-daemon/evidence/us3-lanes.md

**Checkpoint**: All local lane functionality is daemon-owned; vendor workers remain external, while
Agent Sessions lane managers and their listeners are absent.

---

## Phase 6: User Story 4 — Federate Through the Same Service (Priority: P2)

**Goal**: Embed the existing host federation agent into the daemon while preserving one central hub,
global groups, host-suffixed names, AgentFrame, and remote lane semantics.

**Independent Test**: Connect at least three Linux/macOS host daemons to one hub, exercise cross-host
peer messaging and every remote lane product, restart and sleep/wake hosts, and prove the same host
identities, recipients, results, and no separate federation agent.

### Tests for User Story 4

- [ ] T066 [P] [US4] Extend the test-owned observability manifest through the file-disjoint internal/testutil/observability_hub.go fragment with hub boundaries and add failing embedded-agent lifecycle, ready-product advertisement, host identity, reconnect/backoff, arbitrary-SHA same-protocol interoperability, capability-gated operation availability without release coupling, protocol-mismatch refusal, independent hub/host lifecycle including co-located disjoint release roots and different-release selection/upgrade/rollback/removal, hub role-descriptor use of the shared immutable install/upgrade/readiness/rollback/remove/purge and systemd/launchd engines, hub configuration preservation, stable metadata-only hub status/doctor schema and cause-specific diagnostics, hub content canaries across normal/debug/error logs, service-manager capture, status/doctor, metrics, traces, and crash reports, hub disk/memory/file-descriptor/process pre-acceptance failures, and hub-outage tests in internal/daemon/federation_test.go, internal/federation/protocol_interoperability_test.go, internal/federation/hub_lifecycle_test.go, internal/federation/hub_security_test.go, and internal/federation/hub_resource_failure_test.go
- [ ] T067 [P] [US4] Add failing direct remote-lane dispatch, parent/group propagation, idempotency, result notice, and no lane-watch process tests in internal/federation/lane_test.go
- [ ] T068 [P] [US4] Extend federation integration with one-hub/three-host, host-suffix, global-group, restart, sleep-wake, duplicate-delivery, unrelated-SHA equal-protocol host/hub builds, protocol-preserving single-host-upgrade-with-unchanged-hub-identity, protocol-preserving hub-upgrade-with-unchanged-host-identities, and protocol-mismatch handshake discriminators in scripts/federation/integration_test.py

### Implementation for User Story 4

- [ ] T069 [US4] Embed the existing host agent connection/retry loop as a daemon component in internal/daemon/federation.go while moving the reusable connection/protocol implementation from internal/federator/agent.go into logical internal/federation/agent.go with typed in-process callbacks
- [ ] T070 [US4] Move local catalog registration, routing, remote delivery admission, capability publication, and hub registry behavior from internal/federator into internal/federation/registration.go, internal/federation/registry.go, and internal/federation/route.go, reusing the AgentFrame and group contracts already moved in T010; host-owned state remains composed by internal/daemon
- [ ] T071 [US4] Dispatch remote lane requests directly into internal/daemon/lanes.go, move the reusable remote-lane protocol into internal/federation/lane.go, and remove the remote watcher/CLI/manager chain from internal/federator/lane.go and internal/federator/lane_watch.go
- [ ] T072 [US4] Implement the distinct central cmd/agent-sessions-hub/main.go binary from this repository using logical internal/federation wire/identity/routing/protocol code and hub behavior; implement hub-specific configuration, readiness, metadata-only status/doctor, service, removal, and purge projections in internal/federation/hub_admin.go and internal/federation/hub_lifecycle.go while invoking internal/servicecontrol, internal/releaseinstall, internal/statestore, and internal/diagnostics for shared mechanics and a hub-specific release root/selection/lock/journal; migrate cmd/peer-federator/main_test.go into cmd/agent-sessions-hub/main_test.go; add host-independent make install-hub/remove-hub/purge-hub-inspect/purge-hub targets; create deploy/agent-sessions-hub/systemd/user/agent-sessions-hub.service, deploy/agent-sessions-hub/systemd/user/hub.env.example, and deploy/agent-sessions-hub/launchd/net.antst.agent-sessions-hub.plist; and remove the superseded internal/federator tree, cmd/peer-federator/main.go, cmd/peer-federator/main_test.go, deploy/peer-federator/systemd/user/peer-federator-agent.service, deploy/peer-federator/systemd/user/peer-federator-hub.service, deploy/peer-federator/systemd/user/agent.env.example, deploy/peer-federator/systemd/user/hub.env.example, deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example, and deploy/peer-federator/launchd/net.antst.peer-federator-hub.plist.example
- [ ] T073 [US4] Add only host-side hub connection configuration, federation state recovery, ready-product advertisement, and host status/doctor projection fields in internal/daemon/config.go, internal/daemon/federation.go, and internal/daemon/diagnostics.go; central-hub configuration and diagnostics remain in internal/federation/hub_admin.go from T072
- [ ] T074 [US4] Run and record hub-only status/doctor/install/removal/reinstall/purge with stable metadata-only diagnostics, preserved-configuration and exact-deletion discriminators, a co-located host/hub fixture using disjoint role release roots and different releases with independent upgrade/rollback/removal/reinstall proof, real service-manager/log/crash/metric/trace content canaries and resource-failure canaries, one-hub/three-host Linux/macOS peer/remote-lane restart, unrelated-SHA equal-protocol builds, independent protocol-preserving single-host upgrade, independent protocol-preserving hub upgrade, and protocol-mismatch handshake matrix in scripts/federation/test and specs/002-unified-user-daemon/evidence/us4-federation.md

**Checkpoint**: Every host has one daemon acting as its existing host agent; the one central hub remains
separate and unchanged in responsibility.

---

## Phase 7: User Story 5 — Migrate and Diagnose Legacy Runtime State (Priority: P2)

**Goal**: Converge the three operator-controlled legacy hosts after all managed peers and lanes are
closed, preserving durable Agent Sessions metadata and retiring exact obsolete authorities without any
live handoff.

**Independent Test**: Build a split legacy fixture with several runtime roots, two supervisors, stale
counts, active peer/lane blockers, a product manager, a federation agent, and unrelated processes;
prove zero mutation until all blockers close, then migrate and leave one daemon with preserved metadata.

### Tests for User Story 5

- [ ] T075 [P] [US5] Add failing closed-list inventory and exact candidate classification tests for every shipped legacy state/runtime/service root in internal/daemon/migration_inventory_test.go
- [ ] T076 [P] [US5] Add failing all-blocker quiescence refusal, zero-mutation, stale-count non-blocking, unknown-identity debt, and explicit no-live-handoff tests in internal/daemon/migration_quiescence_test.go
- [ ] T077 [P] [US5] Add failing durable catalog/group/name/lane/cursor/notice/hub/debt adoption and vendor-profile exclusion tests in internal/daemon/migration_adopt_test.go
- [ ] T078 [P] [US5] Add failing exact stop, endpoint retirement, unrelated-process preservation, crash-journal, and prior-authority rollback tests in internal/daemon/migration_retire_test.go

### Implementation for User Story 5

- [ ] T079 [US5] Implement the revisioned migration transaction, candidate, blocker, provenance, and debt records in internal/daemon/migration.go
- [ ] T080 [US5] Implement bounded inventory of known claude-code-peer, agent-sessions agent, supervisor/shim/host/manager, historical Linux/Darwin runtime-root, service-job, and federation-agent records in internal/daemon/migration_inventory.go
- [ ] T081 [US5] Implement the mandatory all-peer/all-lane quiescence gate that enumerates exact blockers, ignores disproven scalar counts, and performs no live handoff in internal/daemon/migration_quiescence.go
- [ ] T082 [US5] Implement staged transformation of durable Agent Sessions catalogs, groups, names, completed lane state, collection cursors, notices, host/hub configuration, and debt without copying vendor data in internal/daemon/migration_adopt.go
- [ ] T083 [US5] Implement exact supported-lifecycle stop and re-attested retirement for quiescent legacy supervisors, shims, product hosts/managers, routing/federation agents, jobs, sockets, and files in internal/daemon/migration_retire.go
- [ ] T084 [US5] Integrate first-migration inspect/apply/recovery/rollback into the install transaction before unified-daemon readiness in internal/daemon/install.go and internal/daemon/recovery.go
- [ ] T085 [US5] Implement descriptor-backed metadata-only `migrate inspect`, `migrate status`, blocker diagnostics, transaction status, generated help, stable JSON/error fields, exit classes, and retry guidance in internal/daemon/admin.go, internal/clihelp/catalog.go, and cmd/agent-sessions/main.go
- [ ] T086 [US5] Add and run the quiescent-only split-runtime migration acceptance flow in scripts/test-unified-migration and specs/002-unified-user-daemon/evidence/us5-migration.md

**Checkpoint**: First migration works for the actual three-host unreleased estate without a live
transfer protocol; steady-state upgrades use the normal US1 restart contract afterward.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Remove obsolete topology, close security/operability gaps, update documentation, and run
the complete release gate at one exact commit.

- [ ] T087 Remove obsolete standalone host command packages, assert cmd/peer-federator, deploy/peer-federator, the legacy version file, and every legacy host-agent service asset are absent in internal/releaseevidence/unified_inventory_test.go, prove every installed host command alias resolves to the exact agent-sessions image, and prove agent-sessions-hub is a distinct non-alias image while removing cmd/agent-session-runtime/main.go, cmd/codex-peer/main.go, cmd/claude-peer/main.go, cmd/grok-peer/main.go, cmd/qwen-peer/main.go, cmd/peer/main.go, cmd/codex-peer-lane/main.go, cmd/claude-peer-lane/main.go, cmd/grok-peer-lane/main.go, and cmd/qwen-peer-lane/main.go
- [ ] T088 Set the final declared source release in the already-authoritative deploy/agent-sessions/VERSION and update the four-platform two-binary archive layout, generated manifest, checksums, connector payloads, independent host/hub service assets, and source-pointer verification in scripts/release-inventory, scripts/package-release, and scripts/release-final-gate
- [ ] T089 [P] Re-run the already-green T005 parity gates to prove docs/CLI.md remains byte-consistent with the canonical two-binary descriptor and docs/FEDERATION.md remains field-for-field consistent with specs/002-unified-user-daemon/contracts/federation-protocol.md and the generated hub-protocol descriptor; update the remaining installation, shared host/hub lifecycle implementation, adapters, lanes, groups, hub-protocol interoperability, migration, removal, host/hub purge, and troubleshooting documentation in docs/INSTALL.md, docs/ADAPTER-PROTOCOL.md, docs/CODEX-ADAPTER.md, docs/CLAUDE-ADAPTER.md, docs/GROK-ADAPTER.md, docs/QWEN-ADAPTER.md, docs/CODEX-LANES.md, docs/CLAUDE-LANES.md, docs/GROK-LANES.md, docs/QWEN-LANES.md, docs/GROUPS.md, and docs/FEDERATION.md
- [ ] T090 [P] Add and run an aggregate completeness harness in internal/daemon/security_test.go and internal/daemon/resource_failure_test.go that imports internal/testutil, validates its applicable-platform union of observability_manifest.go, observability_linux.go or observability_darwin.go, and observability_hub.go, and reruns every already-green normal/debug/error/crash, service-manager, status/doctor, metric, trace, peer, lane, and hub content/secret canary plus disk/memory/file-descriptor/process/native-dependency pre-acceptance failure; this final gate introduces no new behavioral invariant, duplicated inventory, or production sink registry
- [ ] T091 [P] Add and run a 100-simultaneous-attachment stress matrix spanning all four products and concurrent production, development, and test groups, proving exact identity, unchanged global-group authorization, one host daemon/listener, restart budgets, accepted-message durability, and no duplicate turn in internal/daemon/stress_test.go and scripts/test-unified-stress
- [ ] T092 Update repository orchestration so make test, make test-race, go vet, make lint, federation integration, and every unified live contract run without starting a second daemon in scripts/test and Makefile
- [ ] T093 Update Linux/macOS CI lint, test, race, vet, both-binary four-platform build, host/hub packaging, and service-fixture gates in .github/workflows/ci.yml
- [ ] T094 Run the complete quickstart.md matrix, rerunning and individually reporting every closed-list cell from specs/002-unified-user-daemon/evidence/baseline-functional-cells.md, including two-binary CLI/help/documentation parity, hub-only install/removal, unrelated-SHA equal-protocol hub/host interoperability, independent lifecycle, protocol-mismatch handshake, and the mixed-workload 100-attachment cell. Validate the release candidate on real installed Linux and macOS at the same signed feature commit, then add separately identified same-repository hub/host artifacts from unrelated commits for the network matrix; record exact toolchains, identities, failures, preserved state, and residue in specs/002-unified-user-daemon/evidence/final-linux.md and specs/002-unified-user-daemon/evidence/final-macos.md
- [ ] T095 Perform the final constitution review of exact identity, lifecycle ordering, rollback, cleanup debt, permissions, groups, canonical CLI/help/environment/JSON/exit completeness, two-binary packaging, one-hub topology, exact hub-protocol software-version interoperability, arbitrary-SHA same-repository deployments, independent host/hub lifecycle, no pre-unification interoperability obligation, no artificial quotas, no content logs, and no obsolete processes in specs/002-unified-user-daemon/evidence/final-review.md

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependency; establishes files and baseline evidence only.
- **Foundational (Phase 2)**: Depends on Setup and blocks all user stories.
- **US1 (Phase 3)**: Depends on Foundational; produces the service-managed daemon MVP.
- **US2 (Phase 4)**: Depends on Foundational and US1's runnable service/control plane.
- **US3 (Phase 5)**: Depends on US2 attachment/adapter contracts and US1 lifecycle.
- **US4 (Phase 6)**: Depends on US2 delivery and US3 lane dispatch; it embeds remote routing without changing them.
- **US5 (Phase 7)**: Depends on US1 install/recovery, the final US2/US3 durable schemas, and US4's final host/hub configuration schema; it does not depend on live handoff.
- **Polish (Phase 8)**: Depends on all selected stories and removes the now-unused legacy entrypoints.

### User Story Dependency Graph

```text
Setup -> Foundation -> US1 -> US2 -> US3 -> US4 -> US5
US1 + US2 + US3 + US4 + US5 -> Polish/Release
```

- **US1** is the suggested MVP and can be validated with zero native products.
- **US2** adds interactive peers without changing service lifetime.
- **US3** adds local lanes using the same adapter/attachment authority.
- **US4** adds the existing remote topology by embedding the host agent.
- **US5** is the deployment gate for the current three legacy hosts and runs only after peers/lanes close.

### Within Each Story

- Add the story's failing tests before implementing its behavior.
- Complete shared state before product adapters; complete adapters before installed acceptance.
- Re-attest exact process/filesystem identity immediately before every signal, retirement, or deletion.
- Stop on the first genuine RED, establish RCA, and do not credit confounded/skipped evidence.

### Parallel Opportunities

- T003 and T004 can run in parallel after T002.
- T005–T009 are independent foundational test files and can run in parallel.
- T020–T025 cover distinct service/install/admin/package/removal boundaries and can run in parallel.
- T035–T040 can run in parallel; T045–T048 can run in parallel after T041–T044.
- T051–T057 can run in parallel; T059–T062 can run in parallel after T058.
- T066–T068 can run in parallel before the federation implementation.
- T075–T078 can run in parallel before the migration implementation.
- T089–T091 can run in parallel after functional completion.

---

## Parallel Examples

### User Story 1

```text
Task T020: systemd lifecycle contract tests in internal/servicecontrol/service_linux_test.go
Task T021: launchd lifecycle contract tests in internal/servicecontrol/service_darwin_test.go
Task T022: shared release transaction and host-hook tests in internal/releaseinstall/transaction_test.go and internal/daemon/install_hooks_test.go
Task T024: optional-product/release inventory tests in internal/releasepkg/unified_archive_test.go
```

### User Story 2

```text
Task T037: Codex interactive adapter tests in internal/bridge/codex_daemon_adapter_test.go
Task T038: Claude interactive adapter tests in internal/bridge/claude_daemon_adapter_test.go
Task T039: Grok interactive adapter tests in internal/bridge/grok_daemon_adapter_test.go
Task T040: Qwen interactive adapter tests in internal/bridge/qwen_daemon_adapter_test.go
```

### User Story 3

```text
Task T052: Codex lane restart tests in internal/bridge/codex_daemon_lane_test.go
Task T053: Claude lane restart tests in internal/bridge/claude_daemon_lane_test.go
Task T054: Grok lane restart tests in internal/bridge/grok_daemon_lane_test.go
Task T055: Qwen lane restart tests in internal/bridge/qwen_daemon_lane_test.go
```

### User Story 4

```text
Task T066: embedded federation lifecycle tests in internal/daemon/federation_test.go
Task T067: direct remote-lane dispatch tests in internal/federation/lane_test.go
Task T068: multi-host integration discriminators in scripts/federation/integration_test.py
```

### User Story 5

```text
Task T075: closed-list legacy inventory tests in internal/daemon/migration_inventory_test.go
Task T076: quiescence/no-live-handoff tests in internal/daemon/migration_quiescence_test.go
Task T077: durable adoption/vendor exclusion tests in internal/daemon/migration_adopt_test.go
Task T078: exact retirement/rollback tests in internal/daemon/migration_retire_test.go
```

---

## Implementation Strategy

### MVP First: User Story 1

1. Complete Setup and Foundational phases.
2. Complete US1 tests and service/install implementation.
3. Validate clean install, explicit stop, crash restart, upgrade rollback, status/doctor, removal, and purge with zero native products.
4. Stop and review the one-process/one-endpoint evidence before moving product behavior.

### Incremental Delivery

1. Add US2 and prove four interactive peers survive service restart.
2. Add US3 and prove the complete local lane matrix without manager processes.
3. Add US4 and prove the existing one-hub/multiple-host topology.
4. Add US5 and migrate the quiescent three-host legacy estate without live handoff.
5. Remove obsolete entrypoints, complete two-platform acceptance, and release only one signed green tree.

### Parallel Team Strategy

After Foundation, keep shared state/control changes serialized. Parallelize only distinct product test
and adapter files, then integrate each batch through the one daemon before starting the next story.

## Notes

- `[P]` means file-disjoint work after stated prerequisites, not permission to bypass a failing shared gate.
- Tests precede implementation and must demonstrate the intended invariant rather than merely compile.
- Unit/live tests use in-process fixtures or the user's one installed daemon; they never start a second daemon for the same OS user-host.
- First migration requires every managed legacy peer and lane to be closed and contains no live-transfer task.
- Vendor credentials, profiles, transcripts, permissions, App Servers, and native workers remain vendor-owned.
- Existing global groups remain the sole collaboration boundary in the one uniform multi-host space.
