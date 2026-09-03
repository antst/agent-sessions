# Federation

Federation connects per-user host daemons through one optional central hub. Local presence and
routing work without it. The hub relays live rosters, messages, and lane calls; it owns no product
process, credential, transcript, history, or lane row.

## Configure a host

Install the normal host first, then set the hub in `~/.config/agent-sessions/service.env`:

```sh
AGENT_SESSIONS_HUB=hub.example:7419
AGENT_SESSIONS_HOST_NAME=workstation-a
```

Restart the Agent Sessions user service and inspect:

```sh
agent-sessions daemon restart
agent-sessions roster --json
agent-sessions doctor
```

`AGENT_SESSIONS_HOST_NAME` is a display label. The daemon keeps a stable catalog host identity.

## Operate the hub

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

The default listener is `:7419`. A hub can run on a machine that also hosts peers, but the host
daemon and hub remain separate service-owned processes.

## Discovery and addressing

The hub distributes complete rosters to connected daemons. A local caller sees remote peers only
when their effective groups intersect, exactly as it sees local peers. Operator output qualifies
remote identities with the host where needed. Native session IDs remain product-owned; federation
does not mint a second peer or lane identity.

Direct and group messages remain synchronous live deliveries. A remote acknowledgement means the
destination product accepted the delivery. Disconnect makes the remote destination unavailable;
no host or hub provides an offline mailbox.

All nine lane capabilities may be advertised by a host from the product descriptor catalog. MCP
lane methods take an optional `host`; CLI lane commands accept `--host`. The source daemon sends
the current live parent context and the destination daemon performs the normal lane admission and
product operation. Results stream back over the same bounded request.

## Reconnect and shutdown

Federation state is in memory. A host reconnects after a transport loss, re-sends unacknowledged
accepted frames held in memory, receives a new complete roster, and resumes live routing. The hub
does not persist those frames.

On shutdown a host stops accepting new work, drains accepted deliveries for at most two seconds,
then exits. A process replacement starts with no remote roster until the next compatible hub
handshake.

See [Federation protocol version 4](federation/PROTOCOL.md) for the exact host/hub wire.
