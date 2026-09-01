# Data Model: Unified User Daemon

## BaselinePortEntry

Traceability record required before replacing old code.

- `id`: stable product/behavior identifier
- `product`: shared, codex, claude, grok, qwen, federation, service, or packaging
- `old_symbols`: exact files and functions at `c056fbc`
- `old_tests`: exact test symbols, scripts, and installed commands
- `invariant`: observed user-visible or safety behavior
- `new_owner`: shared component or product adapter
- `replacement_tests`: named regressions that must pass
- `acceptance_cells`: Linux/macOS cells that must pass
- `deletion_paths`: old paths that remain until evidence is complete
- `status`: current highest completed stage: inventory-required, inventoried, transplanted, shared,
  daemon-backed, installed-green, removable, or removed
- `evidence`: cumulative proof satisfying the current stage and every preceding applicable stage

Transitions are monotonic across recorded manifest revisions. The manifest stores the current status,
not a redundant list of earlier scalar values. A status is valid only when the cumulative predicates
declared by `status_contract` for that stage and every preceding applicable stage are satisfied by the
entry's fields and evidence. `removed` is therefore invalid unless all earlier evidence predicates pass.

The authoritative records live in `contracts/baseline-port-map.yml`. The Markdown port map is a human
projection and cannot independently authorize implementation or deletion.

## AcceptanceCell

One immutable member of the closed 202-cell baseline matrix.

- `id`: unique stable ID from `contracts/acceptance-matrix.yml`
- `family`: source, install, codex, claude, grok, qwen, lane, composition, messaging, federation, or archive
- `evidence_tiers`: required source, installed, interactive, lane, cross-host, fault, or upgrade tiers
- `platforms`: required operating-system scope or explicit capability-based applicability
- `assertion_source`: exact target-topology heading/table row in `docs/ACCEPTANCE-MATRIX.md`, retaining
  the baseline invariant except for a reviewed topology-delta entry
- `prerequisites`: cell IDs whose RED result prevents execution without granting credit
- optional `topology_delta`: reviewed old/new Agent Sessions topology observations plus the unchanged
  functional or native-product invariant

Family defaults plus cell overrides MUST expand every cell to all six fields above. Assertion expansion
resolves one document, exact section, and unique cell-ID locator. The prerequisite graph contains only
known cell IDs, is acyclic, and has no implicit edges. The manifest expands to exactly 202 unique IDs.
Changing that cardinality, applicability, prerequisite edge, or assertion requires an explicit
specification change; process-topology edits alone do not authorize it.

## AcceptanceResult

One attributable execution result for one AcceptanceCell on one candidate and platform.

- stable result ID, cell ID, and exact verdict: `PASS`, `N/A`, `BLOCKED`, `RED`, or
  `NOT_EXECUTED_PREREQUISITE_RED`
- verdict reason, diagnostic classification, applicability evidence, and failed prerequisite IDs as
  required by the verdict
- exact commit, tree, installed release identity, platform, architecture, and native product versions
- literal command, cwd, relevant non-secret environment presence, and exit status
- exact native session/process/artifact identity and destination-visible evidence where applicable
- preserved-state and exact owned-cleanup evidence
- evidence paths and optional prior result ID when this result authoritatively supersedes a rerun

An aggregate runner summary is not an AcceptanceResult. Duplicate results for the same cell/candidate/
platform are rejected unless an explicit supersession record identifies the authoritative rerun.

## HostRuntime

The one Agent Sessions authority for an OS user-host.

- exact user and host identity
- release/runtime identity and generation
- fixed private endpoint identity
- service-manager state
- product readiness map
- attachment, delivery, lane, cleanup-debt, and federation catalog revisions

Only explicit service operations or validated install/upgrade transactions change lifetime.

## ProductProfile

Non-secret identity of one vendor profile selected according to that product's native rules.

- product
- canonical profile/config/runtime paths where applicable
- presence versus explicit-empty environment state where significant
- non-secret native instance identity
- readiness classification and cause

Credential contents and transcript contents are never fields.

## ManagedAttachment

Durable Agent Sessions ownership of one native interactive peer.

- attachment ID and capability hash
- product and ProductProfile identity
- launch intent and exact wrapper preferences
- native session ID, durable name, cwd, groups, and permission mode
- expected and observed NativeEvidence
- daemon generation and catalog revision
- lifecycle state: preparing, prepared, selecting, attached, detaching, detached, debt

Preparation does not grant messaging authority. Attachment becomes active only after the product
adapter corroborates its required native evidence.

## NativeEvidence

Product-specific exact evidence represented through shared typed primitives.

- PID, process start, strong start, executable identity, and required ancestry
- native registry row, socket, App Server thread, launch token hash, leader/roster state, or artifact
  identity as applicable
- selected session/profile/cwd and permission projection
- revision/mtime/type/owner information required for later cleanup

The schema permits product-specific evidence variants; absence of one product's field is not inferred
from another product's lifecycle.

## AgentFrame and Delivery

Existing neutral collaboration envelope and durable local/remote acceptance record.

- exact sender, destination set, groups, host suffix, message/correlation/idempotency ID, timestamp
- delivery state: prepared, accepted, presented, acknowledged, retryable, rejected
- destination-visible acknowledgement and retry debt

Global groups remain the only visibility boundary.

## Lane and Turn

- exact parent attachment and parent context
- target product/profile, groups, cwd, permission mode, and native session
- lifecycle: preparing, idle, running, interrupting, terminal, archived, cleanup-debt
- ordered turns with acceptance, native dispatch, terminal notice, and collection revision
- product-native archive/resume evidence

One accepted turn has one native dispatch and one stable terminal outcome.

## CleanupDebt

- exact owned resource and baseline identity
- intended terminal state
- last verified state and non-secret cause
- retry revision and product-specific reconciliation operation

Changed or unverifiable resources remain debt; they are never broadly deleted.

## PreUnificationCleanupPlan

Repository-only transition record emitted by the direct cleanup utility.

- public cleanup-contract identity
- ordered selected stable target IDs
- non-content metadata tuple for each selected target
- deterministic plan revision derived only from those fields

The apply command accepts the reviewed plan revision, recomputes the complete current plan, and performs
no mutation unless the revisions match exactly. It then revalidates each target immediately before
removal. Opaque target contents are never revision inputs.

## FederationHost and HubState

- hub protocol version
- exact host identity and ready capabilities
- connection generation and reconnect state
- remote delivery/lane idempotency records

Host and hub release identities are diagnostic only and do not decide interoperability.
