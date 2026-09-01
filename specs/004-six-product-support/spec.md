# Feature Specification: Six-Product Symmetric Support

**Feature Branch**: `feature/six-product-support`

**Created**: 2026-09-01

**Status**: Draft — joint Codex/Fable architecture converged; truth spikes precede interface freeze

**Input**: Add fully symmetric Agent Sessions support for OpenCode, KiloCode,
Pi Coding Agent, Oh My Pi, CodeBuddy, and DeepSeek Harness, with shared
implementations wherever the native contracts genuinely match.

## User Scenarios & Testing

### User Story 1 - Use Any New Product as a Managed Peer (Priority: P1)

A developer starts any of the six products through its `*-peer` command and
gets the same Agent Sessions experience as with Claude, Codex, Grok, or Qwen:
the interactive session is attested, discoverable within its groups, can send
messages, receives messages while busy, and wakes into a model turn when an
idle message arrives. Native rename and resume keep the same external identity.

**Why this priority**: Interactive peer communication and wakeup are the core
distinction between Agent Sessions support and a headless process wrapper.

**Independent Test**: For each product, launch a real managed TUI in an
isolated profile, discover it from another grouped peer, exchange messages in
both directions, deliver during a slow turn and while idle, rename it, restart
or resume it, and verify that the same native session is still managed without
terminal scraping.

**Acceptance Scenarios**:

1. **Given** a managed interactive session sharing a group with a sender,
   **when** the sender delivers a message while the session is idle, **then**
   the message renders in that session and starts a model turn without user
   keystrokes.
2. **Given** a managed interactive session is already running a turn, **when**
   another message arrives, **then** the product either steers the current turn
   or durably queues the message, and the sender receives a truthful receipt.
3. **Given** the user renames and later resumes a native session, **when** peers
   list it, **then** Agent Sessions reports the updated name and the same
   attested native session identity.
4. **Given** a product was launched without its managed wrapper or installed
   component, **when** Agent Sessions inspects it, **then** it remains an
   unmanaged opt-out and receives no ambient managed capability.

---

### User Story 2 - Run Durable Local or Remote Lanes (Priority: P1)

An orchestrator starts, steers or queues input to, waits for, interrupts,
resumes, collects, and archives a lane for any new product through the same MCP
and `*-peer-lane` lifecycle used by the existing four products. The same
commands work against an eligible federated host.

**Why this priority**: Durable detached work is the second half of product
symmetry and is required for cross-host orchestration.

**Independent Test**: For each product, run the complete lifecycle locally and
through a hub, including a slow turn, busy input, daemon restart, exact resume,
collection debt, interrupt, and archive. Inspect durable state and prove no
unowned process, server, socket, credential, or worktree is changed.

**Acceptance Scenarios**:

1. **Given** a doctor-ready product, **when** a lane is started, **then** its
   durable Agent Sessions row commits before native dispatch and the returned
   native session ID remains immutable.
2. **Given** a running lane whose native protocol supports steering, **when**
   input arrives, **then** the adapter steers it and records the exact native
   acceptance. If steering is unsupported, the input is durably queued for the
   next turn.
3. **Given** the daemon restarts, **when** the adapter can re-open the exact
   native session, **then** work resumes without changing native identity. If
   native acceptance is ambiguous, Agent Sessions exposes debt and never
   blindly replays the input.
4. **Given** a compatible remote host advertises the product's lane
   capability, **when** a grouped parent starts a remote lane, **then** the
   destination applies the same identity, group, permission, lifecycle, and
   terminal-notice rules as a local lane.
5. **Given** a user or third-party automation prefers the CLI, **when** it uses
   a new `*-peer-lane` alias, **then** it receives the same daemon-owned
   lifecycle contract as the MCP control surface.

---

### User Story 3 - Orchestrate From Any New Product (Priority: P1)

The user's interactive session in any new product can discover peers, send
messages, start and manage lanes, and receive lane terminal notices through a
shipped product-native tool, plugin, extension, or narrowly scoped MCP
connector whose identity is independently attested.

**Why this priority**: Symmetry requires every supported product to be both a
worker and an orchestrator; a lane-only adapter is not full support.

**Independent Test**: From each real managed TUI, invoke the shipped Agent
Sessions tool to list peers, send a message, start a lane of another product,
collect the result, and receive the terminal notice. Verify the tool call's
native session identity using process ancestry and product-native evidence,
not a model-supplied ID.

**Acceptance Scenarios**:

1. **Given** a managed parent session, **when** its Agent Sessions tool calls
   the daemon, **then** authorization binds the call to the exact attachment
   and native session.
2. **Given** the parent starts a child lane, **when** the child terminates,
   **then** the notice renders back in the same visible parent session.
3. **Given** a model supplies a false session ID or a shared MCP process lacks
   per-session evidence, **when** it requests a privileged action, **then** the
   request fails closed.

---

### User Story 4 - Install, Diagnose, and Upgrade Consistently (Priority: P2)

An operator installs one Agent Sessions release and receives every applicable
integration, wrapper alias, skill, component, and service transactionally.
Absent native products are skipped clearly. `doctor`, `roster`, and release
evidence explain readiness and version drift without exposing credentials.

**Why this priority**: Ten products cannot remain operable if installation,
packaging, help, and readiness are maintained as independent product lists.

**Independent Test**: Build a release, install it into isolated Linux and
macOS homes containing different subsets of the ten products, validate the
derived inventory and all readiness probes, upgrade and roll back, then remove
only integration-owned artifacts.

**Acceptance Scenarios**:

1. **Given** one or more native products are absent, **when** Agent Sessions is
   installed, **then** applicable integrations succeed and absent products are
   reported as skipped rather than causing a partial host install.
2. **Given** a native API or exact alpha tuple differs from the tested
   contract, **when** doctor runs, **then** the product is fail-closed with a
   specific capability/version diagnostic and its federation capability is
   not advertised.
3. **Given** installation fails after changing a native registration, **when**
   rollback runs, **then** the exact prior registration and host release are
   restored without altering credentials, profiles, or unrelated plugins.
4. **Given** product metadata changes, **when** CI derives release inventory,
   wrappers, acceptance cells, and documentation projections, **then** drift
   from the single authored product catalog fails the build.

---

### User Story 5 - Recover Safely From Crashes and Partial Native Writes (Priority: P2)

Operators can trust that daemon, worker, component, product server, and machine-local
restart paths converge to an explicit state. Accepted input is never lost,
duplicated, or silently replayed, and cleanup never targets unrelated native
state.

**Why this priority**: Six additional transports multiply the number of crash
boundaries; shared durable semantics are required before product fan-out.

**Independent Test**: Inject a crash at every input-ledger transition and at
component/server/process reconnect boundaries. Verify exact recovery,
idempotency, ambiguity reporting, bounded spool cleanup, and zero collateral.

**Acceptance Scenarios**:

1. **Given** input content is durably written but its catalog receipt has not
   committed, **when** the daemon restarts, **then** the orphan is safely
   collected and was never acknowledged to the caller.
2. **Given** native I/O may have succeeded but its acknowledgment was not
   committed, **when** recovery runs, **then** the receipt becomes ambiguous
   unless the native protocol proves idempotent replay.
3. **Given** a component reconnects after a daemon generation change, **when**
   kernel peer identity, process start, ancestry, or durable evidence differs,
   **then** the reconnect is rejected.
4. **Given** a stale CodeBuddy registry row names a dead TUI or recycled port,
   **when** the daemon tries to deliver, **then** socket-to-PID, executable, and
   ancestry re-attestation rejects the row without contacting another process.

### Edge Cases

- A Kilo TUI attaches to the wrong local server, or two Kilo TUIs share a
  server whose `/tui/*` routes cannot target one exact session.
- A DSH profile contains a mismatched CLI, ACP bundle, or Agent Sessions
  Cordis plugin version; another process concurrently resumes the same DSH
  session; ACP rejects a busy prompt; cancel is sent as a request instead of a
  notification; the sandbox masks `/tmp`.
- A CodeBuddy worker registry row is stale, its literal-loopback port is owned
  by another process, or two live workers claim the same native session.
- A Pi or OMP RPC frame exceeds bounds, arrives out of order, omits the ready
  protocol version, or the process exits between native acceptance and durable
  acknowledgment.
- An OpenCode or Kilo event stream disconnects, replays events, reports idle
  while another queued prompt exists, or changes a documented route in a new
  release.
- A component announces a model-supplied session ID that conflicts with the
  product-native session context or wrapper bootstrap capability.
- The durable input spool is full, corrupt, symlinked, changed after receipt,
  or references content missing from disk.
- An N+1 federation participant reaches an N hub; the handshake must reject it
  before registration without damaging the incumbent roster or misrouting.
- A product cannot represent the requested permission mode without widening
  it; launch must fail instead of silently choosing a broader mode.
- Native rename occurs during delivery or daemon restart; external naming must
  converge without changing the immutable session identity.
- Installation encounters a user-modified integration file or registration;
  it records debt or preserves the file rather than overwriting/removing it.

## Requirements

### Functional Requirements

- **FR-001**: The system MUST add product descriptors, managed peer aliases,
  lane aliases, readiness probes, install assets, and federation lane
  capabilities for `opencode`, `kilo`, `pi`, `omp`, `codebuddy`, and `dsh`.
- **FR-002**: Full support for each product MUST satisfy the PEER, LANE, and
  PARENT acceptance contracts; spawn-and-own headless control alone MUST NOT be
  described as full support.
- **FR-003**: Managed peers MUST support externally delivered idle wake, busy
  steer or durable queue, outbound messaging, visible terminal notices,
  native-session persistence across resume, and externally reflected rename
  without TTY keystroke or output scraping.
- **FR-004**: Lanes MUST support doctor, list, start, run, resume, wait, status,
  interrupt, archive, and local/federated execution through
  both MCP and `*-peer-lane` CLI surfaces.
- **FR-005**: Parent operations MUST be authorized by independently attested
  product-native identity plus exact managed attachment evidence; model-supplied
  IDs and daemon-wide shared MCP identity MUST be insufficient.
- **FR-006**: All products MUST reuse the existing AgentFrame, group admission,
  live delivery and lane-routing semantics
  unless this specification explicitly strengthens a shared contract.
- **FR-007**: One data-only product catalog MUST author stable product identity,
  command aliases, native executable names, capabilities, tested versions,
  install projections, and acceptance metadata. Runtime behavior MUST be
  supplied by one explicit capability registry and composition root.
- **FR-008**: Product dispatch outside product packages and the explicit
  composition root MUST be table/capability driven; package `init` registration
  and new product-name switches are prohibited.
- **FR-009**: The runtime product contract MUST expose optional peer,
  messaging, lane, parent-attestation, and doctor capabilities and MUST include
  a declared optional mid-turn steer operation before product implementation
  begins.
- **FR-010**: Runtime drivers MAY retain ephemeral clients and connections
  keyed by durable native references, but persisted references MUST contain no
  bearer credential, local endpoint, worker password, or other recoverable
  secret and MUST be reconstructible through an explicit recovery operation.
- **FR-011**: Permission mapping MUST be product-specific, documented, and
  fail-closed. No adapter MAY silently widen the requested sandbox, approval,
  or tool policy.
- **FR-012**: Input accepted while a lane is busy MUST be represented by a
  durable bounded receipt ledger. Input bodies MUST live in a separate private
  bounded spool, not in the daemon catalog.
- **FR-013**: The input transaction order MUST be content-spool write, durable
  receipt commit, then caller acknowledgment. Native dispatch intent MUST
  commit before I/O; an unproven post-I/O state MUST become explicit ambiguity
  and MUST NOT be blindly replayed.
- **FR-014**: A driver that supports native steering MUST record its exact
  native acceptance; a typed unsupported result MUST cause the shared lane
  engine to queue the receipt for the next turn.
- **FR-015**: Long-lived in-process integrations MUST use a dedicated bounded
  component stream endpoint separate from the one-request daemon control
  socket. Initial attachment MUST require a one-time wrapper bootstrap
  capability plus kernel peer identity, process start, and ancestry checks.
- **FR-016**: Component reconnect after a daemon generation change MUST
  re-attest kernel/process evidence against durable attachment evidence. The
  same-UID trust model MUST NOT add an asymmetric-key protocol without a named
  adversary it actually mitigates.
- **FR-017**: Shared transport code MUST be limited to genuine common mechanics:
  bounded local component framing, supervised structured child processes, and
  authenticated literal-loopback HTTP/event handling. Pi/OMP RPC, DSH ACP,
  OpenCode/Kilo semantics, and CodeBuddy semantics MUST remain typed clients
  above those mechanics.
- **FR-018**: OpenCode and Kilo MAY share only verified server/plugin behavior;
  Kilo's `/tui/*` peer path and OpenCode's session prompt path MUST remain
  explicit product differences. Kilo peer mode MUST use one authenticated
  server plus one full attach TUI per managed peer; `attach --mini` MUST NOT be
  advertised or accepted as a managed peer surface.
- **FR-019**: Pi and OMP MUST share one extension/RPC core with an explicit
  quirk table for runtime, session paths, native environment, permission, and
  steering differences.
- **FR-020**: CodeBuddy peer and lane surfaces MUST remain distinct. A managed
  peer MUST use the product-owned interactive registry/endpoint, treat
  `X-CodeBuddy-Request: 1` as a constant CSRF header rather than authentication,
  and re-attest registry session/PID/URL plus listening-socket ownership,
  executable, and ancestry before first use and after restart. A lane MUST use
  an Agent Sessions-owned authenticated server whose generated secret remains
  memory-only. Model-turn GA acceptance MAY remain pending only because a
  Tencent account is unavailable; all other implementation and offline
  protocol evidence MUST ship.
- **FR-021**: DSH support MUST bind the CLI, ACP bundle, Agent Sessions Cordis
  plugin, and profile to the exact tested `0.1.2-alpha.3` tuple; require pnpm;
  enforce one Agent Sessions owner per native session; treat busy ACP prompt
  rejection as queueing; send cancel as a notification; and never use the lazy
  projection cache as a liveness signal. Its component socket MUST live below
  an Agent Sessions-owned HOME/XDG path and MUST NOT use sandbox-masked `/tmp`.
- **FR-022**: Federation protocol 4 MUST be one uniform wire contract. The
  explicit handshake version MUST match exactly and every mismatch, including
  N+1 against N, MUST fail before registration. The hub MUST bound, validate,
  deduplicate, and pass through exactly one explicit opaque lane capability;
  the destination product registry MUST make the support decision.
- **FR-023**: Every accepted federation client MUST receive the same complete
  roster. The protocol MUST NOT contain a binary-compatibility marker,
  per-client product filter, empty-capability product inference, downgrade, or
  partial admission path. A hello registers immediately, a same-host reconnect
  replaces the old socket, and a snapshot directly replaces the live peer map.
- **FR-024**: The existing trusted-network federation assumption MUST be stated
  explicitly. TLS/authentication and Windows support are outside this feature.
- **FR-025**: Installation and removal MUST derive deterministic plans from
  the single product catalog, apply native registrations transactionally, and
  preserve credentials, profiles, and user-modified or unrelated plugins.
- **FR-026**: Release inventory, aliases, payloads, help, doctor projection,
  acceptance matrices, and user documentation MUST be generated or validated
  against that same catalog; shell scripts MUST NOT retain a second product
  list.
- **FR-027**: Truth spikes for Kilo exact routing/attach parity, DSH Cordis and
  tuple behavior, CodeBuddy registry restart/stale-row/cross-target isolation,
  shared Pi/OMP and OpenCode
  component identity, catalog projection, federation decoding, and legacy
  reachability MUST complete before runtime interfaces freeze.
- **FR-028**: No new product code may be added to unreachable legacy bridge or
  federator paths. Live helpers MUST move to focused packages, the duplicate
  product catalog MUST collapse, and the normative adapter protocol document
  MUST describe the unified daemon/component contract. Full dead-tree deletion
  is deferred unless the reachability spike proves a bounded safe excision.
- **FR-029**: The current four products MUST migrate behind or be wrapped by
  the shared runtime contracts without losing any existing peer, lane,
  permission, recovery, installation, or federation behavior.
- **FR-030**: Hub tests MUST exercise the live `internal/federation` hub rather
  than a legacy implementation and MUST include hostile malformed-frame fuzz,
  exact mismatch rejection, identical complete rosters, opaque capability,
  reconnect/resend, and graceful shutdown-drain cases.
- **FR-031**: macOS host and hub services MUST receive the same configured
  product and federation environment as Linux services, subject to platform
  path normalization and AF_UNIX limits.
- **FR-032**: Every supported product MUST have focused contract tests,
  keyless or isolated mock-provider integration tests where possible, and real
  Linux and macOS acceptance evidence at the exact tested native version.
- **FR-033**: CodeBuddy federation capability advertisement MUST remain
  disabled or explicitly experimental until its Tencent-authenticated
  model-turn acceptance cell passes. No other product may be advertised when
  doctor is not ready.
- **FR-034**: Operator and user documentation MUST explain what each product
  integration enables, how to install and configure it, its permission and
  sandbox differences, version requirements, failure modes, and local/cross-host
  workflows.
- **FR-035**: Each new durable record domain MUST carry a dedicated exact
  namespaced record-format schema (`agent-sessions.<domain>.vN`). Absent maps
  in a legacy catalog normalize to empty maps, but every present record with a
  missing or unknown record schema MUST fail closed and use exact-match
  migration dispatch. Record-format schemas MUST NOT reuse semantic user
  attributes such as `Lane.Schema`.
- **FR-036**: Every durable fact MUST have exactly one writer. Agent Sessions
  MAY persist only facts that it authors and that have no more-authoritative
  live source. Product- or OS-owned mutable facts, including native title,
  cwd, process presence, and liveness, MUST be derived or reconciled from that
  live authority rather than stored as an independently mutable copy. Native
  session IDs that must survive restart are immutable reattach anchors: live
  product evidence remains authoritative and every recovery MUST fail closed
  on disappearance or divergence.
- **FR-037**: Every proposal for new durable state MUST document its single
  writer, why the fact cannot be derived on demand, its size/retention bound,
  crash matrix, reconciliation and cleanup path, and why its correctness value
  justifies the added edge cost. The lane-input receipt/spool is the adopted
  worked example and reference cost for durable busy-lane delivery; this rule
  does not reopen its frozen contract. State without that justification MUST
  remain bounded ephemeral state or MUST NOT be added.
- **FR-038**: Sensitive launch material MUST move from the daemon to the exact
  already-attested wrapper only through the bounded, generation-local,
  memory-only `launch.sock` binary handoff. The ticket is correlation, never
  authority; live UID/PID/start/strong-start evidence MUST match. A complete
  `go` write transfers lifecycle to adoption, a proven zero-byte write permits
  exact rollback, and a partial or possibly delivered write MUST enter typed
  ambiguity reconciliation and MUST NOT replay or destructively roll back.
  The public wrapper path MUST close its CLOEXEC socket and replace its image
  through the platform exec syscall without serializing, logging, persisting,
  or adding the sensitive values to argv or the wrapper's ambient environment.

### Key Entities

- **Product Descriptor**: Data-only identity, version, command, capability,
  install, and acceptance metadata for one native product.
- **Runtime Product**: Explicit composition of the optional peer, message,
  lane, parent-attestation, and doctor drivers for one descriptor.
- **Native Session Reference**: Secret-free durable identity needed to reopen
  exactly one native peer or lane session.
- **Native Turn Reference**: Secret-free durable identity of one native turn or
  dispatch within a native session.
- **Lane Input Receipt**: Durable metadata and digest for accepted lane input,
  including its ordering, dispatch, injection, ambiguity, and retirement state.
- **Lane Input Spool Object**: Private bounded input content referenced by a
  receipt and removed only after proven consumption or explicit retirement.
- **Component Binding**: Generation-scoped attested connection between one
  managed attachment and an in-process plugin or extension.
- **Native Session Lease**: Durable exclusive Agent Sessions ownership claim
  used where the native product does not prevent concurrent owners, notably
  DSH.
- **Launch Handoff**: Bounded one-shot ephemeral transfer of one
  `NativeCommand` to the exact prepared wrapper, with structurally disjoint
  consume, rollback, and ambiguous-finalization outcomes.
- **Install Projection**: Deterministic product-derived set of aliases, assets,
  native registrations, version checks, and rollback ownership receipts.

## Success Criteria

### Measurable Outcomes

- **SC-001**: OpenCode, Kilo, Pi, OMP, and DSH pass the complete PEER × LANE ×
  PARENT matrix on Linux and macOS at their pinned versions; CodeBuddy passes
  every cell except the explicitly account-gated Tencent model-turn cell.
- **SC-002**: The existing Claude, Codex, Grok, and Qwen matrices remain green,
  including normal, race, vet, lint, install, federation, recovery, and real
  product gates.
- **SC-003**: One accepted busy-lane input survives 100 daemon crash/restart
  iterations without loss or duplicate injection; every crash-after-I/O case
  is either proven idempotent or reported as ambiguous.
- **SC-004**: A synthetic test product can be added by one catalog entry, one
  product package, and one composition-root entry without adding a dispatch
  switch or editing shell product arrays.
- **SC-005**: Every malformed, stale, cross-session, wrong-process, wrong-host,
  wrong-capability, or permission-widening attempt in the new contracts fails
  closed in automated tests.
- **SC-006**: Release installation, upgrade, rollback, and removal complete on
  isolated Linux and macOS homes with zero attributable residue and no change
  to unrelated native profiles, credentials, or plugins.
- **SC-007**: All supported cross-host lane combinations either execute through
  a destination-advertised capability or return an explicit unsupported result;
  no unknown capability is silently mapped to another product.
- **SC-008**: Product-specific code contains no duplicated component framing,
  process supervision, loopback/auth/event handling, durable queue state
  machine, or authored product inventory.

## Assumptions

- The implementation base is the clean released `origin/main` commit
  `679fe9d3068b6362df867f8d78ce6708c4ce1342`; the older 83-commit feature
  history was squash-merged and is not a newer base.
- Linux and macOS remain the only supported operating systems. Windows is not
  part of this feature.
- Same-UID local processes are one trust domain. Exact process identity and
  wrapper capability still prevent cross-session confusion within that domain.
- Federation continues to run on a trusted network as documented for protocol
  3. Transport authentication and encryption require a separate security
  design and are not smuggled into this product-expansion milestone.
- Native credentials and ordinary profiles remain owned by their products.
  Agent Sessions may use existing authenticated contexts for acceptance but
  does not copy, print, or persist their secrets.
- The persistence and state-minimization audit is formal architectural input.
  Rename-incapable products such as DSH project the native title read-only and
  are drift-immune by construction. Rename-capable products write through to
  the product and project the confirmed native title; the daemon does not keep
  a second mutable rename baseline. The recommended peer-plane direction is
  live kernel/process attestation with ephemeral generation-scoped bindings,
  not expansion of the durable attachment catalog.
- Tested native baselines are OpenCode 1.18.25, Kilo 7.5.6, Pi 0.84.4, OMP
  18.0.11, CodeBuddy 2.143.0, and the exact DSH 0.1.2-alpha.3 tuple. Doctor
  combines version policy with capability probes and fails closed on drift.
- The phase-0 audit selected **extract-and-freeze**: three legacy entrypoints are
  unreachable, but no production file is independently deletable. Full tree
  removal is deferred; live helpers MUST move to focused packages and a
  shrinking allowlist MUST reject every new bridge/federator import.
- `*-peer-lane` aliases remain supported for operators and third-party
  automation even though installed product skills normally invoke the MCP
  control surface.
