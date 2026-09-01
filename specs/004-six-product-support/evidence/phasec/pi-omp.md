# Phase C Pi/OMP Product-Slice Closure

**Verdict: GREEN for the product-local clauses of T039, T043, T047, T051,
T054, T056, and T060.**

This verdict covers the Pi/OMP-owned packages, shared native extension, and
their protocol fixtures. It does not award central composition, real-product,
physical-platform, installer, federation, or release credit. A fixture may
model the transport or native protocol, but no mocked product is counted as a
real-product acceptance cell.

## Candidate and scope

- Base product slice: `1456c549a7f20dc680cf3d6cab49eabfe220297a`
  (`Add Pi and OMP runtime integrations`).
- Audited branch baseline: `feature/six-product-support` at
  `93209ca7e7a083293c3c7bac9c4ab1eb9f537233`.
- This closure is an uncommitted product-local delta over that baseline.
- Allowed implementation scope:
  `internal/products/{pifamily,pi,omp}` and `integrations/{pi,omp}`.
- Evidence scope: this file only. No task checkbox or central file is changed.

## Closed gaps

### T039 — peer lifecycle parity

`integrations/pi/pifamily.test.mjs` now runs both closed product rows through
the real shared `ComponentClient` and a real length-prefixed Unix-socket
boundary. For Pi and OMP independently it proves:

- bootstrap and exact native-session announcement;
- `session.bound` correlation;
- native peer kill followed by exact-ID resume without stopping the component
  connection or substituting a session;
- a true socket disconnect and reconnect;
- a daemon generation change from 30 to 31 with the prior binding/generation
  in the reconnect request;
- a new generation-scoped binding followed by re-announcement of the exact
  same native session; and
- exact exported session and binding environment after the rebind.

The fixture models the daemon transport boundary; it does not receive
real-product or physical-platform credit.

### T043 — shared peer/message integration

The existing shared extension and component runtime remain the single Pi/OMP
implementation. The closure found and fixed one fail-open edge in that shared
path: a registered tool could run before an exact `session.bound` witness.
`assertExactContext` now requires a nonempty binding witness whose native
session equals the current product session. Both products prove a foreign
binding and a foreign handler session cannot issue a parent operation.

### T047 and T051 — lane lifecycle and native interrupt

`TestLaneInterruptUsesCorrelatedAbortAndProductTerminalStrategy` invokes the
production `LaneDriver.Interrupt` for both products. It proves the driver
reconciles the exact live native session, emits a correlated `abort` request,
ignores the other product's terminal event, and accepts only:

- Pi `agent_settled`; or
- OMP non-continuing `agent_end`.

Both rows then collect an exact `TurnInterrupted` terminal with the native
result. `TestLaneInterruptWriteFailureRollsBackInterruptedOutcome` proves a
failed native abort does not leave the durable result falsely interrupted: the
later product terminal is collected as completed.

`TestLaneRecoveryAfterProcessDeathPreservesExactNativeSessionForBothProducts`
kills/archives the old owned RPC process, constructs a successor driver at a
new daemon generation, calls `Recover`, renders the product's exact
`--session <native-id>` selector, and rejects session substitution through the
existing handshake guard. Pi and OMP both retain the prior native ID while
advancing only the daemon generation.

### T054 — permission mapping

The prior product-local permission evidence remains green: Pi maps default to
its restricted tool set and explicit bypass to full tools; OMP rejects
unmediated default before process start, admits only explicit bypass/yolo, and
fails an unexpected approval frame closed. The new interrupt/recovery tests
continue to use the exact launch policy and do not weaken it.

### T056 and T060 — complete parent matrix

The parent matrix now runs for both Pi and OMP:

- registered `agent_sessions` tool and `/lane` command;
- exact exported native-session environment;
- direct-child depth-one ancestry from the live registered component process;
- exact product, binding, peer PID/UID, process start/strong-start, attachment,
  and native session;
- false binding rejection;
- false native-session rejection; and
- deeper subagent rejection.

Go exercises the live process/binding attester for both quirk rows. JavaScript
exercises the registered operation surfaces and exported environment for both
rows, including the pre-bind fail-closed case.

## Verification

Audit time: `2026-09-01T16:22Z`. Toolchains: Go `1.26.5` from
`/usr/local/go/bin/go`; Node `v25.9.0`.

```text
/usr/local/go/bin/go test ./internal/products/pifamily ./internal/products/pi ./internal/products/omp -count=3
PASS: pifamily, pi, and omp (three repetitions)

/usr/local/go/bin/go test -race ./internal/products/pifamily ./internal/products/pi ./internal/products/omp -count=1
PASS: pifamily, pi, and omp under the race detector

/usr/local/go/bin/go vet ./internal/products/pifamily ./internal/products/pi ./internal/products/omp
PASS

node --test integrations/shared/component/client.test.js \
  integrations/shared/component/protocol.test.js \
  integrations/pi/pifamily.test.mjs integrations/omp/entrypoint.test.mjs
PASS: 22 tests; 22 passed; 0 failed; 0 skipped

git diff --check -- internal/products/pifamily internal/products/pi \
  internal/products/omp integrations/pi integrations/omp \
  specs/004-six-product-support/evidence/phasec/pi-omp.md
PASS
```

The focused interrupt, recovery, and parent tests also passed five consecutive
runs before the full scoped suites.

## Task-clause result

| Task | Product-local result | Evidence |
|---|---|---|
| T039 | **GREEN** | Both peers traverse native kill/resume and actual component socket disconnect/reconnect with generation rebind and exact-session preservation. |
| T043 | **GREEN** | Shared extension/runtime retained; parent operations now require exact `session.bound` for both products. |
| T047 | **GREEN** | Production `Interrupt` exercised for Pi and OMP, including distinct terminal strategies and abort-error rollback. |
| T051 | **GREEN** | Open/start/wait/steer/interrupt/archive/recover use the typed JSONL driver and exact resume selector for both rows. |
| T054 | **GREEN** | Pi restricted/full and OMP fail-closed/yolo mappings remain covered. |
| T056 | **GREEN** | Registered tool, exported environment, ancestry, false session/binding, and subagent rejection are independently exercised for Pi and OMP. |
| T060 | **GREEN** | Both registered parent surfaces route only through the exact bound session and direct-child attestation. |

## Explicit non-credit

- No product protocol mock is counted as a real-product cell.
- No physical Linux or macOS product acceptance was run here.
- No central registry/coordinator, production component authorizer/broker,
  launch handoff, installer, federation, or end-to-end release credit is
  claimed.
- Pinned real Pi `0.84.4` and OMP `18.0.11` Phase-0 evidence remains
  corroboration only; it is not substituted for these implementation tests.

The Pi/OMP product-local Phase-C gaps identified by the earlier RED audit are
closed. Remaining gates belong to central composition and the platform/product
acceptance phases.
