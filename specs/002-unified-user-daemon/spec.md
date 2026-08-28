# Feature Specification: Unified User Daemon

**Feature Branch**: `feature/unified-user-daemon`

**Created**: 2026-08-26

**Status**: Implementation and cross-platform release acceptance complete

**Input**: User description: "Converge the existing Agent Sessions functionality into one stable
user-space daemon per OS user on each host so adapters, sessions, lanes, local routing, cleanup, and
the embedded host federation agent always run one version and can be restarted and upgraded as one
host service."

## Clarifications

### Session 2026-08-26

- Q: Must existing peer, lane, messaging, plugin, MCP, and federation command interfaces remain backward-compatible while their runtime ownership moves into the unified daemon? → A: No. Breaking public-interface changes are permitted wherever they simplify the unified design.
- Q: When the unified daemon restarts during an already accepted lane turn, must the turn continue transparently, or may it become explicitly interrupted and resumable when the native product cannot reconnect? → A: Prefer transparent continuation; permit one explicit interrupted, collectable, resumable outcome only when supported native evidence shows safe reconnection would require overengineering or brittle compatibility machinery.
- Q: If a peer, lane, messaging, or federation command runs while the unified daemon is not active, how should the command behave? → A: Fail clearly. The daemon is user-managed, and peer or workflow commands never manage its lifetime.
- Q: When Agent Sessions is installed or upgraded, should the installer restart the user-managed daemon, or should it only stage files and require the user to restart the daemon separately? → A: An explicitly invoked install or upgrade performs one validated daemon restart as part of its transaction.
- Q: If removal is requested while managed peers or lanes are still active, should removal refuse until they are quiescent, or detach them and continue? → A: Refuse removal until every managed peer and lane is quiescent.
- Q: When the same OS user needs production, development, and test work at the same time, may additional test daemons run? → A: No. Exactly one host daemon always; all work uses the existing uniform multi-host routing space and groups. The separately deployed central hub is not another host daemon.
- Q: After installation, should the user service start automatically at login and restart automatically after an unexpected daemon crash, while still respecting an explicit user stop? → A: Yes. Install and enable a standard systemd user service on Linux and launchd user agent controlled through launchctl on macOS, including their platform-specific lifecycle behavior.
- Q: What existing federation and naming behavior must process unification preserve? → A: One hub with multiple host agents, global groups, and host-suffixed peer names.
- Q: Should process unification add any collaboration access or isolation model beyond existing groups? → A: No. Preserve the existing uniform multi-host space and group access model.
- Q: After quiescent removal, what should happen to Agent Sessions-owned configuration and metadata while all vendor transcripts remain untouched? → A: Preserve all Agent Sessions configuration and metadata; require a separate explicit purge to delete them.
- Q: Should daemon administration rely solely on the OS user boundary, while model-facing peer and lane tools remain limited to their existing attested and group-scoped operations? → A: Yes. The OS user owns all administration; model-facing tools stay scoped and expose no daemon administration.
- Q: Should the daemon impose any Agent Sessions-specific quotas, or simply accept work while it can commit it durably and otherwise fail before acceptance with the real resource error? → A: Impose no artificial quotas; durably commit or fail before acceptance.
- Q: Should normal daemon logs, status output, and diagnostics ever contain peer messages, prompts, lane results, or vendor transcript content? → A: No. Operational observability is metadata-only and never logs content, including in debug mode.
- Q: What defines collaboration granularity and access after process unification? → A: The existing global groups are the sole collaboration access boundary in one uniform multi-host space behind one hub; host suffixes only disambiguate peer addresses.
- Q: What is the exclusive goal of this feature? → A: Converge existing Agent Sessions functionality into a stable working form with one host daemon per OS user on each host; do not add product functionality.
- Q: Must the first unified install migrate or adopt state from the unreleased split-runtime prototypes? → A: No. Version 0.3 is a greenfield boundary. The operator stops the old stack and removes or archives Agent Sessions-owned prototype state before installation; the installer contains no legacy inventory, adoption, drain, retirement, or compatibility machinery.
- Q: Should the central federation hub be another mode of the host daemon executable? → A: No. Build one `agent-sessions` host executable and one separate `agent-sessions-hub` central-hub executable. They share one explicitly versioned hub protocol, but deployed builds may come from arbitrary commits or releases; there is still no separate host federation-agent process.
- Q: Does upgrading `agent-sessions` on one host require upgrading or restarting `agent-sessions-hub`? → A: No while both processes declare the same hub-protocol version. Host and hub lifecycle are independent. Their SHA, release version, packaging generation, and installation time are irrelevant to software-version interoperability; a protocol-version mismatch fails closed and requires deploying a matching protocol on the affected side. A host's advertised lane capabilities describe available work and do not couple releases.
- Q: Must the new unified host/hub software interoperate with the pre-unification split-process software? → A: No. This feature replaces every old local command, service, process, host-agent interface, and Agent Sessions-owned state layout. Network interoperability among deployed unified hosts and the hub depends only on equal hub-protocol versions, never on software ancestry.
- Q: Where do the host daemon and hub implementations come from, and what identifies network compatibility? → A: Both are built from this repository, but they may be built from unrelated commits and released or installed independently. Exact hub-protocol-version equality is the sole network-interoperability condition; source SHA, release identity, binary identity, build age, and upgrade order are diagnostic only.
- Q: Should host and hub implementations live behind separate internal package trees? → A: No. The repository's `internal/` packages are separated by logical function. Shared service control, release transactions, protocol, identity, routing, storage, and diagnostics have one implementation used by both roles; host- or hub-specific code supplies only genuinely different role behavior.
- Q: If host and hub are installed for the same user and prefix, may shared lifecycle code force them onto one selected release? → A: No. They use separate role-owned release roots, current selections, locks, journals, service transitions, rollback, removal, and purge decisions while invoking the same lifecycle implementation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operate One Runtime Authority (Priority: P1)

An operator installs, starts, stops, restarts, upgrades, and diagnoses one Agent Sessions service for
their user account. The service owns every Agent Sessions runtime responsibility regardless of which
supported native products are installed. An upgrade cannot leave old supervisors, product managers,
or federation agents serving part of the same user estate.

**Why this priority**: A single versioned authority is the class-closing invariant that prevents the
split-runtime, stale-ownership, and missed-restart defects that motivated this feature.

**Independent Test**: Start from a clean host, install the feature, exercise service status and one
same-version restart, then perform an upgrade while monitoring processes, endpoints, runtime identity,
and unrelated native clients. Verify that exactly one current authority is active throughout each
committed state and no obsolete Agent Sessions authority survives.

**Acceptance Scenarios**:

1. **Given** any supported subset of native products, **When** the operator installs and starts Agent
   Sessions, **Then** one current user service owns local runtime state, adapter coordination, and
   federation, and missing native products are reported as unavailable rather than installation
   failures.
2. **Given** a running current service, **When** the operator requests status, restart, or stop through
   the documented user-service controls, **Then** the operation addresses that one exact service and
   reports its version, identity, health, endpoint, managed attachments, lanes, federation state, and
   cleanup debt without exposing credentials.
3. **Given** the user service is stopped, **When** a peer, lane, messaging, plugin, or federation
   workflow command runs, **Then** it fails with an actionable service-unavailable diagnostic and does
   not start, replace, or restart the daemon.
4. **Given** a validated upgrade, **When** installation commits, **Then** the previous authority exits,
   one successor with the staged version becomes authoritative, and no product-specific or federation
   runtime remains on the previous version.
5. **Given** an invalid or incomplete upgrade, **When** preflight or successor readiness fails, **Then**
   the current authority remains usable or is restored without publishing a mixed-version estate.
6. **Given** peers and lanes across products and hosts, **When** they discover, message, broadcast, or
   delegate, **Then** they inhabit the existing uniform multi-host space, groups provide the existing
   collaboration access boundaries, and host suffixes disambiguate peer addresses without creating
   another visibility scope.
7. **Given** an enabled service, **When** the user logs in or the daemon exits unexpectedly, **Then**
   the platform user-service manager starts or restarts it; **When** the user explicitly stops it,
   **Then** it remains stopped until the user explicitly starts it again.
8. **Given** the owning OS user invokes a documented administrative command, **When** the daemon is
   available, **Then** the user can inspect and manage all Agent Sessions-owned state; **When** a
   model-facing peer or lane lists its tools, **Then** no daemon-administration operation is exposed.

---

### User Story 2 - Keep Managed Peers Available Through Restart (Priority: P1)

Users launch and resume managed Codex, Claude, Grok, and Qwen peers through the documented unified
interfaces. One service coordinates their identities, groups, delivery, and lifecycle without
creating a separate state-owning runtime for each peer. Restarting or upgrading Agent Sessions does
not terminate the native interactive sessions, change their identities, or make previously accepted
messages disappear.

**Why this priority**: Interactive peer messaging is the primary live workflow, and reducing process
count is not valuable if service maintenance disrupts user work or weakens attestation.

**Independent Test**: Launch one managed peer for each supported product in shared and isolated groups,
exchange correlated messages, restart the user service while peers remain open, and repeat discovery,
direct send, multicast, and group broadcast. Verify exact identities, group isolation, one delivery,
and unchanged native process identities.

**Acceptance Scenarios**:

1. **Given** managed peers from all four products, **When** they publish through the user service,
   **Then** each has one exact participant identity, native session identity, selected profile,
   working directory, groups, permission state, and delivery attachment.
2. **Given** idle and busy managed peers, **When** the user service restarts, **Then** the native peers
   remain alive and recover messaging without being relaunched or renamed.
3. **Given** a message accepted before a restart, **When** service recovery completes, **Then** the
   message is delivered exactly once or remains in an explicit retryable state; it is never silently
   dropped or reported twice.
4. **Given** a bare native session with installed Agent Sessions integration, **When** it invokes an
   Agent Sessions surface, **Then** it remains inactive because installation alone does not establish
   a managed attachment or authority.
5. **Given** two products or profiles with the same display name, **When** discovery or delivery runs,
   **Then** exact product, profile, instance, and session identity prevent cross-profile selection or
   disclosure.
6. **Given** active managed peers from all four products, **When** an explicitly invoked validated
   upgrade restarts the host daemon, **Then** every native peer process and session identity remains
   unchanged, the successor generation reconstructs all four attachments, and messaging resumes
   without duplicate acceptance or delivery.

---

### User Story 3 - Run Durable Lanes Under One Authority (Priority: P1)

Parents launch, follow up, interrupt, collect, archive, and resume Codex, Claude, Grok, and Qwen lanes
through the same user service. Product-specific native behavior remains intact, while shared ownership,
groups, notices, terminal results, cleanup, and recovery have one authority and one version.

**Why this priority**: Lane managers are currently another independently versioned runtime class.
Moving them under the same authority is required to finish the unification rather than fixing only
interactive peers.

**Independent Test**: Exercise the full lifecycle for every lane product, including an active turn
during service restart and process-failure injection. Verify one durable turn decision, one terminal
result, transcript continuity, exact cleanup, and no separate long-lived Agent Sessions lane manager.

**Acceptance Scenarios**:

1. **Given** any supported managed parent, **When** it starts a lane for any supported target product,
   **Then** the user service records the exact parent context and durable turn before native execution
   becomes externally visible.
2. **Given** an accepted active turn, **When** the user service restarts, **Then** recovery either
   continues the exact native work transparently or, only for a documented native contract that
   cannot reconnect safely without brittle compatibility machinery, records one explicit interrupted
   outcome that can be collected and resumed; it never dispatches the turn twice.
3. **Given** a completed, interrupted, or failed turn, **When** collection is requested concurrently,
   **Then** one collector advances the durable cursor and every later collector receives the stable
   already-collected result.
4. **Given** normal exit, archive, manager-equivalent failure, worker failure, or service restart,
   **When** reconciliation completes, **Then** all attributable state reaches the documented terminal
   condition and unrelated native work survives.
5. **Given** a genuine product-specific permission, archive, or transcript rule, **When** the shared
   lifecycle invokes it, **Then** the native rule is preserved and explicitly reported rather than
   normalized into another product's behavior.

---

### User Story 4 - Federate Through the Same Service (Priority: P2)

An operator configures the existing hub connectivity once, and the same user service advertises local
capabilities, maintains its hub connection, routes remote peer traffic, and starts remote lanes. There
is no independently installed or restarted host federation agent that can remain on another
host-runtime version. The existing separately deployed one-hub, multiple-host-agent topology, global
groups, and host-suffixed peer names do not change.

**Why this priority**: A separate host federation-agent process recreates the same version-skew and
lifecycle failure class under another name. The one central hub is a different deployment role and is
not part of a host's runtime authority.

**Independent Test**: Connect Linux and macOS hosts, exercise peer messaging and all remote lane
products, restart each user service independently, and verify automatic reconnection, exact host and
parent identity, no duplicate delivery or lane, and no separately running federation authority.

**Acceptance Scenarios**:

1. **Given** configured remote connectivity, **When** the user service starts, **Then** it advertises
   only locally ready capabilities under the configured host identity.
2. **Given** an established remote connection, **When** either user service restarts or the machine
   sleeps and wakes, **Then** the same host identity and capabilities recover without manual startup of
   another Agent Sessions process.
3. **Given** a remote message or lane request during an outage, **When** it cannot be accepted durably,
   **Then** the caller receives an explicit retryable failure rather than false success or local
   fallback.
4. **Given** a service upgrade, **When** federation reconnects, **Then** the advertised runtime version
   matches the local service and no older connection remains authoritative.
5. **Given** several host agents connected to the one hub, **When** a direct peer/lane target or global
   group operation is requested, **Then** existing host-suffixed peer naming and global-group routing
   select the same recipients as before process unification.
6. **Given** a host upgrade whose build declares the hub's protocol version, **When** that host daemon
   restarts and reconnects, **Then** the existing hub process is not
   restarted or upgraded, accepts the new host build, and keeps every other host connection intact.
7. **Given** a host and hub installed for the same OS user and prefix but selecting unrelated
   protocol-compatible builds, **When** either role is upgraded, rolled back, removed, or reinstalled,
   **Then** the other role's selected release, process identity, service state, and readiness do not
   change and no path in the other role's release root is mutated.
8. **Given** a running central hub, **When** its owning user invokes hub status or doctor, **Then** the
   result reports exact process/service/build/protocol/listener and bounded routing-health metadata
   without host product state, message content, credentials, or remote-host lifecycle authority.

---

### User Story 5 - Establish a Clean First Installation (Priority: P2)

An operator replaces the unreleased split-runtime prototype with the unified service on an
operator-controlled host. The operator stops the old Agent Sessions processes and archives or removes
only Agent Sessions-owned prototype state before running the first install. Vendor profiles,
credentials, transcripts, and native history remain untouched.

**Why this priority**: A deliberate greenfield boundary avoids shipping a large one-use compatibility
subsystem for three controlled development hosts.

**Independent Test**: On a clean acceptance account, install the unified service and prove exactly one
daemon and endpoint become authoritative. Separately prove that vendor-owned histories remain outside
all remove and purge targets.

**Acceptance Scenarios**:

1. **Given** the operator has stopped the old stack and supplied a clean Agent Sessions state root,
   **When** the first install runs, **Then** it installs and starts one unified daemon without scanning,
   adopting, signalling, retiring, or repairing the old topology.
2. **Given** vendor profiles and transcripts survive from earlier native-client use, **When** the first
   unified install runs, **Then** those vendor-owned stores are neither migration inputs nor cleanup
   targets.
3. **Given** a unified 0.3 installation already exists, **When** it is upgraded, **Then** the ordinary
   durable release transaction preserves unified state and rolls back safely without any legacy path.

### Edge Cases

- The service crashes after an upgrade is staged but before successor readiness is committed.
- Two start requests race, or a stale service record names a recycled PID or replaced endpoint.
- A native peer starts before the user service, or a vendor-required connector loses the service
  during a tool call and reconnects later. Workflow commands must not turn that condition into
  implicit service lifecycle management.
- A service restart occurs while a peer is busy, a message is accepted but not presented, a lane turn
  is active, a result is uncollected, or an archive transaction is between revisions.
- A host has no supported native products, only one product, several profiles for one product, or all
  products at different native versions.
- A vendor process changes PID, process group, socket, cwd, transcript location, or filesystem type
  between observation and action.
- The machine sleeps, loses the network, changes hub reachability, or restarts while federation work
  is pending.
- A new service understands the installed state format but one native adapter fails readiness; other
  ready products must remain usable without advertising the failed product.
- Disk, memory, file-descriptor, process, or native-dependency exhaustion occurs before or after a
  requested message, lane turn, archive, collection, or federation operation reaches its durable
  acceptance point.
- Removal is requested while managed native sessions or lanes are live.
- A normal removal is followed by reinstall, or an explicit purge is interrupted after some
  Agent Sessions-owned metadata has been selected but before deletion commits.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Agent Sessions MUST have exactly one authoritative long-lived host runtime per OS user on
  each host across production, development, and test use. A second daemon for that user on that host
  MUST be rejected rather than treated as an isolated runtime.
- **FR-002**: The user runtime MUST be the sole authority for managed participant state, group routing,
  message delivery, product attachments, lane lifecycle, reconciliation, cleanup debt, local
  capability advertisement, and the federation connection.
- **FR-003**: The user runtime MUST expose one private local endpoint for its authoritative control,
  delivery, and adapter coordination. Product and session selection MUST occur through exact
  authenticated identity rather than additional state-owning per-product or per-session listeners.
- **FR-004**: Agent Sessions MUST NOT require a separate long-lived supervisor, per-session shim,
  product host, lane manager, local routing agent, or federation agent in the unified steady state.
- **FR-005**: A helper process required by a native vendor integration MUST be stateless with respect
  to Agent Sessions authority, MUST NOT own a durable registry or listener, MUST derive behavior from
  the current user runtime, and MUST terminate with its native session or reconnect safely after a
  runtime restart.
- **FR-006**: Native vendor executables and their credentials, transcripts, permission controls, and
  product-specific protocols MUST remain vendor-owned external resources. The user runtime MUST
  coordinate them without claiming their implementation or lifetime as Agent Sessions state.
- **FR-007**: Codex, Claude, Grok, and Qwen MUST use the same shared host runtime authority while retaining
  distinct native adapter rules where their supported contracts genuinely differ.
- **FR-008**: Every managed attachment MUST be authorized from exact, corroborated process, native
  session, product, profile, working-directory, launch, and runtime evidence. Model-supplied names or
  identifiers MUST NOT create authority.
- **FR-009**: Bare native sessions MUST remain an intentional opt-out even when Agent Sessions plugins
  or commands are installed and visible.
- **FR-010**: Discovery, direct send, explicit multicast, named-group broadcast, delivery, and reply
  MUST preserve current group authorization and neutral message-provenance rules across runtime
  restart and upgrade.
- **FR-011**: The runtime MUST attribute and address all state by exact product, native profile,
  managed instance, session, and host identity so equal display names or paths cannot cross-select
  another attachment. Those identities MUST NOT create a collaboration visibility boundary; group
  membership remains the access rule.
- **FR-012**: Operators MUST be able to install, enable, start, stop, restart, disable, and inspect the
  one runtime through a standard systemd user service on Linux and a standard launchd user agent
  controlled through `launchctl` on macOS, without administrative access.
- **FR-013**: Installation and upgrade MUST validate a complete successor before changing authority,
  perform exactly one validated daemon restart and at most one committed authority transition, and
  either publish the exact staged version or leave/restore the previous usable version. This explicit
  administrative transaction is the only non-service-control operation permitted to manage daemon
  lifetime. Host and hub installation, upgrade, rollback, removal, and service-manager operations MUST
  use the same shared functional implementations with role-specific descriptors and readiness hooks;
  code reuse MUST NOT couple their independently invoked deployment lifecycles. Host and hub MUST use
  separate role-owned release roots, current-release selections, locks, journals, service transitions,
  rollback decisions, and removal/purge ownership.
- **FR-014**: After the unified runtime is installed, routine restart and upgrade MUST NOT require
  users to close supported managed native interactive sessions and MUST NOT terminate those native
  sessions solely because the Agent Sessions service exits.
- **FR-015**: Calls attempted while the runtime is unavailable MUST receive an explicit unavailable or
  retryable outcome unless the operation was already durably accepted. No caller may receive success
  for work that neither the old nor new authority owns.
- **FR-016**: A message durably accepted before restart MUST be delivered at most once and remain
  observable until delivered, rejected, or returned as retryable debt.
- **FR-017**: A lane turn MUST commit its exact parent, groups, permissions, target, native identity,
  and dispatch state before external execution is treated as accepted.
- **FR-018**: Runtime restart during a lane turn MUST transparently continue the exact accepted work
  when the native product exposes a safe supported reconnection contract. An adapter MAY instead
  produce one explicit interrupted, collectable, resumable outcome only when recorded native evidence
  proves safe reconnection is unavailable and transparent continuation would require unsupported
  interception or brittle compatibility machinery. Recovery MUST NOT duplicate dispatch, terminal
  notification, collection, archive, or cleanup.
- **FR-019**: Product-specific lane permission, cancellation, transcript, archive, resume, and worker
  behavior MUST remain explicit adapter contracts while shared ownership and lifecycle behavior has
  one implementation.
- **FR-020**: The runtime MUST own the configured federation connection, local capability advertisement,
  remote message routing, remote lane dispatch, result notification, and reconnection; these MUST NOT
  depend on a separately operated host-side federation authority. The remote central hub remains the
  established independent network service.
- **FR-021**: Federation recovery MUST retain exact host identity, reject hub-protocol mismatches,
  advertise only ready local products, and prevent duplicate connection authority, delivery, or lane
  creation. Hub and host release/build identities MAY differ arbitrarily. Software-version
  interoperability MUST be decided only by exact hub-protocol-version equality during handshake,
  never by release string, commit identity, packaging identity, relative age, or best-effort fallback.
  Advertised host capabilities
  select which remote lane operations that host can accept; they MUST NOT require release equality or
  turn a protocol-matching connection into a coordinated deployment. Host-daemon and
  central-hub installation, upgrade, restart, rollback, and service lifetime MUST remain independent:
  a build change that retains the protocol version MUST NOT require or trigger lifecycle work on the
  other side; a protocol-version mismatch MUST fail closed and name the required matching version.
- **FR-022**: Version 0.3 MUST define a greenfield Agent Sessions-owned state and service boundary. It
  MUST NOT inventory, interpret, adopt, drain, retire, repair, or otherwise interoperate with state,
  processes, endpoints, jobs, or release layouts from pre-unification prototypes.
- **FR-023**: Before first installation, the operator MUST stop all pre-unification Agent Sessions
  processes and services and remove or archive their Agent Sessions-owned state and install roots.
  This is an installation prerequisite, not an automated lifecycle implemented by the product.
- **FR-024**: The first unified install MUST use the same role-owned release, connector, service, and
  readiness transaction as later installs. It MUST NOT contain a hidden compatibility or fallback path.
- **FR-025**: Vendor credentials, profiles, settings, transcripts, and native history MUST remain
  outside the greenfield reset and MUST NOT be read, copied, migrated, deleted, or rewritten.
- **FR-026**: Once a unified 0.3 state root exists, steady-state restart, upgrade, rollback, removal,
  purge planning, and recovery MUST use only the unified schemas and exact current authority.
- **FR-027**: Runtime state MUST support crash-safe restart, exact revision checks, idempotent recovery,
  and explicit schema compatibility. An incompatible state version MUST fail before mutation with an
  actionable diagnostic.
- **FR-028**: Missing, unauthenticated, unsupported, or temporarily unready native products MUST be
  isolated to their adapters and MUST NOT prevent the runtime from serving other ready products.
- **FR-029**: The runtime and installer MUST NOT copy, print, hash, broaden, or mutate native
  credentials or owner-wide permission settings. Diagnostics MUST expose only non-secret readiness
  and store metadata.
- **FR-030**: Status and doctor output MUST report the exact runtime version and process identity,
  endpoint identity, active products/profiles, managed attachments, lanes, federation destinations,
  and cleanup debt in stable human-readable and machine-readable forms.
  Central-hub status and doctor MUST use the same shared diagnostic envelope while reporting only the
  hub's exact process/service/build/protocol/listener, bounded routing health, and lifecycle debt.
- **FR-031**: Every process signal, endpoint retirement, state adoption, and destructive cleanup MUST
  be authorized immediately beforehand by exact current identity and MUST preserve unrelated native
  processes, transcripts, settings, credentials, and Agent Sessions instances.
- **FR-032**: Normal exit, interrupt, runtime crash, machine restart, network loss, partial upgrade,
  and repeated recovery MUST converge to the same documented terminal state without manual registry
  editing.
- **FR-033**: The unified contracts, packages, service controls, restart, and complete
  four-product behavior MUST pass equivalent real-installation acceptance on Linux and macOS.
- **FR-034**: Release packages and aggregate installation MUST support zero, one, several, or all
  native products without treating absent optional products as installation failures. Every platform
  archive MUST contain exactly the `agent-sessions` host executable and the `agent-sessions-hub`
  central-hub executable described by that archive's manifest. Host installation MUST enable only the
  host user service; hub installation MUST be a separate explicit deployment action. A deployed host
  and hub MUST both be implementations built from this repository, but MAY come from arbitrary,
  independently built archives and unrelated commits under FR-021 when their hub-protocol versions
  are equal.
- **FR-035**: User and operator documentation MUST describe the one-service lifecycle, product
  boundaries, greenfield first-install prerequisites, restart behavior, diagnostics, failure recovery,
  optional-product installation, and removal behavior in the same release as the implementation.
  The canonical command surface MUST have one authoritative descriptor inventory covering every
  command mode, installed alias, parsed Agent Sessions option, environment variable, stable JSON
  field, and exit-status class. Every parsed option MUST appear in the corresponding generated help,
  and checked documentation, help, parsing, and dispatch MUST fail tests when they diverge. The
  authoritative hub-protocol descriptor, this feature's federation protocol contract, and checked
  `docs/FEDERATION.md` MUST describe the same version, handshake, bounds, frames, capability tokens,
  and mismatch behavior, with a failing parity test on divergence.
- **FR-036**: Removal requested while any managed peer or lane is active MUST fail closed, name every
  exact blocker, and change nothing. After every managed attachment and lane is quiescent, removal
  MUST retire the exact service, installed binaries, integrations, and disposable runtime artifacts
  while preserving Agent Sessions-owned configuration and durable metadata for reinstall. It MUST NOT
  terminate or delete native sessions, credentials, profiles, or transcripts. `remove-hub` MUST stop
  and remove only the exact hub service, selected binary, immutable releases, and disposable runtime
  artifacts; it MUST leave remote hosts running and preserve hub configuration and durable metadata.
  Deleting preserved hub state MUST require a separate revision-bound `purge-hub` operation.
- **FR-037**: Backward compatibility with pre-unification peer, lane, messaging, plugin, MCP,
  host-agent, federation-command, and service-control interfaces is NOT required. A public or local
  interface MAY be replaced when that materially simplifies the single-authority design, provided the
  replacement preserves identity, authorization, fail-closed safety, lifecycle, and native-product
  boundaries and ships with the explicit clean-install prerequisite. This permission does not
  create cross-build coupling in the new architecture: unified host agents and hubs interoperate under
  FR-021 solely when their hub-protocol versions are equal.
- **FR-038**: The daemon MUST be user-managed. Peer, lane, messaging, plugin, connector, and
  federation workflow commands MUST NOT start, stop, restart, replace, or supervise its lifetime.
  When it is unavailable, they MUST fail with a cause-specific diagnostic and no service mutation.
  Explicit install, upgrade, quiescent removal, and direct user-service administration are the only
  lifecycle-management authorities.
- **FR-039**: All managed peers and lanes MUST remain in the existing uniform multi-host space behind
  one hub. Existing global groups MUST remain the sole collaboration visibility and access boundary.
  Product, profile, instance, session, and host identities MUST be used only for exact attribution,
  addressing, routing, native lifecycle, and cleanup; test-owned resource identities MUST be used
  only for exact mutation and cleanup ownership. No additional collaboration access or isolation
  model may be introduced.
- **FR-040**: Installation MUST enable automatic start at user login and automatic restart after an
  unexpected daemon exit on both supported platforms. An explicit user stop or disable MUST suppress
  automatic restart until the user explicitly starts or enables the service. Platform-specific
  service-manager activation, environment, socket, sleep/wake, logout, and stale-job behavior MUST be
  covered by the same lifecycle contract rather than hidden by wrapper commands.
- **FR-041**: Process unification MUST preserve the existing federation topology and semantics: one
  hub, multiple host agents, global groups, and host-suffixed peer names. It MUST NOT change that
  topology or add access, naming, routing, or fallback behavior. The central hub MUST run only as the
  separate `agent-sessions-hub` binary; each host-agent role MUST remain embedded in that host's one
  `agent-sessions` daemon.
- **FR-042**: Deleting preserved host- or hub-owned Agent Sessions configuration or durable metadata
  MUST require a separate explicit purge operation after quiescence. Purge and `purge-hub` MUST
  enumerate their exact Agent Sessions
  targets, require current ownership and revision checks, remain idempotent after interruption, and
  exclude all vendor-owned credentials, profiles, transcripts, and native session data.
- **FR-043**: The owning OS user identity MUST be the sole administrative authorization boundary for
  daemon-wide inspection and management; the system MUST NOT introduce a second administrator
  credential or pretend to distinguish a human from other same-user code. Model-facing MCP peer and
  lane surfaces MUST expose no daemon-administration operations and MUST retain their attested
  session, group, product, and parent-context restrictions.
- **FR-044**: The daemon MUST NOT impose Agent Sessions-specific fixed or configurable quotas on
  messages, peers, lanes, or pending work. An operation MUST report acceptance only after its exact
  durable ownership commits; when an actual OS resource or native dependency prevents that commit,
  the operation MUST fail before acceptance with the specific non-secret cause and MUST NOT discard
  previously accepted work.
- **FR-045**: Daemon logs, service-manager output, status, doctor, metrics, traces, crash reports, and
  debug diagnostics MUST NOT contain peer-message payloads, prompts, lane-result content, tool
  arguments or results containing user content, or vendor transcript content. Operational
  observability MUST be limited to bounded non-secret identities, states, counts, timings, revisions,
  and causes. Debug mode MUST NOT weaken this rule.
- **FR-046**: This feature MUST only converge the existing Agent Sessions functionality by changing
  process topology, version ownership, service lifecycle, and internal responsibility placement.
  Existing peer, lane, group, messaging, permission, native-profile, one-hub/multiple-agent
  federation, archive, collection, cleanup, and resume behavior is the functional baseline. A public
  interface MAY change under FR-037 only to remove or simplify obsolete process boundaries, not to
  invent new product behavior. Separating the central hub into `agent-sessions-hub` is a deployment-role
  boundary, not another per-user host authority or a new collaboration namespace. No compatibility
  with a pre-unification executable or process topology is required.

### Key Entities

- **Host Runtime Authority**: The single current host-side Agent Sessions identity for one OS user, including
  version, exact process identity, private endpoint, state revision, service health, and generation.
- **Managed Native Attachment**: The exact relationship between the host runtime and one managed
  native session, including product, profile, instance, session, process, working directory,
  permissions, groups, delivery state, and reconnect status.
- **Lane Transaction**: One durable parent-authorized unit of delegated work, including target,
  native identity, dispatch state, terminal outcome, collection cursor, archive state, and cleanup
  debt.
- **Federation Connection**: The configured relationship between the user runtime and a remote hub,
  including hub address/identity, federation protocol version, local host identity, current
  capabilities, connection generation, and retry state. Host and hub build identities are reported by
  their respective local services and are not software-version interoperability inputs.
- **Lifecycle Debt**: Durable evidence of an operation that cannot complete safely yet, including its
  exact blocker, last observation, required retry predicate, and prohibited cleanup scope.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After clean install, restart, and successful upgrade, each
  production host user has exactly one current Agent Sessions host runtime authority and one authoritative
  private endpoint, with zero obsolete supervisors, shims, product managers, routing agents, or
  federation agents at return and 1, 5, 10, and 30 seconds later.
- **SC-002**: One host runtime supports at least 100 simultaneous managed native attachments across all
  four products and concurrent production, development, and test work without creating another
  long-lived Agent Sessions authority or authoritative listener; exact identities address every
  attachment and existing global groups produce the same access decisions as before unification.
- **SC-003**: With one live managed peer from each product, a routine runtime restart preserves all
  four native process identities and restores authorized local discovery and messaging within 10
  seconds, with zero lost accepted messages and zero duplicate deliveries.
- **SC-004**: Across the complete 16 parent-target lane matrix, restarting the runtime during active
  work transparently continues every adapter with a supported safe reconnection contract. Every
  evidence-approved exception produces exactly one explicit interrupted, collectable, resumable
  outcome, with zero duplicate native dispatches or terminal notifications.
- **SC-005**: Linux-to-macOS and macOS-to-Linux peer and lane traffic recovers within 30 seconds of
  either runtime restarting, retaining the same host and parent identities with zero local fallback
  and zero duplicate remote work. The matrix includes hub and host builds from unrelated SHAs and
  releases with the same hub-protocol version, which interoperate, and one mismatched protocol version, which is
  rejected before registration, delivery, or lane acceptance.
- **SC-006**: A clean acceptance account installs the first unified daemon without creating or
  invoking any legacy inventory, adoption, retirement, shim, supervisor, manager, or host-agent path;
  vendor-owned profiles and native histories remain unchanged.
- **SC-007**: Every injected failure before the authority transition leaves the previous version
  usable, and every injected failure after transition either completes recovery or restores the
  previous version without a mixed-version authoritative estate.
- **SC-008**: In 100% of bare-session coexistence and restart tests, bare native sessions gain no
  managed identity or authority, and monitored credential and owner-permission stores receive zero
  Agent Sessions writes.
- **SC-009**: Normal tests, race tests, vet, repository-managed lint, all supported package builds,
  both executable builds, and the applicable live service, peer, lane, and federation cells
  pass on real Linux and macOS installations with no platform waiver. Every binary used as release
  evidence has its exact source/build identity recorded and verified. Network participants are NOT
  required to share a commit, release, or build identity; the federation matrix establishes their
  software-version interoperability solely by equal hub-protocol version under SC-005.
- **SC-010**: For every unavailable product, identity conflict, incompatible
  state, failed successor, and federation outage, operators receive a cause-specific diagnostic and
  supported next action without inspecting or manually editing internal state.
- **SC-011**: Aggregate installation succeeds with every tested availability combination from zero
  through all four native products, always starting the same one user runtime and advertising only
  capabilities proven ready.
- **SC-012**: On both Linux and macOS, login starts one enabled host daemon, an unexpected exit restores it
  within 10 seconds, an explicit stop keeps it absent for at least 30 seconds without automatic
  resurrection, and a subsequent explicit start restores the same configured profiles, groups, hub
  connection, managed attachments, and lifecycle debt.
- **SC-013**: With at least three host agents connected to the one hub, the complete existing global
  group, host-suffixed peer discovery, messaging, lane, and collection acceptance matrix returns the
  same recipients, routes, and results before and after process unification. Process census finds one
  `agent-sessions-hub` at the central deployment and no standalone federation-agent process on a host.
- **SC-014**: After normal host or hub removal and reinstall, preserved Agent Sessions groups, hub
  configuration, lane ownership/status, collection cursors, and cleanup debt remain available. An
  explicit host purge or `purge-hub` deletes 100% of its enumerated Agent Sessions targets and 0
  vendor-owned credential, profile, transcript, native-session, or remote-host artifacts.
- **SC-015**: The owning OS user can inspect and manage 100% of Agent Sessions-owned daemon state
  through administrative commands, while completeness tests find zero daemon-administration
  operations in every model-facing MCP tool inventory. The same completeness gate finds 100% of
  parsed Agent Sessions options in the corresponding help output and zero undocumented command modes,
  installed aliases, environment variables, stable JSON fields, or exit-status classes.
- **SC-016**: Under injected disk, memory, file-descriptor, process, and native-dependency failures,
  100% of operations that cannot commit return a non-success result before acceptance, every already
  accepted operation remains durable, and zero queued items are silently dropped or replaced.
- **SC-017**: Automated secret/content canaries across normal, debug, crash, service-manager, doctor,
  metric, and trace outputs find zero peer-message, prompt, lane-result, tool-content, credential, or
  vendor-transcript bytes while retaining the exact metadata needed to diagnose every acceptance
  failure class in this specification.
- **SC-018**: Before implementation changes behavior, acceptance records a closed, individually named
  inventory of every pre-unification functional cell for peers, lanes, global groups, host-suffixed
  names, one-hub/multiple-agent federation, permissions, archive, collection, cleanup, and resume.
  Every inventoried cell passes after unification with unchanged functional assertions except for
  assertions that deliberately verify the new process and service topology; no cell receives credit
  through an aggregate pass alone.

## Assumptions

- One OS user always has one Agent Sessions host daemon. Test and development harnesses obtain isolation
  for collaboration through existing groups and use exact native/profile/instance/host and test-owned
  resource identities only for attribution, routing, lifecycle, mutation, and cleanup. They do not
  start additional daemon processes or create another access model.
- The separately deployed central `agent-sessions-hub` is not a host runtime authority. It may run
  under a dedicated service account or on a host that also has a user daemon without acquiring local
  product, attachment, lane, credential, transcript, or service-lifecycle authority.
- The operator controls the three unreleased deployments and will stop the old Agent Sessions stack,
  then remove or archive only its Agent Sessions-owned state and installation roots before first
  unified installation. No automated compatibility or migration path is required.
- Native interactive sessions can outlive an Agent Sessions service restart. Active
  delegated work is expected to continue transparently; an explicit interrupted and resumable
  outcome is an evidence-gated native limitation, not the default implementation shortcut.
- Native clients, their App Servers or protocol workers, and vendor-mandated session connectors remain
  external processes. Only Agent Sessions-owned state and long-lived authority are unified.
- Native peer and lane transcripts are vendor-owned history, not Agent Sessions metadata. Normal
  removal, explicit metadata purge, and cleanup never delete or rewrite them.
- Operational observability is not an authoritative content store. Any content that must be retained
  for accepted delivery or native execution remains within its bounded durable operation or owning
  native product and is excluded from logs and diagnostics.
- Existing identity, group, permission, neutral message framing, archive, cleanup, and federation
  security contracts remain authoritative unless this specification strengthens them. Their current
  command and plugin representations are not compatibility constraints.
- Code already running with the owning OS user's unrestricted authority is inside the daemon's
  administrative trust boundary. Scoped model-facing tools prevent ambient capability exposure; they
  are not claimed as a security boundary against arbitrary same-user code.
- All connected hosts and the hub speak the same hub-protocol version. Their Agent Sessions
  releases, commits, and upgrade times may differ indefinitely while that protocol version remains
  unchanged.
- Standard systemd user services are available on Linux, and standard launchd user agents controlled
  through `launchctl` are available on macOS. Windows support is out of scope.
- Redesigning native vendor protocols, credential stores, model behavior, or federation behavior is
  out of scope. The hub's executable and service packaging change, but its established one-hub routing
  role does not. The implementation declares one explicit hub-protocol version for software-version
  interoperability. Existing identity, routing, group, and operation authorization checks remain
  independent of that version decision.
- Existing Agent Sessions behavior on the merged `develop` baseline is the functional authority for
  this feature. The specification does not add federation topologies, naming systems, group models,
  policy controls, quotas, or other user features unrelated to consolidating Agent Sessions-owned
  binaries and long-lived responsibilities.
