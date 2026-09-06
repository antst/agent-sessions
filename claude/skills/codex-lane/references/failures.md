# Codex lane failure taxonomy and recovery

Failures are reported two ways at once: an `error` event on stdout with a `message` (and a boolean
`timeout`), and a non-zero exit code. Read the message before deciding anything. Blind retries are
the main way an orchestrator turns a recoverable condition into a duplicated or orphaned lane.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Completed |
| 124 | No terminal event: the bounded collection call expired. `turn.completed` with `outcome: "timed_out"`: the lane deadline fired. |
| 130 | Interrupted |
| 1 | Everything else, including every startup failure |

## Conditions

| Symptom | What it means | Correct response |
|---|---|---|
| `codex-peer-lane` not found | The host runtime alias is not installed | Print the install commands and stop. Never install, build, or start a daemon. |
| Runtime present but no `contract_version` / no `doctor --json` | Runtime predates contract 2 | Report the version gap and stop. Do not guess the event shape. |
| bare name is ambiguous | Sessionbus messaging found multiple group-visible peers, or a lane lifecycle command found multiple host-local lanes | Use the exact lane/session ID. |
| `error` at `start`, supervisor or App Server unreachable | Runtime is not running | Report it. Recovery is a host-level operation, not an orchestrator's. |
| `error` at `start`, bad `--cd` | Directory missing or not permitted | Fix the path with the caller; do not silently substitute another directory. |
| `wait` exits 124 | The **collection call** timed out | Not a lane failure. Re-`wait`, or `status` to check. |
| `turn.completed` with `outcome: "timed_out"`, exit 124 | The lane's own durable deadline fired | This is a real terminal outcome. Report it; a partial answer may still exist. |
| `final_answer` then `turn.schema_retry` | Schema-constrained turn produced an invalid draft | Discard the draft; keep reading until the terminal `turn_id`. |
| Collector killed, or broken pipe | The cursor was never acknowledged | Re-`wait` during the configured terminal grace period is safe and returns the same turn. |
| `interrupt` reports no active turn | The lane already reached a terminal state | Go collect instead. |
| `archive` reports `notices_dropped` > 0 | A terminal notice could not be delivered | Informational. Archive is authoritative and succeeded. |
| `archive` reports `already_archived: true` | Archive is idempotent | Success. Exit code is 0. |
| `status` shows `archived` with an `outcome` | Lane is retired but its transcript survives; an uncollected detached answer does not | `resume` only to start a new follow-up turn. It cannot recover the prior answer. |

## Things that are never true

If runtime services become unavailable after a successful preflight, stop and ask the user to run
`codex-peer-lane doctor` once in a host terminal. Do not bootstrap services from the orchestrator.

- A terminal notice is not a result. `collection=required` means call `wait`.
- A missing answer after a successful `wait` is not normal — a successful turn is not acknowledged
  until its `final_answer` is persisted. Report the anomaly rather than papering over it.
- Two `wait` calls on one lane are never correct. The cursor is single-consumer, and a second
  consumer will take turns the first one needed.
- Killing a collector does not stop the turn. Owner exit interrupts and archives an ordinary lane;
  otherwise the configured terminal grace (one minute by default) still applies. Only
  `--persistent --no-auto-archive` remains indefinitely until explicitly interrupted or archived.
