# Federation

Each user-host runs one service-managed `agent-sessions` daemon. That daemon is
the only local Agent Sessions authority and also owns the host's outbound
federation connection. The central `agent-sessions-hub` is a separate binary
and service; it routes protocol frames but owns no vendor state.

Federation keeps the existing topology: one hub, multiple host daemons, one
global peer space. Groups are the only visibility and routing boundary. The
`--host` suffix disambiguates peers on different machines; it does not create a
namespace or an access policy.

## Configure a host daemon

Install the host normally, then put the hub endpoint in the optional service
environment file:

```sh
mkdir -p ~/.config/agent-sessions
cp ~/.config/agent-sessions/service.env.example \
  ~/.config/agent-sessions/service.env
```

Edit the copied file:

```sh
AGENT_SESSIONS_HUB=10.2.17.1:7419
# Optional display name. The daemon catalog hostname remains the stable ID.
# AGENT_SESSIONS_HOST_NAME=workstation-a
```

Restart only the Agent Sessions user daemon after changing this file:

```sh
systemctl --user restart agent-sessions.service
# macOS:
launchctl kickstart -k "gui/$(id -u)/net.antst.agent-sessions"
```

The daemon reconnects automatically with the same host identity after a daemon
or hub restart. It does not start or manage another host-daemon process.

## Run the hub

Install the independent hub user service on the central machine:

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

It listens on TCP port 7419 by default. On Linux, set
`AGENT_SESSIONS_HUB_LISTEN` in `~/.config/agent-sessions/hub.env` and restart
`agent-sessions-hub.service` to change the listen address. On macOS the installed
launchd label is `net.antst.agent-sessions-hub`.

Host and hub builds need not come from the same commit or release. They must
speak the same hub protocol version; a mismatch is refused before registration
or delivery.

## Use remote peers and lanes

Normal Agent Sessions discovery and messaging include group-visible remote
peers automatically. From a managed product session, use the same MCP tools as
for local peers.

The same-user operator can inspect all connected hosts and current remote
registrations regardless of groups without exposing conversation content:

```sh
agent-sessions roster
agent-sessions roster --json
```

For a shell-owned remote lane, add `--host HOST` to the product lane launcher:

```sh
grok-peer-lane --host workstation-b doctor --json
grok-peer-lane --host workstation-b list --all
grok-peer-lane --host workstation-b start --name remote-review - < brief.md
```

For the managed MCP lane tool, add the `host` field:

```json
{"product":"grok","host":"workstation-b","command":"start","arguments":["--name","remote-review","-"],"input":"Review the change.","session_id":"CURRENT_SESSION_ID"}
```

Remote operations fail closed when the hub or destination is unavailable. They
never fall back to SSH or silently execute on the source host.

## Operational invariants

- Do not run a standalone host federation agent beside `agent-sessions`.
- Restarting or upgrading `agent-sessions` must not restart vendor sessions or
  the central hub.
- The hub stores no vendor credentials, transcripts, profiles, attachments, or
  lane-native state.
- A message is acknowledged only after destination acceptance. Reconnect and
  idempotency prevent duplicate accepted delivery and duplicate lane dispatch.
- Private `host/session` groups remain mandatory; optional groups remain global
  across every connected host.
