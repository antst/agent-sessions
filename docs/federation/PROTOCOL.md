# Federation protocol version 2

The hub and agents exchange newline-delimited JSON over plain TCP. The first agent frame is
`hello`; a diagnostic client can instead send `probe`. Protocol version mismatches are rejected.

Agents publish complete snapshots of their live local peers. The hub broadcasts the combined
roster plus one host record per connected agent. Host records advertise `codex-lane` and/or
`claude-lane` capabilities only when the destination operator explicitly enables remote lane
execution. This opt-in authorizes all hosts on the trusted hub to invoke the native lane CLI as
that agent's OS user. Remote peers are represented locally by supervised shadow processes
with real PIDs, numeric Claude registry records, and Unix sockets. Peer records carry a stable
process-instance ID and the originating permission class; shadow rows preserve both so session-ID
rotation does not move their socket and infrastructure notices do not trip Claude's approval gate.

A delivery contains a source peer ID, target peer ID, and one unchanged native JSON frame. The
destination agent rewrites only addressing metadata needed to make replies route through the
source peer's local shadow. Payload text is not interpreted. Frames are limited to 2 MiB.

A remote lane request contains a request ID, an advertised source peer, destination host, product,
native argv, and at most 1 MiB of stdin. The hub creates an in-memory route only when the source is
currently advertised and the connected destination advertises the requested capability. The
destination streams `lane_stdout`, `lane_stderr`, and one terminal `lane_exit` or `lane_error` back
over that route. Remote lifecycle requests cannot disable the native auto-archive cleanup fuse.
Destination agents cap remote CLI concurrency, argv size, and auto-archive delay. A private
liveness descriptor binds every native CLI to its destination agent even across SIGKILL.
`lane_cancel` travels in the opposite direction. Disconnecting either agent drops
the route, cancels destination work when the source disappears, and fails the source proxy when the
destination disappears.

The protocol intentionally has no authentication, encryption, offline storage, or delivery retry.
It is designed for a trusted isolated VLAN. There is deliberately no agent-to-agent lane or message
path: hub connectivity is mandatory for every cross-host operation.
