<!--
Sync Impact Report
- Version change: unratified scaffold -> 0.1.0
- Added principles:
  - I. Shared Contracts, One Implementation (DRY)
  - II. Exact Identity and Fail-Closed Safety
  - III. Root-Cause Analysis Before Permanent Fixes
  - IV. Evidence-Driven Testing (NON-NEGOTIABLE)
  - V. Linux and macOS Parity
  - VI. Transactional Lifecycle and Zero Collateral
  - VII. Explicit Protocols, Operability, and Documentation
- Added sections:
  - Product Safety and Architecture Constraints
  - Development and Release Workflow
- Removed sections: placeholder-only scaffold content
- Follow-up TODOs: none
-->
# Agent Sessions Constitution

## Core Principles

### I. Shared Contracts, One Implementation (DRY)

Behavior shared by products, platforms, peer types, or lane types MUST have one authoritative
implementation or one deliberately shared contract. Command packages MUST remain thin entry points;
reusable lifecycle, routing, group, protocol, process, and installation behavior belongs in the
appropriate shared `internal/` package. Product-specific code is permitted only where the native
product contract genuinely differs, and that difference MUST be documented and tested.

When the same defect can occur on multiple surfaces, the fix MUST close the class rather than patch
one occurrence. Tests MUST cover the shared invariant across every applicable product. Copying logic,
schemas, option tables, help text, or cleanup rules is prohibited unless a review records why a
single source would make the contract less safe or less clear.

Rationale: Agent Sessions supports several native products over one lifecycle and routing model.
Drift between nearly identical implementations is a recurring correctness and safety risk.

### II. Exact Identity and Fail-Closed Safety

Any action that signals a process, removes an artifact, resumes a session, accepts a message, or
changes durable state MUST be authorized by exact, corroborated identity. Depending on the resource,
that proof includes session and host IDs, product and instance IDs, PID plus process start and strong
start, native registry data, socket identity, filesystem type, revision, and ownership baseline.
Names, PID liveness alone, path shape alone, group membership, and model-supplied claims are never
sufficient authority.

Ambiguous, stale, malformed, missing, changed-type, or unverifiable state MUST fail closed. Cleanup
MUST preserve unrelated processes, rows, sockets, keys, transcripts, settings, and credentials.
Agent Sessions MUST NOT infer that an authenticated sender or shared group has the user's delegated
authority; delegation remains an explicit user policy outside transport authentication.

Rationale: a false positive can kill another session, disclose a roster, widen permissions, or delete
state that cannot be reconstructed. Refusing an operation is safer than guessing ownership.

### III. Root-Cause Analysis Before Permanent Fixes

Every defect MUST have an evidence-backed root-cause analysis before its permanent fix is accepted.
The analysis MUST identify the triggering conditions, causal mechanism, affected invariant, and why
existing tests or gates missed it. Product defects, environment differences, toolchain skew, and
harness errors MUST be classified separately. Failed or confounded evidence MUST be disclosed and
MUST NOT be credited as a passing result.

Retries, longer sleeps, message matching, broad cleanup, warning suppression, manual state edits, or
test weakening MUST NOT substitute for a causal fix. If a defect represents a repeatable class, the
implementation MUST close that class and add a regression that would fail for the original cause.
An emergency mitigation MAY precede full RCA during an active incident, but it MUST be explicitly
temporary and cannot be the final release fix.

Rationale: distributed lifecycle defects often appear as timing or platform flakes. Without RCA,
local workarounds hide residue and move failures to another boundary.

### IV. Evidence-Driven Testing (NON-NEGOTIABLE)

Every behavior change MUST include automated evidence at the lowest useful layer and at every
affected integration boundary. A regression test MUST demonstrate the pre-fix failure when feasible,
then pass because the invariant is restored. Tests MUST use exact identities, real return codes,
terminal state, and attributable artifacts; vacuous checks, skipped assertions, loose process
selection, and fixed sleeps without a state predicate are prohibited.

Changes affecting lifecycle, protocols, permissions, routing, packaging, or recovery MUST include
adversarial cases: stale and recycled identities, changed filesystem types, partial publication,
crash/restart, duplicate collection, ambiguous names, malformed frames, or denied authorization as
applicable. Unit success alone is insufficient when behavior crosses a native client, App Server,
host agent, hub, supervisor, plugin, installer, or operating-system boundary.

Rationale: the product's hardest failures occur between components and during interrupted state
transitions, not on the happy path of one function.

### V. Linux and macOS Parity

Every source, packaging, installer, lifecycle, process, protocol, or release change MUST be validated
on both Linux and macOS before release. The affected behavior MUST run on real installations of each
operating system; cross-compilation alone is not runtime evidence. Platform-specific process tables,
AF_UNIX limits, secure storage, filesystem behavior, signals, and native-client dialogs MUST be
tested where relevant.

The mandatory gate is: normal tests, race tests, vet, repository-managed lint, all supported package
builds, and the applicable live acceptance cells. Tool and native-client versions MUST be recorded.
A Linux-green result cannot waive macOS evidence, and a macOS-green result cannot waive Linux
evidence. Documentation-only changes MUST still pass repository CI and MUST NOT alter behavioral
claims without the same two-platform evidence.

Rationale: Agent Sessions has repeatedly exposed valid Linux/macOS differences that compilation and
single-platform CI cannot detect.

### VI. Transactional Lifecycle and Zero Collateral

Session and lane creation, adoption, registration, permission publication, collection, archive,
resume, reconciliation, and cleanup MUST be explicit state machines with durable ownership. A
transaction MUST publish externally visible state only after its prerequisites commit. Rollback MUST
use exact revision or identity checks and MUST NOT overwrite a newer decision. Cleanup MUST retire or
freeze the exact process before removing its artifacts, and partial cleanup MUST retain durable debt
for deterministic retry.

Normal exit, interrupt, termination, manager crash, worker crash, launcher crash, host-agent restart,
and machine-local reconciliation MUST converge to the documented terminal state. Success requires
zero attributable live process, row, socket, key, temporary settings file, worktree, or pending notice,
while all unrelated state remains byte-for-byte or identity-equivalent intact. Cleanup and archive
operations MUST be idempotent.

Rationale: silent residue and collateral cleanup both corrupt future sessions. Durable retryable debt
is preferable to reporting success before exact cleanup completes.

### VII. Explicit Protocols, Operability, and Documentation

Wire messages, durable records, CLI arguments, environment variables, exit codes, JSON fields,
permission modes, group rules, and lifecycle states MUST have explicit documented contracts. Parsed
options MUST appear in the corresponding help output. Human diagnostics MUST name the actual failed
precondition, while machine-readable output MUST remain stable and unambiguous.

Cross-host deployments assume all participating Agent Sessions binaries are upgraded together unless
a feature specification explicitly requires compatibility. Compatibility shims for obsolete hubs or
agents MUST NOT be added by default; a required shim needs an attested trust model, tests for both
versions, and a documented retirement point. Bare native clients remain an intentional opt-out and
MUST NOT acquire managed capabilities through ambient installation alone.

User and operator documentation MUST change in the same feature as its behavior. Product guides MUST
remain symmetric where contracts are shared and explicitly describe genuine differences. Release
archives, examples, help, skills, and plugin identities MUST describe the shipped implementation, not
an intended future state.

Rationale: operators cannot safely manage a federated multi-product runtime when hidden flags,
implicit compatibility assumptions, or stale instructions differ from executable behavior.

## Product Safety and Architecture Constraints

- The host agent is the authority for durable product, identity, group, and routing state. Hubs route
  attested messages and MUST NOT invent, broaden, or silently discard security-relevant identity.
- Group discovery, direct send, explicit multicast, and named-group broadcast MUST enforce shared
  membership. Empty, wildcard, nonexistent, or sender-nonmember broadcasts MUST fail closed. There is
  no ambient global peer namespace.
- Managed wrappers, peers, and lanes MAY opt into capabilities per launch. Installation and runtime
  MUST NOT silently change owner-wide permission, browser, authentication, credential, or native
  client settings.
- Credentials and normal product profiles remain owned by their native clients. Tests MAY use
  isolated profiles and existing credential namespaces, but MUST NOT copy, print, mutate, or broaden
  secrets unless the user explicitly authorizes that operation.
- Filesystem cleanup MUST preflight type, ownership, baseline, and exact current identity before any
  destructive action. A changed artifact is cleanup debt, not permission to delete.
- New product adapters MUST reuse AgentFrame, groups, durable lane semantics, and federation unless a
  specification proves that the shared contract is insufficient. New global namespaces or parallel
  lifecycle frameworks require a constitution amendment or an explicit approved exception.

## Development and Release Workflow

1. **Specify the invariant.** Every feature or fix MUST state its user-visible contract, authority
   boundary, failure behavior, affected durable state, and Linux/macOS evidence plan before broad
   implementation begins.
2. **Inventory the baseline.** Tests that touch live clients, profiles, processes, sockets, services,
   or remote hosts MUST record exact owned and preserved state before mutation.
3. **Reproduce and classify.** A defect MUST have a deterministic reproduction or the strongest
   available direct evidence. The first genuine RED stops scope expansion until it is understood.
4. **Implement the smallest class-closing change.** The fix MUST preserve unrelated working-tree
   changes, avoid duplicated behavior, and keep security checks at least as strong as before.
5. **Verify in layers.** Run focused tests, full normal and race suites, vet, the Makefile-managed
   linter, four supported platform builds, integration cells, and real Linux/macOS acceptance where
   applicable. Record exact commit, tree, toolchain, native-client versions, and any rejected evidence.
6. **Review the proof.** Review MUST inspect lifecycle ordering, exact identity, rollback, cleanup
   debt, protocol fields, permission propagation, group isolation, packaging, and documentation—not
   only the happy-path diff.
7. **Release reproducibly.** Release commits and tags MUST be signed. The tag MUST point to the exact
   fully tested main commit. Published archives MUST contain every supported binary and plugin payload,
   pass checksums, and complete an isolated real prebuilt installation. Main and develop MUST be
   synchronized after release unless an explicitly documented branch policy says otherwise.

## Governance

This constitution governs specifications, plans, tasks, implementation, review, acceptance, and
release work for Agent Sessions. Where another project document or historical practice conflicts,
this constitution takes precedence unless a newer constitution amendment explicitly delegates that
decision.

Amendments MUST modify only the constitution in their constitution workflow, include a Sync Impact
Report, explain the reason and migration impact, and receive explicit maintainer approval. Governance
versioning uses semantic versioning: MAJOR removes or incompatibly redefines a principle, MINOR adds a
principle or materially expands obligations, and PATCH clarifies existing obligations without changing
their force.

Every feature specification, implementation plan, pull request, and release review MUST state how it
complies with the applicable principles. Any exception MUST be explicit, narrowly scoped, time-bound,
and paired with a tracked remediation. Exceptions cannot waive exact-identity safety, fail-closed
authorization, attributable cleanup, or the Linux/macOS release gate.

**Version**: 0.1.0 | **Ratified**: 2026-08-20 | **Last Amended**: 2026-08-20
