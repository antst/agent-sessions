# Federation operations

## Topology

Run exactly one `agent-sessions` user daemon per user-host and one
`agent-sessions-hub` for the network. The host daemon owns local routing and its
outbound hub connection. There is no separately managed host-agent process.

## Host setup

`make install-all` installs the daemon service and, when absent, the example
configuration at `~/.config/agent-sessions/service.env.example`. Copy it once,
set `AGENT_SESSIONS_HUB`, and restart the Agent Sessions service:

```sh
cp ~/.config/agent-sessions/service.env.example \
  ~/.config/agent-sessions/service.env
${EDITOR:-vi} ~/.config/agent-sessions/service.env
systemctl --user restart agent-sessions.service
```

On macOS:

```sh
launchctl kickstart -k "gui/$(id -u)/net.antst.agent-sessions"
```

The same file is read directly by the daemon on macOS because launchd has no
systemd-style `EnvironmentFile` directive.

Supported settings:

```sh
AGENT_SESSIONS_HUB=10.2.17.1:7419
AGENT_SESSIONS_HOST_NAME=workstation-a
```

The hostname in the daemon catalog is the stable default. Do not change it
casually: it is part of each globally qualified peer address and private group.

## Hub setup

Install the hub-only release on the central host when no host daemon is needed,
or install the full release when that host also runs peers. Install and start
the managed hub user service with:

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

The default listen address is `:7419`. On Linux, an optional
`~/.config/agent-sessions/hub.env` may set
`AGENT_SESSIONS_HUB_LISTEN`. Updating a host daemon does not require updating
the hub when the hub protocol version is unchanged.

## Checks

From a managed peer, `list_peers`, direct send, multicast, broadcast, reply, and
lane calls include remote group-visible peers. A shell can exercise destination
lane readiness directly:

```sh
codex-peer-lane --host workstation-b doctor --json
claude-peer-lane --host workstation-b list --all
qwen-peer-lane --host workstation-b start --name smoke - <<'EOF'
Reply with FEDERATION_OK.
EOF
```

These are visibility-scoped operational surfaces. `list_peers` includes only
peers sharing a group with its managed caller, while each `*-peer-lane list`
command is scoped to one product and its attested parent. The same-user operator
can inspect every current local registration, connected federated host, and live
remote registration regardless of groups:

```sh
agent-sessions roster
agent-sessions roster --json
```

The roster contains operational metadata, not prompts, messages, results,
credentials, capability tokens, or native evidence. `agent-sessions status`
and `doctor` remain the concise count and health surfaces.

Expected failure behavior:

- no hub connection: remote operations fail without local fallback;
- protocol mismatch: the host is refused before registration;
- group mismatch: discovery omits the peer and delivery is rejected;
- hub restart: the host reconnects with the same identity and accepted work is
  not duplicated;
- host daemon restart: vendor processes remain external and the daemon rebuilds
  its projection from durable attachments and lanes.

## Troubleshooting

Check the service first:

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
journalctl --user -u agent-sessions.service -n 100
```

On macOS use `launchctl print gui/$(id -u)/net.antst.agent-sessions` and the
service log paths installed by the launchd definition. Do not start a second
daemon or a legacy host agent to work around a connection problem.
