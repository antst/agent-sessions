# DeepSeek Harness (DSH)

Agent Sessions supports DeepSeek Harness through a pinned Cordis peer plugin,
a typed ACP lane driver, and the native Cordis-registered Agent Sessions parent
tool. DSH is a developer preview, so support fails closed unless the complete
tested tuple is present.

## Required versions

The supported tuple is exact; semver widening is not allowed:

| Member | Required value |
|---|---|
| `@deepseek-ai/dsh` CLI | `0.1.2-alpha.3` |
| `@deepseek-ai/dsh-acp-app` | `0.1.2-alpha.3` |
| `@agent-sessions/dsh-plugin` | `0.1.2-alpha.3` |
| Agent Sessions-owned profile | `0.1.2-alpha.3` |
| Package manager | `pnpm@10.28.1` |

`npm` is not a supported installation path for this integration. Doctor checks
the CLI, ACP app, plugin, profile, and pnpm before it starts a feature probe. A
missing or mismatched member disables readiness and federation advertisement.

## What the integration enables

- Managed peers use a profile-agnostic Cordis protocol-driver plugin over
  `ctx.agents`. Idle delivery calls native `agent.followup()`, busy delivery
  calls native `agent.steer()`, and Cordis status/turn events report completion.
- Each managed peer process starts from an empty, Agent Sessions-owned profile
  and binds the first exact `agent/created` object. A second native session is
  rejected synchronously; sibling IDs, status, close, and turn events are never
  announced or routed. Agent Sessions prepares one profile/process per managed
  DSH session rather than multiplexing sessions through one component binding.
- Local and federated `dsh` lanes run `dsh --profile acp` as an exactly owned
  structured child. They support new, exact resume, prompt, wait, interrupt,
  archive, and recovery.
- Parent operations use the native Cordis-registered `agent_sessions` tool.
  The exact `exec.agent` identity is corroborated with the component binding
  and the native `DSH_SESSION_ID` witness. A model-supplied ID is not authority.
- Doctor performs keyless ACP `initialize` and `session/new`; authentication is
  not needed until a model turn.

The component/parent branch is inert without the managed wrapper bootstrap
environment. The separate policy-only branch activates solely for an explicit
Agent Sessions ACP lane selector. A bare or ambient DSH launch does not become
managed merely because the plugin is installed.

The bootstrap `component_version` is the separate shared wire revision
`agent-sessions.component.v1-r1`; it is never populated with the DSH package
version. The product tuple remains `0.1.2-alpha.3` metadata.

Component admission has one writer. The central broker calls the durable
component Authority first and invokes the DSH gateway only afterward as an
ephemeral session observer/delivery router. The gateway never calls back into
Authority: central handles `tool.call`, `tool.cancel`, and heartbeat frames,
while DSH retains no product-local durable component state.

## Profiles and installation assets

The packaged integration is under `integrations/dsh/`:

- `package.json` is the exact Cordis plugin package;
- `profile/package.json` is the exact ACP/profile tuple;
- `profile/cordis.patch.yml` inserts the plugin through DSH's supported Cordis
  patch mechanism;
- `plugin.cjs` is the profile-agnostic protocol driver and native parent tool;
- `mcp-env.cjs` constructs an explicit non-secret MCP environment when an MCP
  fallback is configured.

Add the plugin only to explicitly selected profiles. Profile installation and
removal must use Agent Sessions ownership receipts; credentials, unrelated
plugins, and arbitrary user profiles are never rewritten.

Peer and ACP profiles may have different names, but both are bound to one
canonical Agent Sessions-owned `DSH_HOME` below HOME/XDG state. Each configured
profile manifest must resolve to
`<DSH_HOME>/profiles/<configured-profile>/package.json` and contain the exact
pinned profile tuple. Peer requests and lane configuration cannot redirect
profile loading: any inherited `DSH_HOME` is replaced with that verified home.
Doctor uses the same home for tuple commands, while its materializing feature
probe remains isolated as described below.

The component plugin never invents product facts. The native session header
must contain the exact ID and a canonical absolute cwd. It waits for
`ctx.sessionTitle` to own a non-empty native title before announcing. Later
product-originated `session/title` events are projected through the shared
`observeRename` seam. There is no `process.cwd()` or `"dsh"` fallback.
Peer launch and durable attachment metadata also require a clean absolute cwd;
adopt, refresh, and authorize accept only the exact announced physical cwd
(lexically different symlink paths may match only when both resolve to the same
directory).

## Socket and sandbox rules

DSH's bubblewrap sandbox mounts a private tmpfs at `/tmp`. The component socket
therefore must be named `component.sock` below an Agent Sessions-owned HOME or
XDG state/runtime root. `/tmp`, macOS `/private/tmp`, the platform temporary
root, and HOME/XDG paths whose existing prefix resolves through a symlink into
one of those roots are rejected before launch. Physical macOS execution remains
pending/no-credit; the Darwin aliases are covered by portable validation tests.

The DSH sandbox controls file effects. It does not express network or process
policy, so Agent Sessions does not claim those restrictions. Tool children may
also run with `--die-with-parent`; long-lived background work should use DSH's
supported jobs mechanism, while ordinary Agent Sessions sends remain
short-lived.

DSH scrubs inherited `DSH_*` and credential-looking names from configured MCP
children. The integration uses an explicit environment block containing only
`AGENT_SESSIONS_SESSION_ID`, the HOME/XDG component socket, and the state root.
It never forges `DSH_SESSION_ID`; DSH supplies that native witness.

## Permission mapping

Mapping is exact and fail-closed:

| Agent Sessions mode | DSH sandbox | DSH approval |
|---|---|---|
| `default` | `workspace-write` | `ask` |
| `bypassPermissions` | `danger-full-access` | `never` |

`bypassPermissions` is honored only when explicitly requested. An unknown mode
returns `ErrUnsupportedPolicy`; the adapter never chooses a broader preset.

`DSH_PERMISSION_MODE` is only DSH's deployment default, so it is not the lane's
permission authority. The Agent Sessions-owned profile also receives one exact
lane-policy selector. Before ACP publishes a newly created or resumed Agent,
the plugin invokes DSH's exported `setSandboxMode` and `setApprovalPolicy`
write paths on that exact session and verifies the live sandbox and approval
folds. This deliberately overwrites persisted wider policy—for example, a
`danger-full-access`/`never` session resumed as Agent Sessions `default` is
durably restored to `workspace-write`/`ask` before `session/resume` returns and
before any prompt can be admitted.

The ACP lane has no interactive user-approval relay. If pinned DSH sends
`session/request_permission`, both ordinary `ask` mode and an unexpected
callback under `never` select only the offered `reject_once` option. Missing,
duplicate, ambiguous, or non-pinned option kinds receive JSON-RPC `-32602`.
The adapter never turns `ask` into an automatic `allow_once`.

## Lane behavior and recovery

Each lane has one immutable DSH session ID and one durable daemon lease keyed
by `(dsh, profile identity, native session ID)`. The lease is acquired before
an existing session is resumed and held by the exact ACP process identity.
Another owner fails before `session/resume`, preventing concurrent JSONL
writers. Generation recovery must prove the prior exact process dead before a
new process acquires and resumes the session. The requested profile identity
must equal the actual configured `--profile`; bounded `session/list` evidence
must show the exact native ID at the exact physical cwd before resume or
recovery. `session/new` must return a fresh ID for its exact cwd.

DSH ACP rejects a second `session/prompt` with JSON-RPC `-32602`. The adapter
maps that exact response to `ErrUnsupportedSteer`; the shared durable input
ledger keeps the same receipt and queues it for the next turn. There is no
product-local queue.

Interrupt sends `session/cancel` as a JSON-RPC notification with no `id`. The
request form is deliberately never used because DSH rejects it with `-32601`.
Completion comes from ACP stop reasons and Cordis turn events. The lazy
`session_projcache` is metadata only and is never used as a liveness signal.

Prompt admission requires either the correlated terminal response or an exact
`session/update` for the owned session carrying a non-empty native message ID;
usage, thought, delayed, and sibling updates do not admit a turn. A timeout
after the prompt may have been written poisons and reconciles the process and
lease as `ErrAmbiguousSession` instead of returning a reusable hidden turn.
Terminal settlement is broadcast and idempotent for concurrent/retried waits.
Pinned ACP may emit multiple ordered assistant message IDs around tool calls;
the lane returns the last non-empty assistant message, bounds each message and
the identity count, and rejects an interleaved/reused earlier message ID.
Settled turn evidence has fixed entry and total-byte budgets; transient chunk
buffers are cleared at projection, recent Wait retries remain idempotent, and
a deterministically evicted terminal returns `ErrStale`.
Archive records its single close attempt plus confirmed process cleanup and
lease release steps. If the close response is lost, it never blindly resends
the non-idempotent request: it kills the exact owned process, releases the
lease with independent deadlines, reports the ambiguity once, and makes the
retry a no-op. A bounded generation-local completion cache provides that retry
evidence without becoming a second durable writer; lease cleanup also
converges from durable `Releasing` and `CleanupDebt` states.

## Doctor and common failures

Run the standard Agent Sessions doctor or the closed-stdin DSH lane doctor.
Readiness checks, in order, are:

1. DSH CLI and pnpm presence;
2. exact CLI/ACP app/plugin/profile tuple;
3. keyless ACP protocol-1 `initialize`;
4. keyless `session/new` and exact close inside a disposable `DSH_HOME` whose
   sole profile is a symlink to the tuple-verified installed profile;
5. central component Authority and installed Cordis profile readiness.

Typical fail-closed diagnostics are tuple mismatch, pnpm missing, plugin absent
from the selected profile, ACP protocol drift, `/tmp` component socket, active
native lease owner, unsupported permission policy, and missing exact parent
identity evidence. Credentials and raw bootstrap values are redacted and are
not persisted in native references or doctor output.

Pinned DSH materializes durable state during `session/new`, and
`session/close` does not remove that store. Doctor therefore never runs the
feature probe against the configured DSH home. It cleans the owned process and
removes the exact disposable home after every outcome; repeated probes leave
the configured product store unchanged.

For cross-host lanes, the destination advertises `dsh-lane` only while doctor
is ready. Federation remains protocol 3 on a trusted network; an older or
unready destination returns explicit unsupported/unavailable rather than
mapping the request to another product.

## Acceptance status

The product-local tuple, permission, ACP, lease, socket, Cordis, parent-witness,
and keyless doctor suites are automated. The opt-in keyless real-product cell is:

```sh
DSH_REAL_CLI=/absolute/path/to/dsh \
DSH_REAL_PNPM=/absolute/path/to/pnpm \
DSH_REAL_PNPM_STORE=/optional/offline/store \
node integrations/dsh/real-scope-spike.cjs
```

Against exact DSH `0.1.2-alpha.3` and pnpm `10.28.1`, it prepares and boots two
independent profiles, proves two exact managed sessions and zero visible
siblings, proves persisted `danger-full-access`/`never` is replaced by
`workspace-write`/`ask` before resume returns, and boots the symlinked
disposable doctor profile with zero configured-store growth. It uses a
HOME/XDG-state component socket and removes its owned profiles afterward.

Full peer acceptance remains pending on the production component Authorizer
and startup-reconciliation gate; there is no product-local persistence or
authentication workaround. The shared component
client has the common daemon-to-component rename request seam. Native Cordis
remains the sole title writer and product-originated title events refresh the
projection, but Agent Sessions-initiated DSH rename is intentionally unwired in
this slice. It remains an accepted documented gap: the driver returns
`productruntime.ErrUnsupportedRename` without mutating a native name or a
daemon-side alias.

Credentialed real-product model-turn evidence remains **pending/no-credit**
until a key is available for the exact tuple. Keyless doctor success and mock
provider protocol coverage do not count as that credentialed acceptance cell.
