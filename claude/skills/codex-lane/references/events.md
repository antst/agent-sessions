# Codex lane event reference

Every lifecycle command writes newline-delimited JSON to stdout, one object per line, each with a
`type`. Human-readable diagnostics go to stderr and are never part of the contract. Redirect stdout
to a file and parse it; do not rely on `jq` being installed.

## Lifecycle events

| `type` | Emitted by | Key fields |
|---|---|---|
| `thread.started` | `run`, `start` | `thread_id`, `session_id`, `peer_name` |
| `thread.resumed` | `resume` | `thread_id`, `session_id`, `peer_name` |
| `turn.started` | `run`, `start`, `resume`, `wait` | `thread_id`, `turn_id` |
| `lane.ready` | `run`, `start`, `resume` | `contract_version`, `name`, `thread_id`, `session_id`, `address`, `turn_id`, `cwd`, `worktree_path`, `notify_target`, `persistent`, `auto_archive`, `auto_archive_after_seconds`, `owner_session_id`; resume also has `resumed: true` |
| `item.completed` | `run`, `resume`, `wait` | `turn_id`, `item` |
| `turn.schema_retry` | `run`, `resume`, `wait` | `thread_id`, `turn_id`, `attempt` |
| `turn.completed` | `run`, `resume`, `wait` | `turn_id`, `status`, `outcome`, `exit`, `accounting`, `usage`, `error` |
| `turn.interrupted` | `interrupt` | `thread_id`, `turn_id` |
| `lane.status` | `status` | see below |
| `lane.list` | `list` | `contract_version`, `lanes` |
| `lane.doctor` | `doctor --json` | `contract_version`, reachability, runtime/profile paths |
| `lane.archived` | `archive` | `name`, `thread_id`, `notices_dropped` or `already_archived`; repeated archive also reports `retirement_reasserted` |
| `error` | any | `message`, `timeout` |

`lane.ready` is the readiness barrier: the lane's turn has already started, its durable state
exists, and the peer is registered. There is no window in which a discoverable lane has no turn to
receive a message. Check `contract_version` here; refuse anything other than `2`.

## Reading the final answer

`item` is passed through from App Server with its `type` normalized to snake case. An agent message
carries `id`, `type`, `phase`, and `text`.

1. Take the **last** `turn.completed` in the stream; read `turn_id`, `outcome`, `exit`.
2. Select the `item.completed` where `turn_id` matches and `item.type == "agent_message"` and
   `item.phase == "final_answer"`.
3. The result is `item.text`.

A `final_answer` followed by `turn.schema_retry` is a rejected draft from a schema-constrained lane.
That retry event's `turn_id` identifies the rejected draft and `attempt` is the 1-based correction
attempt. Only the `final_answer` belonging to the terminal `turn_id` is authoritative. Anchoring on
the terminal `turn_id` handles this case without special-casing it.

## Terminal status

`status` is App Server's raw value. `outcome` is the normalized value and is the one to report.
They differ when a durable deadline fires: raw `status` is usually `interrupted` while `outcome` is
`timed_out`.

| `outcome` | `exit` |
|---|---|
| `completed` | 0 |
| `timed_out` | 124 |
| `interrupted` | 130 |
| anything else | 1 |

## `lane.status`

`name`, `thread_id`, `session_id`, `status`, `cwd`, `turn_id`, `turn_status`, `collected_turn_id`,
`worktree_path`, `notify_target`, `persistent`, `auto_archive`, `auto_archive_after_seconds`, `auto_archive_at`, `owner_session_id`,
`outcome`, `exit`.

`outcome` and `exit` are `null` while a turn is still running. `collected_turn_id` is the `wait`
cursor: a terminal `turn_id` that does not equal it is uncollected. An archived lane reports
`status: "archived"` and keeps its recorded `outcome`. Its transcript is resumable, but a prior
uncollected detached answer is no longer available through `wait`.

## Discovery and preflight

`lane.list` contains a `lanes` array. Each row carries `name`, `thread_id`, `session_id`, `cwd`,
`status`, `turn_id`, `collected_turn_id`, `worktree_path`, `notify_target`, `persistent`,
`auto_archive`, `auto_archive_after_seconds`, `auto_archive_at`, `owner_session_id`, `outcome`, and `exit`.
Active lanes are returned by default and `--all` adds archived, resumable lanes.
`--mine` filters by the current orchestrator's owner PID and process-start identity. It excludes
persistent lanes; combine it with `--all` to include owned archived lanes. It fails closed when no
live Codex or Claude owner can be corroborated.

`auto_archive_at` is a Unix-millisecond deadline or `null`. It is armed only after the latest turn
has reached its final terminal state and cleared when a newer turn starts. It is a not-before
deadline; cleanup normally occurs within five seconds afterward on the supervisor's next
reconciliation tick.
`auto_archive_after_seconds` is the persisted grace duration and defaults to `60`.

`lane.doctor` carries `runtime_version`, `runtime_path`, `appserver_reachable`,
`appserver_socket`, `supervisor_reachable`, `supervisor_socket`, `codex_home`, and `state_root`.
The command exits non-zero when either service is unreachable even though its JSON report is still
complete.

## Accounting

Every `turn.completed` carries an `accounting` block with App Server duration, start, and completion
fields plus normalized token counters, and a `usage` block. `cost` is deliberately `null` with
`cost_available: false` — the bridge does not guess pricing. Do not compute or present a cost.

## Terminal notices

Parent-owned Claude lanes deliver a durable terminal pointer to their corroborated owner unless
`--no-notify` was passed. Persistent lanes deliver one only when launched with `--notify PEER`.
The pointer contains lane, thread, turn, status, outcome, exit, `collection=required`, and the exact
`wait` command. `lane.ready` reports the selected target, ownership, and persistence mode.
Terminal pointers, and ordinary peer messages sent by a parent-owned lane to its exact owner, use
that established parent relationship and do not trigger a prompting/bypass mismatch approval.
Messages from unrelated or persistent lanes retain their actual sender permission class.

The notice describes the App Server turn. It never carries the answer and never attests that
anything was collected. `collection=required` means: call `wait`.
