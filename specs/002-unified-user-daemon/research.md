# Phase 0 Research: Unified User Daemon

## Decision 1: One canonical host binary, one separate hub binary, and one host daemon process

**Decision**: Build one canonical `agent-sessions` host executable. Run exactly one
`agent-sessions daemon` process per OS user on each participating host. Install the existing peer and
lane command names only as filesystem links or equivalent `argv[0]` aliases to that exact host image;
do not build separate host-side thin executables. Build the one central federation deployment as the
separate `agent-sessions-hub` executable with its own explicit `install-hub`/`remove-hub` lifecycle.
The hub imports the shared federation wire, identity, routing, and protocol contracts but no host
daemon, adapter, connector, attachment, or lane runtime. Both deployment roles call shared
function-oriented service-control, release-transaction, atomic-state, and metadata-only diagnostic
packages; they do not duplicate those mechanics under daemon and federator package trees. Existing
shared federation behavior moves from the process-named `internal/federator` tree into the logical
`internal/federation` package used by both host composition and the central hub.

**Rationale**: The release currently builds eleven executables from nearly empty command packages in
[`cmd/`](../../cmd), while the runtime executable already dispatches most long-lived roles from
[`internal/bridge/runtime.go`](../../internal/bridge/runtime.go). One binary makes version identity
unambiguous on each host, while one daemon removes the split authority that allowed old supervisors
and agents to survive an install. The central hub is not one of those host-side authorities: it is one
network service with an independent lifecycle and almost no runtime implementation overlap. Keeping it
as another host-binary mode would couple unrelated installation and help surfaces without improving
host version safety. Independent service lifetime does not require duplicated service-manager or
immutable-release code: role descriptors and readiness hooks preserve the deployment boundary while
one shared implementation closes lifecycle defects across both roles.

Both executables are built from this repository, but hub and host release/build identities need not
match. Network interoperability is decided only by exact equality of the explicitly versioned hub
protocol during handshake. Host-advertised lane capabilities remain the existing per-operation
availability inventory, not another admission layer. Upgrading one host does not upgrade or restart
the hub, and upgrading a protocol-matching hub does not restart hosts. The new architecture has no
requirement to interoperate with the pre-unification split-process executables.

**Alternatives considered**:

- Keep independently built role binaries: rejected because it preserves version drift and packaging
  duplication among host-side roles.
- Keep one daemon per product/profile: rejected because groups already provide collaboration
  granularity and the user explicitly requires one daemon per user-host.
- Run the central hub inside every host daemon: rejected because it changes the existing one-hub
  topology and confuses a central deployment role with the per-host agent being unified.
- Keep the hub as a mode of the host executable: rejected because the hub has independent deployment,
  service lifetime, protocol endpoint, and state, while source reuse is limited to federation
  contracts.
- Require hub and hosts to come from one commit: rejected because the wire protocol, not the source
  revision, defines network interoperability; commit coupling would force unnecessary network-wide
  upgrades.

## Decision 2: Reuse native state machines as daemon-owned actors

**Decision**: Refactor the existing supervisor, delivery adapter, interactive host, and lane manager
implementations into callable components owned by the daemon. Each live attachment or lane may have an
in-process actor/goroutine, but it owns no process-global listener or independent registry.

**Rationale**: Current product logic is substantial and already tested in
[`internal/bridge/supervisor.go`](../../internal/bridge/supervisor.go),
[`claude_lane.go`](../../internal/bridge/claude_lane.go),
[`grok.go`](../../internal/bridge/grok.go),
[`grok_lane_manager.go`](../../internal/bridge/grok_lane_manager.go),
[`qwen_host.go`](../../internal/bridge/qwen_host.go), and
[`qwen_lane_manager.go`](../../internal/bridge/qwen_lane_manager.go). The defect is where that logic
runs and owns authority, not the established vendor behavior.

**Alternatives considered**:

- Rewrite all adapters around a new generic state machine: rejected because genuine vendor identity,
  permission, resume, archive, and transport contracts differ.
- Retain manager subprocesses but have the daemon supervise them: rejected because it preserves the
  same missed-restart and mixed-version class.

## Decision 3: One local socket; no loopback IPC between daemon modules

**Decision**: Expose one private Unix socket for external local clients. In-process routing, lane,
adapter, migration, and federation modules call shared Go APIs directly. The socket multiplexes
administrative clients, launchers/hooks, and vendor-required connectors through role-scoped requests.

**Rationale**: The existing host agent already provides a local control socket in
[`internal/federator/agent.go`](../../internal/federator/agent.go), while supervisors, shims, hosts, and
lane managers add more listeners. Collapsing these into one endpoint removes socket discovery and
length failures without replacing cheap in-process calls with unnecessary IPC.

**Alternatives considered**:

- One socket per product, profile, session, or lane: rejected because it recreates distributed
  authority and Unix-path failure modes.
- Internal RPC between daemon subsystems: rejected because all subsystems share one address space and
  version.
- Socket activation or launcher-driven lazy start: rejected because explicit user service lifetime and
  explicit-stop behavior require commands to fail while the daemon is absent.

## Decision 4: Vendor-required MCP processes become stateless relays

**Decision**: Preserve stdio MCP child processes only where the native product requires a subprocess.
They parse/emit the vendor framing, obtain kernel process identity, and relay requests to the daemon.
They own no Agent Sessions registry, routing rule, lane logic, listener, durable state, or cleanup.

**Rationale**: All installed integrations currently declare stdio MCP commands in [`.mcp.json`](../../.mcp.json),
[`claude/.mcp.json`](../../claude/.mcp.json), [`grok/.mcp.json`](../../grok/.mcp.json), and
[`qwen/mcp.json`](../../qwen/mcp.json). A goroutine cannot replace a child process at a vendor-owned
stdio boundary. Moving the current attestation and tool behavior out of
[`internal/bridge/mcp.go`](../../internal/bridge/mcp.go) still puts all authoritative logic on one
version inside the daemon.

**Alternatives considered**:

- Keep full MCP behavior in each child: rejected because old children would retain authoritative logic
  across daemon upgrades.
- Require every vendor to support a shared network MCP endpoint: rejected because that is not a common
  supported native contract.
- Add connector keepalive daemons: rejected because connectors must remain vendor-scoped and stateless.

## Decision 5: Replace adapter-process authority with attachment revisions

**Decision**: Authorize each managed participant with one daemon-owned attachment revision plus exact
vendor actor identity, native session/profile/cwd evidence, kernel-authenticated connector identity,
and product-specific capabilities. A separate Agent Sessions adapter PID or per-session socket is no
longer required authority.

**Rationale**: Current `PeerRegistration` and parent attestation assume a live adapter process and
socket in [`internal/federator/registration.go`](../../internal/federator/registration.go). That model
cannot survive removal of shims and hosts. Product-specific evidence already exists and can be retained
without treating the old process boundary as security evidence.

**Alternatives considered**:

- Trust model-supplied session IDs: rejected by the existing exact-identity contract.
- Treat same-user socket access as participant identity: rejected because the OS user is sufficient
  for administration, not for selecting a model-facing attachment.
- Preserve dummy adapter processes only for attestation: rejected as process-shaped duct tape.

## Decision 6: Deliver through vendor-native channels selected inside the daemon

**Decision**: Resolve and admit the destination in the daemon, then use the existing native delivery
channel: Codex App Server thread operations, Claude's corroborated native socket, Grok's leader/ACP
interjection, or Qwen's admitted native input/event path. Agent Sessions owns no per-session listener.

**Rationale**: These delivery paths already exist across
[`internal/bridge/supervisor.go`](../../internal/bridge/supervisor.go),
[`internal/federator/route.go`](../../internal/federator/route.go),
[`internal/bridge/grok.go`](../../internal/bridge/grok.go), and
[`internal/bridge/qwen.go`](../../internal/bridge/qwen.go). The daemon can select them by exact
attachment identity after the current group admission rules run.

**Alternatives considered**:

- Keep per-session shim sockets: rejected because those listeners are the main process-proliferation
  source.
- Route all products through Claude native messaging: rejected because the native carrier is not a
  universal vendor contract and prior fallback behavior is deliberately removed.

## Decision 7: Keep atomic filesystem state; do not add a database

**Decision**: Consolidate Agent Sessions-owned state under one schema-versioned state root using the
existing atomic JSON, revision, spool, and journal primitives. Keep file-per-entity records so failed
operations can be retried independently. The daemon serializes authoritative mutation.

**Rationale**: Existing catalog, preparations, lane states, wakes, notices, inboxes, and cleanup debt
already have atomic-file semantics in [`internal/federator/groups.go`](../../internal/federator/groups.go),
[`internal/federator/registration.go`](../../internal/federator/registration.go), and
[`internal/bridge`](../../internal/bridge). A database would add a second migration problem without
solving the observed process-lifecycle defect.

**Alternatives considered**:

- SQLite or an embedded key-value database: rejected as unnecessary migration and dependency risk.
- One monolithic JSON snapshot: rejected because unrelated high-frequency operations would contend and
  one damaged record would affect the entire estate.
- Continue using independent bridge and host-agent roots: rejected because they encode split ownership.

## Decision 8: Use standard per-user service managers as the only lifetime owner

**Decision**: Install a foreground systemd user service on Linux and launchd user agent on macOS.
Enable login start and crash restart. Use `systemctl --user stop/disable` and `launchctl bootout` for a
persistent explicit stop; peer, lane, connector, plugin, and federation workflow commands never invoke
service lifecycle operations. One shared service-control package executes these platform operations
for both host and hub from immutable role descriptors.

**Rationale**: Existing service assets cover only the federation process and are template-only under
[`deploy/peer-federator`](../../deploy/peer-federator). The current Codex launcher lazy-starts an App
Server and detached supervisor from [`internal/launcher/bootstrap.go`](../../internal/launcher/bootstrap.go),
which directly conflicts with the user-managed daemon requirement.

**Alternatives considered**:

- Self-daemonization: rejected because it duplicates service-manager restart and login behavior.
- Socket activation: rejected because an explicit stop must remain stopped when a workflow command
  connects.
- Installer-only process management: rejected because crashes and login start still need a standard
  lifetime authority.

## Decision 9: Stage immutable releases and commit one service transition

**Decision**: Replace in-place installation with role-specific install locks, offline validation,
immutable same-filesystem release staging, independent stable host and hub current selections,
role-specific crash-recoverable transaction journals,
verified connector preparation, one service-manager restart transaction, successor readiness proof,
and exact rollback to the previous pointer if readiness fails. One release-transaction engine serves
both host and hub; role hooks select host connectors/migration or hub configuration/readiness without
sharing invocation, current selection, lock, rollback decision, or service lifetime. Immutable release
layout is role-owned as well, so removal or garbage collection never needs cross-role reference logic.

**Rationale**: The current [Makefile](../../Makefile) removes and rewrites the active installation and
then mutates four vendor integrations independently. A failure can leave a mixed payload. Versioned
staging makes the authority transition explicit and makes rollback possible without running two host
daemons concurrently.

**Alternatives considered**:

- Copy over the running release: rejected because it has no atomic rollback.
- Start old and new daemons together for handoff: rejected because the single-authority invariant
  forbids overlap.
- Require a manual restart after every install: rejected because the clarified contract requires one
  validated installer-managed restart.

## Decision 10: Treat first migration separately from steady upgrades

**Decision**: First migration inventories every known legacy state/root and exact live owner, refuses
before mutation unless every managed legacy peer and lane is closed, stops the exact quiescent legacy
authorities, adopts their durable Agent Sessions state, and starts the unified authority. It does not
transfer live legacy attachments or lane turns. Steady-state upgrades after unification never require
supported native interactive sessions to close.

**Rationale**: Existing installations can contain supervisors, shims, product hosts, lane managers,
and a host federation agent under several runtime roots. A stale scalar count caused the immediate
upgrade deadlock that motivated this redesign. The software is not released and its three deployed
hosts are operator-controlled, so a documented quiescence prerequisite closes the real migration need
without inventing a one-use live handoff protocol. Migration still uses exact process and filesystem
identity primitives rather than trusting old counts or names.

**Alternatives considered**:

- Kill every matching process during install: rejected because names and PID liveness do not establish
  ownership.
- Implement live re-registration or handoff from every legacy runtime: rejected as one-use complexity
  for an unreleased, operator-controlled deployment.
- Let old processes drain indefinitely beside the daemon: rejected because it preserves mixed-version
  authority.
- Treat stale count-only state as a permanent blocker: rejected because absent owners are provable and
  must not deadlock migration.

## Decision 11: Embed the host federation agent; preserve the hub protocol

**Decision**: Move `federator.RunAgent` behavior—local catalog, routing, capability advertisement,
outbound hub connection, remote delivery, and remote lane dispatch—into the daemon. Preserve the
existing hub, global groups, host identity, host-suffixed display names, AgentFrame semantics, and
federation wire protocol. Dispatch remote lanes directly into daemon lane actors instead of spawning
lane-watch/CLI/manager chains.

**Rationale**: The current host agent in [`internal/federator/agent.go`](../../internal/federator/agent.go)
is already the local routing authority and federation client. Embedding it removes an independently
restarted version without changing the existing topology documented in
[`docs/FEDERATION.md`](../../docs/FEDERATION.md).

**Alternatives considered**:

- Design new hub namespaces or per-environment routing: rejected because groups already define the
  existing access boundary.
- Preserve a separate federation-agent service: rejected because it recreates version skew.
- Move the hub into each host daemon: rejected because there is one central hub, not one per host.

## Decision 12: Evidence-gate restart continuity per native adapter

**Decision**: Reconnect transparently when the vendor exposes a supported durable identity/channel.
Codex App Server work is expected to reconnect. Interactive peers are reconstructed from durable and
native evidence. An active Claude stream worker or Grok/Qwen ACP lane turn may become one explicit
interrupted, collectable, resumable result if testing confirms its inherited pipe cannot be safely
reattached. No compatibility shim is introduced to fake continuity.

**Rationale**: The native lifecycle engines differ. The specification explicitly permits this bounded
fallback only after evidence. Reusing the current native resume/archive behavior preserves history
without Agent Sessions copying transcripts.

**Alternatives considered**:

- Promise transparent continuation for every native pipe: rejected because some anonymous stdio pipes
  cannot be inherited by a replacement process safely.
- Always interrupt every turn: rejected because supported reconnectable work should continue.
- Keep a per-turn proxy process solely to preserve pipes: rejected as the process topology being
  removed.

## Decision 13: Removal preserves state; purge is separate and exact

**Decision**: Normal host removal first refuses with exact active blockers, then stops/disables the
service, removes installed binaries, service assets, product connectors, and disposable runtime
artifacts while preserving Agent Sessions configuration and durable metadata. Hub removal uses the
same shared engine without host blockers or connectors: it removes only the exact hub service,
selection, releases, and disposable runtime artifacts while preserving hub configuration and durable
metadata. Separate explicit host and hub purge modes enumerate and revision-check only their respective
Agent Sessions-owned preserved paths. Vendor stores, transcripts, and remote hosts are excluded.

**Rationale**: The current tree has only a Qwen-specific removal target. The clarified specification
requires a uniform, retryable lifecycle and prevents removal from becoming vendor-profile cleanup.

**Alternatives considered**:

- Delete all state during uninstall: rejected because reinstall must restore Agent Sessions metadata.
- Manually clean vendor bookkeeping left by native uninstallers: rejected because vendor state remains
  vendor-owned.

## Decision 14: Preserve the existing validation matrix and add topology/service discriminators

**Decision**: Retain every existing peer, lane, group, resume, archive, cleanup, and federation test.
Add exact process census, daemon-down behavior, accepted-operation restart injection, split-runtime
migration, systemd/launchd lifecycle, optional-product, and install rollback tests. Live acceptance uses
the one installed daemon for that user; unit tests instantiate components in-process and do not create a
second long-lived daemon.

**Rationale**: Existing coverage in [`scripts/test`](../../scripts/test), product contract scripts,
[`scripts/federation`](../../scripts/federation), and
[`docs/ACCEPTANCE-MATRIX.md`](../../docs/ACCEPTANCE-MATRIX.md) is the functional baseline. Process
unification is complete only when those behaviors pass unchanged and the obsolete process roles are
absent.

**Alternatives considered**:

- Replace old acceptance with daemon-only unit tests: rejected because it would not prove behavior was
  preserved.
- Run additional isolated live daemons for tests: rejected by the one-daemon-per-user-host contract.
