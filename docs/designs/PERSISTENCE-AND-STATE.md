# IT IS A TRUSTED ENVIRONMENT. DOT.

Agent Sessions connects the user's own agents on infrastructure they control.
Isolation belongs in deployment: separate VLANs or separate hubs. The host,
local socket, peers, and product adapters have no in-code trust boundary.

The only security item planned is a future daemon-to-hub app key so a hub can
admit known daemons. It is not designed today, has no preparatory hooks, and is
the entire security roadmap.

## Durable state

Peer sessions, messages, turns, results, titles, process identity, presence,
and product session metadata are never persisted by Agent Sessions. Products
and the operating system already own those facts.

The sole durable record is an archived-lane discovery candidate:

- product;
- native session UUID;
- parent;
- parent's primary group;
- secondary groups;
- optional parent host, stored only on the parent's host.

The row remembers only which UUID Agent Sessions may ask a product about. It is
never rendered as an answer. Listing or unarchiving loads eligible rows, asks
the product which UUIDs still exist, and returns only product-confirmed data.
A stale row yields nothing.

## Live state

Connections report UUID, name, groups, and product. A daemon restart begins
with an empty roster and rebuilds it as connections return. Names and lane
lookups may be cached in memory for active sessions and disappear with the
process.

Messages and turns are synchronous pass-through calls. Success is the product
or recipient carrier's acknowledgment. Pending acknowledgments and federation
resends are memory-only. Graceful shutdown stops new messages, drains accepted
work for at most two seconds, then exits.
