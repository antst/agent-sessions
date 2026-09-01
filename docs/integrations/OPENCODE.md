# OpenCode integration

Agent Sessions targets OpenCode CLI, SDK, and plugin version **1.18.25**. The
integration is fail-closed: doctor requires the exact native version and
feature-detects the documented HTTP routes from `GET /doc` because OpenCode's
OpenAPI document does not negotiate a native API version.

## What it enables

- A managed OpenCode TUI loads the shipped first-class plugin. The plugin
  connects outbound to the dedicated Agent Sessions component socket, announces
  exact `ses_*` identities, observes generic native events, injects messages
  with `prompt_async`, and registers the `agent_sessions` parent tool.
- Local and federated `opencode` lanes use one authenticated literal-loopback
  `opencode serve` process per lane. The driver supports create, prompt, SSE
  wait, interrupt, exact resume/recovery, result collection, and archive.
- `shell.env.sessionID` supplies exact parent identity to tool children. The
  plugin injects the corroborating component binding and native session into
  the connector environment. A model-provided session ID is never authority.
- Native title changes flow outward as `session.rename`; managed rename uses
  the correlated `session.rename.request` component operation and succeeds only
  after the native update response matches the requested session and title.
  The shared deadline signal cancels the SDK request. An early matching native
  event is held for the correlated result; rejection releases it afterward as
  an unsolicited observation, while a different title makes the result
  ambiguous. Concurrent writes to the same native session are rejected.

An ambient plugin stays inert without the one-time wrapper bootstrap. A bare
OpenCode launch is therefore an unmanaged opt-out. A flagless TUI without the
installed plugin cannot be attached retroactively and requires restart.
Managed launch claims the exact shared component revision
`agent-sessions.component.v1-r2`; the independent product integration version
is not accepted as a protocol credential.

The bootstrap intentionally does not export process-start or strong-start
claims. On the live local component socket, the broker obtains kernel peer
credentials and independently captures the process identity; host composition
then verifies prepared-launch ancestry, executable identity, and the one-time
capability. Declared process fields would be corroboration only. This is green
at the OpenCode product boundary without a wrapper or side channel. Production
central Authority composition remains an explicit end-to-end acceptance gate.

## Permission modes

| Agent Sessions mode | Native rule |
|---|---|
| `default` | `permission=*`, `pattern=*`, `action=ask` |
| `bypassPermissions` | `permission=*`, `pattern=*`, `action=allow` |

Unknown modes fail with `unsupported-policy`. Default-mode permission events
are answered through the documented session permission reply route; a product
adapter never silently selects all-allow. The selected mode is retained with
the live lane, and every start or steer request must match it exactly. Recovery
requires the host to resolve the mode from its durable lane record; without
that resolver, recovery is unsupported rather than widened.

## Lane protocol and recovery

The lane uses only the stable `/session` API: session create/get/delete,
`/session/{id}/prompt_async`, `/session/{id}/abort`, `/event`, message listing,
and permission reply. The driver supplies a deterministic `msg_*` ID so a 204
response is exact native acceptance. OpenCode queued prompts are not claimed as
mid-turn steering: the lane advertises `Steer=false`, causing the shared durable
input ledger to retain busy input for the next turn.

Peer delivery has a fixed deadline and passes its `AbortSignal` into
`promptAsync`. Only an exact HTTP 204 is accepted. Once native submission has
been attempted, a timeout, missing status, or late response is reported as
ambiguous and can never produce a delayed `delivery.accept`; an explicit native
error/status remains a rejection.

Completion does not depend on subscribing before a fast native turn finishes.
The bounded SSE stream handles state and permission events, while the driver
also reconciles the admitted user `msg_*` against its exact completed assistant
child. An idle event alone is not attributed to a turn.

Only the native `ses_*` ID is persisted as a reattach anchor. Endpoint,
password, HTTP client, cwd, and title remain transient. After daemon restart a
host-owned recovery locator supplies the canonical lane cwd, starts a fresh
authenticated server, and `GET /session/{id}` must return the exact prior ID.
No different session may be substituted.

Lane open and recovery reserve a provisional exact lane entry before native
server startup. If create/get/startup fails, a fresh native session is deleted
before its server closes; a resumed session is preserved and only the new
server is closed. Delete or close uncertainty retains the provisional entry as
cleanup debt. Repeating the identical open/recovery intent converges that exact
cleanup before a later explicit retry may launch again; concurrent or changed
intents fail closed.

## Doctor and failure modes

Doctor checks:

1. exact CLI version `1.18.25`;
2. required documented routes and a bounded create/get/delete round trip;
3. at least one configured provider;
4. the single installed, version-matched component plugin.

Federation capability `opencode-lane` must not be advertised unless the
required feature-depth report is ready. Credentials are memory-only and are
redacted from command formatting, JSON, diagnostics, and durable records.

The product-local automated suites use bounded literal-loopback protocol
fixtures. They do not claim new real-product acceptance credit; the pinned
real-product transport evidence remains the separately recorded phase-0 probe,
and physical Linux/macOS release acceptance is a later gate. Likewise, the
product-boundary attestation design does not claim production central Authority
end-to-end credit.
