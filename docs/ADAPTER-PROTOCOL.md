# Product adapter protocol

## IT IS A TRUSTED ENVIRONMENT. DOT.

Agent Sessions runs the user's own agents on infrastructure the user controls.
Deployment boundaries such as separate hosts, VLANs, or hubs provide isolation.
The local protocol has no capability, token, process-attestation, ancestry, or
generation-fencing layer. A process that can connect to the user-owned socket
may report and operate its session.

The only security item contemplated for the future is an app key for a daemon
joining a hub. It is not designed here, has no preparatory hooks, and does not
change the trusted local environment.

## Product inventory

`internal/productcatalog` contains the authored product descriptors.
`internal/productruntime` contains the optional lane and doctor interfaces used
by product-local packages. There is no product registration from `init` and no
fallback from an unknown product to another adapter.

Product sessions remain product-owned. Titles, state, history, results,
resumability, and native session identity are read from the product. Agent
Sessions attaches, routes, and presents those facts; it does not shadow a
product session store.

## Live session connection

Every live session or lane holds one connection to `presence.sock`. This is the
only adapter socket. Its first newline-delimited JSON object is exactly:

```json
{"uuid":"native-id","name":"native title","groups":["group"],"product":"pi"}
```

The connection itself proves liveness. EOF removes the session immediately and
fails outstanding calls. Reconnection starts from a fresh report. A newer
connection reporting the same UUID replaces the older connection.

After the report, the same connection carries small JSON-RPC-shaped objects:

```json
{"id":"session.1","method":"tool.call","params":{}}
{"id":"daemon.2","method":"message.deliver","params":{}}
{"id":"session.1","result":{}}
{"id":"daemon.2","error":"native delivery failed"}
```

Name or group changes use `session.update` on that connection. A report or
update is generation-local memory only. There is no component socket, broker,
binding, heartbeat, journal, replay window, or handshake capability.

## Messages and parent calls

Messaging is a synchronous live relay. The destination must be present when
delivery occurs. The sender sees the destination adapter's success or failure;
the daemon does not spool messages, synthesize receipts, or promise later
delivery. A caller that loses its call owns any retry.

Parent tool calls use the same live connection and the session UUID already
reported by the product adapter. There is no separate parent-attestation path.

## Lanes

Lane operations call the selected product directly. Start, resume, prompt,
wait, steer, interrupt, archive, and result semantics remain native-product
semantics. A daemon acknowledgement is returned only after the product accepts
the operation. Busy or unsupported operations return truthful errors instead
of entering a daemon queue.

The only durable daemon data is the lane discovery candidate row:

```text
{uuid, product, parent, primary_group, secondary_groups, optional_host}
```

It is used only to decide which UUIDs may be asked of a product when discovering
offline lanes. The product must confirm a candidate and supplies every returned
field. Live routing never reads this table. Stale rows are harmless and never
served as answers.

For an active parent, UUID-to-name results may be held in a disposable in-memory
map. The map is populated from product-confirmed candidates, updated when a lane
opens, and discarded when the parent disconnects.

## Product mechanics

- Pi and OMP use their native JSONL RPC modes for lanes.
- OpenCode and Kilo use their supported HTTP/event surfaces for lanes.
- CodeBuddy uses its native HTTP job surface.
- DSH uses its pinned ACP/Cordis surface.
- Claude, Codex, Grok, and Qwen use their native start and exact resume forms.

Shared helpers may implement process spawning, bounded JSONL, or plain HTTP/SSE,
but they do not add lifecycle authority above the product.

## Permissions and doctor

Permission mapping is product-owned and explicit. Unsupported mappings fail
before native work; adapters do not silently widen them. Doctor probes read the
live installation and product surfaces on each request. Readiness is not stored.

## Federation

Federation protocol 4 uses one explicit version. A mismatched participant is
rejected at hello. Accepted daemons exchange one complete live roster and relay
live calls. Disconnect triggers reconnect and in-memory resend of unacknowledged
frames; nothing federation-related is durable.

On shutdown the daemon stops accepting work, gives already-accepted live calls
up to two seconds to finish and return acknowledgements, then exits.
