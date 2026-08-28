# Global groups and peer addressing

Agent Sessions has one uniform multi-host collaboration space behind one central hub. Existing global
groups are the sole visibility and routing boundary in that space. Process unification does not add a
namespace, realm, profile partition, host policy layer, or product-specific access model.

Each host runs one `agent-sessions` daemon with an embedded host agent. The daemon owns local
registration, liveness, group-filtered discovery, direct/multicast/broadcast admission, durable
delivery, and the outbound hub connection. The central `agent-sessions-hub` routes the same group
space between hosts.

## Membership

Managed peers and lanes may request repeatable `-g NAME` or `--group NAME` values. Group names are
opaque, case-sensitive strings after the existing bounded validation.

Every managed session also belongs to its automatic private group:

```text
session:<host-id>/<stable-session-id>
```

A child lane always receives its own private group and its parent's private group. Other effective
parent groups are copied only when the launch explicitly requests `--inherit-groups`. Omitting an
override on resume restores the durable choice; it does not silently widen membership. A child may add
explicit groups but cannot remove its parent anchor.

The daemon derives parent context from the attested parent attachment. A child process or model cannot
grant itself membership by copying group fields into a request.

Product, profile, instance, native session, host, daemon generation, and test-owned resource identity
are used for exact attribution, addressing, routing, lifecycle, and cleanup. They do not grant access
and never create another collaboration namespace.

## Names and addresses

Peer display names are convenient mutable labels. Federation appends the existing host suffix so a
name is unambiguous across hosts. If more than one visible peer still matches a name, resolution fails
and requires an exact address; it does not silently choose one.

The authoritative network identity is the existing host/session pair. The automatic private group
uses that identity and is global through the hub.

## Discovery and delivery

A managed sender can discover or directly address only peers sharing at least one effective group.
The current operations are:

- direct send to one exact or unambiguous visible recipient;
- explicit-target multicast, where every recipient must be authorized;
- broadcast to the admission-time snapshot of one named group to which the sender belongs.

There is no global broadcast and no implicit compatibility group. A request has a stable message ID;
acceptance occurs only after durable ownership commits. Retries preserve at-most-once destination
outcomes.

The same rules apply locally and remotely. A destination host daemon rechecks group authorization
before native delivery; the hub cannot widen membership. Hub loss preserves local routing but does not
create a fallback carrier.

## Bare native sessions

A bare `codex`, `claude`, `grok`, or `qwen` session has no daemon-prepared launch capability and is not
an Agent Sessions peer. Installing a connector does not opt every native process into the catalog.
This remains the communication opt-out.

Native product features outside Agent Sessions may have their own messaging behavior. Agent Sessions
does not claim that groups constrain an independent vendor transport.

## Delegation and permissions

Groups answer who can discover and route to whom. They do not assert that one peer may issue
instructions with the user's authority. The user establishes delegation separately, and receiving
native instructions, sandbox, approval, and permission modes remain authoritative.

The one daemon is a user service: same-user administrative code is inside its administrative trust
boundary. Model-facing MCP tools nevertheless expose no daemon administration and retain their
attested attachment, group, product, and parent restrictions.

## Restart

Group preferences, names, parent anchors, and delivery cursors are daemon-owned durable metadata.
Daemon restart reconstructs them under the next generation and republishes only corroborated live
attachments. It neither terminates native sessions nor creates a second host agent. Remote hosts see
the same host identity and host-suffixed names when federation reconnects.
