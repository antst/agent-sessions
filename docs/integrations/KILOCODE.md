# KiloCode integration

The catalog product ID is `kilo`; the supported executable is `kilo` from
`@kilocode/cli` **7.5.6**. Kilo is an OpenCode fork, but Agent Sessions shares
only verified HTTP/SSE mechanics. Its peer topology and routes remain explicit.

## Exact managed-peer topology

Every managed peer owns exactly:

1. one authenticated, literal-loopback `kilo serve` process;
2. one native `ses_*` session on that server; and
3. one full `kilo attach <server> --dir <cwd> --session <id>` TUI.

Two peers never share a daemon/server or authentication realm. The component
plugin runs in the isolated server and `/tui/clear-prompt`,
`/tui/append-prompt`, then `/tui/submit-prompt` targets that server's one full
attached TUI. It snapshots native messages before submission and reports
success only after a new exact user `msg_*` appears for the bound session.

`attach --mini` is unsupported. The managed launcher rejects `--mini`, session,
directory, endpoint, server, and daemon overrides before native startup. At
7.5.6 a mini client can read/resume a session but `/tui/*` does not submit or
render through it.

Fresh launch and exact resume are separate fail-closed paths. During attachment
prepare, the daemon supplies either an empty native ID for a fresh session or
the durable exact prior `ses_*`, together with its canonical cwd. The product
holds that launch intent only ephemerally and consumes it only for the matching
attachment/cwd. Resume starts the same isolated topology and profile, requires
an exact `GET /session/{id}`, requires `POST /tui/select-session` to return
HTTP 200 `true` as a supported-surface check, and renders the full attach with
the exact `--session <id>`. The attach flag is the selection authority; the TUI
route alone merely publishes navigation to a live listener. A 404, mismatched
ID/cwd, false selection, or cleanup uncertainty fails closed. There is no
fallback to `--continue`, a latest session, or a fresh session. Fresh creation
is offered only as a separate prepare intent.

Rollback waits for an in-flight launch or reports cleanup debt; it never loses
an eventual native resource. A fresh session that has not reached durable
attachment is deleted exactly before its isolated server closes. A resumed
session is preserved on rollback, and normal detach preserves both fresh and
resumed native sessions. Delete/close uncertainty retains the ephemeral pair
for an exact retry. Endpoints, clients, and basic-auth secrets remain transient.

## Peer and parent behavior

- Rename uses `PATCH /session/{id}` through the correlated component rename
  request and requires the response's ID/title to match exactly. The shared
  cancellation signal is passed to the native request. A matching native event
  arriving before the SDK response is held for correlation; failure releases
  it afterward as an unsolicited observation, and a conflicting native title
  makes the correlated result ambiguous.
- Managed peer resume obtains its authoritative native anchor only from the
  prepared durable attachment. The launcher continues to reject user
  `--session` overrides rather than trust argv. Same-generation component
  reconnect and lane recovery remain independently supported.
- `shell.env.sessionID` is the primary parent-session witness. The plugin
  injects the exact component binding and session into shell/MCP children.
- `GET /background-process` is a second native witness: pid, session ID, cwd,
  status, and a unique `bgp_*` ID must all match. MCP registration must report
  `connected` on the same isolated server.
- The first-class `agent_sessions` tool exposes bounded peer messaging and lane
  lifecycle operations. Its execute context overwrites any model arguments
  with the product-provided native session witness.

An ambient plugin without a prepared one-time bootstrap remains inert. A
foreign/shared Kilo daemon and an embedded-mode TUI are not adopted as managed
peers.
Managed launch claims the exact shared component revision
`agent-sessions.component.v1-r2`; the independent product integration version
is not accepted as a protocol credential.

The bootstrap intentionally omits declared process-start and strong-start
fields. The live local socket supplies kernel peer credentials, from which the
broker independently captures process identity; host composition verifies
ancestry, executable identity, and the one-time capability. Kilo additionally
requires the live component process to match the Agent Sessions-owned isolated
server spawn identity before adopting its exact `ses_*` session. Declared
process fields are optional corroboration, not authority. This is green at the
Kilo product boundary without a wrapper or side channel, while production
central Authority composition remains an explicit end-to-end acceptance gate.

## Lanes and permissions

Kilo lanes use a separately owned authenticated `kilo serve` instance. Fresh
and resumed sessions are verified through `/session`; server-owned turns use
the supported `/api/session/{id}/prompt` route. The initial delivery is
`queue`; a busy input uses `delivery=steer` and records the exact admitted
`msg_*`/session response. Peer `/tui/*` and lane `/api/session/*` routes are
never substituted for one another.

| Agent Sessions mode | Native rule |
|---|---|
| `default` | `permission=*`, `pattern=*`, `action=ask` |
| `bypassPermissions` | `permission=*`, `pattern=*`, `action=allow` |

Permission events are answered by `/api/session/{id}/permission/{id}/reply`.
Unknown policy modes fail closed. Interrupt uses the v2 session interrupt
route; result collection and archive use the bounded stable message/session
routes. The mode selected at lane open is retained and must match every start
or steer. Recovery obtains the exact prior mode from the host-owned durable
lane record or returns unsupported.

The v2 event stream is paired with bounded message reconciliation: the exact
admitted user `msg_*` must have a completed assistant child. This closes the
fast-terminal window between prompt acceptance and SSE subscription without
attributing a different session turn.

Lane open/recovery reserves a provisional exact lane entry before starting its
server. Failed fresh opens delete any created session before close; failed
resume/recovery preserves the prior session and closes only the newly owned
server. Delete/close uncertainty remains retryable cleanup debt under the same
intent digest, and concurrent or changed open/recovery attempts fail closed.

Only the verified `ses_*` ID is durable. Server endpoint, basic-auth password,
client, cwd, and title are ephemeral. Recovery gets canonical cwd from the
host-owned lane record, starts a new isolated authenticated server, and rejects
any session-ID mismatch. Federation capability `kilo-lane` is advertised only
after doctor is ready.

## Doctor and evidence scope

Doctor checks exact version 7.5.6, `/doc`, stable session/provider routes, the
v2 per-session event route, the `/tui/*` peer routes, v2 prompt/interrupt routes,
background-process support, a create/get/delete round trip, provider
availability, and the single installed plugin location. Credentials never
enter durable state or diagnostics.

Product-local automated tests use hostile bounded HTTP/SSE fixtures, including
two isolated pair endpoints, zero cross-delivery assertions, exact resume
GET/select/attach routing, missing-ID no-fallback, and rollback/cleanup races.
Those fixtures do not earn real-product credit. A bounded real probe against
the pinned CLI and SDK 7.5.6 confirmed the documented `attach --session` flag,
the generated/live `/tui/select-session` contract, 200/true before and after a
full attach, a 404 missing-ID control, and a full TUI title/exit summary bound
to the exact created `ses_*`. The real pinned two-server/two-full-attach
evidence remains the separate S1 phase-0 artifact; physical Linux/macOS
release acceptance is a later gate. The product-boundary attestation design
also does not claim production central Authority end-to-end credit.
