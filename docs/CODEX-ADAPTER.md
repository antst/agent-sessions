# Codex daemon adapter

The Codex integration is an in-process adapter of the one per-user-host `agent-sessions` daemon. The
Codex CLI/TUI and App Server remain external vendor processes. Agent Sessions no longer runs a
profile supervisor, per-thread shim, per-lane shim, or Codex-specific listener.

## Interactive peers

`codex-peer` is an installed alias of the canonical `agent-sessions` image. It asks the running daemon
to prepare a launch or resume, resolves one exact root thread and canonical cwd, then execs the native
Codex client. It never starts or supervises the daemon.

The daemon publishes the peer only after its prepared capability, profile, cwd, thread ID, native TUI
owner, hook context, App Server identity, and effective permission mode agree. Ordinary Codex sessions
have no capability and remain bare/unmanaged even though the connector is installed.

The adapter maintains one App Server coordinator per configured Codex profile inside the daemon. It
uses native thread operations for delivery and lifecycle observation; durable message acceptance,
groups, retries, and at-most-once destination results belong to the daemon.

## Restart and resume

Daemon restart does not restart the Codex TUI or App Server. Recovery reopens the App Server client,
loads durable attachment evidence, and re-corroborates the exact live owner before republishing it.
An ambiguous owner stays unavailable and records debt.

Codex owns rollout history and its paginated history projection. If a large legacy thread opens with
blank history through the App Server while native standalone resume can read it, run the vendor
maintenance command:

```sh
codex migrate-rollouts --apply
```

Close active writers first or rerun for threads reported busy. Agent Sessions does not copy or repair
Codex history databases.

## Codex lanes

Codex lane state and turn ownership live in the daemon. The adapter starts, reconnects, interrupts,
collects, and archives through the App Server using the committed thread/turn identity. Restart must
not redispatch an accepted turn. A terminal notice and collection cursor are daemon-owned metadata;
the native transcript remains Codex-owned.

See [CODEX-LANES.md](CODEX-LANES.md) for commands and lane behavior.

## Safety boundary

Before delivery, interruption, archive, or cleanup the adapter revalidates the exact thread, profile,
cwd, App Server, and native owner evidence. PID or thread ID alone is insufficient. Cleanup removes
only exact Agent Sessions records; it never deletes Codex rollouts, archived sessions, settings,
credentials, or the App Server's native state.

For the shared contract, see [ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md).
