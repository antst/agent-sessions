# Daemon-owned adapter protocol

The one `agent-sessions` daemon owns every Agent Sessions host-side lifecycle, durable transaction,
local route, group decision, and lane actor. Codex, Claude, Grok, and Qwen adapters are callable
in-process components. They implement only genuine native protocol differences.

Native vendor processes remain external and vendor-owned. A daemon restart must not be confused with
native session deletion, and Agent Sessions never owns vendor credentials, profiles, transcripts, or
history databases.

## Shared operations

Each product adapter supplies the applicable operations from this closed lifecycle:

1. `Probe` reports executable, non-secret profile identity, authentication state, and readiness.
2. `PrepareInteractive` validates one launch/resume intent and returns expected native evidence.
3. `ObserveSelection` proves the authoritative native session and canonical name after selection.
4. `VerifyAttachment` re-corroborates the exact native actor/channel before a sensitive operation.
5. `Deliver` sends one already-durably-admitted AgentFrame through the native channel.
6. `StartTurn` starts one already-committed lane turn.
7. `ReconnectTurn` reconnects only through a supported native identity after daemon recovery.
8. `InterruptTurn` interrupts the exact active native turn.
9. `CollectTurn` returns stable terminal metadata without advancing the daemon's collection cursor.
10. `Archive` invokes and verifies the native archive contract.
11. `Reconcile` classifies exact native and Agent Sessions artifacts after exit, crash, or restart.
12. `Cleanup` removes only exact Agent Sessions-owned artifacts after the native actor is retired or
    proven absent.

The daemon wraps these calls with revisions, idempotency, parent context, existing global groups,
permission proof, accepted-delivery state, lane collection cursors, terminal notices, retries, and
cleanup debt.

## Process and listener ownership

Long-running adapter loops are daemon goroutines. The unified topology has no detached Agent
Sessions supervisor, per-session shim, product host, lane manager, remote lane watcher, or standalone
host federation agent. Adapters do not bind Agent Sessions control or delivery sockets and do not
write an independent registry.

There is one Agent Sessions-owned local Unix listener per OS user-host. Product callbacks re-enter the
daemon through typed Go calls, not loopback RPC. Only short-lived external clients and vendor-mandated
MCP stdio connectors cross that socket.

The short-lived peer launcher prepares a capability with the daemon, then replaces itself with the
native client or exits after a vendor-supported launch handoff. It does not own the resulting peer and
does not start the daemon. Lane commands are ordinary clients of the daemon-owned lane engine.

## Identity and admission

Preparation is not publication. A managed attachment becomes visible only after the daemon proves:

- a valid daemon-issued launch capability;
- the expected product, profile, working directory, and permission mode;
- the authoritative native session identity;
- exact process start/strong-start or vendor channel evidence; and
- the current daemon generation and attachment revision.

An unknown or changed observation fails closed. PID liveness, path shape, plugin presence, a copied
environment variable, or a model-supplied session ID never authorizes adoption or cleanup.

Bare native clients have no prepared capability and remain unmanaged. Their connector returns the
standard inactive result, exposing no peer or lane authority.

## Stateless MCP connector

When a vendor mandates an MCP child, that child is a stateless stdio relay. It:

- serves the vendor's bounded JSON-RPC framing;
- connects to the fixed daemon endpoint;
- forwards `initialize`, `ping`, `tools/list`, and `tools/call`;
- supplies launch capability and non-authoritative corroboration;
- obtains tool inventory and decisions from the daemon;
- reconnects after a daemon-generation change; and
- exits on vendor-session end or stdin EOF.

It owns no listener, catalog, lifecycle authority, durable accepted work, or fallback carrier. The
model-facing MCP inventory contains peer and lane operations only; same-user administrative commands
remain outside it.

## Product boundaries

### Codex

Codex CLI/TUI, App Server, control socket, thread/rollout history, and native approval/sandbox behavior
are vendor-owned. The daemon embeds one coordinator per configured profile, verifies hook and owner
evidence, delivers through the App Server contract, and owns Codex lane transactions. No profile
supervisor or thread/lane shim remains. A missing Codex history projection is a native readiness issue;
the supported remedy is Codex's `migrate-rollouts`, not Agent Sessions history mutation.

### Claude

Claude TUI, stream-JSON worker, native session registry/socket, transcript, authentication namespace,
permissions, and resume selection are vendor-owned. The daemon adopts the selected UUID/name, verifies
PID/ancestry/profile/cwd/socket evidence, maintains the required synthetic Agent Sessions service
projection, and delivers through the corroborated native socket. No detached Claude lane manager
remains.

### Grok

Grok TUI, leader/observer, ACP sessions, roster, interjection, authentication, and transcript are
vendor-owned. The daemon runs ACP roster/wake coordination as goroutines, adopts exact native identity,
and delivers through supported interjection or MCP calls. There is no `grok-host`, host listener, or
Grok lane-manager process.

### Qwen

Qwen TUI, native daemon/ACP worker, event/input files, archive store, authentication, and transcript
are vendor-owned. The daemon verifies profile/readiness/extension state, dual-output selection,
ancestry, cwd, and native artifacts, then runs event observation and input delivery in process. There
is no `qwen-host`, delivery listener, or Qwen lane-manager process.

## Restart and terminal outcomes

On restart the daemon reopens its one endpoint, recovers durable state, then asks each adapter to
re-corroborate native actors before admission. An accepted message or lane turn is never dispatched a
second time merely because the daemon restarted.

An active turn reconnects when the vendor exposes a supported stable contract. If real native evidence
proves reconnection impossible, the adapter records exactly one explicit `interrupted` terminal
outcome with the native evidence and resumable identity. Silence, an empty result, or a missing child
process is never converted into success.

## Permissions and groups

Adapters translate the existing requested permission mode into the vendor-native mechanism and report
the proven effective mode. They do not create a parallel permission model.

All visibility and routing uses the existing global Agent Sessions groups. Product, profile, instance,
session, host, and test-resource identities are attribution and exact-lifecycle facts only; they do not
create namespaces or access boundaries. See [GROUPS.md](GROUPS.md).

## Cleanup and observability

Cleanup re-attests exact process, native actor, endpoint, filesystem type, owner, and revision
immediately before mutation. Unknown identity becomes bounded debt. Vendor and unrelated paths are
excluded.

Logs, status, doctor, metrics, traces, service output, and crash reports may include bounded identities,
states, counts, timings, revisions, and causes. They must never include peer messages, prompts, lane
results, tool content, credentials, or vendor transcripts, including in debug mode.
