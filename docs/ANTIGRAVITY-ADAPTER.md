# Antigravity adapter: findings, constraints, and continuation plan

Status: **parked design and implementation work**

This memo records what was learned while developing Antigravity support on
`feature/antigravity-support`. The pull request remains open so the working
implementation, tests, review history, and cross-host evidence are not lost,
but it is not ready to merge as a reactive peer product.

The decisive product requirement is simple:

> A message sent to an idle agent must start processing without a human typing
> a dummy prompt in another terminal.

The current stock-CLI adapter does not meet that requirement. It provides
authenticated discovery, durable delivery, outgoing messaging, and lane
ownership, but an Antigravity CLI conversation that has returned to its input
prompt does not observe an incoming message until the user starts another
invocation. Requiring the user to type `.` is not automation and makes the
interactive peer unsuitable as a reactive worker.

## 1. Product objective

Antigravity support should eventually provide two related but distinct
surfaces:

1. An interactive integration that lets an Antigravity user discover peers,
   send messages, and launch Codex or Claude lanes.
2. A durable `agy-peer-lane` worker with the same practical lifecycle contract
   as `codex-peer-lane` and `claude-peer-lane`: start, wait, resume, message,
   interrupt, archive, persistence policy, owner policy, and federation.

The second surface must be genuinely reactive. An idle lane must consume a peer
message without a terminal, timer-driven model invocation, or human action.

The Grok adapter should not copy Antigravity-specific mechanisms. Shared launch
attestation and peer routing belong in generic internal infrastructure; product
adapters should own only their host protocol and lifecycle semantics.

## 2. What the parked branch implements

The current branch contains a functioning stock Antigravity CLI integration:

- `agy-peer` prepares a process-bound, random launch token and then executes the
  headless `agy` CLI selected from the user's environment.
- The launcher removes only Agent Sessions' `-n/--peer-name` option and forwards
  the remaining Agy argument vector.
- CLI selection fails closed. It probes the executable's CLI contract and
  rejects known Antigravity desktop/GUI helpers on Linux and macOS, including
  symlinked and case-variant macOS application paths.
- Globally installed Agy hooks are inert outside an attested `agy-peer` launch.
- The first attested hook binds one launch token to one Agy conversation ID.
- A native shim publishes the Agy conversation in local and federated peer
  discovery with a stable reply address.
- The `agent_sessions` MCP surface exposes peer discovery, messaging, inbox
  recovery, identity, and rename operations. The caller's session ID is derived
  from the attested host process; model-supplied identity is optional and cannot
  grant authority.
- An Agy conversation can own local or remote Codex and Claude lanes. The
  existing lane engines remain authoritative; no third copy of their lifecycle
  logic was introduced.
- Plugin installation is performed directly rather than through `agy plugin`
  because current macOS CLI builds delegate that command to the desktop
  application and can open or update the GUI.

The branch also generalized the process-bound launch record so another product
adapter can reuse token, ancestry, and lifecycle attestation without copying
the policy.

### Verified behavior

The implementation was exercised on Linux and macOS with Agy CLI 1.1.13.
Verified behavior includes:

- CLI-versus-GUI executable selection and fail-closed override validation.
- Agy launch attestation and cross-product isolation.
- Agy publication in peer discovery.
- Agy-to-Codex messaging and a cross-host Agy-to-Codex round trip.
- Durable delivery to an idle Agy inbox.
- Codex and Claude lane ownership from an attested Agy conversation.
- Linux and macOS builds, normal tests, race tests, vet, and lint at the tested
  branch revisions.

The cross-host experiment also demonstrated the blocking defect precisely: the
message was durably waiting, but Agy did not process it until the user typed a
new prompt. Delivery after manual input is evidence for persistence, not for
wake-up.

## 3. Why the stock CLI cannot currently be woken

Antigravity CLI 1.1.13 exposes lifecycle hooks and MCP tools during an active
agent invocation. Those integrations are request/turn surfaces, not an idle
control plane.

When the interactive CLI is sitting at its prompt:

- lifecycle hooks are not running;
- the MCP server cannot initiate a model turn;
- the CLI exposes no documented local HTTP, gRPC, Unix-socket, ACP, or other
  control endpoint for submitting a prompt to the live process;
- a second process must not concurrently resume and write the same conversation
  while the interactive process remains alive;
- changing transcript files on disk does not cause the live process to reload
  its in-memory conversation state.

The current adapter therefore queues the message and injects it at the next
real hook boundary. This is lossless but not reactive.

### Approaches considered and rejected

#### Periodic schedule or continuation loop

A short recurring schedule eventually reaches a hook boundary, but every tick
can cause another model invocation. It burns tokens and quota, grows context,
introduces delivery latency, and keeps the interactive session busy. A long
interval merely trades cost for worse latency.

#### Blocking MCP wait

A long-lived `wait_for_peer_message` tool can block without consuming model
tokens while it waits, but the Agy session remains in a busy tool call. The user
cannot use the interactive prompt normally, cancellation aborts the turn,
headless execution has a print timeout, and repeated wait/result steps pollute
the transcript. It is useful only as a bounded synchronization tool, not as an
indefinite peer runtime.

#### Concurrent `agy --conversation ID --print ...`

Starting a second Agy process against a conversation still owned by a live
interactive process is unsupported and unsafe. The two processes can diverge
in memory and append incompatible trajectory steps. Agent Sessions must never
use this as an idle-wake shortcut.

#### Owned PTY or tmux injection

Owning the terminal would permit synthetic keystrokes, but it would make Agent
Sessions responsible for terminal allocation, resizing, signals, suspend and
resume, alternate-screen behavior, SSH disconnects, nested multiplexers,
process groups, output forwarding, crash recovery, and platform differences.
It would still be a UI automation contract rather than a supported agent
protocol. The complexity and fragility are disproportionate, particularly when
the same mechanism would also need to supervise durable Agy lanes.

PTY ownership is not the planned solution.

## 4. Official Antigravity SDK investigation

Google released the `google-antigravity` Python SDK after the original Agy
adapter design was written. It is not a thin text-generation client. Its
platform wheel includes Google's compiled `localharness` runtime, and the SDK
provides the same core agent loop used by Antigravity CLI and Antigravity 2.0.

Relevant capabilities include:

- built-in file, edit, search, shell, web, image, and subagent tools;
- stateful multi-turn conversations and conversation resumption;
- MCP servers, skills, custom Python tools, policies, and lifecycle hooks;
- context compaction, structured output, artifacts, and usage reporting;
- long-lived asynchronous **triggers** that react to external events and call
  `TriggerContext.send()` to submit an `automated_trigger` input event to the
  harness connection.

An Agent Sessions trigger could block efficiently on peer traffic and send the
message into the harness without polling, terminal injection, or a concurrent
conversation writer. SDK custom Python tools could provide outgoing Agent
Sessions operations directly, so an SDK-backed adapter would not need MCP as
its Agy-facing tool transport.

### SDK boundary: same harness, different product surface

The SDK retains the agent runtime, but it does not reproduce the complete Agy
CLI product shell. A wrapper would still own or lose:

- the native Agy TUI and slash commands;
- CLI plugin discovery and installation behavior;
- approval and question presentation;
- native status and artifact presentation;
- exact compatibility with the CLI conversation store;
- CLI-specific argument and model-selection semantics.

This is acceptable for a headless lane, but an SDK console must not be marketed
as the native Agy interactive experience.

### Decisive SDK blocker: authentication and billing

The SDK does **not** reuse the Google-account OAuth credentials or consumer
subscription used by Agy CLI. Current supported model authentication is a
Gemini Developer API key or Vertex AI configuration. Reusing the CLI login is
an open upstream feature request.

Consequences:

- an SDK lane consumes separate API or Vertex quota and may create separate
  charges;
- a user with a working paid Agy subscription may still be unable to start the
  SDK;
- the set of available models and policies can differ from the CLI account;
- SDK credentials are not configured on the development host, so an end-to-end
  reactive wake test cannot currently be performed there;
- silently replacing `agy-peer` with the SDK would change authentication,
  billing, storage, and frontend behavior.

For those reasons, the SDK is not the default lane implementation for this
project today. It remains a good optional backend if Google adds CLI OAuth
reuse, or if Agent Sessions deliberately introduces an explicitly configured
API/Vertex lane product later.

Official references:

- [Google Antigravity SDK announcement](https://antigravity.google/blog/introducing-google-antigravity-sdk)
- [SDK overview](https://antigravity.google/docs/sdk/overview)
- [Official SDK source](https://github.com/google-antigravity/antigravity-sdk-python)
- [SDK trigger design](https://github.com/google-antigravity/antigravity-sdk-python/blob/main/google/antigravity/triggers/README.md)
- [Open request to reuse CLI OAuth credentials](https://github.com/google-antigravity/antigravity-sdk-python/issues/20)
- [Agy CLI headless mode](https://antigravity.google/docs/cli/headless)

## 5. Recommended subscription-compatible lane architecture

The best currently implementable route is a serialized, headless Agy lane
manager built on the supported CLI:

```text
peer message / resume request
            |
            v
   Agent Sessions lane manager
            |
            | exactly one child at a time
            v
agy --print --conversation <id> --output-format stream-json ...
            |
            v
  events + final result -> lane journal / peer reply
```

This is process-per-turn rather than a persistent App Server, but it can expose
the same external lifecycle contract as the existing lane products.

### Required invariants

1. **Single writer.** At most one Agy child may operate on a conversation.
2. **No interactive co-ownership.** A managed lane conversation is never
   simultaneously opened by a stock interactive Agy process.
3. **Durable queue.** Incoming messages are persisted before the manager
   acknowledges ownership.
4. **Serialized turns.** Messages that arrive during a turn are queued for the
   next turn unless Agy documents a safe in-turn injection contract.
5. **Stable lane identity.** Agent Sessions keeps one durable lane ID and stores
   the Agy conversation ID as product state.
6. **Native authentication.** Every child uses the user's existing Agy CLI
   login/subscription and configuration.
7. **Exact policy forwarding.** Model, effort, sandbox, permission, workspace,
   and timeout flags are forwarded only when explicitly selected by the caller
   or recorded as durable lane policy.
8. **Bounded execution.** Each print-mode turn has an explicit manager timeout
   and a defined interrupt escalation path.
9. **Crash convergence.** A manager restart reconstructs state from the lane
   journal and either collects/reconciles the prior child or marks the turn
   failed; it never blindly submits the prompt twice.
10. **No transcript scraping when structured output exists.** Use Agy's
    documented `stream-json` events and explicit conversation identifier.

### Proposed command contract

The new product should follow contract version 2 and mirror existing lane
verbs:

```bash
agy-peer-lane doctor --json
agy-peer-lane run --name NAME -
agy-peer-lane start --name NAME -
agy-peer-lane wait NAME --timeout SECONDS
agy-peer-lane resume NAME -
agy-peer-lane status NAME
agy-peer-lane interrupt NAME
agy-peer-lane archive NAME
agy-peer-lane list
```

The shared event vocabulary should remain `lane.ready`, `turn.started`,
`item.completed`, `turn.completed`, and `lane.archived`. Product-specific raw
events may be included as diagnostic fields but must not force orchestrators to
parse Agy transcripts.

### Turn flow

For a fresh lane:

1. Validate the selected executable as the headless Agy CLI.
2. Create the lane journal and manager authority.
3. Start one `agy --print --output-format stream-json` child.
4. Capture the Agy conversation ID from structured output and persist it before
   announcing a reusable lane.
5. Stream normalized events and persist the final outcome.

For a peer message or resume:

1. Resolve and durably queue the request.
2. If idle, start `agy --conversation <id> --print ...` immediately.
3. If busy, retain it for the next serialized turn.
4. Normalize the final response into the lane result and send the terminal
   notice or peer reply.
5. Apply persistent/auto-archive policy exactly as for other lane products.

### Agy-originated messaging and other lanes

The Agy process can continue using the installed Agent Sessions plugin during
each managed turn. Hooks attach the process-bound launch to the known lane
conversation, and the model can use Agent Sessions tools to:

- reply to the sender;
- discover peers;
- launch or manage Codex and Claude lanes;
- operate remote lanes through `peer-federator`.

The manager must inject stable identity and the original peer envelope into the
turn. It must not depend on a transient model-retained session ID for authority.

## 6. Interactive compatibility surface

Until Agy exposes a supported idle control endpoint, the stock interactive
adapter has three honest options:

1. Remain available for outgoing messaging and lane orchestration, while
   documenting that inbound messages are delivered only at the next invocation.
2. Stop advertising idle interactive Agy sessions as immediately messageable,
   but retain durable inbox recovery.
3. Defer the interactive adapter entirely and ship only `agy-peer-lane` when
   the headless lifecycle is proven.

The future implementation should choose explicitly. It must not present
queue-only delivery as equivalent to Codex/Claude wake-up.

## 7. Acceptance criteria for continuation

The Antigravity PR should not leave draft status until all required cells below
pass on Linux and macOS where applicable.

### Lane lifecycle

- Fresh named and unnamed lane starts produce a stable Agy conversation ID.
- `run`, `start` plus `wait`, `resume`, `status`, `interrupt`, and `archive`
  conform to contract version 2.
- Resume uses the same conversation and retains prior context.
- Persistent and auto-archive policies survive resume without implicit changes.
- Manager, child, shim, socket, and registry state converge after normal exit,
  failure, interrupt, owner death, and archive.

### Reactive messaging

- A message sent to a fully idle Agy lane starts a turn without terminal input.
- The lane receives the exact message once and can reply to the originating
  peer.
- Messages arriving while busy are serialized without loss or duplication.
- Local and federated round trips pass in both directions.
- No polling model turns occur while the lane is idle.

### Safety and correctness

- Two Agy processes never write one conversation concurrently.
- A forged conversation ID, launch token, parent PID, or product type cannot
  claim lane authority.
- Default and bypass permission modes are reported accurately, including
  repeated boolean flag spellings where the final value wins.
- GUI helpers are rejected without execution on Linux and macOS.
- Interrupt is bounded and cannot leave a child continuing without manager
  ownership.
- A crash between prompt submission and journal update has a tested recovery
  outcome and does not resubmit blindly.

### Compatibility

- Existing Codex and Claude lane behavior remains unchanged.
- An attested Agy orchestrator can launch, collect, resume, and archive local
  and remote Codex/Claude lanes.
- Packaging contains every required binary and plugin asset on linux-amd64,
  linux-arm64, darwin-amd64, and darwin-arm64.
- The implementation is tested against the documented Agy CLI version and
  fails with an actionable diagnostic when the CLI contract changes.

## 8. Work to preserve when rebasing or redesigning

Even if the current interactive implementation is substantially replaced, the
following branch work should be retained where it still applies:

- generic process-bound agent launch records and product isolation;
- Agy executable validation and desktop-helper rejection;
- direct, non-GUI plugin staging;
- optional caller session IDs derived from host attestation;
- Agy ownership corroboration for Codex and Claude lanes;
- federated product naming and protocol support;
- tests covering legacy hook no-ops and cross-product token rejection;
- the agent-lanes skill's rules against reusing unrelated agents or falling
  back silently from remote to local execution.

The product-independent Codex resume-CWD correction on this branch must be
extracted and merged separately rather than waiting for Antigravity support.

## 9. Upstream developments that would change the decision

Re-evaluate the SDK backend if Google adds any of the following:

- supported reuse of Agy CLI OAuth/subscription credentials in the SDK;
- a supported SDK endpoint backed by the user's Antigravity subscription;
- a stock CLI control socket or ACP-like prompt-injection API;
- a documented sidecar/trigger plugin surface for the stock CLI;
- a supported guarantee for concurrent external prompt submission to a live
  interactive conversation.

Until then, the subscription-compatible headless CLI manager is the preferred
future lane design, and the existing PR remains an intentionally parked body of
implementation and evidence rather than a releasable reactive peer.
