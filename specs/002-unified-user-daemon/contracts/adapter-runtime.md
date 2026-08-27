# Contract: Daemon-Owned Product Adapters

## Shared boundary

Codex, Claude, Grok, and Qwen use one daemon-owned attachment and lane lifecycle. Product adapters
implement only native differences; they do not own daemon lifetime, routing, groups, authoritative
registries, durable acceptance, or local listeners.

Each adapter exposes callable operations to the daemon:

1. `Probe`: report native executable, profile, authentication/readiness metadata without credential
   values.
2. `PrepareInteractive`: validate launch/resume intent and return expected native evidence.
3. `ObserveSelection`: prove the selected native session and canonical name for late-bound products.
4. `VerifyAttachment`: re-corroborate the exact native actor/channel before sensitive operations.
5. `Deliver`: deliver one already-admitted AgentFrame through the vendor-native channel.
6. `StartTurn`: start one already-committed lane turn.
7. `ReconnectTurn`: reconnect after daemon recovery only through a supported native contract.
8. `InterruptTurn`: interrupt the exact current native turn.
9. `CollectTurn`: return stable terminal metadata/result without advancing the daemon cursor itself.
10. `Archive`: invoke and verify the native archive contract.
11. `Reconcile`: classify exact native and Agent Sessions artifacts after exit/crash/restart.
12. `Cleanup`: remove only exact Agent Sessions-owned artifacts after the native actor is retired or
    proven absent.

The daemon owns transaction revisions, idempotency, group decisions, parent context, collection
cursors, terminal notices, retries, and cleanup debt around these calls.

## Shared invariants

- `ProductDescriptors` remains the authoritative supported-product/capability inventory.
- An adapter receives a daemon-owned context and stops work when that context is cancelled.
- Long-running adapter loops are goroutines within the daemon, not detached Agent Sessions processes.
- Vendor processes are started with exact executable and argv; they remain vendor-owned external
  resources.
- No adapter binds an Agent Sessions local control or delivery socket.
- No adapter writes another registry/catalog independently of the daemon state transaction.
- Product callbacks re-enter the daemon through typed in-process functions, not loopback RPC.
- Unknown or changed native evidence fails closed and records debt; it never selects a similar process.

## Interactive lifecycle

```text
daemon prepare commit
  -> short-lived launcher execs native client
  -> adapter observes exact native actor
  -> daemon adopts authoritative session ID and publishes attachment
  -> adapter maintains native observation/delivery channel
  -> native exit or explicit detach
  -> daemon unpublishes, reconciles, and records any cleanup debt
```

After preparation, the launcher replaces itself with the native vendor process or exits after a
vendor-supported launch handoff. No long-lived Agent Sessions launcher remains as the native TUI's
parent. If a vendor truly mandates a child process at a protocol boundary, only the stateless connector
contract below may satisfy it; that exception cannot own lifecycle, state, or a listener.

## Codex adapter

### Vendor-owned resources

- Codex CLI/TUI
- Codex App Server process and control socket
- Codex thread/rollout history and projection
- native sandbox/approval behavior

### Daemon-owned behavior

- one App Server client/coordinator for every configured Codex profile inside the one daemon;
- prepared launch and interactive-owner revisions;
- hook attestation and late lifecycle updates;
- thread selection/name/cwd/profile corroboration;
- peer delivery through the supported App Server thread contract;
- Codex lane states, turns, notices, collection, archive coordination, and cleanup debt.

### Removed roles

- detached profile supervisor;
- per-thread/per-lane shim process and socket;
- launcher lazy bootstrap of Agent Sessions runtime.

### Restart expectation

The daemon reconnects to the existing App Server and reconstructs attachments from durable thread state,
live TUI/hook evidence, and exact process identity. Accepted turns use the App Server's durable identity
to continue without redispatch. A missing required Codex history projection is reported as a native
readiness/remediation issue; Agent Sessions does not migrate vendor history automatically.

## Claude adapter

### Vendor-owned resources

- Claude TUI and stream-JSON lane worker
- Claude native session registry and messaging socket
- native transcript, authentication namespace, permissions, and resume selection

### Daemon-owned behavior

- gated launch/lifecycle record;
- selected UUID/name adoption after native resume resolves;
- exact PID/start/ancestry/profile/cwd/socket attestation;
- one synthetic Agent Sessions service projection required by Claude's native registry contract;
- direct delivery through the corroborated native Claude socket;
- Claude lane state, queue, notices, collection, archive coordination, and cleanup debt.

### Removed roles

- wrapper-owned authoritative registration/lifecycle state;
- detached Claude lane-manager process and control socket.

### Restart expectation

Interactive peers reattach from their exact live native registry/socket evidence. An active stream-JSON
lane worker may be recorded once as interrupted, collectable, and resumable if real native testing
proves the daemon cannot reconnect to its inherited pipes safely. A proxy process may not be retained
solely to preserve those pipes.

## Grok adapter

### Vendor-owned resources

- Grok TUI, private leader/observer, ACP sessions, roster, interjection, authentication, and transcript

### Daemon-owned behavior

- launch capability and exact owner/leader/session attestation;
- ACP client and roster/wake coordination as daemon goroutines;
- authoritative selection/name adoption;
- delivery through supported interjection/MCP calls;
- Grok lane state, turns, notices, collection, archive coordination, and cleanup debt.

### Removed roles

- per-peer Grok host process/control socket/delivery listener;
- detached Grok lane-manager process/control socket;
- remote lane watcher/CLI/manager process chain.

### Restart expectation

Interactive attachments reconstruct from exact leader/roster evidence. Active ACP turns reconnect only
when Grok exposes a supported session contract; otherwise the one in-flight turn becomes explicitly
interrupted and is resumable with native `session/load` semantics.

## Qwen adapter

### Vendor-owned resources

- Qwen TUI, daemon/ACP worker, native event/input files, archive store, authentication, and transcript

### Daemon-owned behavior

- profile/readiness and extension attestation;
- launch capability, dual-output admission, selected UUID/name, ancestry, cwd, and artifact evidence;
- native event observation and input writer as daemon-owned adapter components;
- Qwen lane state, turns, notices, collection, archive coordination, and cleanup debt.

### Removed roles

- per-peer Qwen host process/delivery listener;
- detached Qwen lane-manager process/control socket;
- remote lane watcher/CLI/manager process chain.

### Restart expectation

Interactive attachments reconstruct from exact admitted native artifacts and live ancestry. Active ACP
turns reconnect only through a supported Qwen session contract; otherwise the one in-flight turn is
explicitly interrupted and resumable through the native transcript.

## MCP connector contract

The vendor-spawned MCP process:

- serves the vendor's required stdio JSON-RPC framing;
- connects to the fixed daemon endpoint;
- supplies only launch capability and non-authoritative corroboration;
- lets the daemon derive the peer identity and tool inventory;
- forwards `initialize`, `ping`, `tools/list`, and `tools/call` results;
- reconnects after daemon generation change;
- exits with the vendor session or on stdin EOF;
- owns no Agent Sessions listener or durable file.

The daemon owns all tool descriptions and implementation. The connector cannot fall back to Claude
native messaging or another carrier when the daemon is unavailable.

## Bare native sessions

Installation may make the integration visible, but a bare native client has no daemon-prepared launch
capability and no managed attachment. Its connector receives the existing inactive result. The daemon
must not create an attachment from plugin presence, process name, same-user access, or model-supplied
session ID.

## Product availability

Each adapter independently reports:

- `not_installed`
- `installed_unready`
- `ready`
- `degraded`

One unavailable adapter does not prevent daemon readiness for other products. Federation advertises
only `ready` capabilities. Aggregate installation skips absent optional native products while explicit
product installation remains strict and diagnostic.

## Removal and cleanup

The daemon reports exact live attachment/lane blockers before removal. Adapter connector removal uses
the vendor's supported installer and reports vendor bookkeeping left behind. It never manually edits
credential, transcript, or profile state to manufacture a byte-clean uninstall.
