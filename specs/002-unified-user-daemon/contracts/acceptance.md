# Contract: Acceptance and Evidence Credit

## Evidence levels

1. **Baseline regression**: exact old test or a behavior-preserving transplant.
2. **Shared invariant**: one test table covering every product to which a DRY mechanism applies.
3. **Protocol fixture**: realistic framing, failure, and recovery behavior; never product readiness.
4. **Installed product**: authenticated native binary completes the actual user workflow.
5. **System lifecycle**: service restart/crash/upgrade and exact process/artifact census.
6. **Cross-host**: real hub/host routing across Linux and macOS.

No lower level substitutes for a required higher level. Skipped, confounded, inferred, or aggregate-only
results receive no cell credit.

## Closed 202-cell inventory

`contracts/acceptance-matrix.yml` is the machine-readable inventory and
`docs/ACCEPTANCE-MATRIX.md` is the target-topology assertion source. Functional and native-product
assertions remain those of the baseline. Family defaults and cell overrides MUST
expand every cell to a nonempty Linux/macOS scope, exact assertion section plus unique cell-ID locator,
and an acyclic list of known prerequisite cell IDs. Expansion MUST yield exactly 202 unique IDs with
these cardinalities:

| Family | IDs | Count |
|---|---|---:|
| Source/package | `S-01..S-08` | 8 |
| Install/upgrade | `U-01..U-12` | 12 |
| Codex interactive | `C-01..C-18` | 18 |
| Claude interactive | `CL-01..CL-11` | 11 |
| Grok interactive | `G-01..G-21` | 21 |
| Qwen interactive | `Q-01..Q-10` | 10 |
| Lane lifecycle | `L-01..L-30` | 30 |
| Parent-target composition | sixteen explicit `P-*` IDs | 16 |
| Peer/lane messaging | sixty-four explicit `M-*` IDs | 64 |
| Federation/groups | `X-01..X-08` | 8 |
| Archive/unarchive | `A-C`, `A-CL`, `A-G`, `A-Q` | 4 |
| **Total** |  | **202** |

Every runner MUST validate its requested IDs and prerequisite closure against this inventory and emit
one structured result per cell. An aggregate pass line, test process exit code, inferred ordering, or
family summary cannot create cell credit. A prerequisite RED produces an explicit
`NOT EXECUTED - PREREQUISITE RED` result for each dependent cell rather than `BLOCKED` or an omitted
row. Missing prerequisite results invalidate the run; runners may not invent dependency edges.

## Reviewed topology substitutions

An acceptance assertion changes only when its obsolete Agent Sessions process, service, or package
observation is listed in `topology_deltas` in the manifest. Each entry MUST identify the baseline
observation, target observation, and preserved functional/native invariant. The ledger cannot change
vendor argv, profiles, authentication, histories, permissions, native processes, delivery, groups,
lanes, archive behavior, cleanup safety, or any other product behavior. An unledgered topology change
is an assertion mismatch, not an implicit update to the contract.

`S-06` applies to active production, build, packaging, standard lifecycle, service, help, plugin, and
executable dependency surfaces. Historical baseline/deletion evidence, tests, and the directly invoked
repository-only `scripts/cleanup-pre-unification` may name obsolete artifacts so they can prove their
removal; none may make those artifacts reachable from a supported product path.

The one-time cleanup script is external preparation for `U-03`, not an installed-product capability or
new acceptance cell. `contracts/pre-unification-cleanup.yml` is its sole target authority. Direct
controlled-host evidence must prove external old-stack quiescence, mutation-free default planning,
separate explicit `--apply <plan-revision>`, zero mutation when the recomputed complete plan differs,
exact allowlisted removal of legacy Agent Sessions integrations and opaque legacy-owned operational
data, immediate metadata-revision revalidation, changed-target refusal, no content reads or hashes,
repeat-safe convergence, preservation of vendor and unrelated state, and
absence from operational Make targets, standard lifecycle dependencies, and release archives. Generic
test discovery may exercise the script only against fixture-owned roots; no Makefile may name either
cleanup artifact.

## Per-cell result contract

Every result has a stable result ID and exact verdict. Verdict-specific evidence is mandatory:

- `N/A` identifies the unsupported capability or platform with applicability evidence;
- `BLOCKED` identifies an external prerequisite and diagnostic classification and receives no credit;
- `RED` identifies the failed product, harness, environment, cleanup, or safety assertion;
- `NOT_EXECUTED_PREREQUISITE_RED` lists the exact declared prerequisite cell IDs that failed; and
- a rerun supersedes an earlier result only through an explicit prior-result link for the same cell,
  candidate, and platform. The earlier result then receives no credit.

Missing conditional evidence, duplicate unsuperseded results, or a cross-cell/candidate/platform
supersession invalidates the evidence set.

## Mandatory installed cells per product

- fresh named session and real turn;
- normal exit with exact cleanup;
- resume by durable name and exact UUID with native history;
- native argv/profile/permission passthrough;
- bare hook and MCP inactivity;
- managed attestation, discovery, messaging, and destination acknowledgement;
- daemon restart while idle and busy;
- archive/unarchive or the product's established equivalent;
- every parent and target lane relationship;
- group isolation/inheritance and permission restart behavior.

Linux and macOS results are reported separately at the same release candidate.

The bullets above are a navigation summary, not a replacement matrix. The exact per-product,
composition, messaging, lane, federation, archive, and safety assertions are the 202 baseline cells.
