# Phase 0 Research: Qwen Support

## Research baseline

The current local reference client is standalone Qwen Code `0.21.15` for Linux x64, built from
upstream tag commit `5dce2515a778f9cf2013168962b4fbc3454636e3`. The historical
`docs/designs/QWEN-ADAPTER.md` targeted `0.21.12` and an older Agent Sessions architecture, so every
proposal below was re-evaluated against the current client and the current repository.

The syntax/protocol floor for implementation is Qwen Code `0.21.15`. Each requested operation must
also pass the live transport and lifecycle contract probes described below.

## Decision 1: keep the shared Agent Sessions protocol

**Decision**: Qwen is a fourth product behind the existing AgentFrame, catalog, group, parent,
notice, lane, and federation contracts. Add `qwen-peer`, `qwen-peer-lane`, the `qwen-lane`
federation capability, and Qwen runtime roles; do not add a Qwen namespace, frame, hub, or routing
model.

**Rationale**: The current product-neutral layers already express the required identity, group,
multicast, broadcast, ownership, collection, and cross-host behavior. Qwen differs at the native
session boundary, not at the collaboration boundary.

**Alternatives considered**:

- A Qwen-specific messaging frame or native channel was rejected because it would bypass shared
  authorization and violate FR-018.
- A legacy-hub compatibility envelope was rejected because coordinated current binaries are an
  explicit feature assumption.

## Decision 2: close product-enumeration drift before adding Qwen

**Decision**: Introduce a shared product descriptor as the single source for product ID, label, peer
executable, lane executable/runtime role, federation capability, MCP eligibility, and stdin option
class. Generate or table-test every applicable switch, schema enum, CLI help entry, package list,
skill target, and documentation inventory from that descriptor. Extract the existing Grok tool-root
ledger into a product-neutral exact-ownership primitive with product callbacks.

**Rationale**: Current three-product enumeration is scattered across launcher, bridge, runtime,
federation, packaging, plugins, skills, documentation, and tests. Recent help/implementation drift
demonstrates that mechanically adding a fourth switch everywhere is not durable. Native adapter
logic remains product-specific where contracts genuinely differ.

**Alternatives considered**:

- Adding Qwen to each switch independently was rejected as a direct constitution DRY violation.
- A universal native adapter manager was rejected because Codex, Claude, Grok, and Qwen have
  materially different native lifecycle and permission mechanisms.

## Decision 3: use Qwen dual-output v2 for an interactive peer

**Decision**: A managed interactive peer allocates one Agent Sessions UUID and passes it as Qwen's
`--session-id`. It owns a private `0700` lifecycle root, a regular `0600` `--input-file`, and a
regular `0600` structured event file. A PTY-hosted TUI uses `--json-file`; fd 3 is allowed only when
the wrapper truly inherits the operator's terminal directly. Registration commits only after the
first `system/session_start` event corroborates UUID, canonical cwd, Qwen version, and dual-output
protocol.

Inbound deliveries append exactly one JSONL `submit` record through an inode/body-attested writer.
Resume uses `--resume UUID`; managed `--continue` and `--fork-session` are rejected. Launch always
enables chat recording. `qwen sessions ps --json` is supplementary diagnostics only and never an
authorization source.

**Rationale**: The official [dual-output protocol](https://github.com/QwenLM/qwen-code/blob/v0.21.15/docs/users/features/dual-output.md)
preserves Qwen's real TUI while providing an exact session-start event and a supported structured
input path. PTYs close arbitrary inherited descriptors, so a private file is the portable transport.

**Alternatives considered**:

- `--input-format stream-json` was rejected for interactive peers because it is a headless SDK
  surface and does not preserve the native TUI.
- Treating `qwen sessions ps` as the host registry was rejected because it has no Agent Sessions
  authority, groups, delivery endpoint, or durable ownership transaction.

## Decision 4: use a gated product-neutral preparation transaction

**Decision**: Generalize the existing durable launch preparation into a product-neutral transaction
with product validators. For Qwen it records selected profile identity, lifecycle identity,
structured transport paths and fingerprints, requested initial permission preference, exact native
UUID, and catalog revision. Publication and catalog commit happen only after native corroboration. Every
post-prepare exit either rolls back by exact revision or retains durable cleanup debt.

**Rationale**: The existing `PreparePeerLaunch` name hides Claude-specific fields and recovery.
Qwen must gain the same fail-before-publication and retryable-cleanup guarantees without fabricating
Claude socket/key evidence or introducing a weaker parallel transaction.

**Alternatives considered**:

- Persisting preferences after starting Qwen was rejected because startup failure could leave a
  false durable identity or overwrite a newer choice.
- A separately implemented Qwen journal was rejected unless generalization proves unsafe; current
  transaction concepts are shared.

## Decision 5: use native `QWEN_HOME` as the explicit profile identity

**Decision**: The default launch leaves `QWEN_HOME` unset and records the canonical effective
default. An explicit profile override sets a canonical absolute `QWEN_HOME`; relative values are
rejected. The exact `QWEN_HOME` and `QWEN_RUNTIME_DIR` value-or-absence are persisted and required on
resume. Agent Sessions never searches, copies, or borrows another profile.

**Rationale**: Qwen has no native `--profile`; `QWEN_HOME` is its supported global-state override
and owns credentials, settings, transcripts, memory, extensions, and skills. Relative values are
cwd-sensitive. Qwen does not migrate state into a redirected home.

**Alternatives considered**:

- An Agent Sessions-created shared Qwen profile was rejected by FR-023.
- Inferring a prior profile from a transcript or scanning multiple homes was rejected as ambiguous
  credential/profile selection.

## Decision 6: install a dedicated Qwen Agent Plugin v1 payload

**Decision**: Add a staged Qwen payload with root `plugin.json`, root `mcp.json`, and direct-child
skills for `agent-sessions` plus all four lane targets. `install-qwen` invokes the selected Qwen
binary's supported extension installer with the exact selected `QWEN_HOME`, explicit consent, and
user scope, then verifies the installed manifest version, enabled state, MCP inventory, and skills.
`install-all` includes the default-profile Qwen install. Non-default profiles require a separate
explicit install.

Managed launches still inject a private per-launch MCP capability/config. Installed inventory may be
visible in a bare Qwen session, but every operation returns inactive without exact process ancestry,
launch record, host registration, profile, and raw capability attestation.

**Rationale**: Qwen's supported extension mechanism owns lifecycle and discovery. Directly copying
files into `~/.qwen/skills` is not an installation contract. The existing Codex plugin manifest is
not a valid Qwen Agent Plugin manifest.

**Alternatives considered**:

- Auto-install at peer/lane startup was rejected because launch must not mutate the selected
  profile.
- Ambient installed MCP identity was rejected because bare sessions must remain an opt-out.

References: [extensions](https://qwenlm.github.io/qwen-code-docs/en/users/extension/introduction/),
[Agent Plugins v1](https://github.com/QwenLM/qwen-code/blob/v0.21.15/docs/users/extension/agent-plugins.md),
and [skills](https://qwenlm.github.io/qwen-code-docs/en/users/features/skills/).

## Decision 7: use stdio ACP for lane execution

**Decision**: The Qwen lane manager uses `qwen --acp` over private stdio, persists a native Qwen UUID
separately from the Agent Sessions lane UUID, and implements initialize, new/load/resume, prompt,
permission, cancellation, and terminal-observation transitions. The manager is the only ACP client.
It preserves Qwen's native default when no permission option is present, maps an explicit durable
launch preference to the initial native mode, and live-probes the ACP
mode/permission/cancel/resume contracts before advertising readiness. Later native mode changes are
Qwen behavior, not an Agent Sessions policy violation.

Detached local tool processes use the shared exact tool-root ledger. Admission closes before worker
retirement; PID/start/strong-start, raw capability, state revision, path type, inode/body, and process
ancestry are rechecked before signaling or deletion.

**Rationale**: Current Qwen ACP supports durable session load/resume and cancellation and is much
closer to Grok's execution manager than to Codex hooks. The manager is the protocol client and must
handle Qwen's permission requests, but it does not become a separate permission-policy authority.

**Alternatives considered**:

- `qwen serve` as the long-lived lane transport was rejected by the feature's no-long-lived-Qwen-
  service boundary.
- Copying Grok's tool ledger under Qwen names was rejected; the safety state machine is shared.

## Decision 8: use Qwen's native archive/unarchive model

**Decision**: Explicit Agent Sessions archive and resume use Qwen's native archive/unarchive
operations, following the Codex model. After the lane manager has canceled/collected work, closed the
writer, and retired the exact worker/tool tree, it starts a bounded private archive helper:

- `qwen serve --bare --hostname 127.0.0.1 --port 0 --require-auth --token <random> --no-web`
- exact selected `QWEN_HOME`, `QWEN_RUNTIME_DIR`, and workspace
- private lifecycle root and captured PID/start/strong-start
- capability check for `session_archive`
- authenticated workspace-qualified archive or unarchive request
- mandatory helper/preheated-child shutdown and exact residue sweep

Archive becomes committed only after Qwen reports the UUID archived and the helper tree is clean.
Resume unarchives first, starts exact ACP resume second, and re-archives by exact UUID/revision if the
resume transaction fails before native adoption. Any ambiguous external native change or partial
helper cleanup remains durable debt.

**Rationale**: Qwen `0.21.15` now has writer-leased, conflict-aware, idempotent archive/unarchive in
its public `qwen serve` control surface, with active/archived stores and explicit `session_archive`
capability. That is closer to Codex's foundation and satisfies the user's clarified rule. The helper
is ephemeral control-plane use, not a long-lived network service or federation transport.

**Alternatives considered**:

- The historical bridge-owned archive choice was discarded because current native evidence changed.
- Manual JSONL moves or imports of Qwen internals were rejected as unsupported and unsafe.
- A permanently running Qwen daemon was rejected as out of scope.

Reference: [Qwen serve protocol](https://github.com/QwenLM/qwen-code/blob/main/docs/developers/qwen-serve-protocol.md).

## Decision 9: leave in-session permissions to Qwen

**Decision**: Agent Sessions preserves Qwen's native default when the wrapper has no permission
option. Wrapper `--yolo` requests native yolo, while wrapper `--no-yolo` translates exactly to
native `--approval-mode default`. With no wrapper permission choice, a supported native
`--approval-mode MODE` passes through unchanged and its exact requested mode is retained for resume.
Wrapper/native or repeated/contradictory wrapper permission choices fail with exit 2 before any
mutation rather than using implicit precedence. The managed launch retains and passes the exact
request; Qwen's public interactive dual-output protocol does not expose an effective-mode event, so
Agent Sessions does not fabricate one. After publication, `/approval-mode`,
Shift+Tab, and ACP-native controls may change the mode in either direction, including entering or
leaving yolo. Agent Sessions does not add a sandbox, hook, deny list, tool guard, PTY filter, or other
permission-enforcement layer.

The durable catalog stores the requested launch preference for resume defaults. It is not a lifetime
security classification. Status reports the current native mode only when Qwen exposes a trustworthy
observation; otherwise it reports it as unknown rather than repeating the launch preference as if it
were current.

**Rationale**: Peer mode should preserve the native product's normal behavior. It augments Qwen with
authenticated communications and the ability to launch local or remote lanes for any supported
product; it does not replace Qwen's UI, tools, permission model, or session controls. Agent Sessions'
authority boundary remains exact managed-session attestation, groups, routing, lifecycle ownership,
and cleanup—not native tool approval.

**Alternatives considered**:

- Locking or emulating Qwen permissions with settings, hooks, sandboxing, deny lists, or an
  Agent Sessions TUI was rejected as unnecessary policy duplication and brittle duct tape.
- Treating the launch preference as the current mode forever was rejected because it would make
  status misleading after a native mode change.

Reference: [approval modes](https://github.com/QwenLM/qwen-code/blob/v0.21.15/docs/users/features/approval-mode.md).

## Decision 10: doctor uses active probes, not root-help text

**Decision**: `qwen-peer-lane doctor --json` starts no model session and reports executable/version,
profile and integration identity, non-secret credential/provider configuration state, trust,
dual-output parser contract, ACP initialize contract, archive capability, supervisor/runtime
readiness, and requested/expected initial permission mode. It reports configuration state as
`ready`, `unknown`, or `unready` rather than claiming live authentication. A real managed start is the
first provider-authentication and effective-initial-mode validation.

Parser probes use invalid/mutually-exclusive arguments that exit before a session. ACP readiness may
send `initialize` only—never session/new/load/resume/prompt—and inspect read-only status/settings
capabilities. Root `qwen --help` is not proof because the fast help path omits supported default-command
flags including ACP, approval, session ID, dual-output, input, and MCP options.

**Rationale**: Qwen has no supported headless authentication-status command and removed `qwen auth`.
Doctor must distinguish local configuration readiness from live provider validity and must not create
a transcript merely to diagnose.

**Alternatives considered**:

- Parsing root help was rejected as incomplete.
- Running an authenticated prompt in doctor was rejected because doctor must be non-mutating.

## Decision 11: publish real session-stable sockets, not symlink aliases

**Decision**: Every adapter-owned session-stable delivery path is a real Unix socket. The Qwen peer
uses that invariant from its first implementation, and the shared foundation replaces the existing
Codex/Grok `session-<hash>.sock -> <pid>.sock` symlink publication while preserving the stable
session address and exact process ownership. Legacy aliases remain recognizable only for
conservative local migration and cleanup after their exact stale backend is corroborated. This is
not a legacy transport or mixed-version compatibility path; obsolete binaries remain unsupported.

**Rationale**: The two-name design originally combined a PID-bound backend with a stable session
address. Current Claude rejects a native reply target when `lstat` identifies that stable address as
a symlink, producing asymmetric communication even though the target socket itself is healthy. A
caller-side `readlink` workaround spreads transport knowledge into every sender and leaves the
published contract internally inconsistent. A real stable socket preserves the intended identity
without requiring sender-specific path resolution; PID, process-start, state revision, and artifact
identity remain the lifecycle and cleanup authorities.

**Alternatives considered**:

- Advertising the PID-bound backend was rejected because it makes the public endpoint change across
  shim replacement and creates stale-address races.
- Teaching individual senders to resolve the symlink was rejected as duplicated policy and because
  native Claude deliberately refuses that shape.
- A permanent forwarding process was rejected as an unnecessary listener, hop, and lifecycle owner.

## Decision 12: federation remains capability-gated and exact-version

**Decision**: Add `qwen-lane` to the shared product/capability descriptor. The host advertises it only
when Qwen doctor proves the requested target contract. Remote launch uses the existing attested
ParentContext, explicit host selection, private parent/child anchors, and source runtime directory.
No local fallback occurs. Hubs and endpoints are upgraded together.

**Rationale**: Existing federation messages already carry arbitrary product and complete parent
context. A Qwen-specific remote protocol would weaken existing proofs.

**Alternatives considered**:

- Advertising capability from executable presence alone was rejected because initial-mode mapping,
  integration, profile, and ACP/archive contracts may still be unavailable.
- Backward compatibility with older hubs was rejected by scope and constitution.

## Decision 13: mandatory evidence and release gate

**Decision**: Automated coverage includes product-table completeness, native parser/protocol
fixtures, exact identity, initial-mode mismatches and native mode changes, publication rollback,
PID/path reuse, duplicate collection, native archive conflicts, helper/worker/manager/agent/supervisor restart, packaging, and
prebuilt installation. Real acceptance runs the complete Qwen matrix on Linux and macOS, all live
composition edges involving Qwen, and bidirectional remote Qwen lanes. Every test inventories owner
state and checks cleanup at return and +1/+5/+10/+30 seconds.

Release is prohibited if exact interactive mode mapping/retention, ACP lane corroboration, or either
platform gate fails.

**Rationale**: The dominant risk is native contract drift and detached process/artifact cleanup, not
product-neutral routing or Qwen's own permission choices.

**Alternatives considered**:

- Unit-only ACP/dual-output mocks were rejected as insufficient across the native-client boundary.
- Cross-compilation was rejected as a substitute for real macOS/Linux runtime evidence.

## Historical pre-design disposition

| Historical proposal | Disposition | Current reason |
|---|---|---|
| Two surfaces: `qwen-peer`, `qwen-peer-lane` | Retain | Matches current first-class product contract. |
| Reuse AgentFrame, groups, federation, no global namespace | Retain | Current shared layers are sufficient. |
| Interactive structured input + sidecar output | Revise | Keep dual-output v2; PTY launches must use `--json-file`, not inherited fd 3. |
| Exact UUID from `session_start`; reject continue/fork | Retain | Current Qwen supports exact session ID and resume. |
| Per-invocation MCP is the whole integration | Revise | Keep private capability injection, but also install profile-scoped plugin/skills explicitly. |
| Bridge-owned archive; no native archive | Discard | Current Qwen provides reliable archive/unarchive through capability-gated `qwen serve`. |
| Stdio ACP manager with distinct lane/native IDs | Retain with probes | Current ACP supports resume/cancel, but method and permission contracts are live-gated. |
| Fixed `QWEN_CODE_NO_RELAUNCH=1` behavior | Revise | Use only for safe parser probes; normal native relaunch behavior remains Qwen-owned. |
| `QWEN_HOME` as normal profile selection | Revise | Default remains unset; explicit override must be absolute, exact, installed, and persisted. |
| Generalize Grok tool-root ledger | Retain | Extract one shared primitive rather than copy it. |
| Two binaries / eleven-binary packages | Retain | Still the minimal public command surface. |
| Qwen-only optional install | Revise | Add explicit `install-qwen`; default-profile Qwen is also part of `install-all`. |
| Q-01 through Q-12 acceptance | Revise | Expand to current permissions, profile plugin, native archive, 4x4, crash, packaging, and two-platform contracts. |
