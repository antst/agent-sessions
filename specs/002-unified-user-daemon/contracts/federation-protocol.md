# Federation Protocol Contract

## Purpose and topology

This contract preserves the existing Agent Sessions multi-host behavior while changing process
placement:

- one network has one central `agent-sessions-hub` service;
- every participating host has one embedded federation agent inside its `agent-sessions` daemon;
- groups are global across the hub and host suffixes disambiguate peer addresses;
- the hub owns network roster and relay behavior only; it owns no host adapter, attachment, lane,
  vendor, credential, transcript, or local-service state;
- no standalone host federation-agent process remains.

The host and hub are separate deployment roles built from this repository. They share this wire
contract, not a release lifecycle or source revision.

## Network interoperability boundary

<!-- BEGIN: generated federation protocol inventory -->
- protocol version: 3 (exact equality only; release identity is irrelevant)
- mismatch behavior: reject before registration or work acceptance
- handshake: `hello` -> `hello_ok`; health: `probe` -> `probe_ok`
- bounds: frame 2 MiB; lane input 1 MiB; AgentFrame 1 MiB
- AgentFrame version: 1
- capabilities: `claude-lane`, `codex-lane`, `grok-lane`, `qwen-lane`
- frame types: `hello`, `hello_ok`, `probe`, `probe_ok`, `snapshot`, `roster`, `group_deliver`, `terminal_notice_deliver`, `delivery_ack`, `delivery_error`, `lane_exec`, `lane_cancel`, `lane_stdout`, `lane_stderr`, `lane_exit`, `lane_error`, `ping`, `pong`
- legacy flat `deliver`: rejected
<!-- END: generated federation protocol inventory -->

The repository declares one numeric hub-protocol version for both binaries. The initial unified
implementation declares `3` because this feature does not change the established wire frames. That
number describes the new host/hub wire contract; it does not promise interoperability with any
pre-unification executable or process topology.

Two deployed processes are network-interoperable exactly when both declare the same hub-protocol
version. Release version, commit identity, executable digest, packaging generation, installation time,
service start time, and relative build age are diagnostic facts and MUST NOT participate in the
software-version interoperability decision. Host identity, routing, group, and operation validation
remain separate protocol checks.

Consequently:

- a host and hub built from unrelated repository commits interoperate when their protocol versions
  match;
- upgrading or rolling back one protocol-matching host never installs, restarts, or upgrades the hub;
- upgrading or rolling back the protocol-matching hub never installs, restarts, or upgrades host
  daemons;
- a different protocol version is rejected before host registration, roster publication, delivery, or
  lane acceptance; there is no best-effort downgrade or native fallback.

A deliberate wire-contract change increments the protocol version and requires deployment of a
matching protocol at each connection boundary. A source or binary change that preserves the declared
protocol does not.

## Framing and bounds

The transport remains bounded newline-delimited JSON over the configured TCP connection:

- one JSON object per line;
- maximum encoded frame size: 2 MiB;
- maximum lane input body: 1 MiB;
- maximum neutral AgentFrame content: 1 MiB;
- malformed, oversized, unsupported, or out-of-sequence frames terminate or reject the affected
  connection/request without creating work;
- logs and diagnostics contain bounded identities, frame types, counts, timings, protocol versions,
  and non-secret causes, never peer messages, prompts, lane results, tool content, credentials, or
  vendor transcripts.

## Handshake

The first host frame is the existing hello shape:

```json
{
  "type": "hello",
  "version": 3,
  "host_id": "pdev",
  "host_name": "pdev",
  "capabilities": ["claude-lane", "codex-lane", "grok-lane", "qwen-lane"]
}
```

`host_id` is the simple stable routing identity; `host_name` is its display name. `capabilities`
contains the remote lane products currently ready on that host; the receiver normalizes it to a
sorted, duplicate-free set of known values. The existing capability tokens are exactly:

- `codex-lane`
- `claude-lane`
- `grok-lane`
- `qwen-lane`

Capabilities are operation availability, not a second compatibility or authorization namespace. A
missing capability means only that the corresponding remote lane cannot target that host. It does not
reject the host, hide its peers, alter group membership, or require a hub upgrade. Unknown capability
tokens are ignored until supported by the receiving build.

The hub accepts only a matching version and valid host identity, then returns:

```json
{"type":"hello_ok","version":3}
```

A different version fails before the host is registered. The health probe remains:

```json
{"type":"probe","version":3}
{"type":"probe_ok","version":3}
```

The unified host and hub both implement this exact handshake from their shared repository contract.
Whether some other build also implements version `3` is established by the wire behavior, never by
its age, ancestry, or release label.

## Established frame behavior

After hello, the host publishes `snapshot` frames containing its current live peers. The hub publishes
`roster` frames containing all connected hosts and peers. The existing routed frame families remain:

- grouped peer delivery: `group_deliver`, `terminal_notice_deliver`, `delivery_ack`, and
  `delivery_error`;
- remote lanes: `lane_exec`, `lane_cancel`, `lane_stdout`, `lane_stderr`, `lane_exit`, and
  `lane_error`;
- liveness: `ping` and `pong`.

Protocol version `3` rejects the flat `deliver` frame. Peer snapshots retain the established
group protocol version, exact `host/session` identity, global session identity, product entrypoint,
instance identity, and effective groups. The hub routes within the one global group space and does not
create per-host or per-release namespaces.

The neutral AgentFrame body remains independently versioned at `1`. Its current discover, direct send,
explicit multicast, named-group broadcast, provenance, and result behavior is unchanged.

## Lifecycle and evidence

Host status records the connected hub address, federation protocol version, host build identity,
advertised capabilities, connection generation, and retry state. Hub status/logs record their own
build identity plus connected host IDs and protocol versions without content.

Acceptance MUST prove all of the following:

1. host and hub builds from the same repository revision interoperate when their protocol versions
   match;
2. separately identified host and hub builds from unrelated repository revisions interoperate when
   their protocol versions match;
3. upgrading one host preserves the exact hub PID/start/build identity and every other host
   connection;
4. upgrading the hub preserves every host daemon PID/start/build identity and hosts reconnect;
5. a protocol-mismatch fixture fails before registration or work acceptance;
6. global groups, host-suffixed names, delivery, and all remote-lane products select the same targets
   as the merged baseline.
