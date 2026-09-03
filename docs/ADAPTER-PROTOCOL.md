# Shipped adapter architecture

The public native wire is [Native Agent Sessions Presence Protocol v1](specs/NATIVE-PEER-PROTOCOL.md).
This document describes how the shipped product adapters reach that one wire.

## One live plane

Every live peer or lane is represented by one acknowledged `session.hello` on `presence.sock`.
After the hello, the newline-delimited JSON-RPC 2.0 connection carries first-class peer, message,
session-update, and lane methods. EOF is the sole live-presence boundary; a newer accepted
connection for the same native UUID replaces the older one.

The state-root search order is identical in the Go and JavaScript clients:

1. `AGENT_SESSIONS_PRESENCE_SOCKET`;
2. `AGENT_SESSIONS_STATE_ROOT/run/presence.sock`;
3. `$XDG_STATE_HOME/agent-sessions/run/presence.sock`;
4. `$HOME/.local/state/agent-sessions/run/presence.sock`.

OpenCode, Kilo, Pi, and OMP share the JavaScript native client. DSH implements the same protocol in
its native profile. Codex, Grok, and Qwen use the shared launcher-held Go client where the product
topology requires the parent process to hold presence. Claude uses its product plugin/connector
path. All of them use the same first-class methods and the same presence socket.

## Identity and product authority

`session.hello.uuid` is the product's native session ID and is the only identity exposed after lane
open. Product names and info come from product events or live product reads:

- Codex and Pi publish native title events.
- OpenCode and Kilo publish session updates.
- Claude resolves the custom transcript title at query time.
- OMP resolves the exact UUID through one batched product listing per query.
- Grok resolves the exact UUID through its global native session listing.
- Qwen and DSH report their product-owned title through the native client.

Live title reads update one in-memory projection only. They are never durable. `session.update` may
replace name and info but cannot change UUID, product, or connection groups.

Fresh product-generated sessions receive no provisional session ID in their launch environment.
The daemon re-keys its in-memory lane actor to the returned native ID in one locked operation. A
daemon-owned lane keeps its already-computed effective groups when its integration reports; the
report identifies the lane and does not become a second group authority.

## Messages and tools

Delivery is structured on the wire:

```json
{"message_id":"m1","from":{"uuid":"source","name":"reviewer","product":"claude","groups":["team"]},"body":"Please inspect this."}
```

The shared JavaScript client and the shared Go adapter helper each render that structure once for
their native product input. Per-product code chooses only the native input call. Acceptance or the
product's rejection returns synchronously and verbatim; nothing is spooled.

The promptless tool vocabulary covers identity, peer listing, direct/multicast send, group
broadcast, supported rename, and lane lifecycle. Host-supplied invocation metadata identifies a
tools-only caller where the product supplies it. Models do not restate their session identity.

## Lane drivers

`productruntime.LaneDriver` is the single engine boundary. The coordinator performs generic
admission, groups, result delivery, candidate persistence, and routing; product packages implement
native Open, StartTurn, WaitTurn, Interrupt, Archive, and genuine optional Steer/message methods.
The registry is constructed explicitly from the product catalog. Generic lane code contains no
product dispatch literals.

Lane-capable native protocol clients, currently DSH, advertise `capabilities:{"lane":true}` and
accept the protocol's daemon-to-session lane requests over their held presence connection. Other
drivers use their product's App Server, JSONL, stream-JSON, ACP, HTTP/event, or leader interface.

## Trust and errors

Agent Sessions runs in a trusted same-user environment. The local socket has no authentication or
security-redaction layer. Native product errors reach the caller unchanged. Protocol
errors use the closed v1 code table; unclassified product failures use `-32006` with the product
text preserved.

See [Persistence and state](designs/PERSISTENCE-AND-STATE.md) for the governing doctrine.
