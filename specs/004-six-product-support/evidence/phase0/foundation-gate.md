# Phase-B Shared Foundation Gate

Date: 2026-09-01

Candidate: `863032e` (`Complete six-product shared foundation gate`), based on
the frozen Phase-A source at `f66229e` and the first shared-engine cut at
`1779e53`.

## Verdict

Implementation and local Linux gates: **PASS**.

Fable architecture review: **PASS** on 2026-09-01 for candidate `863032e` and
this evidence at `055d748`. T037 is closed and Phase-C fan-out is authorized.

No OpenCode, KiloCode, Pi, OMP, CodeBuddy, or DSH runtime product is registered
at this checkpoint.

## Frozen-contract status

- Protocol remains component v1 and federation v3; no new federation field was
  introduced.
- `internal/daemon/state.go` and the frozen `internal/productruntime` driver
  interfaces were not changed after T020.
- Component Ready-loss handling is an idempotency/adoption clarification over
  the existing frames. There is no Ready-ack frame, reusable signing key, or
  version bump.
- Initial lane input uses a validator-legal two-commit sequence: CAS1 stages a
  queued, invisible, inert receipt without caller acknowledgment; CAS2 commits
  the accepted Turn and Queued-to-Dispatching intent atomically and is the
  caller-acceptance boundary.

## Shared engines

- `internal/localtransport` validates the complete no-follow AF_UNIX ancestor
  chain and captures platform peer identity.
- `internal/component` uses durable authorizer-supplied binding IDs, live
  process re-attestation, adopted-predecessor fencing, bounded generation-local
  replay state, mandatory durable handling, and rollback on handler failure.
- `integrations/shared/component` keeps a bounded memory-only operation journal,
  correlates rejects, cumulatively acknowledges heartbeats, and clears replay
  across generations.
- `internal/structuredprocess` has bounded nonblocking frame retention,
  exact-group cleanup, independent reaping, repeatable exit evidence, and a
  bounded ephemeral evidence cache.
- `internal/productserver` enforces literal loopback/auth/redirect bounds,
  returns exact typed cleanup debt when exit is unproven, and checkpoints SSE
  IDs only after complete event blocks.
- `internal/testutil/mockprovider` provides one deterministic shared streaming,
  tool-call, slow-turn, cancel, malformed-stream, and credential-redaction rig.

## Durable lane-input gate

- Every local, MCP, and federated lane input, including initial `run`, `start`,
  and `resume`, enters the same no-follow spool and durable receipt ledger.
- Stable operation identity survives CLI reconnect, MCP relay, federation, and
  daemon generation negotiation.
- A stable start operation key locates its internal staged lane; the receipt ID
  separately hashes the complete immutable request, including owner, product,
  name, cwd, groups, policies, arguments, input digest, run/start semantics,
  and timeout.
- CAS1-only lanes are externally invisible, do not reserve names, cannot
  dispatch, and retire on dead owner or bounded staging abandonment.
- CAS2 promotion revalidates public-name uniqueness and rejects conflicts.
- Receipt sequence is the FIFO authority. Requeued attempts allocate a fresh
  daemon Turn while preserving receipt identity and sequence.
- Exact native acceptance commits receipt proof and native Turn identity at the
  native-acceptance boundary. Proven pre-write refusal may requeue; possible
  write without acknowledgment becomes Ambiguous and is never auto-replayed.
- Stored native IDs are reattach anchors only. Recovery reconciles exact product
  session, canonical cwd, active/preferred Turn, and terminal history; it fails
  closed on divergence and collects exact Codex results completed during daemon
  downtime.
- Terminal notices are projections of the committed durable Turn outcome.
- Retry, archive, cleanup-debt, missing-spool, dead-owner, and result/notice
  crash windows converge without blind replay.
- Remote receipt metadata remains destination-owned. An acknowledged source
  retry re-queries the destination using the same message ID; no durable
  destination-receipt copy is stored at the source.

## Federation compatibility and robustness

- New protocol-3 hosts advertise transport-only
  `federation-peer-products` in the existing capability list.
- A pre-feature-compatible validator proof demonstrates the required
  asymmetry: it tolerates the unknown capability token but rejects a roster
  containing an unknown `Peer.Product`.
- The new hub strips whole new-product peer rows from every initial and updated
  roster sent to unmarked clients. Marked clients receive the complete roster.
- The marker is never lane-dispatchable. Empty-capability inference remains the
  frozen four-product compatibility map only.
- Hello/peer/count fields are bounded. Snapshot admission computes every actual
  post-filter per-client roster before replacement and rejects amplification
  without damaging the last-good roster or disconnecting incumbents.
- Extra receipt keys in protocol-3 `Message.Data` are additive and ignored by
  pre-feature peers.

## Adversarial regressions closed

- bootstrap/reconnect Ready loss, daemon restart replay, foreign-process
  capability reuse, adoption, and stale-predecessor replay;
- disconnected component delivery retention and bounded 1,000-attachment
  churn;
- unread structured-process frame deadlock, producer overflow, and completed
  child retention;
- product-server post-start cleanup ambiguity and incomplete SSE event replay;
- old-v3 roster rejection and per-client roster amplification;
- busy Claude queued-turn collision (#42 compatibility);
- accepted-Turn/Dispatching atomicity, exact-native-ack crash, stale/mismatched
  recovery, competing intents, missing/changed spool, and non-Codex acceptance
  CAS ambiguity;
- CAS1 response loss, same-key bounded reconnect, staged ghost invisibility and
  timeout retirement, dead-owner-before-kick, immutable replay conflicts,
  timeout restoration, and promotion-time name collision;
- exact receipt-target waiting after crash audit Turns, FIFO resume, MCP
  duplicate prevention, truthful terminal notice, and destination-only receipt
  re-query.

## Commands and results

All commands ran from the isolated feature worktree on Linux amd64 with
`/usr/local/go/bin` on `PATH`.

```text
go test ./cmd/agent-sessions ./internal/daemon ./internal/bridge \
  ./internal/federation ./internal/sessiontools -count=3                 PASS
go test -race ./cmd/agent-sessions ./internal/daemon ./internal/bridge \
  ./internal/federation ./internal/sessiontools -count=1                 PASS
go vet ./cmd/agent-sessions ./internal/daemon ./internal/bridge \
  ./internal/federation ./internal/sessiontools                           PASS

go test ./internal/productruntime ./internal/productserver \
  ./internal/structuredprocess ./internal/testutil/mockprovider \
  ./internal/component ./internal/localtransport ./internal/federation \
  -count=10                                                               PASS
go test -race ./internal/productserver ./internal/structuredprocess \
  ./internal/testutil/mockprovider ./internal/component \
  ./internal/localtransport ./internal/federation -count=2                PASS
go vet ./internal/productruntime ./internal/productserver \
  ./internal/structuredprocess ./internal/testutil/mockprovider \
  ./internal/component ./internal/localtransport ./internal/federation    PASS

node --check integrations/shared/component/client.js                      PASS
node --check integrations/shared/component/client.test.js                 PASS
node --test integrations/shared/component/client.test.js                  PASS
./scripts/federation/test                                                  PASS
./scripts/test                                                             PASS
git diff --check                                                           PASS
```

The focused component reread additionally ran 20 normal repetitions, five race
repetitions, vet, and three Node repetitions. The final staged-promotion reread
ran ten normal and three race repetitions.

## Honest pending cells

- Mixed-version federation against a real pre-feature binary remains required
  before production cross-version rollout. This gate proves the asymmetry and
  additive `Message.Data` behavior through the production validators and a
  faithful old-host model, not a separately installed old executable.
- A statestore-backed production component `Authorizer` must be wired and
  exercised end to end before the first component-broker product peer receives
  acceptance credit. This gate proves the broker contract, durable record
  schema/transitions, and faithful authorizer behavior in isolation.
- Physical macOS execution for this exact candidate is pending and receives no
  acceptance credit here.
- CodeBuddy's Tencent-backed model-turn GA cell remains pending by product
  design; it cannot pass without an account.
- DSH credentialed product cells belong to the later exact-tuple product gate,
  not this product-neutral foundation gate.

## Fable review packet

The review request must cover, in one pass:

1. federation marker asymmetry, complete roster filtering, and prospective
   per-client amplification rejection;
2. component Ready-loss idempotency/adoption and bounded memory-only replay;
3. the full lane-input crash matrix, CAS1/CAS2 acceptance boundary, staged
   liveness/visibility, exact replay identity, native reconciliation, and
   destination-only receipt projection;
4. structured-process and product-server cleanup/retention RED closures;
5. the commands above and the explicit pending/no-credit cells.
