# Live component socket

## Trust and lifetime

Agent Sessions runs in a trusted environment. A component that can connect to
the user-owned Unix socket is accepted. The socket permission is the complete
boundary; there are no capabilities, process attestations, bindings,
generations, or reconnect credentials.

A connection is live state. Disconnect means gone. A reconnect is a fresh
connection, and the component reports its current product sessions again. The
daemon keeps no component session state on disk.

The installed component receives four ordinary environment values:

- `AGENT_SESSIONS_COMPONENT_SOCKET`
- `AGENT_SESSIONS_PRODUCT_ID`
- `AGENT_SESSIONS_ATTACHMENT_ID`
- `AGENT_SESSIONS_COMPONENT_VERSION`

If any value is absent, the integration stays inactive. No secret environment
value exists.

## Messages

The current installed vocabulary uses one length-prefixed JSON object:

```text
{ version: 1, type, id, payload }
```

There is no connection sequence, journal, replay window, ready exchange,
heartbeat, or generation retirement. The live message types are:

- `session.announce`, `session.rebind`, `session.rename`,
  `session.rename.request`, `session.state`, `session.close`, `session.bound`
- `delivery.present`, `delivery.accept`, `delivery.reject`
- `turn.event`
- `tool.call`, `tool.cancel`, `tool.result`
- `reject`

On connect, each integration reports its currently managed session. The live
connection supplies the product and attachment context; the report supplies
the product-native UUID and name. A reconnect repeats the report. Supplying
groups from command-line/session context is central-composition work and is not
credited by this transport-only slice; groups never come from durable peer
state.

Delivery and tool calls are live request/response operations correlated by
`id`. Disconnect rejects outstanding calls; nothing is replayed. If the caller
wants to retry, it starts a new operation.

## Names

The product owns the native title. `session.rename.request` asks the product to
rename; the correlated `session.rename` reports the product-confirmed result.
Unsolicited product title observations also use `session.rename`. The daemon
holds the current title only in memory and never stores a mutable copy.

An observed title may be empty. It must be valid UTF-8, at most 1024 bytes, and
contain no control characters. A daemon rename request and its confirmation
must be non-empty and exact.

## Failure

Malformed or unknown live messages close or reject that operation. A socket
disconnect removes the component's live sessions. There is no crash recovery,
cleanup debt, or durable protocol bookkeeping.
