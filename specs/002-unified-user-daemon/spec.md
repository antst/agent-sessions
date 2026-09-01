# Feature Specification: Unified User Daemon

**Feature Branch**: `feature/unified-user-daemon-v2`

**Created**: 2026-08-29

**Status**: Draft — specification restart from the working baseline

**Input**: "Converge the existing working Agent Sessions implementation into one stable user-space
daemon per OS user-host. Preserve the behavior captured by the pre-unification code and tests. Share
common behavior through DRY implementations, but retain product-specific rules wherever the native
products differ. MCP processes may become stateless relays."

## Behavioral Authority

The merged `develop` commit `c056fbc5015d4ab0a673f66cac5404206f7bcee6` is the executable
functional baseline. Its launchers, bridge code, product integrations, tests, scripts, and installed
acceptance procedures capture two weeks of product debugging and are normative for this feature.

The feature may change Agent Sessions process topology, executable packaging, internal ownership,
state layout, and private protocols. It may not change native-product behavior merely because a new
generic design is simpler. A generalized implementation is correct only when it satisfies every
applicable baseline behavior. Where products differ, the shared abstraction must expose the
difference rather than erase it.

Before replacing a baseline path, implementation work must record:

1. the old functions and tests that implement or prove the behavior;
2. the native-product invariant they capture;
3. the shared mechanism or product-specific adapter that will own it;
4. the automated and real-installed acceptance cells proving parity; and
5. the point after which the old implementation may be removed.

No baseline implementation or test may be deleted before its mapped replacement passes those gates.

## Closed Acceptance Matrix

The working baseline's acceptance surface is a closed set of exactly **202 individually reportable
cells**. The authoritative assertions remain in `docs/ACCEPTANCE-MATRIX.md`; the stable IDs and family
cardinalities are frozen in `contracts/acceptance-matrix.yml`:

- 8 source and packaging cells (`S-01` through `S-08`);
- 12 installation and upgrade cells (`U-01` through `U-12`);
- 18 Codex interactive cells (`C-01` through `C-18`);
- 11 Claude interactive cells (`CL-01` through `CL-11`);
- 21 Grok interactive cells (`G-01` through `G-21`);
- 10 Qwen interactive cells (`Q-01` through `Q-10`);
- 30 shared and product lane lifecycle cells (`L-01` through `L-30`);
- 16 explicitly named parent-product to target-product composition cells (`P-*`);
- 64 explicitly named peer/lane sender-to-destination messaging cells (`M-*`);
- 8 federation, global-group, and host-suffix cells (`X-01` through `X-08`); and
- 4 product archive/unarchive cells (`A-C`, `A-CL`, `A-G`, and `A-Q`).

Every cell keeps its baseline user-visible and native-product invariant. An observation that names an
Agent Sessions process, service, or package boundary removed by this feature may change only through the
reviewed topology-delta ledger in `contracts/acceptance-matrix.yml`; the cell ID and functional assertion
remain stable. Each run must emit one structured result per applicable cell and distinguish `PASS`,
`N/A`, `BLOCKED`, `RED`, and `NOT EXECUTED - PREREQUISITE RED`. Aggregate commands, summaries,
fake-vendor results, and inferred ordering receive no cell credit.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Preserve Interactive Peers (Priority: P1)

A user launches, resumes, uses, exits, and later resumes Codex, Claude, Grok, and Qwen peers exactly as
they did before unification. Authentication, profiles, native arguments, permissions, hooks, session
selection, messaging, and native history continue to behave according to each vendor's established
contract.

**Why this priority**: Interactive peers are the entry point for every other Agent Sessions workflow.
A one-daemon topology has no value if the four products no longer start or resume correctly.

**Independent Test**: On authenticated native installations, run the complete baseline interactive
matrix for each product before and after moving its Agent Sessions ownership into the daemon. Compare
native process arguments, selected profiles and sessions, published identities, messaging, permission
state, cleanup, and residue.

**Acceptance Scenarios**:

1. **Given** an authenticated native product and a running Agent Sessions daemon, **When** its peer
   command starts a new named session, **Then** the native client starts with its normal authenticated
   profile, the managed identity becomes authoritative only after exact native evidence exists, and
   the user can complete a real model turn.
2. **Given** a previously used managed session, **When** the user resumes it by durable name or exact
   native UUID, **Then** the same native history appears and no empty or duplicate session is created.
3. **Given** native flags, permission options, profile variables, working-directory options, or a
   native argument delimiter, **When** the peer command launches or resumes, **Then** wrapper-owned
   options are handled once and every vendor-owned argument retains its baseline ordering and meaning.
4. **Given** a bare native client with the integration installed, **When** hooks or MCP connectors run,
   **Then** hooks are silent successful no-ops, MCP tools remain inactive, and no managed attachment or
   peer identity is created.
5. **Given** a managed peer, **When** native hooks or connectors run, **Then** they attest the exact
   launch and native actor using the product's established evidence and never trust a model-supplied
   identity.
6. **Given** normal exit, interrupt, crash, stale artifacts, or daemon restart, **When** reconciliation
   completes, **Then** only exact Agent Sessions-owned attachment artifacts are retired and the native
   session, profile, credentials, settings, and transcript remain intact; a retired attachment is absent
   from peer discovery, and direct send to its former address returns the canonical no-live-target result
   without queuing a message.

---

### User Story 2 - Preserve Lanes and Collaboration (Priority: P1)

A managed parent from any supported product starts and controls a lane using any supported target
product. Messaging, groups, permission inheritance, follow-up, interruption, collection, archive,
unarchive, resume, and cleanup retain their baseline semantics while shared durable ownership moves
into the daemon.

**Why this priority**: The complete 4×4 composition and collaboration surface is existing product
functionality, not an optional enhancement.

**Independent Test**: Exercise every parent-target combination with real installed products and prove
destination-visible messaging, group isolation and inheritance, permission behavior, lifecycle
operations, native history, restart outcomes, and exact cleanup.

**Acceptance Scenarios**:

1. **Given** any managed parent product, **When** it starts any lane product, **Then** the exact parent,
   groups, working directory, permission mode, target product, and native session are durably recorded
   before work is accepted.
2. **Given** shared and disjoint global groups, **When** peers discover, message, multicast, broadcast,
   or delegate, **Then** recipients match the baseline group rules and no additional namespace or
   visibility boundary exists.
3. **Given** an accepted lane turn, **When** the parent follows up, interrupts, waits, collects,
   archives, unarchives, or resumes, **Then** the product-specific native operation and the shared
   durable state each advance exactly once.
4. **Given** a parent permission mode and a later restart or resume, **When** a session or lane becomes
   active again, **Then** the baseline product-specific permission propagation and sticky/non-sticky
   rules remain unchanged.
5. **Given** a daemon restart during active work, **When** the native product supports safe
   reconnection, **Then** the exact work continues without redispatch; otherwise the established
   product behavior yields one explicit collectable and resumable interruption.
6. **Given** worker failure, manager-equivalent failure, parent exit, or cleanup interruption, **When**
   reconciliation runs, **Then** it preserves unrelated work and converges through exact ownership or
   durable cleanup debt. Once a lane is archived or otherwise has no live message recipient, it is absent
   from peer discovery and direct send to its former address returns the canonical no-live-target result
   without queuing a message; a terminal but deliberately resumable unarchived lane retains only the
   baseline visibility its product already defines.

---

### User Story 3 - Operate One Host Authority (Priority: P1)

An operator installs and manages one service-owned `agent-sessions` daemon per OS user-host. Shared
Agent Sessions state, routing, delivery, lane ownership, product coordination, and the outbound host
federation connection run at one version in that daemon.

**Why this priority**: Eliminating mixed-version supervisors, managers, shims, and host agents is the
reason for the refactor.

**Independent Test**: Install on clean Linux and macOS users, start and restart the service, exercise
all four products, and prove one Agent Sessions authority and endpoint while native vendor processes
remain external.

**Acceptance Scenarios**:

1. **Given** a clean user account, **When** Agent Sessions is installed, **Then** one standard systemd
   user service or launchd user agent is enabled and becomes the sole Agent Sessions host authority.
2. **Given** the daemon is stopped, **When** a peer, lane, hook, connector, messaging command, or host
   federation workflow runs, **Then** it reports the daemon unavailable and does not manage the
   daemon's lifetime.
3. **Given** a peer first requires vendor-owned infrastructure, **When** the daemon prepares that
   product, **Then** it preserves the baseline lazy vendor-infrastructure behavior—including starting
   and reconnecting a Codex App Server—without treating that vendor process as another Agent Sessions
   authority.
4. **Given** an explicit install or upgrade, **When** the transaction commits, **Then** it performs one
   validated daemon restart and cannot leave mixed Agent Sessions versions serving the same user.
5. **Given** a routine daemon restart, **When** native peers or lanes are live, **Then** the service
   manager does not signal them and the successor reconstructs coordination using exact native
   evidence.
6. **Given** zero, one, several, or all native products installed, **When** the daemon reports
   readiness, **Then** available products work independently and missing products are reported without
   breaking the daemon or other adapters.

---

### User Story 4 - Preserve Multi-Host Federation (Priority: P2)

The existing single hub and multiple host agents continue to provide one uniform multi-host space,
global groups, host-suffixed peer addresses, messaging, and remote lanes. Each host's outbound agent
runs inside its one daemon; the central hub remains a separate `agent-sessions-hub` deployment.

**Why this priority**: Process unification must not alter collaboration topology or authorization.

**Independent Test**: Connect Linux and macOS hosts to one hub, run cross-host peer and lane traffic,
restart and independently upgrade hosts and hub, and compare recipients and outcomes with the baseline.

**Acceptance Scenarios**:

1. **Given** multiple hosts connected to one hub, **When** peers discover or address one another, **Then**
   global groups remain the only collaboration boundary and host suffixes only disambiguate names.
2. **Given** matching hub protocol versions from unrelated builds, **When** a host or hub is upgraded
   independently, **Then** they interoperate without SHA or release coupling.
3. **Given** a hub protocol mismatch, **When** a connection is attempted, **Then** registration and work
   acceptance fail before any partial participation.
4. **Given** a host daemon restart or network interruption, **When** connectivity returns, **Then** the
   same host identity reconnects without duplicate delivery or remote lane dispatch.

---

### User Story 5 - Install Across a Greenfield Boundary (Priority: P2)

The three controlled development hosts move from the unreleased split-runtime prototype to version
0.3 through an explicit clean stop and removal of Agent Sessions-owned prototype state. Vendor-owned
profiles and histories are retained.

**Why this priority**: A one-use compatibility subsystem would add risk without user value before the
software has been released.

**Independent Test**: On each of the three controlled hosts, first prove the old stack is quiescent
outside the product, invoke the repository's one-time cleanup script with no arguments to review its
exact plan and metadata-only plan revision from `contracts/pre-unification-cleanup.yml`, and then invoke
it separately with `--apply <plan-revision>`.
Remove only the selected pre-unification Agent Sessions MCP registrations, skills, binaries, service
artifacts, and data produced and owned solely by the legacy implementation, then install version 0.3.
Prove that the script and contract are absent from release packages and unreachable from operational
Make targets, `install`, `install-all`, update, remove, and service paths, while isolated automated tests
may exercise the script only against fixture-owned roots and native profiles still authenticate and
resume their existing histories.

**Acceptance Scenarios**:

1. **Given** the operator has stopped every old Agent Sessions process, **When** the clean install runs,
   **Then** it neither scans nor migrates pre-unification Agent Sessions state.
2. **Given** existing vendor profiles, credentials, transcripts, and histories, **When** version 0.3 is
   installed or removed, **Then** those vendor resources are neither migration inputs nor cleanup
   targets.
3. **Given** a failed install or upgrade, **When** the operation exits, **Then** the prior selected
   unified release and service state remain usable without modifying vendor state.
4. **Given** one of the three controlled hosts still contains the unreleased split-runtime prototype,
   **When** the operator runs `scripts/cleanup-pre-unification` without arguments and later reruns it with
   `--apply <plan-revision>`, **Then** the first invocation is plan-only, the apply invocation recomputes
   the complete allowlisted selection and refuses all mutation unless its metadata-only plan revision
   exactly matches the reviewed revision, revalidates each target's exact metadata revision immediately
   before deletion, and removes only confirmed matching legacy MCP registrations, skills, binaries,
   service artifacts, registries, journals, caches, logs, locks, sockets, and other private operational
   data.
   A changed or ambiguous target fails closed; a later plan reports no remaining work; and no invocation
   reads or removes vendor credentials, non-Agent-Sessions settings, transcripts, histories, ordinary
   product files, or unrelated integrations.

### Edge Cases

- A native executable is installed in a vendor-specific location rather than the shell's first PATH
  entry.
- A profile variable is absent, explicitly empty, or points to a non-default authenticated profile.
- Native resume selection changes a display title while durable Agent Sessions naming remains stable.
- Two sessions share a display name, a PID is recycled, a socket changes type, or a registry row is
  stale.
- A connector starts under a vendor-owned private leader rather than the visible TUI.
- A hook belongs to a bare session, a prepared launch, an adopted session, or a stale generation.
- The daemon restarts after durable acceptance but before native delivery or terminal publication.
- The machine sleeps, the network drops, the hub restarts, or one product is unavailable.
- Removal is requested while exact managed peers or lanes remain active.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `c056fbc5015d4ab0a673f66cac5404206f7bcee6` MUST remain the functional
  authority for every behavior not explicitly limited to the new process, service, packaging, private
  protocol, or Agent Sessions-owned state topology.
- **FR-002**: Every replaced baseline path MUST have a reviewed port-map entry naming its old symbols,
  old tests, captured invariant, new owner, parity evidence, and deletion gate.
- **FR-003**: The implementation MUST generalize behavior shared by products into one implementation
  and MUST retain explicit product-specific rules when the baseline or native contract differs.
- **FR-004**: A generalized implementation MUST NOT receive credit unless every mapped product
  behavior passes unchanged functional assertions. Similar-looking code is not evidence that native
  semantics are equivalent.
- **FR-005**: No baseline implementation, regression test, or installed acceptance path may be deleted
  before its mapped replacement passes focused tests and real installed Linux and macOS acceptance.
- **FR-006**: Exactly one long-lived `agent-sessions` daemon MUST own Agent Sessions host state,
  attachments, routing, delivery, lanes, cleanup debt, product coordination, and the outbound host
  federation connection for one OS user-host.
- **FR-007**: The daemon MUST expose one private Agent Sessions local endpoint. No supervisor, shim,
  product host, lane manager, local router, or host federation agent may remain an independent
  Agent Sessions authority.
- **FR-008**: Peer, lane, hook, MCP, messaging, and federation workflow commands MUST NOT start, stop,
  restart, replace, or supervise the Agent Sessions daemon. Direct service administration and explicit
  install or upgrade transactions are its only lifecycle authorities.
- **FR-009**: The daemon MUST preserve baseline vendor-infrastructure lifecycle behavior required by a
  product. In particular, the first managed Codex operation MUST lazily ensure the selected profile's
  App Server is running and reconnect to it without requiring manual startup.
- **FR-010**: Native executables, App Servers, private leaders, ACP workers, credentials, profiles,
  settings, permissions, transcripts, and histories MUST remain vendor-owned resources. Starting or
  coordinating a required vendor process does not make it Agent Sessions authority.
- **FR-011**: Vendor-required MCP processes MAY remain short-lived stateless relays. They MUST own no
  Agent Sessions durable registry or listener and MUST forward the product-specific attestation
  evidence needed by the daemon rather than replacing it with a generic identity assumption.
- **FR-012**: Bare native sessions MUST remain inactive. Their hooks MUST exit successfully without
  output or state mutation, and their MCP calls MUST return the canonical inactive result.
- **FR-013**: Managed hook events MUST be authorized from exact prepared or adopted attachment evidence
  and MUST preserve each product's established start, refresh, permission, stop, and cleanup behavior.
- **FR-014**: Every managed actor MUST be authorized by exact corroborated product, profile, native
  session, process identity and ancestry, working directory, launch evidence, and runtime generation as
  applicable. Names and model-supplied identifiers are never authority.
- **FR-015**: Codex behavior MUST preserve native argument placement and passthrough, fresh and resumed
  thread preparation, durable-name and UUID resume, cwd validation, loaded-owner handling, permission
  modes, App Server recovery, hook/MCP ancestry, archive/unarchive, history continuity, and cleanup.
- **FR-016**: Claude behavior MUST preserve profile and secure-storage namespace selection, caller
  settings merging, managed permission constraints, launch gating, native row and socket publication,
  late UUID/name adoption, delivery, permission refresh, key-sidecar handling, rollback, and exact
  cleanup.
- **FR-017**: Grok behavior MUST preserve executable discovery, native argument and permission rules,
  launch capability, exact TUI/host-or-equivalent/private-leader/MCP ancestry, ACP roster and wake
  coordination, interjection delivery, late session selection, resume, archive, and cleanup.
- **FR-018**: Qwen behavior MUST preserve profile and runtime selection, readiness, native argument and
  permission rules, launch capability, dual-output admission, daemon/ACP ancestry, event/input artifact
  evidence, delivery, resume, archive, rollback, and cleanup.
- **FR-019**: Peer launch and resume MUST preserve the baseline Agent Sessions options and native
  passthrough behavior, including native delimiter and repeated-option semantics. An intentional public
  interface change is allowed only for removing an obsolete process boundary and requires explicit
  before/after acceptance; it cannot change vendor-visible behavior.
- **FR-020**: Discovery, direct send, explicit multicast, group broadcast, reply, and destination
  acknowledgement MUST preserve baseline neutral provenance and global-group authorization. A retired
  peer or archived/non-addressable lane MUST be omitted from discovery, and direct send to its former
  address MUST fail as no live target without creating queued delivery state.
- **FR-021**: Global groups MUST remain the sole collaboration visibility boundary in one uniform
  multi-host space. Product, profile, session, instance, and host identity are for exact attribution and
  addressing, not new namespaces.
- **FR-022**: The complete 4×4 parent-target lane matrix MUST preserve parent context, groups,
  permissions, native selection, messaging, follow-up, interrupt, status, wait, collection, archive,
  unarchive, resume, restart, and cleanup behavior.
- **FR-023**: Shared attachment and lane state machines MUST commit durable ownership before external
  acceptance, prevent duplicate dispatch and collection, and retain exact retryable cleanup debt.
- **FR-024**: One `agent-sessions-hub` binary MUST remain the central network hub while every host's
  outbound agent runs in that host's daemon. Hub and host builds interoperate solely through exact hub
  protocol version equality.
- **FR-025**: Federation MUST preserve one-hub/multiple-host topology, global groups, host-suffixed
  addresses, routing, remote lanes, result notification, reconnect, and duplicate-prevention behavior.
- **FR-026**: Installation MUST enable a standard systemd user service on Linux and launchd user agent
  on macOS, perform one validated restart on explicit upgrade, and prevent mixed Agent Sessions versions.
- **FR-027**: Version 0.3 MUST be a greenfield Agent Sessions state boundary. The operator supplies a
  quiescent old stack; the product MUST contain no pre-unification inventory, adoption, drain,
  retirement, or state-migration subsystem.
- **FR-028**: Install, remove, cleanup, and tests MUST never read, copy, print, hash, rewrite, or
  delete vendor credential values or transcript content. Destructive actions require exact type,
  ownership, identity, and revision checks.
- **FR-029**: Existing Agent Sessions logs, service output, status, doctor, and crash diagnostics MUST
  remain metadata-only and contain no message, prompt, result, tool-content, credential, or vendor
  transcript bytes. This requirement does not create a new observability surface.
- **FR-030**: Implementation MUST proceed product-by-product from transplanted baseline regressions to
  shared extraction to daemon relocation to real installed acceptance. A fake vendor process may test a
  protocol primitive but cannot prove product readiness or functional parity.
- **FR-031**: The 202-cell acceptance matrix in `contracts/acceptance-matrix.yml` MUST remain closed and
  machine-validated. Family defaults and explicit cell overrides MUST expand each cell to its exact
  platforms or capability applicability, unique target-topology assertion row grounded in the
  baseline and topology-delta ledger, and acyclic explicit
  prerequisite-cell set. Any changed Agent Sessions topology observation MUST have a reviewed ledger
  entry containing the old observation, new observation, and unchanged functional invariant. Every
  applicable cell MUST produce an individual structured result linked to its target assertion, exact
  executable evidence, platform, release identity, and preserved-state and cleanup evidence. No cell may
  be merged, silently skipped, or credited from an aggregate result.
- **FR-032**: The repository MUST provide `scripts/cleanup-pre-unification` solely as an explicitly
  invoked, one-time transition utility for the three controlled hosts. The sole allowlist authority MUST
  be the repository-only `contracts/pre-unification-cleanup.yml`, whose entries identify a stable ID,
  platform scope, exact path or bounded root, expected filesystem type and owner, legacy source evidence,
  removal scope, and metadata-only revision rule for each pre-unification Agent Sessions MCP
  registration, skill, binary, service artifact, or item of data produced and owned solely by the legacy
  implementation—including its registries, journals, caches, logs, locks, sockets, and private
  operational state. Entries MUST NOT use globs, PATH lookup, process scans, or content inspection to
  discover additional targets. With no arguments the script MUST be plan-only and mutate nothing;
  its output MUST include a deterministic plan revision derived only from the public contract identity,
  ordered selected stable IDs, and their non-content metadata tuples. Mutation MUST require a separate
  explicit `--apply <plan-revision>` invocation. Apply MUST recompute the complete plan and refuse all
  mutation unless the recomputed revision exactly matches the reviewed revision; immediately before each
  removal it MUST also revalidate type, ownership, no-follow path identity, and the target's metadata
  revision captured during that apply invocation. Opaque legacy-owned files MUST be removed without
  reading, printing, hashing, or diffing their content. Changed, missing-authority, stale-plan, or
  ambiguous targets MUST fail closed; repeated plan/apply invocations MUST be safe; and vendor credentials,
  non-Agent-Sessions settings, transcripts, histories, ordinary product files, unrelated plugins, and
  unrelated processes MUST be preserved. No Makefile may name the script or contract or define a cleanup
  target. Neither artifact may be installed, packaged, called by `install`, `install-all`, update,
  remove, or service code, or become a supported product compatibility subsystem. Generic automated
  test discovery MAY execute isolated cleanup tests only with fixture-owned roots and cannot grant an
  operational Make surface.

### Key Entities

- **Baseline Port Map**: Traceability record from old symbols and tests to a captured invariant, new
  owner, parity evidence, and deletion gate.
- **Topology Delta**: Reviewed replacement of one obsolete Agent Sessions process/service/package
  observation while retaining the cell ID and user-visible or native-product invariant.
- **Host Runtime**: The one Agent Sessions daemon authority for an OS user-host and its exact generation.
- **Managed Attachment**: A durably prepared and then natively attested interactive peer identity.
- **Product Adapter**: Shared lifecycle interface plus only the native rules that genuinely differ.
- **Lane**: Durable parent-target native session with groups, permissions, turns, results, collection,
  archive state, and cleanup debt.
- **Agent Frame**: Existing neutral peer or lane message with exact sender, recipients, groups, host,
  correlation, and idempotency identity.
- **Hub Protocol**: Explicit versioned contract between independently deployed host daemons and the one
  central hub.
- **Controlled-Host Cleanup Utility**: Repository-only, explicitly invoked one-time script that removes
  an exact allowlist of unreleased pre-unification Agent Sessions artifacts and legacy-owned operational
  data from the three controlled hosts. It is not part of the installed product lifecycle or release
  payload and has no authority over vendor-owned transcripts, histories, credentials,
  non-Agent-Sessions settings, or ordinary files.
- **Pre-Unification Cleanup Contract**: Repository-only machine-readable allowlist at
  `contracts/pre-unification-cleanup.yml`. It is the sole source for cleanup target identity and is
  consumed only by the direct cleanup script and its fixture-isolated tests.
- **Pre-Unification Cleanup Plan Revision**: Deterministic identifier over only the public cleanup
  contract identity, ordered selected stable IDs, and their non-content metadata tuples. It binds an
  explicit apply invocation to the exact plan the operator reviewed without reading or hashing opaque
  file content.
- **Baseline Acceptance Bound**: The exact existing state predicate and its existing deadline captured
  from the authoritative baseline test or script for a particular acceptance cell and recorded in
  `evidence/baseline-functional-cells.md`. This feature does not synthesize a new aggregate recovery
  deadline.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every baseline functional cell is individually mapped to old executable evidence and
  passes after unification with unchanged functional and native-product assertions; only
  process/service/package observations present in the reviewed topology-delta ledger may differ.
- **SC-002**: Fresh launch, real turn, normal exit, durable-name resume, UUID resume, messaging, and
  cleanup pass for Codex, Claude, Grok, and Qwen on authenticated Linux and macOS installations; every
  ended peer disappears from discovery and rejects direct delivery without queuing it.
- **SC-003**: The complete 16-cell parent-target lane matrix passes with destination-visible evidence,
  correct group and permission inheritance, lifecycle operations, archive/resume, archived-target
  discovery/send rejection, and zero duplicate dispatch or terminal result.
- **SC-004**: An 8×8 peer/lane sender-destination messaging matrix preserves exact recipients,
  provenance, group isolation, and destination acknowledgement.
- **SC-005**: Bare-session hook and MCP matrices pass for all four products with zero attachment,
  process, socket, registry, settings, or durable-state mutation.
- **SC-006**: Clean install, login start, explicit stop, crash restart, upgrade, and removal pass through
  systemd and launchd with one daemon, one endpoint, and zero obsolete Agent Sessions authority
  processes; a failed install or upgrade leaves the prior selected unified release and service state
  intact.
- **SC-007**: Daemon restart with four live peers preserves all native session identities and restores
  discovery and messaging within each affected cell's exact baseline state predicate and existing
  deadline recorded in the baseline functional-cell evidence, without relaunching a native client or
  losing an accepted message.
- **SC-008**: Linux-to-macOS and macOS-to-Linux peer and lane traffic satisfies each affected cell's
  exact baseline reconnect predicate and existing deadline recorded in the baseline functional-cell
  evidence after a host or hub restart, with unchanged host identities and zero duplicate remote work.
- **SC-009**: Same-protocol host and hub builds from unrelated commits interoperate; a protocol mismatch
  is rejected before registration or work acceptance.
- **SC-010**: Process census finds exactly the two released executable images—`agent-sessions` on hosts
  and `agent-sessions-hub` at the hub—and no long-lived Agent Sessions supervisor, shim, manager,
  product host, local router, or host federation agent.
- **SC-011**: Normal tests, race tests, vet, repository lint, four platform builds, and all applicable
  real installed acceptance cells pass at one exact release candidate on Linux and macOS.
- **SC-012**: A review can trace 100% of removed baseline product functions and tests through the port
  map to a passing replacement or an explicitly topology-only deletion.
- **SC-013**: The matrix validator expands exactly 202 unique stable cell IDs with nonempty platform
  scope, one uniquely resolvable target-topology assertion locator, an acyclic known-ID prerequisite
  set, and a valid reviewed topology-delta ledger; final Linux/macOS evidence accounts for every
  applicable ID without duplicate, missing, aggregate-only, silently skipped, conditionally incomplete,
  ambiguously superseded, or misclassified prerequisite-red results.
- **SC-014**: Direct execution of the one-time cleanup utility on each controlled host removes every
  target in the authoritative cleanup contract—including opaque legacy operational data—and no other
  target. The no-argument invocation is mutation-free and emits a metadata-only plan revision;
  `--apply <plan-revision>` performs no mutation when the recomputed complete plan differs and refuses
  any target whose metadata revision changes after apply-time enumeration. A later plan reports no
  remaining work, operational Make/install/update/remove/service paths contain no invocation of the
  utility, release archives contain neither the utility, its contract, nor legacy payloads, and all
  vendor transcripts, histories, credentials, non-Agent-Sessions settings, and ordinary files remain
  intact.

## Assumptions

- The old split-runtime implementation is unreleased and runs only on three operator-controlled hosts.
  The first version 0.3 install may require the operator to stop it and clear Agent Sessions-owned state.
- Vendor profiles are already authenticated and remain owned by their native clients.
- Native products may require vendor processes in addition to their visible TUI. The single-daemon rule
  prohibits additional Agent Sessions authorities, not vendor-required processes.
- DRY means one implementation for proven shared behavior. It does not mean forcing four different
  vendor protocols through the same unverified launch or attestation sequence.
- The existing one-hub/multiple-host topology, global groups, host suffixes, AgentFrame semantics, and
  permission model remain unchanged.
- The separately deployed hub and host daemon may be built from unrelated commits as long as they speak
  the same explicit hub protocol version.

## Out of Scope

- Redesigning native vendor protocols, authentication, profiles, histories, permissions, or model
  behavior.
- Adding collaboration namespaces, policy layers, quotas, or alternate federation topologies.
- Compatibility with pre-unification Agent Sessions processes, private state, or network protocol.
- Integrating the controlled-host cleanup utility into installation, update, removal, service control,
  release packaging, or any supported post-0.3 compatibility workflow.
- Treating fake vendor helpers or aggregate green output as evidence that a real installed product works.
