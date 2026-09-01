# Troubleshooting

## Distinguish the daemon from connectors

The supported local topology has exactly one service-managed Agent Sessions
daemon per operating-system user and host:

```text
.../bin/agent-sessions daemon
```

Processes such as these are not additional daemons:

```text
.../bin/agent-sessions connector auto --release-identity SHA256
.../bin/agent-sessions connector claude --release-identity SHA256
```

A connector is a short-lived stdio MCP relay. A vendor client starts one for
each open MCP connection, and the relay forwards authorized calls to the
already-running daemon. Therefore a healthy host may have zero, one, or many
connector processes. Several connectors with the same 64-character release
identity are expected: the value is the SHA-256 identity of the executable
image they share, not a daemon or session identifier.

The literal value `@AGENT_SESSIONS_RELEASE_ID@` is a source-manifest
placeholder. A current installed connector replaces it with its executable
identity before serving. If it remains visible for more than the brief startup
window, the client has retained a stale plugin/MCP snapshot or is resolving a
source-tree manifest. Complete the current host install, then restart only the
affected vendor client so it reloads the installed inventory. Do not start a
second daemon to compensate.

## Check Linux service health

Use service and product diagnostics first:

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
systemctl --user status agent-sessions.service
journalctl --user -u agent-sessions.service -n 100 --no-pager
```

For a process census that does not match its own search command:

```sh
pgrep -a -u "$(id -u)" -f '/agent-sessions daemon$'
pgrep -a -u "$(id -u)" -f '/agent-sessions connector '
```

The first command should report one daemon. The connector count follows the
number of open product MCP clients and is not expected to be one.

If the daemon is absent or unhealthy, let systemd own recovery:

```sh
agent-sessions daemon restart
agent-sessions doctor
```

The lifecycle command delegates to the native user service manager; it does
not send a shutdown or restart request over the daemon endpoint.

Peer, lane, hook, connector, and federation workflows require an already
running daemon. They fail closed and never create a replacement daemon.

## Check macOS service health

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
tail -n 100 ~/.local/state/agent-sessions/logs/agent-sessions.stdout.log
tail -n 100 ~/.local/state/agent-sessions/logs/agent-sessions.stderr.log
```

Ask launchd to restart the one daemon with:

```sh
agent-sessions daemon restart
```

## Preserve vendor sessions during recovery

A daemon restart must preserve live vendor processes, native session IDs,
transcripts, credentials, and the Codex App Server. Do not kill a vendor TUI,
App Server, Grok leader, or Qwen process merely because a connector or daemon
is unhealthy. After daemon recovery, live managed participants reattach with
their existing identities; a vendor client that retained an old connector
inventory may need that client alone restarted.

## Pre-unification development cleanup

The normal installer, updater, remover, and daemon do not discover or delete
the old unreleased split stack. Only the three controlled development hosts
may use the repository-only cleanup utility. Review a mutation-free plan first,
then apply exactly its revision in a separate command:

```sh
./scripts/cleanup-pre-unification
./scripts/cleanup-pre-unification --apply REVISION_FROM_PLAN
```

The utility is not shipped in release archives and has no authority over
vendor profiles, credentials, transcripts, histories, or unrelated processes.
Do not use it as routine daemon recovery.

If an interrupted unified install already removed the legacy `grok-peer` or
`qwen` wrapper, the reviewed plan records that exact tool as absent and removes
only the contracted directory-discovered Grok/Qwen residue. Codex and Claude
native unregister tools remain mandatory. Read-only directories inside a
current-user-owned contracted legacy tree are made owner-writable only while
that tree is being removed.
