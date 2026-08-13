# Operations

## Topology

Run one hub on a stable VLAN address and one agent for every OS user whose Claude registry should
be federated. Agents make an outbound TCP connection to the hub on port 7419 by default. The hub
does not need access to users' home directories.

Use a stable, unique `PEER_FEDERATOR_HOST` on every host/user agent. Reusing a host ID causes the
new connection to replace the old one. One runtime directory permits exactly one agent process;
the second process fails instead of unlinking the first process's control socket.

## Session prerequisites

The agent exports only live numeric records in `$CLAUDE_CONFIG_DIR/sessions` that contain a
connectable `messagingSocketPath`.

- Interactive Codex sessions need the `agent-sessions` peer integration installed and active.
- Codex lanes use the same registry and need no federation-specific setup.
- Claude Code must be launched with peer messaging enabled. In the currently tested Claude Code
  release this means `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` in the environment that launches
  Claude.

`peer-federator doctor --hub HOST:7419` reports live registry records that do not meet this
requirement. Zero active sessions is valid; a live record without a socket is not.

Remote lane execution is disabled by default because it authorizes every host connected to the
trusted hub to execute native lane commands as the destination OS user. Set
`PEER_FEDERATOR_ENABLE_REMOTE_LANES=true` (or pass `--enable-remote-lanes`) only where that trust is
intended. There is no per-request approval prompt.

When enabled, remote lane execution additionally requires the matching destination launcher. The agent searches
`PATH` and then `~/.local/bin` for `codex-peer-lane` and `claude-peer-lane`. Override either path
with `PEER_FEDERATOR_CODEX_LANE` or `PEER_FEDERATOR_CLAUDE_LANE`. `peer-federator hosts` reports
only capabilities actually available on each currently connected agent.
Claude lane workers are currently conformance-tested with Claude Code 2.1.227; an executable can
be discoverable while an older Claude release still fails its native worker-readiness check.

Each enabled destination accepts at most 32 concurrent remote lane CLI processes. Remote callers
cannot disable auto-archive and cannot request an auto-archive delay above 86,400 seconds. The
native CLI inherits the agent service's working directory, so callers should pass `-C`/`--cd` on
`run` or `start` whenever repository location matters. `resume` retains its established cwd.
Remote stdin is capped at 1 MiB; `--prompt-file` is a destination-local path, not a transfer.
Automatic source-session selection requires a corroborated Codex or Claude ancestor; detached
automation must provide `--source-session` explicitly.

## Linux user services

Install the binary and unit files:

```sh
make install-systemd-user
cp ~/.config/peer-federator/agent.env.example ~/.config/peer-federator/agent.env
```

The install target copies both environment templates into `~/.config/peer-federator/` and never
overwrites `agent.env` or `hub.env`. Edit `agent.env`, then enable the agent:

```sh
systemctl --user daemon-reload
systemctl --user enable --now peer-federator-agent.service
peer-federator status
```

On the hub host, also copy `~/.config/peer-federator/hub.env.example` to
`~/.config/peer-federator/hub.env` and enable `peer-federator-hub.service`. Permit TCP port 7419
inside the VLAN firewall.

## macOS launchd

Install the binary and launchd templates:

```sh
make install-launchd-user
cp ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist.example \
  ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
```

The install target updates only the `.plist.example` templates and never overwrites or loads an
active `.plist`. Replace every `CHANGE_ME` and network placeholder in the copied file, validate it,
then load it:

```sh
plutil -lint ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
launchctl bootstrap "gui/$(id -u)" \
  ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
peer-federator status
```

On the hub host, copy and configure `net.antst.peer-federator.hub.plist.example` in the same way.

## Names and delivery

Remote peers appear as `name--host`. A first name-based send can request the ordinary native
`name--host [ref]` confirmation. Replies to the incoming message's `from` socket work immediately.

Federation is live and best-effort. There is no offline queue. A peer that exits is removed on the
next scan; a message racing that removal can be dropped and logged. Hub loss removes remote shadow
rows while local discovery and local messaging continue. Agents reconnect with bounded backoff and
rebuild the roster after the hub returns.

Remote spawning and lifecycle commands are also hub-only. There is no TCP listener on an agent for
another agent to call directly and no SSH fallback. A disconnected local agent rejects a new remote
lane request. Hub or source-agent loss sends cancellation to an active destination command; agent
shutdown also interrupts commands it owns. A persistent lane created by a completed remote `start`
is owned by the destination's native lane supervisor, so it may continue locally during an outage,
but its federated peer and cross-host control path disappear until the hub reconnects. Remote
callers cannot pass `--no-auto-archive`, so every remotely created persistent lane retains a local
cleanup deadline.

If an agent is killed abruptly, its per-command liveness descriptor closes. The surviving
watchdog interrupts and then forcibly reaps the native lane CLI, preventing abandoned collectors
from retaining lane locks.

## Diagnostics

```sh
peer-federator doctor --hub 10.2.17.1:7419
peer-federator status
peer-federator hosts
journalctl --user -u peer-federator-agent.service
```

`doctor` validates the protocol handshake, registry, messageable-session count, and local agent
presence. `status` queries the running agent and reports hub connectivity, lane capabilities, and
local, remote, host, and shadow counts as JSON. `hosts` fails when the local agent is disconnected
rather than returning a stale roster.

## Rolling upgrades

The protocol uses an integer compatibility version. Upgrade the hub first, then agents one host at
a time. A protocol mismatch fails closed during the hello/probe handshake. During each agent
restart its remote shadows disappear briefly and are reconstructed after reconnection.

Before tagging a release, run `make lint`, `make test`, `make test-race`, and the yolo3 remote smoke.
The release tag must equal `v$(cat VERSION)`.
