# Implementation Plan: Unified User Daemon

**Branch**: `feature/unified-user-daemon` | **Date**: 2026-08-26 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-unified-user-daemon/spec.md`

## Summary

Converge the existing Agent Sessions host runtime into one service-managed Go daemon per OS user on
each host. The daemon will embed the current local routing/catalog, outbound federation agent, Codex
supervisor behavior, interactive adapter coordination, delivery, and all four lane managers as
in-process components using one private Unix socket and one durable state authority. Existing native
Codex, Claude, Grok, and Qwen processes remain external and vendor-owned. Vendor-required MCP stdio
processes remain only as stateless relays to the daemon. Short-lived peer, lane, administration, and
connector commands use one installed multi-call host executable so every host-side Agent Sessions code
path comes from one release image. The central federation role is a second, hub-only
`agent-sessions-hub` executable with no host adapters or runtime authority. This repository builds
both executables from one hub-protocol contract, while deployed hub and host builds may come from
unrelated commits or releases whenever they declare the same hub-protocol version. Their service
lifecycles are independent; host capability
advertisement remains an operation-availability inventory, not a release-coupling handshake.
Both roles use the same logically organized service-control and immutable-release transaction
packages; role descriptors provide different executable, service, configuration, readiness, and
connector/migration hooks without creating separate lifecycle implementations. A shared immutable
transaction algorithm does not mean shared deployment state: host and hub use separate role-owned
release roots, current selections, locks, journals, service transitions, readiness decisions,
rollback, removal, and purge ownership.

The implementation will reuse the existing product state machines rather than rewrite product
behavior. It will replace their detached process, per-session listener, and duplicated ownership
boundaries with daemon-owned attachment and lane actors. First migration requires an operator-owned
maintenance window: close all legacy peers and lanes, explicitly stop every responsive legacy
supervisor, product manager, and federation authority through its old supported lifecycle, and prevent
new legacy launches until installation completes. The installer fails closed naming any live legacy
authority even when it reports zero shims; it never stops, signals, or restarts one. After proving
absence, it stages an immutable release, adopts Agent Sessions metadata, retires exact legacy artifacts,
and performs the unified service transition. Neither live handoff nor a compatibility drain protocol is
planned.

## Technical Context

**Language/Version**: Go 1.22; Bash and Python 3 remain for repository install, packaging, and live
acceptance scripts

**Primary Dependencies**: Go standard library; existing `golang.org/x/sys`, `spf13/pflag`, JSON Schema,
and JSON canonicalization dependencies; systemd user manager on Linux; launchd user agent on macOS;
existing vendor-native Codex, Claude, Grok, and Qwen interfaces

**Storage**: Existing user-owned atomic JSON records, bounded append/spool files, revision tokens, and
transaction journals consolidated beneath the Agent Sessions state root; vendor profiles, credentials,
and transcripts remain external

**Testing**: Go unit and integration tests through `scripts/test`, race suite, `go vet`, repository
`golangci-lint`, canonical parser/help/documentation completeness, active-peer installer-upgrade
reconstruction, closed observability-content canaries across daemon, service-manager, status/doctor,
metrics, traces, and crash-report boundaries, four release-platform builds,
two-binary packaging/install tests, same-protocol arbitrary-SHA and protocol-mismatch federation
integration, product contract scripts, complete 4×4 lane composition with an active-turn daemon
restart in every parent-target cell, mixed-workload 100-attachment stress, and real installed
Linux/macOS service acceptance

Observability completeness uses a test-owned manifest assembled incrementally as boundaries are
implemented: Foundation declares host-core sinks, US1 adds systemd/launchd capture, and US4 adds hub
sinks. The final aggregate gate validates the union. This manifest is test evidence, not a production
sink registry or a new runtime subsystem. Its shared API lives in `internal/testutil`, with
file-disjoint core, Linux-service, Darwin-service, and hub fragments so parallel tasks never edit one
manifest file and cross-package tests consume one DRY applicable-platform union.

**Target Platform**: Linux and macOS on amd64 and arm64; systemd user service on Linux and launchd user
agent on macOS; Windows is out of scope

**Project Type**: Two-binary Go system: one multi-call `agent-sessions` host image for the per-user
daemon, thin local aliases, and connector payloads; one separately deployed `agent-sessions-hub`
central-hub image

**Performance Goals**: At least 100 simultaneous managed attachments; local peer recovery within 10
seconds of daemon restart; cross-host recovery within 30 seconds; no duplicate accepted message or lane
dispatch; no artificial Agent Sessions capacity quota

**Constraints**: Exactly one authoritative host daemon and one Agent Sessions-owned local listener per OS
user on each host; no launcher-driven daemon startup; metadata-only diagnostics; exact process and
filesystem identity before mutation; no credential/profile/transcript ownership; existing global-group,
host-suffixed-name, one-hub/multiple-agent behavior remains unchanged

**Scale/Scope**: Four native products, every parent-target lane combination, local and remote routing,
legacy split-runtime migration, optional installation of any product subset, and four release platforms

## Constitution Check

*GATE: Passed before Phase 0 and re-checked after Phase 1.*

- **I. Shared Contracts, One Implementation — PASS**: one daemon composition root, attachment model,
  local protocol, delivery engine, lane contract, product inventory, shared host/hub service-control
  and release-transaction packages, and migration engine replace the repeated process-local
  implementations. Role- and product-specific code remains separate only for genuine contract
  differences.
- **II. Exact Identity and Fail-Closed Safety — PASS**: local clients are kernel-attributed, managed
  attachments retain product-specific corroboration, and migration/cleanup re-attest PID plus process
  start, socket/file identity, native identity, and durable revision immediately before mutation.
- **III. Root-Cause Analysis Before Permanent Fixes — PASS**: the plan closes the observed class of
  missed restarts and mixed supervisor/shim/host/manager/federation versions by removing those separate
  authorities, not by adding another predecessor-discovery workaround.
- **IV. Evidence-Driven Testing — PASS**: the design requires focused state-machine tests, exact process
  censuses, transition failure injection, full product matrices, service-manager tests, and live
  Linux/macOS evidence.
- **V. Linux and macOS Parity — PASS**: systemd and launchd contracts are explicit; AF_UNIX limits,
  filesystem aliases, process visibility, sleep/wake, install, restart, and removal are tested on real
  installations of both systems.
- **VI. Transactional Lifecycle and Zero Collateral — PASS**: durable acceptance precedes publication;
  install and migration are journaled; the operator owns legacy shutdown while the installer owns
  absence verification and exact artifact retirement; first-migration rollback leaves unified and
  legacy authorities stopped and restores only installer-changed surfaces; exact cleanup is
  idempotent; vendor and unrelated state are excluded.
- **VII. Explicit Protocols, Operability, and Documentation — PASS**: the local control protocol,
  service lifecycle, adapter boundary, storage migration, diagnostics, authoritative CLI/help
  inventory, exit classes, environment inputs, and operator actions are documented in Phase 1
  contracts and checked for parser/help/documentation parity.

The host and hub are separate deployment roles built from this repository. Their software-version
interoperability uses only exact hub-protocol-version equality; commit, release, executable, build
age, and upgrade order do not participate. Tests cover unrelated host/hub build identities with an equal protocol and
fail-closed protocol mismatch. No interoperability with pre-unification executables or process
topology is required beyond the explicit quiescent migration contract.

No constitutional exception is required. The central federation hub remains a separate deployment
role because this feature replaces the per-host federation agent, not the existing one-hub topology.

## Project Structure

### Documentation (this feature)

```text
specs/002-unified-user-daemon/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── adapter-runtime.md
│   ├── cli-surface.md
│   ├── federation-protocol.md
│   ├── local-control-protocol.md
│   ├── migration-and-storage.md
│   └── service-lifecycle.md
└── tasks.md                    # dependency-ordered implementation and acceptance ledger
```

### Source Code (repository root)

```text
cmd/
├── agent-sessions/             # host image: daemon, clients, connectors, aliases
└── agent-sessions-hub/         # central federation hub only

internal/
├── clihelp/                    # one mode/alias/option/environment/JSON/exit descriptor inventory
├── productcatalog/             # shared products, roles, capabilities, protocol and release inventory
├── servicecontrol/             # shared systemd/launchd operations driven by role descriptors
├── releaseinstall/             # shared immutable stage/commit/readiness/rollback/remove engine
├── statestore/                 # shared atomic records, revisions, journals, and corruption handling
├── diagnostics/                # shared metadata-only envelopes, redaction, and bounded errors
├── federation/                 # groups, wire protocol, host connection, hub routing, remote dispatch
├── daemon/
│   ├── runtime.go              # one process composition root and subsystem lifecycle
│   ├── control.go              # one private local listener and role-scoped dispatch
│   ├── state.go                # host runtime schemas composed over statestore
│   ├── attachment.go           # shared live attachment ownership and attestation
│   ├── delivery.go             # durable AgentFrame admission and routing
│   ├── lanes.go                # shared daemon-owned lane actors
│   ├── federation.go           # embedded host agent and outbound hub connection
│   ├── migration.go            # quiescence gate, durable adoption, retirement, debt
│   └── diagnostics.go          # host status/doctor projection over shared diagnostics
├── bridge/                     # existing native product engines refactored into callable adapters
├── launcher/                   # short-lived clients that prepare and then exec native vendors
├── releasepkg/                 # two-binary archive and immutable release validation
├── releaseevidence/            # obsolete-topology and release-completeness assertions
├── testutil/                   # in-process daemon fixtures and test-only observability manifest fragments
├── fileutil/
├── pathidentity/
├── procinfo/
├── socketpath/
└── product profile/readiness packages

deploy/
├── agent-sessions/
│   ├── VERSION
│   ├── systemd/user/agent-sessions.service
│   └── launchd/net.antst.agent-sessions.plist
└── agent-sessions-hub/
    ├── systemd/user/agent-sessions-hub.service
    ├── systemd/user/hub.env.example
    └── launchd/net.antst.agent-sessions-hub.plist

scripts/
├── native-entry               # thin MCP connector entry into the canonical host binary
├── release-inventory
├── package-release
├── test
└── live product/service acceptance scripts
```

**Structure Decision**: Introduce `internal/daemon` as the only Agent Sessions host authority and
composition root. Refactor, rather than duplicate, existing code in `internal/bridge` and
`internal/federator`. In-process host subsystems call shared Go APIs directly; only short-lived
external clients and vendor-mandated connectors use the local socket. Build one canonical
`agent-sessions` host binary and install every existing product command name as a filesystem link or
equivalent `argv[0]` alias to that exact image; do not build independent host-side thin executables.
Build the central hub separately as `agent-sessions-hub`, importing only the shared federation
wire/identity/routing contracts and hub implementation from this repository. It is never a per-host
authority, does not share host service lifetime, and interoperates with same-repository host builds
from any commit solely through exact hub-protocol-version equality. Host-advertised product
capabilities control operation availability, not software-version interoperability. Internal package
boundaries follow logical function rather than executable consumer: `internal/servicecontrol`,
`internal/releaseinstall`, `internal/statestore`, `internal/diagnostics`, and
`internal/federation` implement shared mechanics once. `internal/daemon` is only the host composition
root; hub-specific behavior is a component of the logical federation package rather than a parallel
consumer-owned internal tree.
Shared package use never implies a shared deployment transaction: co-located host and hub roles select
and mutate disjoint role-owned release roots.

## Phase 0 Research Decisions

The decisions and rejected alternatives are recorded in [research.md](research.md). All technical
context ambiguities are resolved.

## Phase 1 Design

- [data-model.md](data-model.md) defines daemon generations, attachments, deliveries, lane transactions,
  federation state, migration candidates, and cleanup debt.
- [contracts/local-control-protocol.md](contracts/local-control-protocol.md) defines the single private
  endpoint, client roles, attestation, request/response envelope, and unavailable behavior.
- [contracts/cli-surface.md](contracts/cli-surface.md) defines the canonical modes and aliases, shared
  descriptor inventory, help/parser parity, environment inputs, stable JSON surfaces, and exit classes.
- [contracts/federation-protocol.md](contracts/federation-protocol.md) defines the explicit one-hub
  wire contract, exact protocol-version interoperability rule, build-independent deployment, and host
  capability advertisement without promising pre-unification software compatibility.
- [contracts/adapter-runtime.md](contracts/adapter-runtime.md) defines the shared daemon-owned adapter
  lifecycle and the genuine vendor-specific boundaries.
- [contracts/service-lifecycle.md](contracts/service-lifecycle.md) defines systemd/launchd ownership,
  transactional installation, restart, explicit stop, removal, and rollback.
- [contracts/migration-and-storage.md](contracts/migration-and-storage.md) defines the canonical state
  root, revision rules, operator-owned maintenance window, fail-closed legacy absence proof, adoption,
  exact artifact retirement, stopped-authority rollback, and purge exclusions.
- [quickstart.md](quickstart.md) defines the end-to-end validation sequence without creating a second
  daemon for the same user.

## Post-Design Constitution Check

Phase 1 introduces no new gate violation. The contracts preserve exact identity, global-group routing,
vendor ownership, transactional durability, and the real Linux/macOS acceptance gate. The design adds
no parallel namespace, access model, lifecycle authority, compatibility daemon, artificial quota, or
product behavior. Complexity tracking is therefore not required.

## Complexity Tracking

No constitution violations require justification.
