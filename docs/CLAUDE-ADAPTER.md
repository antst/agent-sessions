# Claude adapter contract

**Contract version: 2**

This document is the source of truth for what a Claude-side orchestrator may depend on when driving
`codex-peer-lane`. The `codex-peer` plugin under [`claude/`](../claude) implements exactly this
contract; it summarizes the document and never restates it in conflicting words.

An adapter that stays inside this surface keeps working across runtime updates. Anything not listed
here — internal state files, socket layout, App Server request shapes, stderr text — is not part of
the contract and will change.

## Compatibility

A contract-2 runtime must provide:

- the lifecycle subcommands below, including `list`;
- `doctor --json`, returning at least `contract_version`, `runtime_version`,
  `appserver_reachable`, `supervisor_reachable`, `codex_home`, and `state_root`;
- `contract_version` in both `doctor --json` output and every `lane.ready` event.

An orchestrator must check `contract_version` before parsing anything else, and must refuse a
runtime whose major contract version it does not implement rather than guessing at the event shape.
`claude/skills/codex-lane/scripts/lane-preflight` performs this check and exits non-zero when it is not satisfied. It
calls the recorded native runtime directly; preflight never runs the bootstrapping launcher.

## Invocation

`codex-peer-lane` is the launcher. When it is not on `PATH`, read the first line of
`${CLAUDE_PEER_DATA_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/claude-code-peer}/native-runtime-path`
and invoke `<that path> lane …` instead. The two forms are equivalent.

Prompts are supplied on stdin (`-`) or through `--prompt-file FILE`. Never on argv.

## Subcommands

| Command | Blocking | Purpose |
|---|---|---|
| `start --name NAME [POLICY] -` | Returns at readiness | Create a lane and begin its first turn |
| `run --name NAME [POLICY] -` | Until the turn ends | Foreground equivalent; unsuitable for detached use |
| `resume TARGET [POLICY] -` | Until the turn ends | New turn on the same transcript; unarchives if needed |
| `wait TARGET [--timeout S]` | Until a turn is collected | Single-consumer cursor over turn history |
| `status TARGET` | No | One `lane.status` object |
| `list [--all] [--mine]` | No | Enumerate known lanes as JSONL; `--all` includes archived and `--mine` selects this orchestrator's owned lanes |
| `interrupt TARGET` | No | Interrupt the active turn |
| `archive TARGET` | No | Retire the peer; idempotent |

`TARGET` is a lane name or a thread ID.

`run` must not be shell-backgrounded to emulate a detached lane. Use `start` followed by `wait`.
Because a Claude `Bash` call is limited to 600 seconds, a foreground `run` is only appropriate for
work that certainly finishes inside that window.

`resume` starts one new turn and collects it synchronously. A Claude adapter may run that command
as a background `Bash` call for a long follow-up, but must not attach `wait` concurrently. If the
resume collector is killed, the unacknowledged turn is recoverable through a later `wait`. Collect
any older outstanding turn before invoking `resume`: an explicit resume begins a new collection
cursor for its follow-up turn, while the older content remains only in the Codex transcript.

## Output framing

Every command writes newline-delimited JSON to stdout, one object per line, each carrying a `type`.
Diagnostics go to stderr and are not part of the contract.

## Events

| `type` | Fields |
|---|---|
| `thread.started` | `thread_id`, `session_id`, `peer_name` |
| `thread.resumed` | `thread_id`, `session_id`, `peer_name` |
| `turn.started` | `thread_id`, `turn_id` |
| `lane.ready` | `contract_version`, `name`, `thread_id`, `session_id`, `address`, `turn_id`, `cwd`, `worktree_path`, `notify_target`, `persistent`, `auto_archive`, `auto_archive_after_seconds`, `owner_session_id`; resume also has `resumed: true` |
| `item.completed` | `turn_id`, `item` |
| `turn.schema_retry` | `thread_id`, `turn_id`, `attempt` |
| `turn.completed` | `turn_id`, `status`, `outcome`, `exit`, `accounting`, `usage`, `error` |
| `turn.interrupted` | `thread_id`, `turn_id` |
| `lane.status` | `name`, `thread_id`, `session_id`, `status`, `cwd`, `turn_id`, `turn_status`, `collected_turn_id`, `worktree_path`, `notify_target`, `persistent`, `auto_archive`, `auto_archive_after_seconds`, `auto_archive_at`, `owner_session_id`, `outcome`, `exit` |
| `lane.list` | `contract_version`, `lanes` |
| `lane.doctor` | `contract_version`, `runtime_version`, `runtime_path`, `appserver_reachable`, `appserver_socket`, `supervisor_reachable`, `supervisor_socket`, `codex_home`, `state_root` |
| `lane.archived` | `name`, `thread_id`, and `notices_dropped` or `already_archived` |
| `error` | `message`, `timeout` |

`lane.ready` is the readiness barrier: the lane's turn has already started, durable state exists,
and the peer is registered. There is no interval in which a discoverable lane has no turn able to
receive a message.

`item` is passed through from App Server with its `type` normalized to snake case. An agent message
carries `id`, `type`, `phase`, and `text`.

## Collecting a result

1. Take the **last** `turn.completed`; read `turn_id`, `outcome`, `exit`.
2. Select the `item.completed` whose `turn_id` matches, with `item.type == "agent_message"` and
   `item.phase == "final_answer"`.
3. The result is that item's `text`.

A `final_answer` emitted before a `turn.schema_retry` is a rejected draft from a schema-constrained
lane. On `turn.schema_retry`, `turn_id` identifies that rejected draft and `attempt` identifies the
1-based correction attempt. Anchoring on the terminal `turn_id` discards the draft without
special-casing.

`wait` is a **single-consumer cursor**. Its first successful call collects the initial turn; within
that cursor, each later call blocks for the oldest uncollected turn, including one started by an
inbound peer message. Its durable order is preserved when several wakes finish before collection
and when a schema correction replaces a rejected draft. An explicit `resume` begins the new cursor
described above. `wait` never replays the last completed turn. Concurrent `wait` calls on one lane, or mixing `wait` with
another collector, are contract violations. Interactive `codex-peer` sessions do not use this cursor
at all.

Acknowledgement happens only after the terminal items and the terminal event have been emitted, so
an interrupted collector or a broken output pipe never marks unseen content as collected — re-`wait`
returns the same turn while the lane remains in its terminal grace period. `auto_archive_at` in
`status` and `list` is the exact Unix-millisecond deadline; it is `null` before the timer is armed.

`lane.list` returns active lane records by default; `--all` also includes archived, resumable
records. `--mine` matches the current orchestrator by owner PID plus process-start identity rather
than mutable session ID; it excludes persistent lanes and fails when no live Codex, Claude, or Antigravity owner
can be corroborated. `lane.doctor` always emits its JSON report and exits non-zero when App Server or the peer
supervisor is unreachable.

## Terminal status and exit codes

`status` is App Server's raw value; `outcome` is normalized and is the one to report. They differ
when a durable deadline fires: raw `status` is usually `interrupted` while `outcome` is `timed_out`.

| `outcome` | `exit` |
|---|---|
| `completed` | 0 |
| `timed_out` | 124 |
| `interrupted` | 130 |
| anything else | 1 |

A `wait` call that exits 124 because its own `--timeout` expired means the collection call ended,
not that the lane failed. Unlike `--timeout` on `run`, `start`, or `resume`, it never interrupts or
relabels the lane.

## Accounting

Every `turn.completed` carries an `accounting` block with App Server duration, start, and completion
fields plus normalized token counters, and a `usage` block. `cost` is `null` with
`cost_available: false`; the runtime does not guess pricing, and neither should an adapter.

## Terminal notices

Parent-owned lanes automatically register a durable supervisor-owned terminal job for their Claude
owner. On any terminal outcome the owner receives a pointer carrying the lane name, thread ID,
turn ID, raw status, normalized outcome, exit code, `collection=required`, and the exact `wait`
command. `--no-notify` suppresses this pointer without changing ownership.

Direct Claude ownership is accepted only when environment hints, the live registry row, socket,
available process identity, and process ancestry all agree. The resulting owner is the corroborated
Claude session process, not a short-lived shell or adapter subprocess. Other launchers use their
direct parent process as owner and have no automatic peer notification. Inherited Claude environment without
matching ancestry is ignored. `lane.ready.notify_target` and `owner_session_id` expose the result.

`--persistent` removes parent ownership. Only persistent lanes accept `--notify PEER`; this creates
the same durable pointer job for an explicitly selected peer. Persistent lanes survive every
launcher or orchestrator exit; use `--no-auto-archive` as well when they must remain idle indefinitely.

The notice describes the App Server turn. It never carries the answer and never attests that a
collector received anything. `collection=required` means: call `wait`.

`archive` makes one last delivery attempt, then cancels any undeliverable notice and reports
`notices_dropped` rather than letting a hint block authoritative cleanup.

### When the orchestrator exits

The supervisor monitors the recorded owner process. When it exits or crashes, a parent-owned lane's
active turn is interrupted and the lane is archived after becoming terminal. Archiving removes the
discovery shim and retains its Codex transcript and resumable lane metadata. This rule also covers
an owner that exits after the turn completed, preventing idle shim accumulation.

Killing the shim itself is not owner exit and never means cleanup: the supervisor replaces it while
the underlying thread remains eligible for discovery. Use `archive` for explicit retirement. Lane
records created before contract 2 have no trustworthy owner and are left untouched for an explicit
one-time archive rather than being guessed or adopted.

`--persistent` is required when work must outlive the orchestrator. It disables owner cleanup but
does not disable the default terminal grace timer.
It is also required when the lane is launched from a plain shell, cron job, or CI runner rather than
beneath a live Codex, Claude, or Antigravity process; those callers have no stable lifecycle owner to corroborate.

Auto-archive is independent of ownership and enabled by default. After the latest turn reaches its
final terminal state, including any schema correction, the lane remains idle and messageable for one
minute and is then archived. `--auto-archive-after SECONDS` configures a different grace with
millisecond granularity (minimum `0.001`).
A newer turn cancels the timer. Callers must collect within that grace
period; after archive the transcript remains resumable but the detached result is no longer available
through `wait`. `--no-auto-archive` disables this timer and conflicts with a custom grace. A permanent lane therefore uses
`--persistent --no-auto-archive` and must be retired explicitly.
Each `resume` preserves persistent versus parent-owned lifecycle and its notification target. Re-pass
a custom auto-archive grace or it returns to 60 seconds.
`auto_archive_at` is an exact not-before deadline; the supervisor normally performs cleanup within
five seconds after it on the next reconciliation tick.
If this archive happens before collection, the detached answer is no longer available through
`wait`. `resume` starts a new follow-up turn; it does not recover the archived answer.

## Policy

All policy flags are optional and every omitted flag inherits the user's normal Codex and
`CODEX_HOME` configuration: `-m/--model`, `--effort`, `--sandbox`, `--approval-policy`,
`--web`/`--no-web`, repeated `-c KEY=VALUE`, the lane deadline `--timeout` on
`run`/`start`/`resume`, `--schema`, `--worktree`,
`--allow-duplicate-name`, `-C/--cd`.

The lane deadline defaults to zero (disabled). `wait --timeout` also defaults to zero and therefore
waits without a bridge-imposed collection bound.

`--persistent`, `--auto-archive-after`, `--no-auto-archive`, `--notify`, and `--no-notify` control lifecycle and result
routing, not Codex model policy. `--notify` requires `--persistent`; parent-owned Claude lanes notify their owner
automatically unless `--no-notify` is present. The runtime never infers model, sandbox, approval,
or web policy.

An automatic terminal pointer is generated by the supervisor rather than by the lane model. Its
transport envelope matches the live Claude target's permission class so the pointer is not held by
Claude's prompting/bypass gate. Ordinary messages from a parent-owned lane to that exact parent use
the same established relationship; messages to unrelated peers retain the lane's actual permission
class. This delivery behavior does not alter the lane's Codex policy.
If the target cannot be mapped to a live peer process and classified, the send is retried from the
durable notice queue; the runtime does not mark an unclassified socket write as successful.

An adapter must pass through what its caller stated and omit everything else. The runtime overlays
exactly one internal setting, `features.code_mode_host=false`, which is headless execution plumbing
rather than agent policy.

A detached lane cannot answer an interactive approval prompt, so unattended tool use normally wants
`--approval-policy never`. Neither the runtime nor an adapter may choose that silently.

## Naming

Use unique, stable, role-based names and check both `list --all` (active and archived lanes) and
`claude agents --json` (other live peers); neither registry is complete alone. Archived names stay
reserved for transcript-preserving `resume`. Claude's native sender may require a transient `name [ref]`
confirmation token; refs must never be persisted. Lane lifecycle commands accept raw thread IDs,
but Claude's native sender does not address peers by Codex UUID.

## Failure handling

| Symptom | Correct response |
|---|---|
| Runtime absent | Report the install path and stop. An adapter never installs, builds, or starts host services. |
| `contract_version` missing or unknown | Stop. Do not guess the event shape. |
| `error` at `start` | Read `message`; name collision, unreachable supervisor, and bad `--cd` are distinct conditions. No blind retry. |
| `wait` exits 124 | Collection call ended, not lane failure. Re-`wait` or `status`. |
| `final_answer` then `turn.schema_retry` | Discard the draft; keep reading to the terminal `turn_id`. |
| Collector killed or pipe broken | Cursor unacknowledged; re-`wait` is safe. |
| `interrupt` reports no active turn | Already terminal; collect instead. |
| `archive` reports `notices_dropped` > 0 | Informational; archive is authoritative. |
| `status` shows `archived` with an `outcome` | `resume` only to start a new follow-up turn; it cannot recover an uncollected prior answer. |

Killing a collector does not stop a lane while its owner remains alive. Parent exit does: the
supervisor interrupts active work and archives the lane. A persistent lane outlives launchers; add
`--no-auto-archive` when it must also remain idle and messageable until explicit `interrupt` or `archive`.

## Related

- [LANES.md](./LANES.md) — the runtime-side description of lanes.
- [CLAUDE-INSTALL.md](./CLAUDE-INSTALL.md) — installing the Claude-side plugin.
