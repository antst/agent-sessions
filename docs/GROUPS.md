# Agent Sessions groups and host agent

Agent Sessions has no global peer namespace. A participating peer belongs to
one or more groups and can discover or address only peers with which it shares
at least one group.

This document intentionally defines only the operations required now. It does
not introduce a general policy engine or an extension framework.

## Host agent

One Agent Sessions agent runs per host and profile. It is authoritative for:

- local peer registration and liveness;
- explicit and inherited group membership;
- group-filtered discovery;
- direct, multicast, and group-broadcast routing; and
- federation with agents on other hosts.

Federation is an optional transport of the same host agent. Local routing must
work without a federation hub.

Product-specific managers retain sole ownership of Codex threads, Claude
processes, Grok ACP sessions, and future product sessions. Restarting the host
agent must not terminate those sessions; live participants reconnect and
register again.

The agent keeps one small durable session catalog keyed by stable Agent
Sessions ID. Its initial schema stores the product, explicit groups, parent and
inherited groups, and last explicit always-approve (`--yolo`) choice. A resume
with no group or permission override restores those values; supplied values
replace them. Additional durable fields are added only for a demonstrated
need.

## Membership

`codex-peer`, `claude-peer`, and `grok-peer` accept repeatable
`--group NAME` arguments. Group names are opaque, case-sensitive strings after
basic length and character validation.

Every participating session also belongs to an automatic private group:

```text
session:<host-id>/<stable-session-id>
```

A child lane always inherits the parent's private session group and receives
its own private session group. That is the fixed parent/child communication
path. The parent's other effective groups are copied only when the parent
explicitly requests group inheritance for that launch. Omission on resume
restores the child's last choice; it does not silently widen membership. A
child may request more explicit groups but cannot remove its parent anchor.

The host agent computes effective membership from the corroborated parent
registration. A child process cannot grant itself membership by copying a
group list into its own registry row.

A bare product CLI that does not register with the host agent is not an Agent
Sessions peer. This is the Agent Sessions communication opt-out.

The parent layer and target layer are independent. Any registered Codex,
Claude, or Grok parent can launch any supported Codex, Claude, or Grok lane.
Parent resolution supplies lifecycle and group context; the selected target
adapter continues to own its native thread/process/ACP semantics.

## Claude-native carrier

The host agent publishes exactly one additional Claude-compatible service
session in Claude's native session registry. Consequently, `claude agents
--json` shows ordinary local Claude sessions plus one Agent Sessions service
session.

Participating Claude sessions send messages to that service through Claude's
native communication protocol. The native Claude envelope is only the outer
carrier. Its message body is one complete Agent Sessions protocol frame,
including routing fields, correlation identifiers, service metadata, and the
actual content.

Within the same-user trust boundary, the host agent maps the top-level native
`from` address to one live registered Claude socket and replaces the inner
source fields before routing. The native stream does not independently prove
that the connecting process owns the claimed reply socket. Delivery to Claude
wraps the same Agent Sessions frame in a new Claude-native envelope. Codex,
Grok, federation, and future product adapters carry the same inner frame.

Claude's outer envelope remains unchanged and uses its exact native attribute
grammar. Agent Sessions fields exist only in the inner frame. On a relayed
message the outer sender and permission-mode attestation describe the host
agent, not the original peer; original provenance remains in the inner frame
and never upgrades the outer permission class.

The service session is not a group member and is never a broadcast recipient.
There is one service session per host/profile, not one service session per
group or remote peer.

## Minimal protocol operations

The initial grouped protocol supports:

- `register`: register the corroborated session, requested groups, and parent;
- `unregister`: retire the caller's live registration;
- `discover`: list only peers sharing a group with the caller;
- `send`: address one or more named/session-addressed peers; and
- `broadcast`: address every current member of one named group.

Direct send uses a one-element recipient list. Multicast uses the same
operation with multiple recipients. Every direct or multicast recipient must
share at least one group with the sender. A broadcast sender must itself belong
to the target group. Global broadcast is not supported.

Each request has a protocol version and stable message ID. The agent rejects
malformed frames, duplicate recipients, unknown peers, incompatible versions,
and group violations explicitly. The protocol gains new fields or operations
only when an implemented feature requires them.

For broadcasts the agent snapshots the live members of the requested group at
admission time and attempts one ordinary delivery to each snapshot member.
Results report the accepted and failed recipients; there is no implicit global
fan-out.

Group filtering is authoritative for Agent Sessions discovery and routing.
Claude's independent native direct-session command remains an out-of-band
transport and can address native sessions without consulting the Agent
Sessions agent. Hard policy enforcement over that separate native feature is
not part of this first grouped protocol.

## Federation

Each host agent is authoritative only for sessions registered on that host.
Federated agents exchange peer identities and effective groups. Remote direct,
multicast, and group-broadcast deliveries pass through the destination host
agent, which applies the same membership checks before local delivery.

Legacy flat peers are not silently placed into a global compatibility group.
Grouped and flat protocol versions fail closed rather than bypass group
isolation during a rolling upgrade.
