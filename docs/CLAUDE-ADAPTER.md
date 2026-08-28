# Claude daemon adapter

The Claude integration is an in-process adapter of the one per-user-host `agent-sessions` daemon.
Claude's TUI, native session registry and messaging socket, stream-JSON lane worker, transcript,
authentication namespace, permission behavior, and resume selection remain vendor-owned.

## Interactive peers

`claude-peer` is an alias of the canonical host image. It requests one prepared launch from the
already-running daemon and then hands off to the native Claude process. It does not remain as an
Agent Sessions owner and cannot bootstrap daemon lifetime.

Claude may bind the final UUID and name after native selection. The daemon adopts that identity only
when the prepared capability, PID/start ancestry, profile, cwd, selected UUID, native registry row,
socket, and effective permission mode corroborate one another. An installed plugin in a bare `claude`
session is not enough; without a prepared capability its Agent Sessions tools remain inactive.

The daemon maintains the one synthetic Agent Sessions service row required by Claude's native carrier.
It does not project one service per peer and does not turn remote peers into shadow Claude processes.
Messages are durably admitted and group-authorized by the daemon before the adapter writes the
corroborated destination's native socket.

## Permissions

Agent Sessions preserves Claude's native permission semantics. A managed launch supplies only its
explicit launch overlay and records the proven effective mode. It does not rewrite shared profile
defaults or create an Agent Sessions permission namespace. Same-user administrative commands remain
outside model-facing MCP tools.

## Restart

Daemon restart leaves the Claude TUI and its native registry/socket intact. Recovery re-attests the
exact live row, process ancestry, profile, cwd, and socket before republishing the attachment. Missing
or changed evidence fails closed and records debt; no similar process or row is substituted.

## Claude lanes

The daemon owns Claude lane and turn transactions. The adapter starts the native stream-JSON worker,
tracks terminal metadata, interrupts the exact worker, collects once, and archives through the native
contract. If real platform evidence proves inherited streams cannot safely reconnect across daemon
restart, the active turn receives one explicit `interrupted`, collectable, resumable result. A proxy
process is not retained merely to keep the pipe alive.

See [CLAUDE-LANES.md](CLAUDE-LANES.md) for commands and lane behavior.

## Safety boundary

Cleanup revalidates the native UUID, owner process, registry/socket identity, and Agent Sessions
revision immediately before mutation. It may retire exact Agent Sessions metadata and connector
artifacts only. Claude credentials, settings, secure-storage namespace, transcripts, and native session
records remain untouched.

For the shared contract, see [ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md).
