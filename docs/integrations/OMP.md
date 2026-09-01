# Oh My Pi integration

Agent Sessions targets `@oh-my-pi/pi-coding-agent` and the `omp` executable at
the exact tested version **18.0.11**. OMP shares only verified Pi-family
mechanics; every native difference is represented by a closed product quirk
row.

## Differences from Pi

| Aspect | Pi 0.84.4 | OMP 18.0.11 |
|---|---|---|
| Runtime | Node | Bun |
| Extension option | `--extension <path>` | `--extension=<path>` |
| RPC mode option | `--mode rpc` | `--mode=rpc` |
| Startup readiness | correlated `get_state` | required first `ready` frame for protocol 1 |
| Fully terminal event | `agent_settled` | `agent_end` with `willContinue != true` |
| Busy steer framing | original text | OMP adds its native interjection envelope |
| Default permission | restricted `--tools` | unsupported until RPC approval mediation is available |
| Explicit bypass | full tools | `--approval-mode=yolo` |

The OMP entry point imports the same
`integrations/pi/pifamily.mjs` implementation. It passes busy steer text
unchanged to `pi.sendUserMessage(text, {deliverAs: "steer"})`; OMP alone adds
its native `<system-notice>` interjection framing. Agent Sessions never
pre-wraps or double-frames that message.

## Identity, rename, and parent authority

After the managed component handshake, the extension takes the UUIDv7 session
ID from `ctx.sessionManager.getSessionId()` and exports it for foreground tool
children. A bare/global extension without the complete one-time bootstrap is
inert: it registers no model tool, command, or native event hook and performs
no component I/O. There is no fallback to an ambient `OMP_*` or `PI_*` bash
variable.
Central composition must construct `NewComponentObserver`, invoke it only
after durable `componentruntime.Authority` admission, and pass that exact
pointer into `NewRuntime`; a nil or implicitly trapped observer is rejected.

Delivery requires the exact current `session.bound` identity. Native title
observations use `session_info_changed`; daemon-requested rename is a
broker-correlated request with `pi.setSessionName` as the single writer and the
matching native event as confirmation. A cleared native title is observed as
genuine empty data and cannot confirm a pending nonempty daemon rename.
Product whitespace is preserved, while unsafe controls and oversized titles
fail closed without an observation. The registered `agent_sessions` tool
and `/lane` command execute in-process. Parent attestation compares the exact
binding, live kernel socket credential, independently captured process identity,
and native session, and rejects model-supplied false IDs. Declared
`PROCESS_START` and `STRONG_START` values are optional corroboration: launch
preserves an exact pair, accepts both omitted, and rejects partial or malformed
declarations. They never replace live attestation. A foreground connector must
be the registered process itself or its direct child; a deeper
descendant/subagent chain is rejected.

## Lane lifecycle

Every lane owns a private `omp --mode=rpc` structured process. Its first frame
must be the version-advertising `ready` object for protocol 1 with the pinned
1 MiB frame limit; output before ready, a duplicate ready, an unknown response
ID, or a mismatched response command is a protocol failure. Adoption then
corroborates the exact native ID with `get_state`.

The driver supports open, prompt, wait, native steer, abort, archive, and exact
resume via `--session <native-id>`. An `agent_end` carrying
`willContinue:true` is not terminal. Cwd, title, JSONL client, and process
journal remain ephemeral; only the native ID is reconciled as the reattach
anchor, and host-owned canonical launch facts are required for recovery.

Unknown permission modes fail closed. OMP's `always-ask` mode delivers native
approval prompts as `extension_ui_request` RPC frames, but the frozen host
contract has no permission-authority callback for answering them. Consequently
Agent Sessions rejects `default` before starting an OMP RPC child, and also
fails a surprise approval frame promptly instead of hanging or widening it.
`bypassPermissions` is the only currently supported mapping, to the explicit
`--approval-mode=yolo`; it is never inferred. Lane open rejects all
caller-supplied native argv because positional, `@file`, print-mode, and
extension-defined arguments can inject model input before the receipt-backed
`StartTurn`. Prompts enter a managed lane only through the receipt ledger.

## Doctor and evidence scope

Doctor checks the `omp` executable, exact version 18.0.11, required RPC,
extension, resume, and approval-mode options, the readable managed extension
asset, the exact exported shared component contract revision
`agent-sessions.component.v1-r2`, and an injected central
component-authority readiness check.
Product-local Go and native-extension tests exercise ready
negotiation, continuing terminal events, raw steer preservation, permission
mapping, delivery identity, rename correlation, and parent anti-impersonation.
They do not earn real-product or end-to-end peer credit; shared Authorizer
re-freeze and physical Linux/macOS acceptance are independent gates.
