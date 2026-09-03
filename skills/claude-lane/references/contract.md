# Claude lane contract v1

## Commands

`run`, `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list [--all] [--mine]`, and `doctor --json` mirror the Codex lane lifecycle. `--mine` matches the current orchestrator's owner PID plus process-start identity, excludes persistent lanes, and fails if no live Codex or Claude owner can be corroborated.

Non-persistent `run`, `start`, and `resume` likewise require a corroborated live Codex or Claude
lifecycle owner. Callers from a plain shell, cron, or CI environment must pass `--persistent`.

`-C/--cd` is implemented by the manager changing the Claude worker's cwd. `--schema` maps to Claude's `--json-schema`. Claude-specific policy flags are `--permission-mode`, `--tools`, `--allowed-tools`, `--disallowed-tools`, and `--max-budget-usd`. Workers enable Agent Teams and add `SendMessage,ListAgents` to the effective allowed-tools list by default; caller allowances are preserved. Explicit tool availability or deny policy can still remove them. `--bare` is rejected because Claude 2.1.227 publishes no native peer socket in that mode.

There is no Claude thread-archive API. Archive shuts down the manager, native stream worker, control socket, and discovery row while leaving Claude's transcript resumable.

## Events

- `thread.started`: `session_id`
- `thread.resumed`: `session_id`
- `turn.started`: `session_id`, `turn_id`
- `lane.ready`: contract version, product, lifecycle identity, owner, notification, and auto-archive fields
- `item.completed`: normalized `user_message` or `agent_message`
- `turn.completed`: terminal `status`, `outcome`, `exit`, `error`, `usage`, and `accounting`
- `turn.interrupted`: acknowledgement from the explicit `interrupt` command
- `error`: command failure; `timeout: true` distinguishes a collection bound from other errors
- `lane.status`, `lane.list`, `lane.archived`, `lane.doctor`

`lane.doctor` reports `contract_version`, runtime identity, `claude_available`,
`claude_logged_in`, `claude_auth_method`, `claude_api_provider`, profile state, and supervisor
reachability. Require `claude_logged_in: true` before starting work, but treat it only as Claude
Code's local credential-state report. A successful first inference turn is the end-to-end
authentication and entitlement proof.

Claude accounting can include `total_cost_usd`; `accounting.cost_available` is true only when Claude supplied it.

## Outcomes

- `completed` → exit 0
- `failed` → exit 1
- `timed_out` → exit 124
- `interrupted` → exit 130

A `wait` collection timeout also exits 124 but emits no terminal event. A turn deadline emits `turn.completed` with `outcome: "timed_out"`; inspect the event rather than the integer alone.

`resume` requires the lane to have no outstanding active, queued, or uncollected terminal turn. Collect the current debt with `wait` before resuming.
It preserves persistent versus parent-owned lifecycle and the existing notification target unless an
explicit lifecycle or notification option changes them; omitting `--persistent` never demotes a persistent lane.

Unknown Claude stream frame types are ignored. Only the authoritative `result` frame terminalizes a turn.

## Identity and messaging

Claude's SDK worker is the lane's sole discoverable identity and owns the address returned by
`lane.ready`. It receives native peer messages directly. The manager observes peer-origin user and
result frames and records them as lane turns; it does not proxy the message or rewrite the worker's
registry row. Workers pass `--settings '{"crossSessionInbound":"accept"}'` because a headless lane
has no approval UI for native inbound messages. The override is process-local and never rewrites
the host profile's default. Their outbound messages still author under the
lane's real permission class. A corroborated bypass owner is inherited only when the caller did not
explicitly select another permission mode.

An idle-worker peer message creates a collectable turn. A message accepted during an active turn is
a native steer of that turn and shares its single result. Launcher turns are not considered active
until their replayed local-user frame appears, which preserves attribution when native input races a
CLI submission.
If a local replay never appears, a 30-second submission-acknowledgement watchdog fails the turn and
retires the worker whenever no native turn is active. A peer turn that legitimately runs first
refreshes this watchdog when it finishes. The caller's durable `run`/`start`/`resume --timeout`
begins when its launcher turn becomes active; native peer messages do not carry an execution
deadline and can defer the acknowledgement check until they finish.

Lifecycle ownership uses the corroborated Claude process or Codex session-bridge PID plus its
process-start identity. A live Codex-host ancestor is a launch-context precondition; Codex does not
expose a per-thread host PID, so the session bridge provides the stable owner identity. Session IDs
are retained for addressing and notification but are not trusted as process identity because a
long-lived Claude process can rotate its session ID.

Terminal pointers are persisted and retried by the manager. Failed delivery postpones automatic archive for at most 30 seconds; explicit archive remains authoritative and may discard an undelivered pointer.
Pointers are collected with `wait`, not answered through their `from` address; a crash-retry may be delivered after the native worker address has retired.
