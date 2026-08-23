# Feature Specification: Qwen Support

**Feature Branch**: `develop`

**Created**: 2026-08-20

**Status**: Draft

**Input**: User description: "Implement Qwen support"

## Clarifications

### Session 2026-08-20

- Q: Which Qwen profile should managed peers and lanes use in production? → A: Default to the
  existing native profile, with an explicit profile override when Qwen supports one.
- Q: Should Qwen follow this same launch-time permission contract as the other managed products? →
  A: Yes; use the same launch-time contract.
- Q: When resuming a managed Qwen session created with a non-default profile, how should that profile
  be selected? → A: Require the same explicit profile override and fail closed on mismatch.
- Q: How should Qwen's archive ownership be decided? → A: Follow the Codex model if current Qwen
  supports equivalent native archive and unarchive operations; otherwise follow the Claude model.
- Q: How should Agent Sessions handle an installed Qwen version that was not the exact version used
  for the v0.2.4 release tests? → A: Require a minimum version plus live contract checks.
- Q: Should Qwen follow this same installed-but-inactive-until-attested model? → A: Yes; use the
  same managed-session attestation model as Codex, Claude, and Grok.
- Q: What should happen when an operator selects a non-default Qwen profile that does not have the
  Agent Sessions plugin and skills installed? → A: Fail closed and require explicit installation
  into the selected profile.
- Q: Must Agent Sessions prevent a managed Qwen session from entering yolo after a default or
  non-yolo launch? → A: No. Map any explicit launch choice to Qwen's initial native mode, then allow
  Qwen's normal in-session mode changes, including entering or leaving yolo; do not add a separate
  enforcement layer.
- Q: What should peer mode change about the native Qwen product experience? → A: Only add
  authenticated Agent Sessions communications and the ability to launch supported local or remote
  "foreign" product lanes. Qwen's normal UI, tools, permission prompts, mode changes, and session
  behavior remain native unless a change is strictly required for those two integration surfaces.

### Session 2026-08-21

- Q: Which archive foundation did current Qwen evidence select? → A: Qwen Code 0.21.15 provides
  the required native archive/unarchive, writer-lease, conflict, and idempotence contracts, so use
  the Codex-style native archive transaction and reject readiness if that contract is unavailable.
- Q: How should the pre-existing Claude-to-Codex/Grok symlink refusal be handled? → A: Make every
  session-stable adapter delivery path a real Unix socket as part of the Qwen shared foundation;
  retain stable addressing and exact ownership, and migrate only exactly corroborated stale aliases.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Use Qwen as a Managed Interactive Peer (Priority: P1)

An operator launches or resumes Qwen as a named Agent Sessions peer, assigns it to explicit groups,
and uses the same discovery and messaging workflow available to other managed products. The Qwen peer
can receive trusted cross-session deliveries while idle or busy and can discover, send, multicast,
and broadcast through its authorized groups. A normally launched, unmanaged Qwen session remains an
intentional communication opt-out.

**Why this priority**: Interactive peer participation is the minimum useful Qwen integration and
proves that Qwen can join the existing product-neutral collaboration model without weakening its
boundaries.

**Independent Test**: Launch one managed Qwen peer and one existing managed peer in the same group,
exchange correlated direct replies and a group broadcast in both directions, then confirm an
unmanaged Qwen session has no Agent Sessions identity or usable messaging or lane authority.

**Acceptance Scenarios**:

1. **Given** an authenticated Qwen installation and a running host agent, **When** an operator starts
   a named Qwen peer in one or more groups, **Then** exactly one live Qwen participant appears with
   the expected identity, working directory, groups, durable launch permission preference,
   exact initial native-mode request, honest current-native-mode-or-unknown status, and messageable
   status.
2. **Given** two managed peers sharing a group, including one Qwen peer, **When** either peer sends a
   direct message, explicit multicast, or named-group broadcast, **Then** only authorized recipients
   receive one correlated delivery and can reply to the exact sender.
3. **Given** a Qwen peer that is processing another turn, **When** an authorized delivery arrives,
   **Then** the delivery is retained and presented exactly once without corrupting the active turn.
4. **Given** a bare Qwen session, **When** it runs alongside managed sessions, **Then** it creates no
   Agent Sessions participant, group membership, delivery endpoint, or messaging authority.
5. **Given** a managed Qwen peer exits normally, is interrupted, or its wrapper is terminated,
   **When** reconciliation completes, **Then** its attributable processes and private artifacts are
   gone while unrelated sessions remain intact.
6. **Given** managed Codex and Grok peers with session-stable delivery endpoints, **When** a managed
   Claude peer sends one correlated message to each through Agent Sessions, **Then** both deliveries
   and replies succeed through the exact published paths, each path is an actual Unix socket rather
   than a symbolic link, and no sender resolves or rewrites the path.

---

### User Story 2 - Delegate Durable Work to a Qwen Lane (Priority: P1)

An orchestrator starts a named Qwen worker lane, collects its terminal answer, sends follow-up work on
the same transcript, interrupts active work, archives it, and resumes it later. The lane follows the
same ownership, persistence, notification, group inheritance, and collection rules as existing lanes.

**Why this priority**: Durable delegated work is the primary Agent Sessions workflow and must be
complete before Qwen can be considered a first-class target.

**Independent Test**: From one existing managed parent, run the complete Qwen lane lifecycle—start,
wait, follow-up, interrupt, archive, idempotent archive, and resume—and verify transcript continuity,
one terminal result per turn, exact owner notification, and zero post-archive residue.

**Acceptance Scenarios**:

1. **Given** a live managed parent, **When** it starts a non-persistent Qwen lane, **Then** the lane
   becomes discoverable with its own identity, the parent anchor, and the requested inherited groups.
2. **Given** an active or completed Qwen turn, **When** the parent waits or sends a follow-up,
   **Then** exactly one collectable terminal result is produced and later work continues on the same
   native transcript.
3. **Given** an active Qwen turn, **When** the parent interrupts it, **Then** the result is terminal,
   reports interruption consistently, remains collectable once, and can later be resumed.
4. **Given** an archived lane, **When** archive is repeated, **Then** the result is successful and
   explicitly idempotent; **When** resume is requested, **Then** the same lane and native transcript
   identities are restored.
5. **Given** an owned lane and a persistent lane from the same parent, **When** the parent exits,
   **Then** the owned lane is retired and the persistent lane remains available.

---

### User Story 3 - Compose Qwen with Every Supported Product (Priority: P2)

Users can choose Qwen as either the orchestrating parent or the delegated target without changing
the collaboration rules. Codex, Claude, Grok, and Qwen parents can launch Qwen lanes, and a managed
Qwen parent can launch each of the four target products.

**Why this priority**: A partial one-way adapter would force users to reason about product-specific
topologies and would violate the product's shared contract.

**Independent Test**: Exercise every parent-target combination at contract level and live-test every
edge involving Qwen, verifying the immediate parent identity, notification target, private anchors,
explicit group inheritance, terminal answer, and cleanup.

**Acceptance Scenarios**:

1. **Given** any supported managed parent, **When** it launches a Qwen lane, **Then** ownership,
   notification, permissions, groups, and lifecycle behavior match the common lane contract.
2. **Given** a managed Qwen parent, **When** it launches Codex, Claude, Grok, and Qwen targets,
   **Then** all four complete and notify the exact Qwen parent without product-specific routing steps.
3. **Given** a parent with an explicit group, **When** it requests inheritance for a child,
   **Then** the child receives that group plus both mandatory private anchors and no unrelated group.
4. **Given** inheritance is not requested, **When** the child starts, **Then** it receives only its
   mandatory private groups and not the parent's other groups.

---

### User Story 4 - Run Qwen Across Federated Hosts (Priority: P2)

An operator can target a connected remote host for a Qwen lane and collect its result through the
same parent session. Remote execution remains an explicit destination choice and does not create a
new public listener or a fallback transport.

**Why this priority**: Federation is a shipped Agent Sessions capability; Qwen support must not be
local-only or introduce a separate remote-execution model.

**Independent Test**: Run one Linux-to-macOS and one macOS-to-Linux Qwen lane round trip, collect the
exact requested answer from the source parent, and prove destination cleanup and source non-mutation.

**Acceptance Scenarios**:

1. **Given** two connected current-version hosts advertising Qwen capability, **When** a parent starts
   a remote Qwen lane, **Then** the lane runs only on the selected host and returns one terminal notice
   to the exact source parent.
2. **Given** a remote Qwen lane completes, **When** the emitted collection instruction is executed
   verbatim, **Then** the source parent collects the result successfully without adding hidden flags.
3. **Given** the destination lacks Qwen readiness or advertises no Qwen capability, **When** remote
   execution is requested, **Then** it fails before creating a lane or changing remote state.
4. **Given** a completed or failed remote lane, **When** it is archived or reconciled, **Then** no
   attributable manager, worker, tool process, socket, or temporary artifact survives remotely.

---

### User Story 5 - Install and Diagnose Qwen Safely (Priority: P3)

An operator installs the Qwen integration as part of Agent Sessions, verifies readiness without
starting a session, and receives actionable diagnostics for missing executables, authentication,
workspace trust, or permission incompatibility. Installation preserves Qwen credentials and
unrelated native settings, plugins, skills, and transcripts.

**Why this priority**: Operators need a safe path to deploy and troubleshoot the feature, but this
becomes valuable only after the peer and lane behaviors are defined.

**Independent Test**: Install from a release archive into an isolated location, run readiness checks
for ready and intentionally unready environments, verify all Qwen surfaces are present, and prove the
owner's native profile and credentials are unchanged.

**Acceptance Scenarios**:

1. **Given** a supported release archive, **When** an operator performs a complete installation,
   **Then** Qwen peer, lane, skills, documentation, and remote capability surfaces are installed and
   discoverable alongside the existing products.
2. **Given** no Qwen session is running, **When** the operator runs the readiness check, **Then** it
   reports executable version, non-secret credential/provider configuration state, workspace trust,
   headless capability, and requested versus expected initial permission mode without creating a
   session. Actual provider authentication is first exercised by the intended managed launch. The
   requested initial mode is retained exactly, while current mode is reported as unknown unless a
   supported native event exposes it.
3. **Given** Qwen rejects the requested native approval-mode option, **When** a peer or lane starts,
   **Then** startup fails clearly before publication rather than silently selecting another option.
4. **Given** an existing native Qwen profile, **When** Agent Sessions is installed, upgraded, used,
   and removed, **Then** credentials and unrelated native settings remain unchanged.

### Edge Cases

- The Qwen executable is missing, unsupported, upgraded during a run, or reports an incompatible
  interactive or headless contract.
- Authentication is absent or expires between readiness checking and launch.
- The workspace is untrusted and Qwen downgrades or refuses the requested permission mode.
- A resume selector is unknown, ambiguous by name, already live, native-archived, or belongs to a
  different product.
- A managed wrapper exits before identity publication, after publication but before registration,
  or while a message is being delivered.
- A lane manager or worker is killed while detached shell work and messaging children are live.
- A PID or artifact name is reused, an owned path changes type, or an unrelated process appears in
  the same process namespace during cleanup.
- The host agent, supervisor, or machine restarts during an active turn or partial cleanup.
- Two collectors race for one terminal result, or follow-up work arrives while an earlier result is
  uncollected.
- A sender attempts cross-group discovery, mixed authorized/unauthorized multicast, wildcard
  broadcast, or forged parent context.
- Connected hosts run different Agent Sessions versions even though coordinated upgrades are
  required.
- A transcript was archived or unarchived by native Qwen outside Agent Sessions.
- An explicit non-default profile lacks the Agent Sessions-owned Qwen plugin or skills.
- A mechanism proposed by the historical Qwen pre-design conflicts with the current Agent Sessions
  architecture or the supported Qwen release.
- An existing Codex or Grok adapter exposes its session-stable delivery endpoint as a symbolic link
  to a PID-bound socket, while a native sender refuses symlink reply targets.
- An upgrade encounters a legacy session-socket symlink or an abruptly terminated adapter leaves
  one behind; migration and reconciliation must remove only the exactly owned artifact.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST provide managed interactive Qwen peers and durable Qwen worker lanes as
  first-class product choices alongside Codex, Claude, and Grok. Managed Qwen peer mode MUST preserve
  normal Qwen behavior except where necessary to add authenticated Agent Sessions communications and
  the ability to launch local or remote lanes for supported products. Agent Sessions MUST NOT
  introduce unrelated policy or behavioral changes to Qwen's UI, tools, permissions, or native
  session controls.
- **FR-002**: The system MUST report Qwen readiness without launching a session, including executable
  version, non-secret credential/provider configuration state (`ready`, `unknown`, or `unready`),
  workspace trust, interactive and headless availability, and requested versus expected initial
  permission mode. Doctor MUST NOT claim that provider authentication or the effective initial mode
  has been proven before a session exists; the intended managed launch MUST corroborate both before
  publication. Admission MUST require the documented minimum Qwen
  version plus live checks for every native capability and safety contract used by the requested operation. A
  newer version MUST be rejected when any required check is unavailable, ambiguous, or fails. For
  the selected profile, readiness MUST verify the exact Agent Sessions-owned Qwen plugin and skills.
- **FR-003**: A complete installation MUST register the Agent Sessions-owned Qwen plugin and skills
  through Qwen's supported installation mechanism. Qwen MAY expose their skill, command, or
  tool-inventory surfaces to bare sessions, matching the other supported products. Installation and
  surface discovery alone MUST NOT activate any Agent Sessions operation: every operational call
  MUST remain inactive unless exact process and host registration evidence attests the caller as a
  live managed Qwen peer or lane. Model-supplied identity MUST NOT activate an operation.
- **FR-004**: A managed Qwen peer MUST publish exactly one participant only after its exact native
  session identity, working directory, selected launch preference and effective initial native mode,
  live lifecycle owner, and delivery endpoint are corroborated.
- **FR-005**: Operators MUST be able to assign a stable name, repeatable explicit groups, and an
  explicit initial-yolo choice at managed Qwen launch using the common peer conventions.
- **FR-006**: Managed Qwen peers MUST support grouped discovery, direct send, explicit multicast,
  named-group broadcast, inbound delivery, and exact-sender replies under the existing authorization
  rules.
- **FR-007**: Bare and managed Qwen sessions MUST be able to coexist without bare sessions appearing
  in the Agent Sessions roster, receiving a managed delivery endpoint, acquiring group membership or
  lifecycle ownership, or affecting managed cleanup. Starting, resuming, or exiting a bare Qwen
  session MUST NOT mutate managed participant/catalog state or cause managed artifacts to be adopted
  or removed.
- **FR-008**: Operators MUST be able to resume a managed Qwen peer by exact session identifier or
  unique durable name while preserving its native transcript, canonical working directory, groups,
  and initial permission preference unless an explicitly supported override is supplied. A session
  created with a non-default profile MUST require the same explicit profile override on resume; Agent Sessions
  MUST NOT automatically select or search profiles.
- **FR-009**: Resume MUST fail before mutation when the target is missing, ambiguous, belongs to
  another product, is already live without an exact stale-owner proof, or cannot prove the expected
  native transcript state or exact profile context.
- **FR-010**: Normal exit, interrupt, termination, and crash recovery for an interactive Qwen peer
  MUST remove only its attributable participant, delivery endpoint, temporary input/output artifacts,
  wrapper, native process, and managed descendants.
- **FR-011**: Qwen lanes MUST support readiness checking, listing, start, run, wait, status, follow-up,
  interrupt, archive, idempotent archive, and transcript-preserving resume. Explicit Agent Sessions
  archive and resume MUST use Qwen's native archive/unarchive transaction with the admitted
  writer-lease, conflict, capability, and idempotence contracts established for Qwen Code 0.21.15.
  Readiness MUST fail when any required native archive contract is absent, changed, or ambiguous;
  there is no bridge-owned archive fallback. Acceptance MUST test explicit archive, idempotent
  archive, external native state changes, compensation, exact resume, and helper cleanup.
- **FR-012**: Each Qwen lane MUST retain distinct Agent Sessions and native Qwen identities and MUST
  preserve both identities across follow-up turns, archive, restart, and resume.
- **FR-013**: Each lane turn MUST produce at most one collectable terminal result, and a second
  collection attempt MUST return an explicit no-debt or already-collected result without duplication.
- **FR-014**: A parent-owned Qwen lane MUST retire when its exact owner exits; a lane explicitly made
  persistent MUST survive owner exit; automatic archive behavior MUST match the common lane contract.
- **FR-015**: Every Qwen lane MUST receive its own private anchor and the exact immediate-parent
  anchor. Other parent groups MUST propagate only when inheritance is explicitly requested.
- **FR-016**: Codex, Claude, Grok, and Qwen managed parents MUST all be able to target Qwen lanes using
  the same lifecycle and group semantics.
- **FR-017**: A managed Qwen parent MUST be able to target Codex, Claude, Grok, and Qwen lanes locally
  and on eligible connected hosts.
- **FR-018**: Qwen parent and target composition MUST NOT create a new global namespace, messaging
  frame, group rule, or parallel lifecycle contract.
- **FR-019**: Connected hosts MUST advertise Qwen lane capability only when the host can actually
  launch the supported Qwen integration.
- **FR-020**: A remote Qwen lane MUST run only on the selected connected host, retain the source-parent
  and destination-child anchors, and return a collection instruction that works verbatim from the
  source environment.
- **FR-021**: Remote Qwen execution MUST fail before target creation when the destination is
  disconnected, unready, incompatible, or does not advertise Qwen capability; no alternate transport
  or local fallback is permitted.
- **FR-022**: Agent Sessions MUST preserve Qwen's native initial approval behavior when the operator
  supplies no permission option and map explicit common permission choices to Qwen's initial native
  mode without claiming an observation unavailable from Qwen's public interactive protocol. `--yolo` MUST request native
  yolo at launch; `--no-yolo` MUST translate exactly to native `--approval-mode default`. With no
  wrapper permission choice, a supported native `--approval-mode MODE` MUST pass through unchanged
  and the exact requested mode MUST be retained as the resume default. Combining a wrapper permission
  choice with a native approval-mode choice, or supplying contradictory/repeated wrapper choices,
  MUST fail with exit 2 before preparation, catalog, profile, or native-process mutation; the wrapper
  MUST NOT silently choose precedence even when the two choices are semantically equivalent. After
  publication, Qwen's normal in-session approval controls MAY change the native mode in either
  direction, including entering or leaving yolo. The current mode MUST be reported as `unknown`
  unless a supported native event exposes it. Agent Sessions MUST NOT add a sandbox, hook,
  deny-list, guard, input filter, or other enforcement layer whose purpose is to prevent that native
  transition. Documentation and status MUST distinguish the durable launch preference from Qwen's
  mutable current native mode and MUST NOT represent the launch preference as a lifetime security
  boundary.
- **FR-023**: Managed Qwen launches MUST default to the user's active native Qwen profile and MAY use
  an explicit profile override only through a native Qwen-supported mechanism. Agent Sessions MUST
  NOT implicitly create a managed profile or select, copy, print, mutate, or broaden Qwen credentials,
  authentication, owner-wide settings, unrelated native plugins or skills, or unrelated transcripts.
  A peer or lane created with a non-default profile MUST require the same explicit profile override
  on resume and MUST fail before mutation when it is missing or mismatched. If the selected profile
  lacks the required Agent Sessions-owned integration, launch MUST fail before starting Qwen and
  instruct the operator to install it explicitly through Qwen's supported mechanism. Launch MUST NOT
  install into that profile automatically or borrow integration files from another profile. Normal
  Qwen-owned runtime bookkeeping MAY occur within the selected profile and MUST be distinguished
  from Agent Sessions-owned mutation during acceptance.
- **FR-024**: The system MUST use exact process and artifact ownership to clean managed Qwen wrappers,
  workers, detached tool processes, messaging children, sockets, state files, and temporary roots.
- **FR-025**: Ambiguous or incomplete cleanup MUST remain durable retryable debt and MUST NOT be
  reported as successful or authorize deletion of unrelated state.
- **FR-026**: Reconciliation after wrapper, manager, worker, host-agent, supervisor, or machine restart
  MUST converge to the documented lane or peer state without duplicate work or collateral cleanup.
- **FR-027**: Release packages MUST contain all existing product surfaces plus the Qwen peer, Qwen
  lane, Qwen-facing skills, operator documentation, and remote capability support for every published
  platform. The repository's actual GitHub release workflow MUST consume or validate the same
  authoritative executable/plugin inventory as local packaging, MUST build all eleven executables
  and four plugin payloads, and MUST fail before publication when workflow, package, installer, or
  descriptor inventories drift. Candidate and tag-triggered builds from the same commit, declared
  release version, toolchain, and inventory MUST produce byte-identical archives whose SHA-256 values
  match the signed evidence artifact; both stages MUST derive `0.2.4` from the authoritative version
  file and independently validate tag `v0.2.4` rather than injecting different ref-dependent version
  strings.
- **FR-028**: Every Qwen command option accepted by the product MUST be visible in its corresponding
  help output, and failures MUST identify the actual unmet precondition.
- **FR-029**: Qwen behavior, genuine product differences, installation, lifecycle, permissions,
  messaging, federation, and recovery MUST be documented alongside the equivalent product guides.
- **FR-030**: The complete Qwen acceptance scope MUST pass on real Linux and macOS installations,
  including normal, adversarial, crash, restart, packaging, installation, and bidirectional federated
  scenarios. Final tagged-commit proof MUST be emitted by the release workflow as the versioned
  `agent-sessions-v0.2.4-release-evidence.json` contract artifact, retained under an immutable
  workflow-run identity, validated against the checked-in Draft 2020-12
  `release-evidence.schema.json`, and attached unchanged to the GitHub release. The signed annotated
  tag MUST identify that workflow run, artifact name, and SHA-256 digest. Tag creation MUST refuse a
  pre-existing local or remote tag; the later tag-triggered job MUST require and verify that exact
  signed tag while refusing a pre-existing release or same-named release asset.
- **FR-031**: Every session-stable Unix delivery endpoint published by an Agent Sessions adapter MUST
  be an actual Unix socket at the published path, not a symbolic link. The Qwen implementation MUST
  use this same transport invariant, and this feature MUST replace the existing Codex and Grok
  session-socket symlink publication without changing stable session addressing. Exact process,
  session, and artifact ownership MUST still govern replacement and cleanup. Upgrade reconciliation
  MUST recognize and remove a legacy symlink only after proving that it is an Agent Sessions-owned
  alias to the exact stale PID-bound backend; ambiguous links, sockets, and unrelated files MUST be
  preserved as retryable debt. Managed Claude-to-Codex and Claude-to-Grok delivery MUST be tested
  against the published endpoint without caller-side symlink resolution or another path-rewriting
  workaround. Legacy-alias removal is local stale-artifact reconciliation only: current binaries
  MUST NOT publish aliases or interoperate with obsolete Agent Sessions binaries through that path.
- **FR-032**: Every Codex, Claude, Grok, and Qwen plugin MUST expose the shared structured messaging
  and lane tools under the single product-neutral MCP namespace `agent_sessions`. Public manifests,
  model-facing skills, runtime instructions, dynamic-tool dispatch, documentation, and acceptance
  evidence MUST NOT expose the historical vendor-specific `claude_peer` namespace or a compatibility
  alias for it.
- **FR-033**: Every Claude-facing model skill MUST use the structured `agent_sessions` tools for
  Agent Sessions discovery, send, multicast, broadcast, acknowledgment, and reply. If a structured
  tool is unavailable, inactive, or fails, the skill MUST require reporting that exact failure and
  stopping. It MUST NOT instruct the model to retry through native Claude `ListAgents`,
  `SendMessage`, a host-agent service row, or a framed native carrier. The low-level carrier may
  remain implemented as an explicit protocol-compatibility surface, but it is not an automatic
  model fallback.

### Key Entities

- **Managed Qwen Peer**: One interactive Qwen session with exact native identity, Agent Sessions
  identity, name, working directory, initial permission preference, mutable native approval mode,
  groups, lifecycle owner, and delivery state.
- **Qwen Lane**: A durable delegated worker with Agent Sessions identity, native transcript identity,
  parent context, ownership or persistence policy, group state, turn state, terminal result, cleanup
  state, and optional remote host.
- **Qwen Turn**: One unit of delegated work with one terminal outcome, one collection cursor, optional
  interruption, and transcript continuity into later turns.
- **Parent Context**: The exact source host, session, product, instance, groups, initial lane
  permission preference, and notification target that authorize child composition.
- **Qwen Lifecycle Debt**: Durable evidence that attributable process or artifact cleanup is incomplete
  and must be retried without broadening ownership.

### Scope Boundaries

**In scope**:

- Managed Qwen interactive peers, durable Qwen lanes, messaging, groups, resume, permissions,
  ownership, collection, archive, crash recovery, packaging, skills, documentation, and federation.
- All four parent products targeting all four lane products, with complete live coverage for every
  composition edge involving Qwen.
- Coordinated current-version Agent Sessions hosts on Linux and macOS.

**Out of scope**:

- Granting Agent Sessions operational authority to bare Qwen sessions.
- A long-lived Qwen network service or a separate Qwen-specific federation protocol.
- Compatibility shims for obsolete Agent Sessions hubs, agents, launchers, or plugins.
- Native Qwen transcript archive or unarchive as an implicit Agent Sessions side effect.
- Windows support in this release.
- Changing Qwen authentication, credential storage, owner-wide permission policy, or unrelated
  native configuration.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Users can launch a managed Qwen peer, discover one authorized peer, exchange a direct
  message and reply, and complete a group broadcast in under five minutes using documented commands.
- **SC-002**: Every one of the 16 parent-target product combinations passes contract validation, and
  every live composition edge involving Qwen returns the requested terminal answer to the exact
  parent with no unrelated recipient.
- **SC-003**: The full Qwen lane lifecycle completes with exactly one terminal result per turn and
  preserves both lane and native transcript identity across follow-up, archive, and resume in 100% of
  acceptance runs.
- **SC-004**: Interactive and lane crash scenarios leave zero attributable live processes, endpoints,
  temporary artifacts, worktrees, or pending notices at return and at 1, 5, 10, and 30 seconds after
  cleanup, while all inventoried unrelated state survives.
- **SC-005**: All mandatory Qwen scenarios pass on both Linux and macOS with zero data races and no
  platform-specific waiver.
- **SC-006**: One Linux-to-macOS and one macOS-to-Linux Qwen lane completes, notifies, collects, and
  archives successfully using the emitted operator instructions verbatim.
- **SC-007**: Across install, upgrade, normal use, crash recovery, and removal tests, 100% of monitored
  unrelated Qwen credentials, owner settings, and native transcripts remain unchanged.
- **SC-008**: A bare Qwen session produces zero Agent Sessions participants, groups, delivery
  endpoints, successful messaging operations, or successful lane operations in every coexistence
  test, even when installed Agent Sessions surfaces are visible.
- **SC-009**: Each published platform archive passes checksum validation, contains every declared
  executable and integration surface, and completes one isolated installation without a development
  toolchain. The real tag workflow, rather than a separate rehearsal-only command, produces all four
  archives from the authoritative eleven-executable/four-plugin inventory and publishes the exact
  schema-valid `agent-sessions-v0.2.4-release-evidence.json` named by the signed tag.
- **SC-010**: For every intentionally unready state—missing executable, unauthenticated client,
  untrusted workspace, missing selected-profile integration, incompatible initial permission mode,
  disconnected host, or mixed Agent Sessions version—the operator receives a cause-specific failure
  before partial publication.
- **SC-011**: On both Linux and macOS, every live Codex, Grok, and Qwen session-stable delivery path
  is observed as an actual Unix socket and not a symbolic link; correlated managed Claude-to-Codex and
  Claude-to-Grok messages succeed through those exact published paths, and normal exit, crash,
  restart, and legacy-symlink migration leave zero attributable socket residue or collateral removal.
- **SC-012**: All four installed product plugins advertise exactly one `agent_sessions` MCP server,
  every structured discover/send/broadcast/lane call uses that namespace, and repository completeness
  tests find zero public `claude_peer` namespace references.
- **SC-013**: Every packaged Claude-facing skill contains structured `agent_sessions` guidance and
  contains no framed-native-carrier instruction; a regression test fails if the carrier marker or
  host-agent service target reappears in any of those skills.

## Assumptions

- Qwen support ships in Agent Sessions v0.2.4.
- The minimum supported Qwen Code version is 0.21.15. That version and every newer version are
  admitted only through the operation-specific live contract checks defined by this feature.
- `docs/designs/QWEN-ADAPTER.md` is historical pre-design evidence prepared against Qwen Code 0.21.12
  and an earlier Agent Sessions architecture. It is advisory research only: it is not an authority,
  a source of requirements, or a default implementation plan.
- Planning must audit every material pre-design proposal against the current v0.2.0 architecture and
  the selected Qwen release, recording whether each proposal is retained, revised, or discarded and
  why. No pre-design proposal carries forward without current evidence. The constitution and this
  approved specification govern required outcomes; current shared contracts and direct Qwen evidence
  govern design decisions; the historical pre-design remains subordinate to all of them.
- Users manage Qwen authentication and native profiles through Qwen itself. Managed launches default
  to the active native profile; an operator may select another profile only through an explicit,
  natively supported override, and that profile remains Qwen-owned.
- All participating Agent Sessions binaries are upgraded together; legacy federation compatibility
  is not required.
- Existing Agent Sessions identity, group, messaging, lane, ownership, permission, federation, and
  release contracts remain authoritative unless this specification explicitly says otherwise. Qwen's
  mutable in-session approval mode in FR-022 is one explicit product-specific difference.
- Real authenticated Qwen installations and real Linux and macOS hosts are available for acceptance.
- Native-client behavior may differ by operating system or Qwen version; such differences require
  explicit documentation and evidence rather than silent normalization.
