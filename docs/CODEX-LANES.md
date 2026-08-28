# Codex lanes

A Codex lane is a durable daemon-owned Agent Sessions lane backed by a vendor-owned Codex thread in
App Server. `codex-peer-lane` is an alias of the canonical `agent-sessions` image and is a client of
the already-running user daemon; it is not a lane manager and binds no socket.

## Lifecycle

```sh
codex-peer-lane start --name review --cd /srv/project --prompt-file prompt.txt
codex-peer-lane status --name review --json
codex-peer-lane wait --name review --json
codex-peer-lane interrupt --name review
codex-peer-lane resume --name review --prompt-file followup.txt
codex-peer-lane archive --name review
codex-peer-lane list --mine --json
codex-peer-lane doctor --json
```

`run` combines start and wait for a foreground workflow. `start` durably accepts a turn before native
dispatch. `wait`/`collect` advances the daemon-owned collection cursor at most once. `resume` adds a
turn to the same lane only after exact native thread corroboration. `interrupt` and `archive` target
the exact lane/turn revision and are idempotent.

The daemon owns lane names, parent context, existing global groups, permission mode, turn state,
terminal notices, collection cursor, archive state, and cleanup debt. Codex owns the thread, rollout,
history projection, sandbox, approvals, token/accounting events, and native archive behavior.

## Parent, groups, and permissions

Every child receives its own private group and its parent's private group. Other effective parent
groups are copied only when the launch requests `--inherit-groups`; explicit `--group` values use the
same global group space as peers. Product, profile, host, and lane identities do not create another
access namespace.

The requested Agent Sessions permission mode is translated to Codex's native sandbox/approval
contract and the effective result is recorded. Agent Sessions does not bypass a native denial merely
because the parent is managed.

## Restart

The lane actor is a daemon goroutine. After daemon restart it reconnects through the committed App
Server thread and turn evidence; it never starts the accepted turn twice. If exact native evidence
cannot establish the outcome, the turn stays retryable/debt rather than being guessed complete.

The Codex TUI/App Server may outlive the daemon. No supervisor or discovery shim is reconstructed.
Missing vendor history projection is reported with the native `codex migrate-rollouts` remedy.

## Archive and cleanup

Archive first records durable intent, invokes Codex's native archive operation, verifies the exact
thread outcome, and then commits the daemon lane state. Cleanup removes only exact Agent
Sessions-owned artifacts. Rollouts, archived sessions, credentials, settings, and native history are
never deleted by lane cleanup or normal Agent Sessions removal.

Remote Codex lanes use the same lane engine through the embedded host agents and the one central hub.
There is no remote watcher/CLI/manager process chain and no SSH or local fallback.
