# Grok daemon adapter

The Grok integration is an in-process adapter of the one per-user-host `agent-sessions` daemon.
Grok's TUI, private leader/observer, ACP sessions, roster, interjection, authentication, and transcript
remain vendor-owned. Agent Sessions no longer runs `grok-host`, a Grok delivery listener, or a detached
Grok lane manager.

## Interactive peers

`grok-peer` is an alias of the canonical host image. It obtains a daemon-issued launch capability and
hands off to the native Grok client. The daemon adopts the selected session only after exact owner,
leader, ACP session, roster, profile, cwd, and capability evidence agree.

The daemon runs the Grok ACP client, roster observation, wake coordination, and delivery callbacks as
goroutines. Delivery uses supported native interjection or MCP calls after durable AgentFrame admission
and existing global-group authorization. There is no loopback host protocol between these components.

A bare Grok client remains unmanaged. Plugin availability, a process name, or same-user access cannot
mint a managed identity.

## Restart

Daemon restart does not terminate the Grok TUI or leader. Recovery reconnects only after exact native
leader and roster evidence identifies the same session. Unknown or changed evidence keeps the
attachment unavailable and records bounded debt.

## Grok lanes

The daemon owns the lane transaction and invokes the Grok adapter's start/load, interject, interrupt,
collect, archive, and cleanup operations in process. An accepted turn is never started twice. Active
ACP work reconnects only through Grok's supported session identity. If that proof is unavailable, one
explicit interrupted result retains the native session reference for resume.

See [GROK-LANES.md](GROK-LANES.md) for commands and lane behavior.

## Safety boundary

Before wake, delivery, interruption, archive, or cleanup, the adapter revalidates the exact owner,
leader, process session, ACP session, and Agent Sessions revision. Cleanup cannot target a process
group, socket, or record whose identity changed. Grok credentials, plugin data, settings, transcripts,
and native session storage remain vendor-owned.

For the shared contract, see [ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md).
