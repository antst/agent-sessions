---

description: "Dependency-ordered implementation tasks for first-class Qwen support"
---

# Tasks: Qwen Support

**Input**: Design documents from `specs/001-qwen-support/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md),
[data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md)

**Tests**: Required by the specification and constitution. Within each story, write the listed tests
first, demonstrate the intended RED where feasible, then implement until the tests pass. Live tests
must record exact commit, tree, toolchain, native-client versions, owned state, and preserved state.

**Organization**: Tasks are grouped by user story so each story can be implemented and validated as
an independent increment after the shared foundation is complete.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel because it changes different files and has no dependency on unfinished work
- **[Story]**: User-story traceability label from [spec.md](spec.md)
- Every task names the exact file or evidence path it must create or modify

## Phase 1: Setup (Qwen Test and Evidence Scaffolding)

**Purpose**: Establish deterministic native fixtures and evidence locations before production code.

- [X] T001 Add pinned Qwen 0.21.15 dual-output, ACP initialize/update, and serve-capability golden fixtures in `internal/bridge/testdata/qwen/version.json`, `internal/bridge/testdata/qwen/dual-output.jsonl`, `internal/bridge/testdata/qwen/acp.jsonl`, and `internal/bridge/testdata/qwen/serve.json`
- [X] T002 [P] Add reusable fake-Qwen process, private-path, JSON-RPC, and lifecycle polling helpers in `internal/bridge/qwen_test_helpers_test.go` and `internal/launcher/qwen_test_helpers_test.go`
- [X] T003 [P] Create the exact Linux/macOS evidence template and ownership-baseline checklist in `specs/001-qwen-support/evidence/README.md`

---

## Phase 2: Foundational (Blocking Shared Prerequisites)

**Purpose**: Close product-enumeration and lifecycle duplication and correct the shared stable-socket
transport before any Qwen story implementation.

**⚠️ CRITICAL**: No user-story implementation begins until this phase is green. Existing Codex,
Claude, and Grok contracts remain unchanged except for the intentional Codex/Grok endpoint-type
correction required by FR-031.

- [X] T004 [P] Establish the single generated four-product completeness table and write its failing descriptor, uniqueness, capability, executable, role, and resume-matrix tests in `internal/federator/product_test.go`
- [X] T005 Extend the generated completeness table and harness created by T004 with failing launcher/runtime/MCP projection and parsed-option-versus-help checks; add failing existing-adapter tests proving Codex/Grok session-stable paths are real Unix sockets, managed Claude-to-Codex/Grok delivery uses those exact paths without caller resolution, and legacy owned aliases are distinguished from unrelated links/files in `internal/launcher/product_test.go`, `internal/bridge/product_test.go`, `internal/bridge/integration_test.go`, `internal/bridge/cleanup_test.go`, and `internal/federator/route_test.go`
- [X] T006 Implement the authoritative Codex/Claude/Grok/Qwen product descriptor and lookup API in `internal/federator/product.go`
- [X] T007 Refactor product validation, capabilities, registrations, remote launcher selection, and resume-kind checks to consume the descriptor in `internal/federator/protocol.go`, `internal/federator/registration.go`, `internal/federator/lane.go`, and `internal/federator/registry.go`
- [X] T008 Refactor launcher and runtime/MCP product projections to consume the descriptor without duplicating product tables; implement the shared real session-socket primitive; replace Codex/Grok stable-socket symlink publication with that primitive; and retain exact local legacy-alias migration plus crash cleanup in `internal/launcher/product.go`, `internal/launcher/peer.go`, `internal/launcher/lane.go`, `internal/bridge/product.go`, `internal/bridge/runtime.go`, `internal/bridge/cleanup.go`, `internal/bridge/grok_lane.go`, `internal/bridge/mcp.go`, and `internal/bridge/mcp_lane.go`
- [X] T009 [P] Write failing product-neutral preparation, exact-revision rollback, restart reconciliation, existing current-format Claude preparation/registry compatibility, and supported local durable-state migration tests—explicitly excluding obsolete mixed-version binary compatibility—in `internal/federator/registration_test.go` and `internal/federator/registry_test.go`
- [X] T010 Generalize durable peer preparation to versioned product payloads and typed cleanup debt in `internal/federator/registration.go` and `internal/federator/registry.go`
- [X] T011 [P] Write failing shared detached-tool-root ownership tests covering strong-start reuse, admission closure, crash retry, and unrelated-process preservation in `internal/bridge/tool_root_ledger_test.go`
- [X] T012 Extract the Grok tool-root implementation into the product-neutral ledger while retaining Grok callbacks in `internal/bridge/tool_root_ledger.go`, `internal/bridge/grok_tool_wrapper.go`, and `internal/bridge/grok_lane_manager.go`
- [X] T013 [P] Write failing value-versus-absence, canonical-path, fingerprint, resume-match, and non-secret Qwen profile tests in `internal/qwenprofile/profile_test.go`
- [X] T014 Implement exact `QWEN_HOME` and `QWEN_RUNTIME_DIR` identity handling in `internal/qwenprofile/profile.go`
- [X] T015 [P] Write failing Qwen Agent Plugin v1 schema, MCP name, version, five-skill inventory, isolated default/explicit-profile native install, exact post-install verification, and single session-free readiness-engine tests covering version, parser, ACP initialize-only, archive capability, trust, profile/integration identity, and non-secret credential/provider configuration state in `internal/bridge/qwen_plugin_test.go` and `internal/qwenreadiness/readiness_test.go`
- [X] T016 Create the schema-valid `agent-sessions` Qwen plugin manifests, complete messaging skill, and minimal inventory/schema-valid shells for the four lane skills; implement the foundational profile-scoped `install-qwen` target through Qwen's supported installer with exact post-install verification; and implement the sole Qwen-specific session-free readiness engine consumed later by launch admission, federation advertisement, and doctor in `qwen/plugin.json`, `qwen/mcp.json`, `qwen/skills/agent-sessions/SKILL.md`, `qwen/skills/codex-lane/SKILL.md`, `qwen/skills/claude-lane/SKILL.md`, `qwen/skills/grok-lane/SKILL.md`, `qwen/skills/qwen-lane/SKILL.md`, `internal/bridge/qwen_plugin.go`, `internal/qwenprofile/profile.go`, `internal/qwenreadiness/readiness.go`, and `Makefile`; behavioral Qwen-parent lane guidance is owned exclusively by T052
- [ ] T017 Run all existing product, group, lifecycle, help, race, and cleanup regressions; on real Linux and macOS prove stable delivery paths are sockets rather than symlinks, managed Claude can deliver to Codex and Grok through the exact published paths without caller-side resolution, and legacy-alias/normal/crash cleanup preserves unrelated artifacts; record the green shared-foundation boundary in `specs/001-qwen-support/evidence/foundation.md`

**Checkpoint**: One authoritative product model, one exact-ownership lifecycle foundation, one real
session-socket transport invariant, and one supported selected-profile Qwen installer/verifier support
all four products. Existing contracts remain green, including the intentional FR-031 endpoint-type
correction.

---

## Phase 3: User Story 1 - Use Qwen as a Managed Interactive Peer (Priority: P1) 🎯 MVP

**Goal**: Launch and resume a native-behaving Qwen TUI as an attested grouped peer with reliable
Agent Sessions messaging, while bare Qwen remains an intentional opt-out.

**Independent Test**: Launch one managed Qwen peer and one existing peer in the same group, exchange
correlated direct messages and a named-group broadcast in both directions, exercise native approval
mode changes, resume the exact transcript, and prove a simultaneous bare Qwen has no Agent Sessions
authority or cleanup impact.

### Tests for User Story 1

- [X] T018 [P] [US1] Write failing `qwen-peer` parsing/help tests for names, `-g/--group`, profile selection, native-default, exact `--no-yolo` to `--approval-mode default` translation, `--yolo`, pass-through native `--approval-mode MODE`, exit-2 wrapper/native and repeated/contradictory permission conflicts before mutation, exact resume, and continue/fork rejection in `internal/launcher/qwen_peer_test.go`
- [X] T019 [P] [US1] Write failing dual-output v2 admission and input-delivery tests for exact `session_start`, UUID/cwd/version, JSONL submit, inode/body cursor, truncation, replacement, and changed path type in `internal/bridge/qwen_test.go`
- [X] T020 [P] [US1] Write failing Qwen MCP attestation, bare-session inactivity, grouped discover/send/multicast/broadcast, busy delivery, duplicate suppression, exact-sender reply, real-socket endpoint, and transport instrumentation tests proving these operations add no extra hub round trip or caller-side path resolution in `internal/bridge/qwen_authorization_test.go`
- [X] T021 [US1] Write failing preparation/commit/rollback and UUID-or-unique-name resume tests for missing, ambiguous, live, profile-mismatched, native-authentication-failed-after-readiness, and other startup-failed peers in `internal/federator/registration_test.go` and `internal/launcher/qwen_peer_test.go`
- [X] T022 [P] [US1] Write failing normal-exit, Ctrl+C, SIGTERM, wrapper-SIGKILL, recycled-PID, changed-artifact, and restart cleanup tests in `internal/bridge/qwen_cleanup_test.go` and `internal/federator/qwen_registration_test.go`

### Implementation for User Story 1

- [X] T023 [US1] Implement Qwen wrapper parsing, exact permission translation and pre-mutation conflict rejection from T018, pass-through native mode retention, exact profile selection, private dual-output paths, and launch transaction orchestration that consumes T016 readiness evidence before native start in `internal/launcher/qwen_peer.go`
- [X] T024 [US1] Implement Qwen structured-event admission, attested input writer, message delivery host using the shared real session-socket primitive from T008, status projection, and exact cleanup in `internal/bridge/qwen.go`
- [X] T025 [US1] Add Qwen preparation payload validation, catalog adoption, live registration, rollback, and reconciliation in `internal/federator/registration.go` and `internal/federator/registry.go`
- [X] T026 [US1] Add the thin `qwen-peer` command, its Makefile build/install surface, and generic exact Qwen resume dispatch in `cmd/qwen-peer/main.go`, `internal/launcher/peer.go`, and `Makefile`
- [X] T027 [US1] Extend managed MCP authorization, instructions, product labels, and grouped messaging inventory to attested Qwen callers in `internal/bridge/mcp.go`, `internal/bridge/dynamic_tools.go`, and `internal/bridge/product.go`
- [X] T028 [US1] Preserve native Qwen permission controls while recording the exact tagged launch preference (`native_default`, `non_yolo`, `yolo`, or admitted `native:<mode>`), exact requested/default initial contract, and honest current-mode-or-unknown status in `internal/launcher/qwen_peer.go`, `internal/bridge/qwen.go`, and `internal/federator/registration.go`
- [X] T029 [US1] Add a real interactive dual-output, messaging, resume, native-mode-change, and elapsed-time runner that live-tests native default, exact `--no-yolo` to initial `default`, `--yolo`, pass-through native `--approval-mode plan`, and pre-child wrapper/native conflict rejection; requires the published Qwen delivery path to be a real Unix socket rather than a symlink; proves no caller-side path resolution or extra hub round trip; exercises post-readiness native authentication failure; verifies exact cleanup; and fails unless the documented launch/discover/direct-reply/broadcast workflow completes in under five minutes in `scripts/test-qwen-contract`
- [x] T030 [US1] Using the foundational T016 selected-profile installer/verifier, execute the US1 runner against a real authenticated Linux Qwen installation and record exact elapsed time plus state/nonmutation evidence in `specs/001-qwen-support/evidence/us1-linux.md`
- [ ] T031 [US1] Using the foundational T016 selected-profile installer/verifier, execute the US1 runner against a real authenticated macOS Qwen installation and record exact elapsed time plus state/nonmutation evidence in `specs/001-qwen-support/evidence/us1-macos.md`

**Checkpoint**: Managed Qwen is independently useful as a native interactive peer; Qwen permissions
remain Qwen-owned, and unmanaged Qwen remains outside Agent Sessions.

---

## Phase 4: User Story 2 - Delegate Durable Work to a Qwen Lane (Priority: P1)

**Goal**: Provide the complete durable Qwen lane lifecycle over stdio ACP with exact native archive,
resume, collection, ownership, notification, and cleanup semantics.

**Independent Test**: From one managed parent, start a Qwen lane, collect once, follow up on the same
native transcript, interrupt, archive twice, resume, and verify owned-versus-persistent owner exit and
zero process/socket/helper residue.

### Tests for User Story 2

- [X] T032 [P] [US2] Write failing `qwen-peer-lane` command, option, help, JSON output, selector, exit-code, exact `--no-yolo` mapping, native-mode pass-through, and pre-mutation permission-conflict contract tests in `internal/bridge/qwen_lane_test.go`
- [X] T033 [P] [US2] Write failing ACP initialize/new/resume/prompt/update/cancel/mode/MCP tests, including unknown versions and malformed/out-of-order frames, in `internal/bridge/qwen_lane_manager_test.go`
- [X] T034 [US2] Write failing serialized-turn, queued-follow-up, one-terminal-observation, one-collection, interrupt-130, notice, and owner/persistent state-machine tests in `internal/bridge/qwen_lane_test.go`
- [X] T035 [P] [US2] Write failing authenticated loopback archive/unarchive tests for capabilities, exact workspace/UUID, idempotence, conflicts, compensation, and helper/preheated-child cleanup in `internal/bridge/qwen_archive_test.go`
- [X] T036 [P] [US2] Write failing manager/worker/tool-root/helper crashes, recycled identities, agent/supervisor restart, and durable cleanup-debt tests in `internal/bridge/qwen_lane_cleanup_test.go`

### Implementation for User Story 2

- [X] T037 [US2] Implement Qwen lane CLI parsing including the exact permission mapping/conflict contract from T032, durable lane/turn records, selectors, status/list/run/start/wait/follow-up/interrupt/archive/resume, and JSON contracts in `internal/bridge/qwen_lane.go`
- [X] T038 [US2] Implement the single-client stdio ACP manager, serialized prompt queue, native identity tracking, output aggregation, cancellation, permission-request handling, and terminal normalization in `internal/bridge/qwen_lane_manager.go`
- [X] T039 [US2] Add Qwen lane and manager runtime roles, launcher environment/profile selection and T016 readiness admission, thin command and Makefile build/install surface, and generic resume routing in `internal/bridge/runtime.go`, `internal/launcher/lane.go`, `cmd/qwen-peer-lane/main.go`, `internal/launcher/peer.go`, and `Makefile`
- [X] T040 [US2] Implement the bounded token-authenticated `qwen serve` archive/unarchive transaction and exact helper-tree retirement in `internal/bridge/qwen_archive.go`
- [X] T041 [US2] Integrate Qwen detached descendants with the shared tool-root ledger and reconciliation state in `internal/bridge/qwen_lane_manager.go` and `internal/bridge/tool_root_ledger.go`
- [X] T042 [US2] Implement exact parent ownership, persistence, auto-archive, private anchors, terminal notices, and retryable notice debt in `internal/bridge/qwen_lane.go` and `internal/bridge/group_context.go`
- [X] T043 [US2] Add a real Qwen ACP lifecycle, archive/unarchive, crash, and zero-residue runner that live-tests native default, exact `--no-yolo` to initial `default`, `--yolo`, pass-through native `--approval-mode plan`, and pre-worker permission-conflict rejection in `scripts/test-qwen-lane-contract`
- [x] T044 [US2] Using the foundational T016 selected-profile installer/verifier, execute the full US2 lifecycle and crash runner on Linux and record exact ownership/archive evidence in `specs/001-qwen-support/evidence/us2-linux.md`
- [ ] T045 [US2] Using the foundational T016 selected-profile installer/verifier, execute the full US2 lifecycle and crash runner on macOS and record exact ownership/archive evidence in `specs/001-qwen-support/evidence/us2-macos.md`

**Checkpoint**: Qwen is independently usable as a durable target lane with Codex-style native
archive semantics and the common Agent Sessions lifecycle contract.

---

## Phase 5: User Story 3 - Compose Qwen with Every Supported Product (Priority: P2)

**Goal**: Make Qwen a symmetric managed parent and target in the complete four-product composition
matrix without adding product-specific routing rules.

**Independent Test**: Contract-test and live-test all 16 parent-target combinations, checking exact
parent/notification identity, both private anchors, opt-in inherited
groups, terminal answer, and cleanup.

### Tests for User Story 3

- [X] T046 [P] [US3] Expand the generated/table-driven 4x4 parent-target lifecycle and permission-preference matrix tests in `internal/bridge/group_agent_test.go`
- [X] T047 [P] [US3] Write failing Qwen parent/child anchor, explicit inheritance, no-inheritance, unrelated-group exclusion, and notice-target tests in `internal/bridge/group_context_test.go`
- [X] T048 [P] [US3] Write failing Qwen-parent MCP lane authorization and all-four-target inventory tests in `internal/bridge/mcp_lane_test.go` and `internal/bridge/peer_authorization_test.go`

### Implementation for User Story 3

- [X] T049 [US3] Extend the parent lane MCP schema, instructions, product resolution, role dispatch, and result metadata from three to four targets in `internal/bridge/mcp.go`, `internal/bridge/mcp_lane.go`, and `internal/bridge/product.go`
- [X] T050 [US3] Add Qwen parent-context inference, exact attestation, group inheritance, notification routing, and child composition in `internal/bridge/group_context.go` and `internal/federator/registration.go`
- [X] T051 [P] [US3] Add Qwen-target lane guidance for Codex and Claude parents in `skills/qwen-lane/SKILL.md`, `skills/qwen-lane/agents/openai.yaml`, and `claude/skills/qwen-lane/SKILL.md`
- [X] T052 [P] [US3] Add Qwen-target guidance to Grok and replace the four inventory-only Qwen-parent lane skill shells from T016 with their complete, tested behavioral guidance in `grok/skills/agent-lanes/SKILL.md` and `qwen/skills/codex-lane/SKILL.md`, `qwen/skills/claude-lane/SKILL.md`, `qwen/skills/grok-lane/SKILL.md`, and `qwen/skills/qwen-lane/SKILL.md`
- [X] T053 [US3] Add a deterministic complete 16-edge composition runner with unique tokens and exact anchor assertions in `scripts/test-qwen-composition`
- [X] T054 [US3] Execute all 16 live composition edges on Linux and record the contract plus cleanup evidence in `specs/001-qwen-support/evidence/us3-linux.md`
- [ ] T055 [US3] Execute all 16 live composition edges on macOS and record the contract plus cleanup evidence in `specs/001-qwen-support/evidence/us3-macos.md`

**Checkpoint**: Qwen is symmetric with Codex, Claude, and Grok as both parent and target.

---

## Phase 6: User Story 4 - Run Qwen Across Federated Hosts (Priority: P2)

**Goal**: Advertise and execute Qwen lanes on an explicitly selected current-version remote host,
with verbatim collection and no local fallback or destination residue.

**Independent Test**: Run one Linux-Qwen-parent to macOS-Qwen-target lane and the reverse direction,
execute each emitted `Collect:` command verbatim, and prove exact placement, parent notification,
archive, nonmutation, and cleanup.

### Tests for User Story 4

- [X] T056 [P] [US4] Write failing protocol/capability/current-version tests for `qwen-lane` advertisement and typed ParentContext preservation in `internal/federator/protocol_test.go` and `internal/federator/hub_test.go`
- [X] T057 [P] [US4] Write failing destination-readiness, explicit-host selection, Qwen launcher argv/stdin, disconnect, mixed-version, and no-fallback tests in `internal/federator/lane_test.go` and `internal/federator/agent_test.go`
- [X] T058 [P] [US4] Write failing source-runtime `Collect:` pointer, exact parent/child anchors, terminal notice, archive, and destination cleanup tests in `internal/federator/lane_watch_test.go` and `internal/bridge/group_agent_test.go`

### Implementation for User Story 4

- [X] T059 [US4] Add Qwen capability constants, host readiness advertisement, status output, and current-version validation by consuming the sole `internal/qwenreadiness` evidence engine from T016; generic federator code MUST NOT implement a second Qwen probe path, in `internal/federator/protocol.go`, `internal/federator/agent.go`, and `internal/federator/diagnostics.go`
- [X] T060 [US4] Route explicit remote Qwen lane execution, stdin, lifecycle ownership, typed ParentContext, and verbatim collection pointers in `internal/federator/lane.go` and `internal/federator/lane_watch.go`
- [X] T061 [P] [US4] Own and complete the Qwen launcher variables and runtime/profile examples without adding listeners or legacy compatibility in `deploy/peer-federator/systemd/user/agent.env.example` and `deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example`
- [X] T062 [US4] Extend grouped federation fixtures and exact destination residue checks for Qwen in `scripts/federation/grouped_peer_fixture.go`, `scripts/federation/integration_test.py`, and `scripts/federation/test`
- [ ] T063 [US4] Execute Linux-source to macOS-destination Qwen federation and record verbatim collection plus both-host cleanup evidence in `specs/001-qwen-support/evidence/us4-linux-to-macos.md`
- [ ] T064 [US4] Execute macOS-source to Linux-destination Qwen federation and record verbatim collection plus both-host cleanup evidence in `specs/001-qwen-support/evidence/us4-macos-to-linux.md`
- [ ] T065 [US4] Execute disconnected, unready, non-capable, and mixed-version remote negatives and record zero target creation/fallback in `specs/001-qwen-support/evidence/us4-negative.md`

**Checkpoint**: Qwen lanes work in both federation directions through the existing current-version
protocol and nowhere else.

---

## Phase 7: User Story 5 - Install and Diagnose Qwen Safely (Priority: P3)

**Goal**: Ship explicit profile-scoped Qwen installation, non-mutating readiness diagnostics, complete
prebuilt artifacts, and actionable failures while preserving native credentials and unrelated state.

**Independent Test**: Install a prebuilt archive into an isolated prefix/profile on each OS, verify
all eleven binaries and Qwen plugin/skills, run ready and deliberately unready doctor cells without
creating a transcript, smoke one peer and one lane, then upgrade/remove and prove owner state intact.

### Tests for User Story 5

- [X] T066 [P] [US5] Write failing session-free doctor projection tests proving it consumes T016's readiness evidence for executable/package/version, parser probes, ACP initialize-only behavior, archive capability, trust, non-secret credential/provider configuration state (`ready`/`unknown`/`unready`), profile identity, requested-to-expected initial-mode mapping, and cause-specific failures without creating a second probe path or claiming live provider authentication/effective launch mode in `internal/bridge/qwen_diagnostics_test.go`
- [X] T067 [P] [US5] Extend the foundational installer tests with manifest/version/enabled-state drift, missing-integration diagnostics, upgrade/removal, unsafe-live-operation refusal, availability-aware `install-all` aggregation that skips absent products while retaining strict direct targets, and owner-nonmutation cases in `internal/bridge/qwen_plugin_test.go` and `internal/qwenprofile/profile_test.go`
- [X] T068 [P] [US5] Write failing eleven-binary, four-plugin, checksum, generated-installer, no-Go, and help/inventory package tests in `internal/bridge/integration_test.go` and `scripts/test`; include regressions that validate representative valid/invalid evidence documents against normative `specs/001-qwen-support/contracts/release-evidence.schema.json` plus its RFC-8785-plus-LF/cross-field rules, build each archive twice to require byte-identical output, prove candidate and tag entrypoints use the same authoritative `deploy/peer-federator/VERSION`, and parse the real `.github/workflows/ci.yml` build/release entrypoints to fail if they use a private hard-coded executable/plugin list, omit either Qwen binary, omit any plugin payload, omit schema validation, inject ref-dependent package versions, allow pre-tag creation over an existing local/remote tag, reject the exact signed triggering tag, replace an existing release/asset, or publish without the authoritative inventory and evidence artifact

### Implementation for User Story 5

- [X] T069 [US5] Implement the stable JSON doctor projection by consuming the sole `internal/qwenreadiness` evidence engine from T016 (also consumed by T059), distinguishing credential/provider configuration state and expected initial mode from managed-launch authentication/effective mode without secret values, transcript creation, or duplicate Qwen probe logic, in `internal/bridge/qwen_diagnostics.go`
- [X] T070 [US5] Extend the foundational profile-scoped verifier with drift diagnostics plus exact live-upgrade/removal refusal and reconciliation in `internal/bridge/qwen_plugin.go` and `internal/qwenprofile/profile.go`
- [X] T071 [US5] Integrate the foundational `install-qwen` target into availability-aware default-profile `install-all`, upgrade/removal, and release-install flows in `Makefile`, with every absent native product skipped explicitly and every direct product target remaining strict; qwen peer/lane binary build targets are owned by T026 and T039
- [X] T072 [US5] Extend release packaging and the generated prebuilt installer to validate and install eleven binaries plus the Qwen payload in `scripts/package-release` and `scripts/test`; make archive construction deterministic; update `.github/workflows/ci.yml` to call the repository-owned authoritative build/package entrypoint instead of its hard-coded binary loop, derive the identical `0.2.4` package/binary version from `deploy/peer-federator/VERSION` in candidate and tag builds while independently validating tag `v0.2.4`, build all eleven executables and four plugin payloads for every platform, generate RFC-8785-plus-LF evidence and validate it against `specs/001-qwen-support/contracts/release-evidence.schema.json` plus the documented cross-field rules, upload the exact artifact defined by `specs/001-qwen-support/contracts/release-evidence.md`, make pre-tag creation refuse an existing local or remote tag, and make the tag-release job require/verify the exact signed triggering tag and its run/digest/commit/tree while refusing an existing release or same-named asset before attaching the unchanged JSON and byte-identical checksummed archives
- [X] T073 [P] [US5] Write the symmetric Qwen operator guides in `docs/QWEN-ADAPTER.md`, `docs/QWEN-LANES.md`, and `docs/QWEN-INSTALL.md`
- [X] T074 [P] [US5] Update the four-product overview, install, groups, protocol, federation, acceptance, and safety guidance in `README.md`, `docs/README.md`, `docs/INSTALL.md`, `docs/GROUPS.md`, `docs/ADAPTER-PROTOCOL.md`, `docs/FEDERATION.md`, `docs/federation/PROTOCOL.md`, `docs/federation/OPERATIONS.md`, and `docs/ACCEPTANCE-MATRIX.md`
- [X] T075 [P] [US5] Validate the deployment examples owned by T061 against the frozen Qwen runtime, profile, install, listener, and help contracts without editing those examples again, recording the review in `specs/001-qwen-support/evidence/us5-deployment-examples.md`; release version ownership remains exclusively with T082
- [ ] T076 [US5] Perform a fresh no-Go prebuilt installation and peer/lane smoke on Linux and record hashes, profile preservation, and uninstall evidence in `specs/001-qwen-support/evidence/us5-linux.md`
- [ ] T077 [US5] Perform a fresh no-Go prebuilt installation and peer/lane smoke on macOS and record hashes, profile preservation, and uninstall evidence in `specs/001-qwen-support/evidence/us5-macos.md`
- [ ] T078 [US5] Exercise default and explicit-profile upgrade/removal with live-session refusal and record credential/settings/transcript nonmutation in `specs/001-qwen-support/evidence/us5-upgrade-remove.md`

**Checkpoint**: Operators can install, diagnose, upgrade, and remove Qwen support from release
artifacts without a source checkout or collateral native-profile changes.

---

## Phase 8: Polish, Adversarial Proof, and Release Readiness

**Purpose**: Close cross-story classes, run the constitutional gates, and produce one exact release
boundary only after every desired story is green.

- [X] T079 [P] Add remaining cross-story adversarial regressions for partial publication, path/PID reuse, same-name socket replacement, legacy symlink migration, symlink-substitution refusal, duplicate collection, native external archive conflicts, agent/supervisor restart, and neutral peer-message provenance without transport-generated trust or authority instructions in `internal/bridge/review_regression_test.go`, `internal/bridge/mcp_test.go`, and `internal/federator/runtime_regression_test.go`
- [ ] T079A Close the repeated Linux/macOS adapter-platform defect class: introduce shared canonical existing/future path identity, exact `sockaddr_un` byte budgeting, compact socket test roots, and fail-closed process-environment observability in `internal/pathidentity`, `internal/socketpath`, `internal/testutil`, and `internal/procinfo`; migrate all four applicable adapters and Qwen profile-mutation guards; add stock-Darwin alias, long-socket, empty-environment, and unrelated-state regressions; and document the mandatory new-adapter platform contract in `docs/ADAPTER-PROTOCOL.md`, `specs/001-qwen-support/spec.md`, and `specs/001-qwen-support/plan.md`
- [ ] T079B Close cross-product DRY drift: use one established parser for owned lane CLIs, centralize lane dispatch/parent/state/list/readiness/notice/control contracts, environment operations, permission classification, and install projections; enforce a 100-token clone gate; retain explicit native lifecycle differences; and record the audit plus rationale in `docs/ADAPTER-PROTOCOL.md` and `specs/001-qwen-support/evidence/dry-audit.md`
- [X] T080 [P] Extend the single generated completeness table and harness introduced by T004/T005—without adding another product inventory—to cover descriptors, help, runtime roles, the exact product-neutral `agent_sessions` MCP namespace with no `claude_peer` alias, skills including the absence of any Claude native-carrier fallback instruction, neutral `Message from <peer>:` delivery framing without trust/instruction-hierarchy injection, package entries, documentation, normative `specs/001-qwen-support/contracts/release-evidence.schema.json`, and the actual `.github/workflows/ci.yml` build/release inputs in `internal/bridge/product_test.go`, `internal/launcher/product_test.go`, and `internal/federator/product_test.go`; workflow drift from the authoritative eleven-executable/four-plugin inventory, schema, canonicalization rules, or staged tag/release collision semantics must fail this gate
- [X] T081 Audit the implementation against every Qwen requirement and append any missing executable work to `specs/001-qwen-support/tasks.md`
- [ ] T082 Freeze the release inputs before rehearsal: run T080's completeness checks against the actual CLI help, Qwen/Codex/Claude/Grok skill payloads, deployment examples owned by T061, documentation owned by T073-T075, `.github/workflows/ci.yml`, and normative `specs/001-qwen-support/contracts/release-evidence.schema.json`; return any discrepancy to its owning task rather than editing a second copy here; reconcile `README.md`, `docs/README.md`, `docs/QWEN-ADAPTER.md`, advisory `docs/designs/QWEN-ADAPTER.md`, `specs/001-qwen-support/contracts/release-evidence.md`, and its schema; and update v0.2.4 in `deploy/peer-federator/VERSION`
- [ ] T083 Run the complete Linux rehearsal gate—normal tests, race tests, vet, repository-managed lint, focused Qwen contracts, and exact toolchain/native versions—on the frozen T082 tree and record results in `specs/001-qwen-support/evidence/gates-linux.md`; this in-tree report is pre-candidate evidence, not the final tagged-commit gate
- [ ] T084 Run the equivalent complete macOS rehearsal gate on the frozen behavior from T082 and record results in `specs/001-qwen-support/evidence/gates-macos.md`; this in-tree report is pre-candidate evidence, not the final tagged-commit gate
- [ ] T085 Exercise the same repository-owned build/package entrypoint called by `.github/workflows/ci.yml` to build every platform rehearsal package twice from the frozen behavior and require byte-identical hashes, verify checksums and eleven-binary/four-plugin contents, prove both candidate/tag modes use version `0.2.4` from `deploy/peer-federator/VERSION`, perform the isolated no-Go prebuilt-install checks, validate RFC-8785-plus-LF valid and adversarial invalid `agent-sessions-v0.2.4-release-evidence.json` fixtures against normative `specs/001-qwen-support/contracts/release-evidence.schema.json` plus every cross-field rule, exercise existing-tag versus existing-release/asset collision fixtures separately, and record results in `specs/001-qwen-support/evidence/release-archives.md`; these artifacts MUST NOT be published as the final release artifacts
- [ ] T086 Execute every step of `specs/001-qwen-support/quickstart.md` as the pre-candidate rehearsal and record all accepted, rejected, and confounded evidence in `specs/001-qwen-support/evidence/quickstart-final.md`
- [ ] T087 Perform an independent RCA/safety review of identity, rollback, cleanup debt, native permissions, archive compensation, group isolation, packaging, `.github/workflows/ci.yml`, and two-platform evidence; write `specs/001-qwen-support/evidence/release-review.md` and a non-self-referential precommit release checklist in `specs/001-qwen-support/evidence/release.md` containing the intended version/tag, hashes of all T083-T086 evidence, and the expected schema version/artifact name from `specs/001-qwen-support/contracts/release-evidence.md` plus `release-evidence.schema.json`, but no claim about a not-yet-created final commit, workflow run, artifact digest, or tag; explicitly review that tag creation rejects a pre-existing local/remote tag while the later job requires that exact signed tag and rejects only a pre-existing release or colliding asset
- [ ] T088 Commit the complete frozen source, documentation, workflow, schema, and T083-T087 in-tree evidence as a signed release commit on `main`; without modifying that commit's tree, run the final `.github/workflows/ci.yml` gate at that exact commit, including normal tests, race tests, vet, repository-managed lint, focused contracts, all four authoritative-inventory package builds, both real-OS quickstart/permission/federation cells, and one fresh prebuilt installation per OS; require the workflow to emit immutable artifact `agent-sessions-v0.2.4-release-evidence-<full-commit-sha>` containing canonical `agent-sessions-v0.2.4-release-evidence.json` validated against normative `specs/001-qwen-support/contracts/release-evidence.schema.json` and every documented cross-field invariant, with toolchain/native-client versions, gate links/digests, commit/tree IDs, four archive hashes, and the exact eleven-executable/four-plugin inventory; after independent digest verification and an explicit local/remote tag-absence check, create signed annotated tag v0.2.4 pointing exactly to that commit with the required evidence run/artifact/SHA-256 trailers; require the tag-release job to require and verify that exact signed triggering tag, retrieve the exact artifact by run identity, revalidate its schema/canonical bytes/cross-field rules, refuse an existing GitHub release or same-named asset, attach the unchanged JSON plus checksummed packages rebuilt from the exact commit, and only then synchronize `develop` to `main` without changing the tagged tree; any failure, missing/changed evidence byte, workflow/package/schema drift, collision, or tree change restarts T082-T088

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: No dependencies; T001-T003 may begin immediately.
- **Phase 2 (Foundation)**: Depends on Phase 1 and blocks every user story. T005 follows T004; tests
  T004, T005, T009, T011, T013, and T015 precede their implementations. T016 establishes both the
  selected-profile installer/verifier and the sole Qwen-specific readiness engine.
- **US1 (Phase 3)** and **US2 (Phase 4)**: Both depend on the green foundation, including the T016
  selected-profile installer/verifier, and may proceed in parallel on separate files after shared
  interfaces stabilize.
- **US3 (Phase 5)**: Depends on US1's managed Qwen parent and US2's Qwen target lane.
- **US4 (Phase 6)**: Depends on US2 and US3 so both remote Qwen target and Qwen-parent source paths
  are available.
- **US5 (Phase 7)**: Doctor, upgrade/removal, and package work may begin after Foundation; package
  smoke tasks T076-T078 depend on US1-US4 being complete.
- **Phase 8 (Polish/Release)**: Depends on every story selected for the release. Execution is T079-T082,
  then T083-T087 as in-tree rehearsal/evidence generation, and finally T088 to create and test the
  signed `main` release commit without further tree writes before tagging. Any failure or source/tree
  change after that commit restarts T082-T088; final self-identifying evidence remains in the exact
  workflow artifact and GitHub release asset defined by `contracts/release-evidence.md` and is bound
  into the signed tag by run identity, artifact name, and SHA-256.

### User Story Dependencies

```text
Setup -> Foundation (including install-qwen) -> +-> US1 -+
                                                 +-> US2 -+-> US3 -> US4
                                                 +-> US5 doctor/upgrade/package core

US1 + US2 + US3 + US4 + US5 -> Polish/Release
```

- **US1 (P1)**: Independently testable after Foundation, whose T016 installer establishes the selected
  profile integration, with one Qwen peer and one existing peer.
- **US2 (P1)**: Independently testable after Foundation, whose T016 installer establishes the selected
  profile integration, with one existing managed parent.
- **US3 (P2)**: Integrates the independently green US1 and US2 surfaces into the 4x4 matrix.
- **US4 (P2)**: Extends the green composition/lane contracts across two current-version hosts.
- **US5 (P3)**: Its doctor and plugin contracts are independent after Foundation; complete prebuilt
  smoke validation consumes the finished peer, lane, composition, and federation surfaces.

### Within Each User Story

- Write and run the story's tests first; record the expected failure when feasible.
- Implement durable state and native adapters before command wiring or live acceptance.
- Close exact cleanup/reconciliation before crediting normal lifecycle success.
- Run focused Go tests before real native-client cells.
- A first genuine RED stops broader cells until RCA and a class-closing regression are complete.

## Parallel Opportunities

- Setup fixture, helper, and evidence-template tasks T001-T003 are independent.
- Foundation test tasks T004, T009, T011, T013, and T015 can be authored in parallel; T005 starts
  only after T004 establishes the generated table/harness interface.
- US1 and US2 can proceed in parallel after T017 if shared interfaces are held stable.
- Within each story, tasks marked `[P]` touch separate contracts/files and can run concurrently.
- Linux and macOS evidence runs must use the same exact commit but can execute concurrently after the
  corresponding automated tests are green.
- Documentation tasks T073-T075 and final adversarial tests T079-T080 can proceed in parallel with
  non-overlapping implementation once behavior is frozen.

## Parallel Example: User Story 1

```text
Task T018: qwen-peer CLI contract tests in internal/launcher/qwen_peer_test.go
Task T019: dual-output/input contract tests in internal/bridge/qwen_test.go
Task T020: messaging authorization tests in internal/bridge/qwen_authorization_test.go
Task T022: lifecycle cleanup tests in internal/bridge/qwen_cleanup_test.go
```

## Parallel Example: User Story 2

```text
Task T032: lane CLI/state contract tests in internal/bridge/qwen_lane_test.go
Task T033: ACP protocol tests in internal/bridge/qwen_lane_manager_test.go
Task T035: native archive helper tests in internal/bridge/qwen_archive_test.go
Task T036: lane crash/recovery tests in internal/bridge/qwen_lane_cleanup_test.go
```

## Parallel Example: User Story 3

```text
Task T046: 4x4 contract matrix tests in internal/bridge/group_agent_test.go
Task T047: group/anchor tests in internal/bridge/group_context_test.go
Task T048: Qwen parent MCP authorization tests in internal/bridge/mcp_lane_test.go
Task T051/T052: product-specific skill surfaces in skills/, claude/skills/, grok/skills/, and qwen/skills/
```

## Parallel Example: User Story 4

```text
Task T056: protocol/capability tests in internal/federator/protocol_test.go
Task T057: remote placement/failure tests in internal/federator/lane_test.go
Task T058: collection/notice tests in internal/federator/lane_watch_test.go
Task T061: deployment examples in deploy/peer-federator/
```

## Parallel Example: User Story 5

```text
Task T066: doctor tests in internal/bridge/qwen_diagnostics_test.go
Task T067: plugin/profile install tests in internal/bridge/qwen_plugin_test.go
Task T068: package/prebuilt/real-workflow inventory and release-evidence schema tests in internal/bridge/integration_test.go, scripts/test, .github/workflows/ci.yml, and specs/001-qwen-support/contracts/release-evidence.schema.json
Task T073/T074/T075: non-overlapping product/global/deployment documentation
```

---

## Implementation Strategy

### MVP First (User Story 1)

1. Complete Setup T001-T003.
2. Complete and validate the shared Foundation T004-T017.
3. Use the T016 selected-profile installer/verifier and complete US1 T018-T031 with both Linux and
   macOS evidence.
4. Stop and validate the independent managed-peer workflow before adding lane complexity.

### Incremental Delivery

1. **Foundation**: one product descriptor, generalized preparation, shared tool ledger, exact profile,
   and a valid Qwen plugin payload with a supported profile-scoped installer/verifier.
2. **US1**: native Qwen becomes an authenticated grouped interactive peer.
3. **US2**: Qwen becomes a durable local lane target with native archive/resume.
4. **US3**: Qwen composes symmetrically with all four products.
5. **US4**: Qwen lanes run across exact current-version Linux/macOS hosts.
6. **US5**: release archives install and diagnose Qwen without owner-state mutation.
7. **Release**: adversarial, two-platform, prebuilt, documentation, and signed-boundary gates.

### Multi-Contributor Strategy

After Foundation is frozen:

- Contributor A owns US1 launcher/interactive files.
- Contributor B owns US2 ACP/lane/archive files.
- Contributor C prepares US5 doctor/plugin/package tests and documentation without landing runtime
  claims before US1/US2 evidence exists.
- US3/US4 integration begins only after the relevant parent and target contracts are green.

## Notes

- `[P]` means different files and no dependency on unfinished implementation; shared-file edits must
  be serialized even when the conceptual work is independent.
- T016, T026, T039, and T071 own distinct Qwen installer, peer-binary, lane-binary, and aggregate
  `Makefile` concerns respectively; their edits to that shared file must be serialized in that order.
- Existing products must remain green after every shared refactor; a Qwen pass cannot waive a Codex,
  Claude, or Grok regression.
- Native Qwen permissions remain Qwen-owned. Tasks preserve exact interactive launch requests and
  corroborate ACP lane initialization where the native protocol exposes it; they do not add sandbox,
  hook, guard, deny-list, or input-filter enforcement.
- Every cleanup task requires exact PID/start/strong-start and path/type/inode/revision proof.
- Do not credit retries, fixed sleeps, vacuous assertions, confounded signals, or manually edited
  state as acceptance evidence.
- Commit after each task or coherent dependency group, preserving unrelated worktree changes.
