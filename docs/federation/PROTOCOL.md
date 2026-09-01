# Federation protocol version 4

Agent Sessions federation runs in a trusted environment. Daemons connect to an
optional hub over bounded newline-delimited JSON; each frame is limited to
2 MiB.

## Connection and roster

The first daemon frame is `hello` with protocol version 4. A mismatched version
is rejected before registration. Build strings are diagnostic only.

A valid hello immediately registers the connection. A new connection using the
same host ID replaces the old connection. There is no pending generation,
snapshot promotion, last-good snapshot, or compatibility mode.

Each daemon sends snapshots of its currently live peers. The hub replaces that
daemon's in-memory peer map and broadcasts one complete roster to every
connected daemon. A daemon restart starts with an empty live view; peers return
by reconnecting and reporting UUID, name, groups, and product.

## Messages

`group_deliver` contains the source ID, target ID, and one `AgentFrame`. The hub
routes only between peers in its current live roster. Delivery succeeds when
the recipient's carrier accepts it and that acknowledgment returns to the
sender.

The daemon keeps unacknowledged outbound frames only in memory. If the hub
connection drops, it reconnects and resends those frames. Nothing is written to
disk. On daemon shutdown it stops accepting new messages, drains accepted work
for at most two seconds, then exits.

## Remote lanes

A remote lane request contains a request ID, destination host, product,
capability, argv, stdin, and parent context. The hub forwards it only to a live
host advertising the exact capability. The destination streams stdout/stderr
and one terminal exit or error. Disconnecting either daemon drops the live
route.

Every request carries exactly one explicit opaque capability. The hub never
infers a capability from an empty value or a product name.

## Trust model

The network and peers are trusted. Deployment boundaries such as separate
VLANs or hubs provide isolation. Authentication, TLS, offline storage, and a
policy language are outside this protocol. A daemon-to-hub app key may be added
in the future; no hooks for it exist today.
