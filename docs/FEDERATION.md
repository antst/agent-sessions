# Federation: one hub, multiple embedded host agents

Agent Sessions keeps its existing topology: one central `agent-sessions-hub` and multiple host agents.
Each host agent is embedded in that OS user's one `agent-sessions` daemon; no standalone host
federation-agent process or local federation listener remains.

Local discovery and routing work without a hub. Configuring the daemon's one hub address adds
cross-host discovery, messaging, terminal notices, and remote lanes in the same uniform multi-host
space. Existing global groups remain the sole collaboration boundary and peer names remain
host-suffixed. There is no additional namespace, per-product realm, or fallback topology.

## Authoritative protocol inventory

<!-- BEGIN: generated federation protocol inventory -->
- protocol version: 3 (exact equality only; release identity is irrelevant)
- mismatch behavior: reject before registration or work acceptance
- handshake: `hello` -> `hello_ok`; health: `probe` -> `probe_ok`
- bounds: frame 2 MiB; lane input 1 MiB; AgentFrame 1 MiB
- AgentFrame version: 1
- capabilities: `claude-lane`, `codex-lane`, `grok-lane`, `qwen-lane`
- frame types: `hello`, `hello_ok`, `probe`, `probe_ok`, `snapshot`, `roster`, `group_deliver`, `terminal_notice_deliver`, `delivery_ack`, `delivery_error`, `lane_exec`, `lane_accepted`, `lane_cancel`, `lane_cancelled`, `lane_cancel_refused`, `lane_archive`, `lane_archived`, `lane_archive_refused`, `lane_result`, `lane_result_ack`, `lane_result_refused`, `lane_stdout`, `lane_stderr`, `lane_exit`, `lane_error`, `ping`, `pong`
- legacy flat `deliver`: rejected
<!-- END: generated federation protocol inventory -->

The descriptor above is generated from the same authoritative contract as both binaries and is
checked field-for-field against the feature protocol contract.

## Interoperability

Software interoperability is exact protocol-version equality and nothing else. A host and hub built
from unrelated commits, releases, archive generations, or build times interoperate when both declare
protocol `3`. Release version, executable digest, source ancestry, and upgrade order are diagnostic
facts, not handshake inputs.

A different protocol version is rejected before host registration, roster publication, delivery, or
lane acceptance. There is no best-effort downgrade, legacy flat delivery, native carrier fallback, or
automatic lifecycle action.

Capabilities advertise currently available destination operations only. The four tokens are
`codex-lane`, `claude-lane`, `grok-lane`, and `qwen-lane`. A missing token makes that target product
unavailable on the host; it does not hide peers, change global groups, reject the host, or require the
same release.

## Lifecycle independence

Install and operate the hub explicitly:

```sh
make install-hub HUB_LISTEN=:7419
agent-sessions-hub status --json
agent-sessions-hub doctor --json
```

The hub and each host have disjoint immutable releases, current selections, locks, journals, service
definitions, configuration, durable state, readiness, removal, and purge. Host upgrade/restart leaves
the hub process and every other host untouched. Hub upgrade/restart leaves all host daemons running;
protocol-matching hosts reconnect and republish.

The hub owns network roster and relay state only. It owns no vendor integration, local product
adapter, attachment, lane actor, credential, transcript, or host service.

## Handshake and routing

The transport is bounded newline-delimited JSON over the configured TCP connection. The first host
frame is `hello` with protocol version, stable host ID/name, and ready capabilities. Only a valid exact
version receives `hello_ok`. `probe`/`probe_ok` is the bounded health exchange.

The host publishes `snapshot`; the hub returns `roster`. Grouped delivery uses `group_deliver` or
`terminal_notice_deliver` with explicit acknowledgements/errors. The hub validates source/destination
identity and group intersection, and the destination daemon repeats authorization before local native
delivery.

The neutral AgentFrame remains version `1`. Direct send, explicit multicast, named-group broadcast,
provenance, and result semantics are unchanged. Protocol `3` rejects the obsolete flat `deliver`
frame.

## Remote lanes

A managed Codex, Claude, Grok, or Qwen parent may target any ready Codex, Claude, Grok, or Qwen adapter
on another host. The source daemon commits the parent-authorized request and sends `lane_exec`. The
destination embedded agent hands the normalized request directly to its daemon lane engine, preserving
the exact hub-attested parent host/session, global groups, and permission mode. The destination returns
`lane_accepted` only after durable admission. Exact request replay converges on that acceptance without
starting duplicate native work; changed reuse of a request ID is rejected.

`lane_cancel` receives an explicit `lane_cancelled` or `lane_cancel_refused` decision. A transport loss
leaves cancellation pending for retry rather than claiming success or terminal failure. At completion,
the destination sends bounded `lane_result` metadata and waits for `lane_result_ack`; a refusal keeps
the durable terminal outbox pending. Only then does it publish the content-free terminal notice through
the grouped route. The source persists the result before acknowledging it, validates the exact
destination lane and parent target, and exposes the reference through normal lane collection. Accepted
routes and terminal outboxes survive daemon, hub, and connection restart without redispatch.

Remote archive remains destination-owned: `lane_archive` receives `lane_archived` only after native
archive and cleanup succeed. `lane_archive_refused` or a lost acknowledgement is explicit and safe to
retry, and the source proxy never invokes a local product adapter.

There is no remote lane watcher, extra CLI proxy, lane-manager process, SSH transport, direct agent
listener, or local fallback. Hub loss prevents new remote admission and cross-host delivery but does
not terminate an already-started vendor-native lane or local messaging. Durable state reconciles after
the connection returns.

## Security and observability

The current TCP deployment assumes the trusted network boundary documented by the operator. Group and
identity validation still fail closed; a capability is not authorization.

Host and hub logs/status may report bounded IDs, protocol/build versions, capabilities, generations,
counts, timings, frame types, and causes. They never report peer messages, prompts, lane results, tool
content, credentials, or vendor transcripts.

Normal hub removal stops only the exact hub service and preserves hub configuration/durable metadata.
`purge-hub` is the separate offline revision-bound operation. Neither action stops or mutates remote
hosts. See [INSTALL.md](INSTALL.md).
