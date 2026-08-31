# Agent Sessions groups and user-host daemon

Agent Sessions has no global peer namespace. A participating peer belongs to
one or more groups and can discover or address only peers with which it shares
at least one group.

This document intentionally defines only the operations required now. It does
not introduce a general policy engine or an extension framework.

## User-host daemon

Exactly one service-managed Agent Sessions daemon runs per operating-system
user and host. It is authoritative for:

- local peer registration and liveness;
- explicit and inherited group membership;
- group-filtered discovery;
- direct, multicast, and group-broadcast routing; and
- federation with daemons on other hosts.

Federation is an optional transport of the same daemon. Local routing must
work without a federation hub.

The daemon's in-process product adapters retain Agent Sessions lifecycle
ownership while native Codex, Claude, Grok, and Qwen processes retain their
vendor state and transcripts. Restarting the daemon must not terminate those
native sessions; live participants reconnect and register again. Short-lived
`agent-sessions connector ...` processes are stdio MCP relays owned by vendor
clients, not additional daemons or authorities.

The agent keeps one small durable session catalog keyed by stable Agent
Sessions ID. Its initial schema stores the product, explicit groups, parent and
inherited groups, and last explicit always-approve (`--yolo`) choice. A resume
with no group or permission override restores those values; supplied values
replace them. Additional durable fields are added only for a demonstrated
need.

## Membership

`codex-peer`, `claude-peer`, `grok-peer`, and `qwen-peer` accept repeatable
`-g NAME` or `--group NAME` arguments. Group names are opaque, case-sensitive strings after
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

The daemon computes effective membership from the corroborated parent
registration. A child process cannot grant itself membership by copying a
group list into its own registry row.

A bare product CLI that does not register with the daemon is not an Agent
Sessions peer. This is the Agent Sessions communication opt-out.

## Delegation is separate from membership

Groups answer who can discover and route messages to whom. They do not establish that one peer may
issue instructions with the user's authority. A user who wants one interactive session to take
orders from another must explicitly establish that relationship and decide its scope. Agent
Sessions deliberately does not prescribe a delegation format or hierarchy.

Group membership, a mutable peer name, transport-authenticated source metadata, or a peer's own
claim of authority is not a substitute for that user decision. Operators may identify delegates by
whatever convention fits their workflow; an exact Agent Sessions source ID avoids ambiguity when
names can be reused or changed. Delegation does not override the receiving session's higher-priority
instructions, permission mode, sandbox, or approval requirements.

The parent layer and target layer are independent. Any registered Codex,
Claude, Grok, or Qwen parent can launch any supported Codex, Claude, Grok, or Qwen lane.
Parent resolution supplies lifecycle and group context; the selected target
adapter continues to own its native thread/process/ACP semantics.

## Claude-native carrier

The daemon publishes exactly one additional Claude-compatible service
session in Claude's native session registry. That row is internal transport
plumbing, not the Agent Sessions discovery surface. Use the structured
`agent_sessions.list_peers` operation from a managed peer to discover live
local or federated peers visible through shared groups.

Participating Claude sessions send messages to that service through Claude's
native communication protocol. The native Claude envelope is only the outer
carrier. Its message body is one complete Agent Sessions protocol frame,
including routing fields, correlation identifiers, service metadata, and the
actual content.

Claude-originated bodies begin with the literal text `AGENT_SESSIONS_FRAME `
followed immediately by the compact JSON frame. This fixed carrier marker keeps
Claude's `SendMessage` tool from coercing the JSON into one of Claude's own
typed control messages; it is not a second routing protocol.

Within the same-user trust boundary, the daemon maps the top-level native
`from` address to one live registered Claude socket and replaces the inner
source fields before routing. The native stream does not independently prove
that the connecting process owns the claimed reply socket. Delivery to Claude
wraps the same Agent Sessions frame in a new Claude-native envelope. Codex,
Grok, Qwen, and federation adapters carry the same inner frame.

Claude's outer envelope remains unchanged and uses its exact native attribute
grammar. Agent Sessions fields exist only in the inner frame. On a relayed
message the outer sender and permission-mode attestation describe the host
daemon, not the original peer; original provenance remains in the inner frame
and never upgrades the outer permission class.

The service session is not a group member and is never a broadcast recipient.
There is one service session per host/profile, not one service session per
group or remote peer.

This carrier remains a protocol compatibility surface, not an automatic retry
policy. Current Claude-facing skills use the structured `agent_sessions` MCP
tools exclusively for Agent Sessions operations. If those tools are inactive or
fail, the model must report that failure and stop rather than retrying with
native `ListAgents` or `SendMessage`.

Installation does not change the profile's default `crossSessionInbound`
policy. Each managed Claude peer or lane supplies `accept` only as a launch
override; ordinary Claude keeps the operator's `reject`, `prompt`, or `accept`
choice unchanged. A managed interactive Claude peer also has one durable
permission class for its lifetime. Constrained peers disable Claude's
unpublished in-session bypass toggle in that same launch-only settings overlay;
an explicitly bypassed peer is conservatively advertised as bypass until it is
restarted. This never changes the shared profile's permission defaults.

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

Each daemon is authoritative only for sessions registered on that user-host.
Federated daemons exchange peer identities and effective groups. Remote direct,
multicast, and group-broadcast deliveries pass through the destination host
daemon, which applies the same membership checks before local delivery.

Legacy flat peers are not silently placed into a global compatibility group.
Grouped and flat protocol versions fail closed rather than bypass group
isolation during a rolling upgrade.
