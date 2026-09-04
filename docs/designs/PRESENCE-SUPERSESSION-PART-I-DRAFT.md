# Presence supersession: Part I protocol insertion draft

Status: review-only sectioning input. This page is paste-ready normative text for Part I of the
sectioned protocol. It does not change protocol authority or authorize implementation. It is based
on `c5b280d8db4fc0069dae50365f3515c6de6ab57e` and the stamped PRESENCE SUPERSESSION ruling.

## Authority-list insertion

Add this row to the complete closed method authority after A's Go-only method 20,
`lane.worker.hello`:

| Ordinal | Method | Direction | Phase | Params | Result | Tool | Lane |
| ---: | --- | --- | --- | --- | --- | --- | --- |
| 21 | `session.superseded` | daemon to client | after `session.hello` | exact `{}` | exact `{}` | no | no |

The shared JavaScript authority includes `session.superseded` but not the Go-only
`lane.worker.hello`; its local entry count therefore increases from 19 to 20. The complete protocol
authority calls this method 21. It is an ID-bearing JSON-RPC request, not an idless JSON-RPC
notification. "Supersession notification" is lifecycle terminology only.

## Paste-ready Part I method text

### Daemon to client: `session.superseded`

A newer `session.hello` for a UUID is an intentional takeover of that identity. After validating
the hello, the daemon atomically installs and publishes the newer connection as current. If an
older connection was current for the same UUID, the daemon sends this request to that exact prior
connection immediately before closing it:

```json
{"jsonrpc":"2.0","id":"daemon.2","method":"session.superseded","params":{}}
```

The request ID is drawn from the same monotonic per-connection sequence as ordinary daemon-to-client
calls, so it cannot collide with an outstanding call on the displaced connection. Parameters and
result are closed and exactly `{}`:

```json
{"jsonrpc":"2.0","id":"daemon.2","result":{}}
```

The daemon performs one ordered write and then closes the displaced connection. It does not wait
for the response and adds no acknowledgement timer, lease, grace period, retry-suppression window,
or second socket. Ordered stream delivery places the request before EOF when the write succeeds.

On receiving a valid `session.superseded` request, a client MUST mark that identity terminal before
attempting the best-effort `{}` response. It MUST NOT reconnect or send another hello for the
superseded identity; any later local report, update, or call attempt MUST NOT recreate its
connection. This is never a signal to try again later. The common client handles the method
internally and does not delegate it to a product or lane callback.

Terminality is scoped to the displaced identity. A multi-session client keeps its other identities
live. A single-identity client exits its reconnect loop, fails pending calls exactly once, and
closes its completion signal. A tools connector ties its relay lifetime to that completion signal,
so it exits even if the product continues to hold the connector's standard input open.

Ordinary EOF or daemon unavailability without `session.superseded` keeps the existing reconnect
behavior. No inference from EOF, timing, process identity, or a replacement socket may synthesize
terminal supersession.

## Paste-ready Part I handoff text

The current-connection pointer is the admission authority and handoff linearization point. Once a
new same-UUID connection replaces that pointer, every request from the displaced pointer receives
the existing `-32003` Operation not permitted response before parameter validation, state mutation,
or tool dispatch. Work admitted before the swap may finish only under the displaced connection's
request context; closing that connection cancels the context. Cleanup of the older connection MUST
NOT remove, retire, or publish a leave for the replacement.

The takeover sequence is therefore exactly:

1. validate the newer `session.hello`;
2. atomically make its connection current and publish its report;
3. write one sequenced `session.superseded {}` request to the exact prior pointer, without waiting;
4. close the prior pointer and cancel its request context.

## Part I cross-reference for lane children

A's attach rule uses the same `session.superseded` operation when a newer child replaces the current
child for a lane binding. Only the displaced child identity becomes terminal. The lane parent,
worker binding, generation, and running turn remain unchanged; `tools_attached` continues to report
the current child truthfully. A defines the attach and binding rules but does not define or
reimplement this method.

## Required conformance statements

- A same-UUID replacement remains current and usable after the prior connection is closed.
- The displaced client never sends another hello after more than the normal reconnect interval.
- A pending daemon call and the supersession request use distinct sequence IDs; closing the old
  connection fails the pending call exactly once and never resolves it with the supersession ACK.
- Post-swap traffic from the displaced pointer returns `NotPermitted` and cannot mutate or dispatch.
- Go clients terminate before ACK/EOF handling can reconnect; JavaScript tombstones only the target
  identity; a held connector stdin cannot keep the superseded connector alive.
- Malformed or wrong-phase `session.superseded` frames do not create terminal state.
- Ordinary disconnect without `session.superseded` still reconnects from a fresh hello.
