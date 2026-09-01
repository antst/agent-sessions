# Qwen Code worker lanes

`qwen-peer-lane` is the durable, messageable Qwen target. One lane manager owns
one stdio ACP client and one native Qwen conversation for the lane's lifetime.
It never attaches a second writer to an interactive Qwen session.

Managed Codex, Claude, Grok, and Qwen orchestrators call the process-attested
`agent_sessions.lane` MCP tool with `product: "qwen"`; the daemon owns the
manager and worker. The CLI spelling below is the host-operator and argument
contract rather than the model orchestration transport.

## Commands

```text
qwen-peer-lane run      --name NAME [OPTIONS] < prompt.md
qwen-peer-lane start    --name NAME [OPTIONS] < prompt.md
qwen-peer-lane resume   SESSION_OR_NAME [OPTIONS] < prompt.md
qwen-peer-lane wait     SESSION_OR_NAME [--timeout SECONDS]
qwen-peer-lane status   SESSION_OR_NAME
qwen-peer-lane interrupt SESSION_OR_NAME
qwen-peer-lane archive  SESSION_OR_NAME
qwen-peer-lane list     [--all] [--mine]
qwen-peer-lane doctor   --json
```

`start` returns after `lane.ready`; `run` starts and collects. Use exactly one
`wait` collector. A successful collection acknowledges one terminal turn and
is not replayed. `interrupt` normalizes to `outcome=interrupted`, exit 130.
`archive` is idempotent. `resume` preserves the Agent Sessions lane UUID and
resumes the persisted native Qwen UUID after native unarchive when necessary.

## ACP lifecycle

The manager launches `qwen --acp` with relaunch disabled, then performs one
ordered client sequence:

```text
initialize
session/new or session/resume
session/set_mode (only when explicitly requested)
session/prompt                 # serialized turns
session/cancel                 # interrupt
```

The `agent_sessions` MCP is injected exactly once into the new/resumed native
session. Unknown notifications are ignored. Malformed, out-of-order, or
unsupported-version responses fail closed. No second ACP client is created for
follow-ups.

Qwen owns native permission changes. `--no-yolo`, `--yolo`, and
`--approval-mode MODE` select only the initial request; they conflict and are
rejected before worker creation. Status distinguishes the durable launch
preference, expected initial mode, and current observed mode or `unknown`.

## Ownership, groups, and notices

A normal lane is owned by the exact live parent and archives when that parent
exits. `--persistent` removes parent-exit lifecycle ownership only. Auto-archive
still defaults to a 60-second terminal grace; use `--auto-archive-after SECONDS`
to change it, or pair `--persistent --no-auto-archive` for indefinite idle
retention and explicitly archive later.

Every child receives its private destination anchor and the immediate parent's
private anchor. Repeated `-g/--group` adds explicit groups. `--inherit-groups`
adds the parent's non-private groups; `--no-inherit-groups` retains only the
mandatory anchor. Parent-owned terminal notices route to the exact parent and
carry a runnable `wait` command, including a non-default federator runtime dir.
Remote federation owns lifecycle flags, so callers do not pass
`--persistent` or auto-archive flags to a remote lane request. The destination
retains the hub-attested source peer as the immediate parent for terminal notices.

## Native archive and cleanup

Archive first closes turn admission, cancels an active turn, retires the exact
ACP worker and detached tool roots, withdraws messaging, and then uses a short-
lived token-authenticated loopback `qwen serve` helper for native
archive/unarchive. The helper is capability-gated, bound to localhost on an
ephemeral port, and stopped with its preheated child tree after the transaction.
There is no long-lived Qwen network service.

Manager, worker, tool-root, helper, owner, and socket identities are persisted
with strong process starts. Crash reconciliation preserves reused PIDs and
changed artifacts, records retryable debt, and removes only exact owned state.

## Doctor

`doctor --json` is session-free. It checks the exact executable/package and
version floor, parser semantics, ACP `initialize` only, required session/MCP
capabilities, native archive capability, trusted canonical cwd, selected
profile identity, exact plugin inventory, and non-secret provider/credential
configuration state. It does not create a transcript, authenticate a live
model session, or claim the effective mode of a launch that has not happened.

See [QWEN-ADAPTER.md](QWEN-ADAPTER.md) and [QWEN-INSTALL.md](QWEN-INSTALL.md).
