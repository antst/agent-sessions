# Quickstart: Parity-First Unified Daemon Validation

Run from a clean worktree and record the exact commit, tree, Go version, OS, architecture, native
product versions, installed runtime identity, and test-owned roots.

## 1. Baseline port-map gate

Before changing a product path:

1. expand its authoritative rows in `contracts/baseline-port-map.yml` to exact old symbols and tests,
   then update `contracts/baseline-port-map.md` as the review projection;
2. run those tests at `c056fbc` or the fresh feature branch before behavior changes;
3. record any pre-existing RED separately;
4. add missing hook/installed cells before implementation.

The machine-readable `contracts/baseline-port-map.yml` must have no unmapped deleted symbol and each
current status must satisfy its cumulative stage predicates. Validate that
`contracts/acceptance-matrix.yml` expands to exactly 202 unique IDs, that every ID has a nonempty
platform scope and an acyclic known-ID prerequisite set, and that every ID resolves exactly once inside
its declared assertion section in `docs/ACCEPTANCE-MATRIX.md` before recording evidence. Validate every
topology-delta entry against a known cell and confirm it changes only an Agent Sessions
process/service/package observation while retaining the documented functional/native invariant.

## 2. Repository gates

Run focused mapped regressions first, followed by:

```sh
make test
make test-race
go vet ./...
make lint
```

Build Linux and macOS archives for amd64 and arm64. Each release archive must contain exactly
`agent-sessions`, `agent-sessions-hub`, and the declared connector/plugin payloads.

## 3. Clean user-service gate

On dedicated Linux and macOS acceptance users, the operator or harness—not the version 0.3
installer—establishes the greenfield precondition:

1. on the three controlled transition hosts only, first prove the old stack is quiescent, invoke
   `scripts/cleanup-pre-unification` with no arguments to review the plan from
   `contracts/pre-unification-cleanup.yml` and record its metadata-only plan revision, and only then
   invoke it separately with `--apply <plan-revision>`; prove a changed complete plan causes zero
   mutation, changed target metadata fails closed, and a later no-argument plan has no remaining work; never expose
   it through an operational Make target, `install`, `install-all`, update, remove, or a service, and
   keep both cleanup artifact names out of every Makefile;
2. prove no old Agent Sessions processes or allowlisted Agent Sessions-owned prototype MCP entries,
   skills, binaries, service artifacts, registries, journals, caches, logs, locks, sockets, or private
   operational data remain, without reading opaque removed content or treating that proof as a product
   compatibility feature;
3. retain authenticated vendor profiles without reading credential contents;
4. install and enable the platform user service;
5. prove exactly one daemon and one private endpoint;
6. exercise explicit stop, start, unexpected crash restart, login start, upgrade, failed-update
   preservation, remove, and reinstall;
7. prove vendor profiles and histories remain intact.

## 4. Interactive product matrix

For Codex, Claude, Grok, and Qwen independently:

1. start a fresh named managed session and complete a real turn;
2. record native UUID, durable name, process identity, profile identity, cwd, permission, and groups;
3. verify managed discovery and destination-acknowledged messaging;
4. exit normally and verify exact Agent Sessions cleanup without native archive;
5. resume by durable name, then by exact UUID, and verify full native history;
6. repeat ordinary and elevated permission paths;
7. exercise native passthrough flags and delimiter ordering;
8. run the same hooks/MCP connector from a bare native session and prove silent/no-op inactivity;
9. restart the daemon while the managed peer is idle and busy and verify native identity continuity.

Stop at the first genuine RED. Capture the native error, exact argv/environment presence, process tree,
and relevant non-secret artifact metadata before changing code.

Record one result for every applicable `C-*`, `CL-*`, `G-*`, and `Q-*` ID. Do not replace these IDs with
a product summary.

Execute and validate exact cells through the repository runner, using an absolute executable driver
that performs the named real-product recipe rather than replaying a prior result:

```sh
./scripts/test-real-products \
  --cells C-01,C-02 \
  --driver /absolute/path/to/exact-cell-driver \
  --evidence-dir /absolute/path/to/new-evidence-directory \
  --codex-binary /absolute/path/to/codex
```

The runner expands declared prerequisites, resolves the exact native products used by each cell,
passes each resolved binary to the driver as `AGENT_SESSIONS_ACCEPTANCE_<PRODUCT>_BINARY`, and validates
the emitted `result.json`. Every `evidence_paths` entry is relative to `--evidence-dir`, unique,
nonempty, an existing no-follow regular file, and confined to that directory. `PASS` requires exit 0,
concrete real-installed identity for live cells, all cell-product versions, and destination-visible
receipt fields for every `P-*` and `M-*` cell. The runner terminates only the driver's test-owned
process group and removes only its private temporary root.

## 5. Collaboration and lane matrix

- Run all sixteen parent-target combinations.
- For each cell prove parent context, group inheritance, permission behavior, a real turn, destination
  acknowledgement, follow-up, interrupt where safe, wait/status, collection, archive, resume, and cleanup.
- Run shared-group, disjoint-group, multicast, broadcast, reply, and same-name discriminators.
- Restart the daemon during an accepted turn and prove continuation or the product's established single
  interrupted/resumable result without redispatch.

Record all applicable `L-*`, all sixteen `P-*`, all sixty-four `M-*`, and all four `A-*` cells
individually. The matrix runner must reject missing, duplicate, and unknown IDs, enforce
verdict-conditional evidence, and accept a rerun only through explicit same-cell/candidate/platform
supersession.

## 6. Federation matrix

Connect at least two Linux hosts and one macOS host to one hub. Prove peer discovery, messaging, and remote
lanes in both directions; host restart; hub restart; network loss; sleep/wake; independent same-protocol
host and hub upgrades from unrelated commits; and mismatch refusal.

## 7. Cutover gate

Only after all prior cells pass:

1. review every deletion against the completed port map;
2. verify process census contains no old supervisor, shim, product host, lane manager, local router, or
   standalone host federation agent;
3. rerun the full matrix from installed archives on Linux and macOS;
4. record exact residue and preserved-state evidence;
5. sign only the exact commit and tree that passed.
