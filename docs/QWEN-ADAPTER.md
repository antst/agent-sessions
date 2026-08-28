# Qwen daemon adapter

The Qwen integration is an in-process adapter of the one per-user-host `agent-sessions` daemon.
Qwen's TUI, native daemon/ACP worker, event and input artifacts, archive store, authentication, and
transcript remain vendor-owned. Agent Sessions no longer runs `qwen-host`, a Qwen delivery listener,
or a detached Qwen lane manager.

## Interactive peers

`qwen-peer` is an alias of the canonical host image. It obtains a prepared launch from the running
daemon and hands off to Qwen. Publication waits for the exact extension/readiness state, launch
capability, selected UUID/name, profile, cwd, ancestry, and dual native output artifacts.

The adapter observes native events and writes the corroborated native input path from daemon
goroutines. The daemon owns durable delivery admission, existing global-group authorization, retries,
and at-most-once outcome. A bare `qwen` invocation has no launch capability and its installed MCP relay
returns inactive.

`QWEN_HOME` and `QWEN_RUNTIME_DIR` are presence-sensitive native selectors. They select exact Qwen
resources; they do not create another Agent Sessions daemon, state authority, collaboration namespace,
or group boundary.

## Restart

Daemon restart leaves Qwen's native TUI and worker intact. Recovery reconstructs an attachment only
from the same admitted event/input artifacts and live ancestry. An absent, ambiguous, or changed
artifact produces unavailability or debt rather than inferred ownership.

## Qwen lanes

The daemon owns Qwen lane and turn state. The adapter starts or loads the native ACP session, observes
events, writes follow-up input, interrupts the exact turn, collects a stable result, and archives via
Qwen's native store. An accepted turn is never redispatched. Unsupported active-turn reconnection
produces one explicit interrupted, collectable, resumable outcome with the native reference.

See [QWEN-LANES.md](QWEN-LANES.md) for commands and lane behavior.

## Installation and safety

Connector installation uses Qwen's native Agent Plugins v1 extension manager and verifies the exact
manifest, enabled state, `agent_sessions` MCP server, and shipped skills. Removal uses the same native
manager and may leave Qwen's own bookkeeping; Agent Sessions does not edit the profile to hide it.

Before delivery, interruption, archive, or cleanup, the adapter revalidates profile identity,
ancestry, native artifacts, session/turn identity, and Agent Sessions revision. It never reads or
copies authentication material and never deletes Qwen transcripts, settings, archive data, or unrelated
native files.

For the shared contract, see [ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md).
