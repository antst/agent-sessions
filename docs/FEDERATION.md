# Host agent and federation

`peer-federator agent` is the local authority for Agent Sessions discovery,
group membership, routing, and the small durable session catalog. It works
without a hub for local peers. Connecting agents to `peer-federator hub` adds
cross-host discovery, messaging, and lane execution without changing the
local protocol.

The public Claude registry contains exactly one synthetic Agent Sessions
service row per running host agent. Participating `codex-peer`, `claude-peer`,
`grok-peer`, and lane adapters register their real delivery sockets privately
with that agent. Remote peers are never projected as per-peer Claude records or
shadow processes.

Bare native CLIs are untouched and therefore opted out. In particular, bare
`claude` remains an escape hatch that is neither catalogued nor group-routed.

## Groups and messages

Every registered peer belongs to its automatic private group
`session:<host>/<session>` and to zero or more explicit groups. A child lane
always gets its own private group and its parent’s private group. The parent’s
other groups are copied only when the parent chooses `--inherit-groups` for
that launch.

Peers can discover or address only peers sharing at least one group. The first
protocol supports direct sends, explicit-target multicast, and broadcast to
one named group of which the sender is a member. There is no global broadcast
and no implicit compatibility group. See [GROUPS.md](GROUPS.md).

For Claude-native transport the ordinary outer message is addressed to the
single service row. Its body contains the complete Agent Sessions JSON frame.
Within the same-user trust boundary, the service maps the outer message's
claimed `from` address to one live registered native socket, performs group
routing, and sends a new Claude-native outer message to each local destination.
The connection itself does not prove ownership of the claimed socket. The
service does not add attributes to Claude’s strict native envelope grammar.

## Run locally

```sh
peer-federator agent --host workstation-a --name workstation-a

codex-peer --group project-a -n reviewer
claude-peer --group project-a -n implementer
grok-peer --group project-a -n researcher
```

`peer-federator doctor` accepts this local-only topology. `peer-federator
status` reports the registered local peers. The durable catalog remembers each
stable session’s product, groups, parent/inheritance choice, and effective
yolo status. An exact session can later be resumed without knowing its product:

```sh
peer resume 01234567-89ab-cdef-0123-456789abcdef
```

## Add a federation hub

Start one hub on the trusted network:

```sh
peer-federator hub --listen :7419
```

Connect one agent per participating OS user:

```sh
peer-federator agent \
  --hub 10.2.17.1:7419 \
  --host workstation-a \
  --name workstation-a
```

Agents send only explicitly registered live peers and their effective groups.
The hub validates peer identities and private anchors, distributes snapshots,
and forwards only deliveries whose source and destination share a group. A hub
restart does not require peer restart: agents reconnect, republish, and retain
local routing throughout the outage.

The transport is plain newline-delimited JSON over TCP and assumes a trusted,
isolated network. It intentionally has no authentication, encryption, offline
queue, or high-availability protocol yet.

## Remote lanes

Remote execution is disabled by default. Enable it only on destinations where
every connected hub host is trusted to execute the installed lane launchers as
that OS user:

```sh
peer-federator agent ... --enable-remote-lanes
peer-federator hosts
```

The parent product and target product are independent. A Codex, Claude, or Grok
parent may launch a Codex, Claude, or Grok lane, locally or remotely. The target
is selected explicitly:

```sh
printf '%s\n' 'Inspect the repository.' |
  peer-federator lane --host workstation-b --product grok -- \
    start --name remote-review -C /srv/project -
```

The source agent supplies an attested parent context. The destination stores
the source-host private parent anchor, gives the child its destination private
anchor, and copies optional parent groups only when launch requested
`--inherit-groups`. Terminal notices are ordinary grouped Agent Sessions
frames, not shadow-socket callbacks.

The installed `codex-peer-lane`, `claude-peer-lane`, and `grok-peer-lane`
remain the target-specific lifecycle adapters. The shared parent/group layer
selects the parent context; it does not merge their native runtimes.

Remote stdin is capped at 1 MiB, remote auto-archive delays are capped at
86,400 seconds, and each destination accepts at most 32 concurrent remote lane
CLI processes. There is no SSH or direct agent listener fallback. Hub loss
cancels an in-flight remote CLI proxy; an already-started persistent lane keeps
its local target lifecycle but cannot communicate cross-host until federation
returns.

See [federation/OPERATIONS.md](federation/OPERATIONS.md) for service examples
and [federation/PROTOCOL.md](federation/PROTOCOL.md) for the wire contract.
