# Implementation Plan: Qwen Support

**Branch**: `develop` | **Date**: 2026-08-20 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-qwen-support/spec.md`

## Summary

Add Qwen Code as the fourth first-class Agent Sessions product with managed interactive peers,
durable ACP lanes, profile-scoped Agent Plugin installation, shared grouped messaging, 4x4
parent/target composition, and bidirectional federation. The implementation first replaces scattered
three-product switches with a shared product descriptor and extracts the exact tool-root ownership
ledger, then adds Qwen-specific dual-output, ACP, profile, archive, and readiness adapters behind the
existing AgentFrame/lane/federation contracts. As part of that shared foundation, it also replaces
the historical Codex/Grok session-socket symlink with a real socket at the stable published path and
applies that invariant to Qwen, while retaining exact process ownership and conservative cleanup of
legacy aliases.

Qwen Code `0.21.15` is the syntax/protocol floor and current research baseline. Its admitted native
archive/unarchive surface provides the required writer-lease, conflict, capability, and idempotence
contracts for a Codex-style transaction through a short-lived, token-authenticated loopback
`qwen serve` helper. Permission handling remains native: with no permission option Agent
Sessions preserves Qwen's default; explicit common flags select an initial approval mode that is
corroborated before publication. Agent Sessions then allows
Qwen's normal in-session controls to enter or leave yolo. The durable launch preference is not
misrepresented as a lifetime security boundary.

The governing compatibility rule is additive: peer mode preserves ordinary Qwen behavior and adds
authenticated Agent Sessions communications plus local or remote cross-product lane execution. It
does not otherwise replace Qwen's native UI, tools, permission system, or session controls.

## Technical Context

**Language/Version**: Go module language `1.22`; release validation with Go `1.26.5`; Qwen Code
`>=0.21.15` syntax/protocol floor plus operation-specific live probes

**Primary Dependencies**: Go standard library and existing internal Agent Sessions packages;
external Qwen Code CLI dual-output v2, stdio ACP v1, `qwen serve` archive capability, and Agent
Plugins v1; no new third-party Go runtime dependency is planned

**Storage**: Existing versioned JSON state/catalog files and private filesystem roots; Qwen-owned
JSONL transcripts under the exact selected `QWEN_HOME`/`QWEN_RUNTIME_DIR`; no database and no
Agent Sessions credential store

**Testing**: `./scripts/test`, `RACE=1 ./scripts/test`, `go vet ./...`, Makefile-managed
`make lint`, four target builds, native contract/integration fixtures, prebuilt install tests, the
actual `.github/workflows/ci.yml` build/release path, and real Linux/macOS live acceptance including
federation

**Target Platform**: Linux amd64/arm64 and macOS amd64/arm64; Windows out of scope

**Project Type**: Multi-command Go CLI/runtime with native-client plugins, durable local state, and
optional federated host agents

**Performance Goals**: Do not increase existing launch or control timeout constants; peer/lane
publication occurs only after native proof; direct/multicast/broadcast adds zero new hub round trips;
one active prompt per lane; event/result files and the ephemeral archive helper retain explicit
bounds; the complete SC-001 workflow finishes in under five minutes

**Constraints**: Exact identity and fail-closed Agent Sessions authorization; native Qwen owns
in-session tool permissions; no credential copying or implicit profile creation; no long-lived Qwen
network service; no legacy hub compatibility; published stable delivery endpoints are real Unix
sockets rather than symlinks; legacy alias handling is local stale-artifact reconciliation rather
than a compatibility transport; cleanup debt survives crashes; all behavior and packaging must pass
real Linux and macOS gates; GitHub release CI must use the authoritative product/package inventory
rather than a private hard-coded executable list; final proof follows
`contracts/release-evidence.md`

**Scale/Scope**: Four products, 16 parent-target combinations, eleven release binaries, four
platform archives, two public Qwen commands, one Qwen plugin payload, all live topology edges
involving Qwen, and one bidirectional remote Qwen lane per OS direction

## Constitution Check

*GATE: Passed before Phase 0 research and re-checked after Phase 1 design.*

| Principle | Pre-research gate | Post-design result |
|---|---|---|
| I. Shared Contracts, One Implementation | Qwen must reuse AgentFrame, groups, lane/federation contracts and close product-enumeration/tool-ledger duplication first. | Passed: shared product descriptor, generated/table-tested inventories, generalized preparation/tool-root ledger, established common lane parser, lane contract helpers, environment and permission packages, inventory-driven install projection, and a 100-token clone gate are explicit; native adapters remain separate only for genuine authority/lifecycle differences. |
| II. Exact Identity and Fail-Closed Safety | Native identity, profile, process, initial mode, socket/file, and host registration must agree before Agent Sessions authority or cleanup. | Passed: contracts require raw capability plus exact ancestry/strong-start/native/profile/catalog proof; stable published endpoints are real sockets; legacy aliases are removed only with exact stale-backend proof; ambiguous state becomes debt. Native permission changes do not grant Agent Sessions identity or bypass group authorization. |
| III. RCA Before Permanent Fixes | Historical design and current Qwen behavior must be independently evidenced; no retries or assumed compatibility. | Passed: `research.md` classifies every material pre-design proposal, explains Qwen's native mutable permissions and native archive foundation, and records the stable-symlink/native-sender refusal trigger, mechanism, invariant, and class-closing real-socket decision. |
| IV. Evidence-Driven Testing | Every new native/process/protocol boundary needs lowest-layer and adversarial integration evidence. | Passed: quickstart and contracts require parser/protocol fixtures, live native admission, real-socket Claude-to-Codex/Grok delivery, legacy-alias migration, crash/reuse/conflict/duplicate tests, packaging, and cross-host evidence. |
| V. Linux and macOS Parity | No Qwen feature can ship on single-platform or cross-compile evidence. | Passed: both OSes run source, native lifecycle, installation, permission, crash, and bidirectional federation cells with exact toolchain labels. |
| VI. Transactional Lifecycle and Zero Collateral | Publication/archive/resume/cleanup require durable state machines, exact rollback, and retryable debt. | Passed: generalized peer preparation, lane/turn state, native archive helper lease, tool-root ledger, and typed lifecycle debt are modeled explicitly. |
| VII. Explicit Protocols, Operability, Documentation | CLI/native/wire/install/help contracts and product docs must match current binaries; no legacy shim. | Passed: four interface contracts, symmetric Qwen guides, help completeness tests, eleven-binary packaging, and coordinated current-version federation are planned. |

No constitutional exception is requested. Qwen's mutable native permission mode is a documented
product difference and is deliberately outside Agent Sessions' routing/lifecycle authority boundary.

## Project Structure

### Documentation (this feature)

```text
specs/001-qwen-support/
├── plan.md
├── tasks.md
├── research.md
├── data-model.md
├── quickstart.md
├── evidence/
│   └── README.md
├── contracts/
│   ├── cli.md
│   ├── native-qwen.md
│   ├── attestation-permissions.md
│   ├── federation-install.md
│   ├── release-evidence.md
│   └── release-evidence.schema.json
└── checklists/
    └── requirements.md
```

`tasks.md` contains the dependency-ordered implementation and evidence work generated from this
design by `$agent-sessions:speckit-tasks`.

### Source Code (repository root)

```text
cmd/
├── qwen-peer/
│   └── main.go
└── qwen-peer-lane/
    └── main.go

internal/
├── launcher/
│   ├── product.go                  # launcher projections/help coverage
│   ├── peer_context.go             # existing common group/yolo/name resolution
│   ├── peer.go                     # generic Qwen resume dispatch
│   ├── qwen_peer.go
│   └── qwen_peer_test.go
├── bridge/
│   ├── product.go                  # runtime/MCP/lane descriptor projections
│   ├── tool_root_ledger.go         # extracted Grok/Qwen exact ownership
│   ├── qwen.go                     # MCP/launch attestation and delivery host
│   ├── qwen_diagnostics.go
│   ├── qwen_plugin.go              # profile-scoped install/verify/upgrade safety
│   ├── qwen_archive.go             # bounded native archive transaction
│   ├── qwen_lane.go
│   ├── qwen_lane_manager.go
│   └── qwen_*_test.go
├── federator/
│   ├── product.go                  # authoritative shared product descriptor
│   ├── protocol.go                 # qwen-lane capability via descriptor
│   ├── registration.go             # generalized launch preparation/reconcile
│   ├── agent.go
│   └── lane.go
├── qwenprofile/
│   ├── profile.go                 # exact native profile identity/fingerprint
│   └── profile_test.go
├── qwenreadiness/
│   ├── readiness.go               # sole Qwen session-free evidence engine
│   └── readiness_test.go
├── pathidentity/                   # shared canonical existing/future path identity
├── socketpath/                     # shared Linux/macOS sockaddr_un byte budgets
├── testutil/                       # short private socket roots for cross-platform tests
└── procinfo/                       # shared strong identity and observable environment

qwen/
├── plugin.json                     # Agent Plugin v1
├── mcp.json
└── skills/
    ├── agent-sessions/SKILL.md
    ├── codex-lane/SKILL.md
    ├── claude-lane/SKILL.md
    ├── grok-lane/SKILL.md
    └── qwen-lane/SKILL.md

skills/
└── qwen-lane/
    ├── SKILL.md
    └── agents/
        └── openai.yaml

claude/skills/qwen-lane/
grok/skills/agent-lanes/

docs/
├── QWEN-ADAPTER.md
├── QWEN-INSTALL.md
├── QWEN-LANES.md
├── ACCEPTANCE-MATRIX.md
├── INSTALL.md
├── GROUPS.md
├── FEDERATION.md
└── federation/

deploy/peer-federator/              # Qwen launcher examples/environment
scripts/
├── test                            # product/package/prebuilt assertions
├── package-release                 # eleven-binary archive
└── federation/                     # grouped Qwen fixtures

Makefile                            # build/install/dev/prebuilt Qwen targets
.github/workflows/ci.yml            # authoritative CI build, evidence, and tag-release path
README.md                           # four-product overview and safety guidance
```

**Structure Decision**: Extend the current single Go module and its existing product plugin trees.
Thin `cmd/` entry points call shared `internal/launcher`/`internal/bridge` behavior. Product-neutral
catalog, groups, MCP routing, ownership, notices, and federation stay in their existing packages;
only Qwen's native dual-output, ACP, profile, readiness, and archive control live in Qwen-specific
files. Session-free readiness is owned once by `internal/qwenreadiness`; launch admission,
federation advertisement, and doctor consume that package rather than duplicating probes in generic
federator code. A dedicated `qwen/` Agent Plugin payload is required because Qwen's manifest format
differs from the existing Codex, Claude, and Grok payloads.

## Design Sequence

1. **Shared product completeness**: introduce the product descriptor and table-driven completeness
   gates before adding Qwen values. Refactor MCP inventories onto the single product-neutral
   `agent_sessions` namespace, runtime roles, parent inference,
   federation capability/launcher selection, package lists, help, and docs/skill target inventories
   to consume or validate that source.
2. **Shared lifecycle and transport primitives**: generalize the gated peer preparation with product
   validators; extract the Grok tool-root ledger; replace Codex/Grok stable symlink aliases with real
   session-named sockets; centralize canonical path identity, platform socket budgets/short test
   roots, and fail-closed process observability; retain conservative local legacy-alias cleanup; and
   preserve every existing Claude/Grok contract except that intentional endpoint-type correction.
3. **Qwen installation and readiness foundation**: add Agent Plugin v1
   staging/install/verification and implement one Qwen-specific session-free parser/ACP/read-only
   evidence engine in `internal/qwenreadiness`. Launch admission, capability advertisement, and the
   later operator doctor consume that engine. No probe claims that the selected launch preference
   remains the current mode after launch.
4. **Interactive peer**: implement dual-output/input transaction, MCP attestation, exact resume,
   exact native-mode request retention, real session-stable delivery socket, delivery, and honest
   current-mode reporting when observable. Do not interpose on Qwen's native in-session permission
   controls.
5. **Qwen lane foundation**: implement stdio ACP manager, exact parent/groups/notices, serialized
   turns, collection, interruption, owner/persistence policy, initial native-mode mapping, and crash
   recovery. Use one shared structured ACP MCP-server constructor for Qwen and Grok headless sessions,
   inject the current runtime explicitly at session create/resume, and require live-session identity
   readiness without relying on an operator-profile plugin.
6. **Native archive transaction**: implement the bounded authenticated helper, capability gate,
   archive/unarchive CAS, compensation, helper-child cleanup, external-conflict handling, and
   idempotence.
7. **Federation and composition**: advertise Qwen only after local readiness, extend all parent/target
   surfaces, and prove source runtime collection pointers and destination-only execution.
8. **Operator diagnostics, packaging, documentation, acceptance, release**: expose the shared
   readiness evidence through stable doctor output; build eleven binaries; install all four plugin
   surfaces; make `.github/workflows/ci.yml` consume the same authoritative executable/plugin
   inventory as local packaging; freeze symmetric docs/help and v0.2.4 metadata; run the full
   rehearsal matrix and commit its in-tree evidence in a signed `main` release commit; then, without
   changing that tree, rerun the complete automated, package, real Linux/macOS, federation, and
   prebuilt-install gates at the exact commit. Emit the exact external evidence artifact defined by
   `contracts/release-evidence.md`, bind its workflow run/name/digest in the signed tag, attach the
   unchanged artifact to the release, publish only artifacts rebuilt from that commit, and
   synchronize `develop` to `main` without changing the tagged tree.

## Phase 0 Output

[research.md](research.md) resolves the native version, transport, profile, installation, ACP,
archive, permission, process-ownership, federation, and historical-design questions. There are no
remaining design ambiguities. Qwen's mutable in-session permission behavior is accepted explicitly
and requires no Agent Sessions enforcement layer.

## Phase 1 Outputs

- [data-model.md](data-model.md): durable entities, validation rules, and state transitions.
- [contracts/cli.md](contracts/cli.md): public command/help/exit contracts.
- [contracts/native-qwen.md](contracts/native-qwen.md): dual-output, ACP, archive helper, profile, and
  live-probe contracts.
- [contracts/attestation-permissions.md](contracts/attestation-permissions.md): managed authority,
  native permission ownership, reporting, and cleanup proofs.
- [contracts/federation-install.md](contracts/federation-install.md): remote capability, package, and
  plugin-install contracts.
- [contracts/release-evidence.md](contracts/release-evidence.md) and
  [contracts/release-evidence.schema.json](contracts/release-evidence.schema.json): exact final
  workflow evidence representation, machine-validatable schema, artifact identity, pre-tag collision
  rule, tag binding, and GitHub release publication contract.
- [quickstart.md](quickstart.md): two-platform validation and release guide.
