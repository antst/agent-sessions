# Implementation Plan: Unified User Daemon

**Branch**: `feature/unified-user-daemon-v2` | **Date**: 2026-08-29 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-unified-user-daemon/spec.md`

## Summary

Refactor the working `c056fbc` implementation into one service-managed `agent-sessions` host daemon
without redesigning its four native-product integrations. The work uses a parity-first strangler
sequence: inventory old symbols and regressions, transplant those regressions unchanged, identify
genuinely shared invariants, extract one shared implementation, relocate Agent Sessions ownership into
the daemon, prove real installed parity, and only then remove the superseded process boundary.

The release contains two executable images: `agent-sessions` for the host daemon and its short-lived
multi-call clients, and `agent-sessions-hub` for the independently deployed central hub. Vendor
executables and required vendor processes remain external. Vendor-required MCP processes become
stateless relays where possible, but continue to carry the exact product-specific evidence established
by the baseline implementation.

## Technical Context

**Language/Version**: Go 1.22 or the repository's newer pinned Go version; Bash and Python 3 for install,
packaging, and live acceptance

**Primary Dependencies**: Go standard library; existing `golang.org/x/sys`, `spf13/pflag`; pinned
`gopkg.in/yaml.v3` for build/test-time acceptance and port-map manifests; native Codex App Server,
Claude registry/socket integration, Grok ACP/private leader, Qwen daemon/ACP artifacts, systemd user
services, launchd user agents

**Storage**: Existing bounded atomic Agent Sessions JSON records and journals, consolidated under one
host state root; vendor profiles, credentials, transcripts, histories, and native registries remain
vendor-owned

**Testing**: Baseline Go regressions transplanted before refactoring; normal/race/vet/lint; four platform
builds; real authenticated four-product interactive and lane matrices; Linux/macOS service acceptance;
cross-host federation; exact process and residue census

**Target Platform**: Linux and macOS on amd64 and arm64

**Project Type**: Two-binary Go system with one host multi-call image and one independent hub image

**Performance Goals**: Preserve the baseline workflows and their existing bounded waits; this refactor
introduces no new capacity target or recovery-time service level

**Constraints**: One Agent Sessions daemon and endpoint per OS user-host; no additional collaboration
namespace; no artificial quotas; no credential or transcript content in Agent Sessions state or logs;
no product-parity credit from fake vendors; one repository-only pre-unification cleanup script for the
three controlled hosts, with no operational Make target, release payload, installer, updater, remover,
or service integration and no Makefile reference to the cleanup artifacts; generic test discovery may
execute it only against fixture-owned roots

**Scale/Scope**: Four interactive products, sixteen parent-target lane combinations, one hub with
multiple host agents, three controlled development hosts for the greenfield transition

## Constitution Check

*GATE: Must pass before implementation and be rechecked after design.*

| Principle | Plan response | Gate |
|---|---|---|
| I. Shared Contracts, One Implementation | Shared behavior is extracted only after the port map proves equivalence; product differences remain explicit | PASS |
| II. Exact Identity and Fail-Closed Safety | Existing product evidence is retained and mapped before any replacement; all mutation uses exact identity | PASS |
| III. RCA Before Permanent Fixes | Every parity RED requires product/environment/harness classification before code changes | PASS |
| IV. Evidence-Driven Testing | Old regressions precede refactoring; real installed evidence precedes deletion | PASS |
| V. Linux and macOS Parity | Both platforms must pass at the same release candidate; no cross-compile waiver | PASS |
| VI. Transactional Lifecycle and Zero Collateral | Shared state machines commit ownership before acceptance and retain exact cleanup debt | PASS |
| VII. Explicit Protocols and Documentation | CLI, hooks, MCP, adapters, storage, hub protocol, and service lifecycle have explicit contracts | PASS |

No constitution exception is required.

## Design Method: Baseline-First Strangler

### Phase 0 — Freeze executable behavior

1. Record every relevant old function and regression from `c056fbc` in the authoritative
   `contracts/baseline-port-map.yml`; maintain `contracts/baseline-port-map.md` only as its review
   projection.
2. Validate that `contracts/acceptance-matrix.yml` expands to the closed 202 unique cell IDs, nonempty
   platform scopes, uniquely resolvable assertion locators, an acyclic known-ID prerequisite graph,
   and a reviewed topology-delta ledger limited to Agent Sessions process/service/package observations;
   bind each cell to its exact old test symbols, scripts, native commands, expected artifacts, and
   cleanup assertions. For restart and reconnect cells, capture the exact existing state predicate and
   existing deadline from the authoritative baseline test or script; do not create an aggregate timing
   target. Record these per-cell bindings in `evidence/baseline-functional-cells.md`.
3. Run the baseline gates before changing runtime behavior. A pre-existing failure is classified; it
   is not silently normalized into the new design.

### Phase 1 — Transplant regressions without changing behavior

1. Transplant the complete mapped Codex, Claude, Grok, and Qwen regression families before extracting
   any shared production mechanism. The tests may use a behavior-preserving seam, but default callers
   and native behavior remain unchanged.
2. Preserve exact vendor argv, environment presence, profile paths, process ancestry, settings bytes,
   native session selection, permission semantics, delivery carrier, archive behavior, and cleanup.
3. Add missing event-specific hook/MCP and real-installed entry tests before touching the corresponding
   path.
4. Run all four transplanted families together. No product is allowed to define a supposedly shared
   lifecycle before the other three products can disprove it.

### Phase 2 — Extract proven shared behavior

Extract one implementation only for invariants demonstrated across products:

- wrapper option parsing framework while retaining product option tables and ordering rules;
- attachment preparation/adoption/refresh/detach transaction skeleton;
- exact process and filesystem identity primitives;
- connector relay framing and daemon reconnect.

The extraction keeps callbacks or adapter methods for every native difference named in the port map. A
product does not conform to a shared lifecycle merely because another product does.

Global groups, AgentFrame routing, delivery acceptance, lane turn ownership, notices, collection,
service control, diagnostics, release transactions, connector installation, packaging, and network
federation remain later story-owned shared targets. Local messaging and lane behavior move in User
Story 2 only after all four interactive adapters are green. Each later target is extracted only after
its own failing lowest-layer contract tests in the corresponding story phase.

Before the first product cutover, this phase also composes the minimum runnable `agent-sessions`
daemon: fixed endpoint, generation and state-store ownership, attachment transaction, connector relay,
admin readiness, and the short-lived multi-call command surface. Product adapters can then be exercised
against a real daemon process rather than an in-memory future interface. Standard service installation
and full restart/upgrade policy remain Phase 3 acceptance work.

### Phase 3 — Introduce the daemon alongside the working paths

1. Add the daemon state/control foundation without deleting old launchers or bridge logic.
2. Route one product at a time through daemon-owned Agent Sessions state while calling already proven
   native integration functions.
3. Run that product's old unit regressions and real installed interactive matrix.
4. After all four interactive adapters pass, move local messaging and lane ownership once in User
   Story 2 and run the complete lane, composition, messaging, and archive matrices.
5. Do not proceed to the next product while a genuine parity RED remains.

Product order is Codex, Claude, Grok, Qwen only for dependency clarity; it does not grant earlier
products permission to define later products' native behavior.

### Phase 4 — Move shared collaboration and federation ownership

AgentFrame, global-group routing, local accepted delivery, and lane state are moved exactly once when
User Story 2 is implemented. This phase moves only the remaining network host registration,
reconnection, remote delivery/lane transport, and hub lifecycle into daemon-owned and hub-owned
components. Keep the central hub as `agent-sessions-hub`, sharing logical protocol/routing/storage/
service packages. Validate equal-protocol unrelated-build interoperability and mismatch refusal.

### Phase 5 — Cut over and remove obsolete topology

The old supervisor, shims, product hosts, lane managers, local router, and standalone host federator may
be removed only after:

1. their port-map rows are complete;
2. old and new regression suites pass;
3. the product's real installed Linux and macOS cells pass;
4. daemon restart and exact cleanup discriminators pass; and
5. the deletion diff contains no unmapped baseline symbol or test.

Then install two release images, run the greenfield service transition, and execute the full final
matrix at one signed candidate. The version 0.3 installer does not discover or reason about that old
topology: the operator or acceptance harness establishes quiescence before invoking it. For the three
controlled hosts only, the operator may establish that precondition by directly running the
repository-only `scripts/cleanup-pre-unification`. Its sole target authority is
`contracts/pre-unification-cleanup.yml`. The operator first runs the no-argument plan-only mode and then
runs `--apply <plan-revision>` using the metadata-only revision emitted by the reviewed plan. Apply
recomputes the complete selection and refuses all mutation unless its revision is identical, then
revalidates each target's metadata revision immediately before deletion. The script and contract are not shipped and are not reachable from operational Make,
install, update, remove, or service paths. Generic test discovery may execute the script only against
fixture-owned roots. Its exact allowlist may delete all opaque data produced and owned solely by the
legacy implementation, but it never reads that content and never selects vendor-owned transcripts,
histories, credentials, non-Agent-Sessions settings, or ordinary files.

## Product Port Boundaries

### Codex

Reuse the existing launcher parser and bridge launch/App Server logic as the starting implementation.
Move App Server client coordination, interactive owner state, hook/MCP attestation, delivery, and lanes
into daemon ownership without losing lazy App Server start, native `--remote` behavior, name/UUID
resolution, cwd validation, history readiness, or archive semantics.

### Claude

Reuse the existing gated launcher and native registry/socket lifecycle. Move authoritative Agent
Sessions records and shared delivery/lane state into the daemon while preserving profile-variable
presence, secure-storage namespace, settings merge, permission constraints, native row publication,
late selection, sidecars, rollback, and cleanup.

### Grok

Reuse the existing launch-token and exact owner/host/leader/MCP ancestry model plus ACP roster,
interjection, and resume behavior. Moving host responsibilities into daemon goroutines must not replace
the native process tree with an invented daemon-child assumption. Any stateless helper retained for a
vendor boundary has no Agent Sessions registry or listener.

### Qwen

Reuse the existing profile/readiness, launch capability, dual-output, native daemon/ACP ancestry,
event/input artifact, archive, rollback, and cleanup behavior. Consolidate shared attachment and lane
ownership only after Qwen's existing regressions and installed flows pass.

## Project Structure

### Documentation

```text
specs/002-unified-user-daemon/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── baseline-port-map.md
│   ├── baseline-port-map.yml
│   ├── acceptance-matrix.yml
│   ├── pre-unification-cleanup.yml
│   ├── adapter-runtime.md
│   ├── local-control-protocol.md
│   ├── federation-protocol.md
│   └── acceptance.md
├── checklists/requirements.md
├── evidence/                 # per-cell, per-platform, cleanup, and final proof artifacts
└── tasks.md
```

### Source Code

```text
cmd/
├── agent-sessions/           # host daemon plus short-lived multi-call commands
└── agent-sessions-hub/       # independent central hub

internal/
├── acceptance/               # per-cell result and matrix-runner contracts
├── launcher/                 # baseline-native argv/profile/gating logic; thin after cutover
├── bridge/                   # baseline product integrations, progressively made callable
├── clihelp/                  # one authoritative multi-call command/help inventory
├── daemon/                   # one host authority and in-process composition
├── diagnostics/              # metadata-only output
├── federation/               # shared host/hub wire, groups, routing, and registry logic
├── pathidentity/             # exact filesystem identity
├── procinfo/                 # exact process identity
├── productcatalog/           # one supported-product and capability inventory
├── releaseevidence/          # manifests, traceability, archive, and cleanup-contract validation
├── releaseinstall/           # host/hub install, update, service restart, and removal
├── releasepkg/               # two-image/four-platform package inventory
├── securityboundary/         # credential/message/transcript content canaries
├── servicecontrol/           # shared role-driven systemd/launchd operations
├── statestore/               # bounded atomic Agent Sessions state and journals
└── testutil/                 # test-owned helpers, never fake product acceptance

deploy/
├── agent-sessions/
└── agent-sessions-hub/

scripts/
├── cleanup-pre-unification       # direct, one-time utility; no operational Make target
├── package-release               # four-platform two-image packaging
├── test-daemon-restart           # installed restart/recovery acceptance
├── test-real-products            # authenticated product acceptance by exact cell ID
└── federation/
    └── binary_pair_test.py       # equal/mismatched hub-protocol binary pairs
```

**Structure Decision**: Preserve `internal/launcher` and `internal/bridge` during migration so old code
remains executable documentation. Refactor reusable logic into callable functions and shared logical
packages; delete obsolete command/process boundaries only at the final parity gate.

## Testing Order and Credit

1. Port-map schema/deletion validator and the closed 202-cell manifest/cardinality validator.
2. All four products' old named unit/regression tests transplanted without weakened assertions.
3. New shared invariant tests across every applicable product, followed by the shared extraction.
4. Minimal runnable daemon/control/client tests, then product adapter integration against realistic
   native protocol fixtures.
5. Real installed authenticated interactive product cells, reported by stable ID.
6. Real installed lane and messaging cells, reported by stable ID.
7. Daemon restart/crash/recovery and exact residue tests.
8. Linux/macOS service-controller, diagnostics/content-canary, release transaction, connector rollback,
   package inventory, system service, and cross-host federation tests at their lowest useful layer
   before the corresponding implementation or live acceptance.
9. Full normal/race/vet/lint/build/release gate and an exact 202-cell accounting report.

Fake vendor helpers are limited to failure injection and protocol framing. They cannot satisfy steps 4
through 7 and cannot mark product tasks complete.

## Test-First Subsystem Gates

- Service management: Linux systemd and Darwin launchd contract, explicit-stop, restart, failed-update
  preservation, and unrelated-service preservation tests precede service-controller implementation.
- Diagnostics: schema and content/credential canaries precede the existing status, doctor, log, and
  crash-diagnostic surfaces; this feature adds no metrics or tracing product surface.
- Release and connectors: staged validation, failed-update preservation, optional-product, supported
  native installer, exact removal, and no-credential-access tests precede install code. The design does
  not require a versioned release-pointer or compare-and-swap subsystem.
- Controlled-host cleanup: allowlist, plan-before-mutation, path/type/ownership, opaque deletion of
  legacy-owned operational data without content access, metadata revision revalidation immediately
  before removal, default plan-only behavior with deterministic non-content revision, explicit
  `--apply <plan-revision>`, zero mutation when the recomputed complete plan differs, repeat-safety,
  unrelated-state preservation, and vendor credential/transcript/history/file canary tests precede the standalone
  script. Negative dependency tests prove no operational Make target, install, update, remove, service,
  or packaging path can reach it. Ordinary test discovery may exercise it only with fixture-owned roots.
- Packaging: two-binary/four-platform archive inventory and prebuilt no-Go install tests precede final
  package targets.
- Acceptance: every runner validates requested IDs against the 202-cell manifest and emits one record
  per cell; family summaries are informational only.

## Complexity Tracking

No constitution violation is accepted. Product-specific adapter code is necessary because the four
native contracts differ; each difference must be named in the port map and covered by a regression.
