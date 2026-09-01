# Research: Unified User Daemon

## Decision 1: Use the working implementation as executable documentation

**Decision**: `c056fbc5015d4ab0a673f66cac5404206f7bcee6` is the functional authority. Build a
symbol-and-test port map before replacement.

**Rationale**: The old code contains product behavior discovered through two weeks of live debugging.
The first unification attempt read portions of it but designed a generic lifecycle first, losing
working behavior despite green abstraction tests.

**Alternatives considered**:

- Treat documentation as sufficient: rejected because documentation did not encode launch ordering,
  environment presence, process ancestry, native sidecars, and cleanup details.
- Keep the failed unified implementation and patch regressions: rejected because it anchors future work
  to unverified abstractions and makes omissions hard to distinguish from intentional simplification.

## Decision 2: Generalize after comparison, not before

**Decision**: Derive shared interfaces from mapped old implementations. Extract one shared state machine
for Agent Sessions ownership and leave native operations behind explicit callbacks/adapters.

**Rationale**: DRY prevents drift only when behavior is actually shared. Premature generalization
silently normalizes genuine native differences.

**Alternatives considered**:

- Four independent daemon adapters copied from old code: rejected because shared routing, groups,
  durability, identity primitives, service lifecycle, and cleanup debt would drift.
- One fixed generic launch/observe sequence: rejected because Claude, Grok, Qwen, and Codex establish
  authority through different vendor processes and artifacts.

## Decision 3: Separate Agent Sessions daemon lifetime from vendor infrastructure lifetime

**Decision**: Peer and lane commands never start the Agent Sessions daemon. The running daemon may
start or reconnect vendor-required infrastructure exactly where the baseline did, including lazy Codex
App Server startup.

**Rationale**: A user-managed daemon and lazy vendor preparation are different authority boundaries.
Conflating them removed working Codex behavior.

## Decision 4: Keep MCP connectors dumb but evidence-preserving

**Decision**: Vendor-spawned MCP processes retain vendor-required stdio framing and forward requests to
the daemon. They own no Agent Sessions state or listener. They still carry raw launch capabilities and
the product-specific ancestry/session evidence required for attestation.

**Rationale**: Moving tool implementations and routing into the daemon removes duplication without
pretending all vendors spawn MCP under the same process.

## Decision 5: Migrate product-by-product with a strangler seam

**Decision**: Keep baseline launchers and bridge implementations live while making their native
operations callable from the daemon. Switch one product only after old tests plus real installed entry
and lifecycle tests pass.

**Rationale**: A whole-system rewrite made it possible for every product to be broken simultaneously.
Incremental cutover keeps a known-good reference and localizes each parity failure.

## Decision 6: Real installed products are mandatory acceptance

**Decision**: Protocol fixtures prove framing and failure paths; authenticated native installations
prove product readiness and behavior. No product path is deleted based only on fake processes.

**Rationale**: The failed implementation's fake-vendor acceptance reported green while basic real
workflows failed.

## Decision 7: Version 0.3 is greenfield only for Agent Sessions-owned state

**Decision**: Do not build compatibility machinery for pre-unification Agent Sessions state or
processes. Preserve vendor profiles and histories and prove that the new integrations use them. Provide
one repository-only `scripts/cleanup-pre-unification` for explicit one-time use on the three controlled
hosts. Its sole target authority is the repository-only
`contracts/pre-unification-cleanup.yml`, derived from the working baseline and containing no globs or
discovery rules. The no-argument invocation is plan-only; mutation requires a separate explicit
`--apply <plan-revision>` using the metadata-only revision emitted by the reviewed plan. Apply recomputes
the complete selection, refuses all mutation unless its revision matches, and revalidates each target's
metadata revision immediately before removal. The revision is derived only from the public contract
identity, ordered selected stable IDs, and non-content metadata tuples. The allowlist covers old Agent
Sessions MCP registrations, skills, binaries, service artifacts,
and all operational data produced and owned solely by the legacy implementation. It deletes opaque
legacy data without reading its content and never selects vendor transcripts, histories, credentials,
non-Agent-Sessions settings, or ordinary files. Neither script nor contract is shipped, installed,
exposed through an operational Make target, or called by any standard lifecycle path. Generic test
discovery may execute the script only against fixture-owned roots; no Makefile names either cleanup
artifact.

**Rationale**: The system is unreleased and deployed only on controlled hosts. A direct bounded cleanup
script makes the one physical transition repeatable without turning obsolete topology knowledge into a
permanent product dependency.

## Decision 8: Ship two independently deployed binaries

**Decision**: Ship `agent-sessions` for each host and `agent-sessions-hub` for the single central hub.
They share logical packages but deployment lifecycle and release selection are independent. Network
compatibility depends only on exact hub protocol version equality.

**Rationale**: The hub has no vendor adapters and is not a per-user host authority. Combining it into
the host image would couple unrelated deployments; splitting internal packages by binary would duplicate
shared protocol and lifecycle behavior.

## Decision 9: Freeze the complete 202-cell acceptance contract

**Decision**: Preserve the exact acceptance IDs and all functional and native-product assertions
documented by the working baseline. `contracts/acceptance-matrix.yml` is the machine-readable closed
inventory and contains a reviewed topology-delta ledger for the only permitted substitutions: obsolete
Agent Sessions process, service, or package observations. Each ledger entry records the baseline
observation, target observation, and unchanged invariant. `docs/ACCEPTANCE-MATRIX.md` is the human
target-topology assertion source. Family defaults plus explicit cell overrides expand each cell to its
platform scope, optional capability applicability, exact assertion section and unique cell-ID locator,
and explicit acyclic prerequisite set. Evidence is recorded per cell, and a prerequisite RED is
propagated only through those declared edges.

**Rationale**: The first unification attempt reported aggregate green gates while basic real product
entries were broken. Stable per-cell accounting prevents a broad runner, fake vendor, skipped test, or
ordering inference from standing in for a missing native workflow.

**Alternatives considered**:

- Define a smaller daemon-specific matrix: rejected because topology changes do not reduce the native
  product surface.
- Keep only prose categories: rejected because cardinality and missing-cell detection would not be
  mechanically enforceable.

## Decision 10: Store current port status with cumulative evidence predicates

**Decision**: Each baseline port-map entry stores one current status. The validator treats stages as
monotonic across manifest revisions and requires the current entry fields and evidence to satisfy the
current stage plus every earlier applicable stage. A redundant scalar status-history list is not stored.

**Rationale**: The evidence paths and replacement symbols are the audit record that matters. Repeating
old status labels would not prove that their gates passed, while cumulative predicates make a direct
jump to a later status impossible without all earlier evidence.

**Alternatives considered**:

- Retain an append-only list of status labels: rejected because labels duplicate version-control history
  and do not establish that the evidence gates were satisfied.
- Validate only the current stage's fields: rejected because a later status could bypass earlier
  inventory or parity requirements.

## Decision 11: Transplant all product regressions before shared extraction

**Decision**: Freeze and transplant the mapped Codex, Claude, Grok, and Qwen regressions first. Only
after all four sets run unchanged through behavior-preserving seams may shared production mechanisms be
extracted. A minimal runnable daemon and multi-call client then exist before any product is cut over.

**Rationale**: Extracting a generic lifecycle after observing only one product recreates the failure
mode this restart is intended to eliminate. Product acceptance also cannot exercise a daemon-backed
adapter before a runnable daemon exists.
