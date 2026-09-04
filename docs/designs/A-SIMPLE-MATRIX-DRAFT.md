# A-SIMPLE acceptance-matrix draft

Status: review-only integration input. This page does not change the protocol or authorize
implementation. It is based on `5111540c135bb4ee3a5d58fbd861eba722d2e2ff` and the stamped
connection-first A-SIMPLE story.

## Common rules

- The nine worker rows are W01 Codex, W02 Claude, W03 Grok, W04 Qwen, W05 OpenCode, W06
  Kilo, W07 Pi, W08 OMP, and W09 DSH. They all execute the same helper; the product token is
  data, never a branch.
- Each row reads capabilities and standard-field support from the worker-backed `lane doctor`
  result. The harness never owns a product-to-capability table. In particular, it calls `steer`
  only when that result advertises semantic steer support.
- Every selector after open is the returned lane UUID. Names are evidence labels, not control
  identities. Every response is checked against the same UUID and the current turn ID.
- There are no sleeps, retries, compensating prompts, native pane markers, or product-specific
  control flows. A bounded status observation establishes `running`; all other progress is driven
  by a response or a connection event.
- A failed cell preserves its wire frames, doctor/hello projection, lane status, and cleanup
  result. Cleanup may interrupt and archive but never changes the recorded verdict.

## W01-W09: two turns, conditional steer, and interrupt

Turn 1 proves ordinary reusable completion. `lane run` opens a named lane with a unique echo
token and waits for the worker's terminal result. The cell requires `outcome=completed`, the exact
token in the result, a nonempty worker-confirmed native ID, and an idle live row. It records the
lane UUID, native ID, worker generation, turn ID, doctor result, and caller/worker wire evidence.

Turn 2 uses that same lane and native ID:

1. Connector A starts `lane resume` asynchronously with a deliberately long, tool-free prompt.
   Connector B observes the row become `running` with a new nonempty turn ID. A completed or
   failed result before this observation fails the cell.
2. If doctor advertised steer, connector B sends one unique steer input to the lane UUID. It
   requires `type=turn.steered`, the same lane and turn IDs, and a nonempty native message ID.
   The router unit proof, not a product-specific matrix branch, pins this public call to
   `lane.turn.start` with `mode=steer` on the same binding. If steer was not advertised, the
   harness sends no steer call and records that fact.
3. Connector B immediately calls `lane interrupt`. It requires `type=turn.interrupting` and the
   same lane and turn IDs.
4. Connector A collects the already-running `resume` call. The only passing terminal projection
   is `type=turn.completed`, `outcome=interrupted`, the same lane and turn IDs, and an idle row
   whose current outcome is interrupted. `completed`, `failed`, EOF, a changed native ID, or a
   second terminal response fails the cell. `native_stop_reason` remains opaque when present.
5. The harness archives once, requires one `lane.archived` response, and proves the UUID is absent
   from ordinary list. A final process/roster residue check is shared with the standing matrix.

The long turn input asks the native product to emit a numbered sequence far beyond its ordinary
output limit. Its purpose is only to keep the turn active until the acknowledged interrupt; no
generated text is an assertion.

Per-row evidence is `workers/WNN-<product>.json` with the doctor result, turn-1 result, ordered
turn-2 caller frames, status projections, optional steer response, interrupt response, terminal
result, archive response, and cleanup projection. Secrets, launch-token bytes, and ambient
credentials are never recorded.

### W-row deterministic tests

- One table covers all nine product tokens through one helper and fixtures that vary only the
  reported capability data.
- Steer supported: `run -> resume/running -> steer -> interrupt -> interrupted -> archive`.
- Steer unsupported: the same sequence with exactly zero steer calls.
- A blocked resume handler does not block the second connector's status, steer, or interrupt.
- Wrong UUID/turn ID, empty native message ID, premature terminal, non-interrupted outcome,
  duplicate terminal response, or surviving ordinary-list row each fail.
- Every failure path performs bounded cleanup and retains the pre-cleanup evidence.

## Reference-worker rows

The non-AI `asl-lane-example` has one documented conformance input in addition to its ordinary
echo/transform input: `conformance:eof-on-wait`. It is worker-owned behavior sent through the
normal typed turn input. It adds no environment key, argv mode, daemon hook, product branch, or
alternate parameter channel.

### R01: mid-wait EOF

The runner opens the example lane and starts `conformance:eof-on-wait`. The worker acknowledges
`lane.turn.start`; when it receives `lane.turn.wait`, it closes its control connection without a
reply and exits. The pending public call must fail exactly once with the closed worker-EOF error.
The durable row keeps its UUID/native ID and a visible failed closed projection; no call is
replayed and no replacement worker is spawned. Cleanup archives the failed row. Evidence contains
the start acknowledgement, the unanswered wait ID, the single caller error, the failed status,
and zero process/roster residue.

### R02: nonpersistent parent departure

A test-owned protocol parent A starts a nonpersistent example lane, completes one echo turn, and
then closes its parent connection. The daemon sends exactly one fenced `lane.session.archive` to
the bound worker. After its acknowledgement the worker exits, ordinary list omits the UUID, and
`--all` reports the durable row as archived under parent A. Publication of archive/failure must
precede parent-departure cleanup completion. The row must never appear owned by the observer.

### R03: persistent detach and authorized resume

Protocol parent A and parent B share one explicit group. A starts a persistent example lane and
completes turn 1, then disconnects. The daemon does not archive the worker: status becomes detached
with the same UUID, native ID, durable historical parent, and authorized groups.
Parent B can see it but `--mine` cannot claim it. B explicitly resumes it, becoming the current
in-memory owner, and turn 2 returns a unique echo token through the same live worker binding. The
durable historical parent remains A. B then archives and the residue check is empty.

### R-row deterministic tests

- R01 pins start acknowledgement before EOF, one failure for the pending wait ID, no replay, and a
  visible failed row.
- R02 pins archive acknowledgement before row closure and exact generation/pointer fencing; an
  archive failure leaves a visible failed row.
- R03 pins visibility without ownership mutation, detach without archive, explicit group-authorized
  handover, unchanged durable parent/native ID, and a second turn on the original binding.

Launch-token replay is deliberately not a live reference-worker cell. The worker unsets its token
before starting the native child and never reuses it. The daemon's deterministic token-authority
table proves one successful consume and side-effect-free rejection of a second consume, wrong
purpose, wrong product, and expiry. No token bytes or derivatives enter evidence.

## Integration size and paths

The draft fits five A-owned or existing paths:

| Path | Purpose | Ceiling |
| --- | --- | ---: |
| `scripts/realproducts/matrix.go` | One W helper, three R cells, evidence/cleanup | `+220/-20` |
| `scripts/realproducts/matrix_test.go` | W capability/order/race tables and R lifecycle tables | `+190/-10` |
| planned `cmd/agent-sessions/example_lane.go` | Documented EOF conformance input | `+25/-0` |
| planned `cmd/agent-sessions/example_lane_test.go` | EOF behavior | `+35/-0` |
| the A acceptance-plan page | Cell inventory and evidence mapping | `+20/-0` |

Harness/worker production is at most `+245/-20`; tests at most `+225/-10`; documentation at most
`+20`; aggregate net at most `+460`. The example paths are already required by A and the page entry
is folded into A's one-page launch story, so the only new matrix-specific path pair is
`scripts/realproducts/matrix{,_test}.go`. Any daemon test hook, lane-specific environment value,
product capability table, PID scraping, sleep/retry, or sixth path is scope drift and requires a
new review.
