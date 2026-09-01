# Pi integration

Agent Sessions targets `@earendil-works/pi-coding-agent` **0.84.4** and the
`pi` executable. This is an exact pin: pre-1.0 native protocol changes fail the
doctor version check instead of receiving compatibility guesses.

## Managed peers and parents

A managed wrapper loads `integrations/pi/agent-sessions.mjs` with Pi's
supported `--extension` option. The extension stays inert unless all one-time
component bootstrap fields are present: it registers no model tool, command,
or native event hook and performs no component I/O. Once the component broker returns
`ready`, the extension reads the UUID from
`ctx.sessionManager.getSessionId()`, announces it, and exports that same value
as `AGENT_SESSIONS_SESSION_ID` for native tool children. Bootstrap secrets are
removed by the shared component client and are never session authority.
Central composition must construct `NewComponentObserver`, invoke it only
after durable `componentruntime.Authority` admission, and pass that exact
pointer into `NewRuntime`; a nil or implicitly trapped observer is rejected.

The extension maps `idle-wake`, `busy-steer`, and `busy-follow-up` deliveries
onto `pi.sendUserMessage`. It reports exact native acceptance using the
AgentFrame message ID and never accepts a delivery for a session other than the
current `session.bound` identity. Pi reaches durable idle only on
`agent_settled`; `agent_end` can precede retry, compaction, or queued work and
is not reported as terminal.

Native title changes are observed through `session_info_changed`. A managed
rename uses the broker-correlated `session.rename.request`, calls
`pi.setSessionName` exactly once, and succeeds only after Pi emits the exact
requested title. When native title clearing emits an undefined name, the
extension observes a genuine empty title and never fabricates a product name;
that unsolicited change cannot confirm a pending nonempty daemon rename.
Product whitespace is preserved, while unsafe controls and oversized titles
fail closed without an observation.
Delivery is not used as a rename side channel.

The in-process `agent_sessions` registered tool and `/lane` command expose the
bounded peer/lane operation vocabulary. Parent attestation requires the exact
component binding, live kernel socket credential, independently captured
process identity, and current native session. Declared `PROCESS_START` and
`STRONG_START` values are optional corroboration: launch preserves an exact
pair, accepts both omitted, and rejects partial or malformed declarations.
They never replace live attestation. The in-process tool is exact; the
foreground CLI route admits at most one direct connector edge from the
registered component process. A deeper descendant/subagent chain cannot
impersonate the TUI session.

## Lanes and permissions

Each lane owns one `pi --mode rpc` child above the shared structured-process
supervisor. Pi 0.84.4 does **not** emit a startup `ready` frame, so readiness is
established by a correlated `get_state` response with a non-busy native session
ID. Commands and responses remain bounded LF-delimited JSON objects.

Turns use `prompt`, `steer`, `abort`, `get_state`, and
`get_last_assistant_text`. Completion waits for `agent_settled`. Resume starts
a fresh ephemeral RPC client with `--session <exact-native-id>` and rejects a
different returned ID. Only that native ID is the live-reconciled anchor;
client, process journal, cwd, argv, and mutable native name are not copied into
the driver. Recovery requires the host to provide current canonical launch
facts.

| Agent Sessions mode | Pi native policy |
|---|---|
| `default` | `--tools read,grep,find,ls` |
| `bypassPermissions` | Pi's full native tool set |

Pi has no native approval prompt. The default therefore uses a restricted
read/search allowlist; broad tools require explicit `bypassPermissions`.
Unknown modes and user arguments that override managed mode, session,
extension, name, or tool policy fail closed. Lane open currently rejects all
caller-supplied native argv: Pi positional, `@file`, and print-mode arguments
can create model input before the receipt-backed `StartTurn`. Prompts enter a
managed lane only through the receipt ledger.

## Doctor and evidence scope

Doctor checks the `pi` executable, exact version `0.84.4`, the required RPC,
extension, resume, and tools options, a readable managed extension asset, the
exact exported shared component contract revision
`agent-sessions.component.v1-r1`, and an injected central component-authority readiness check. Peer/parent readiness
is reported only when that integration-depth check succeeds. Product-local
tests cover hostile frame correlation, receipt tampering, exact resume,
settled semantics, component identity, registered-tool false IDs, and native
rename correlation. These fixtures do not claim real-product or end-to-end
peer credit; the shared Authorizer/re-freeze and physical Linux/macOS
acceptance gates remain separate.
