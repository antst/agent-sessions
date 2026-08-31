# US4: remote Qwen federation negatives

- Date: 2026-08-24
- Verdict: GREEN
- Current commit: `ef4fd414aff2e214746e21902ca4daaf6e56f536`
- Current tree: `28a7b1386c339083a809973c3db204fe28261d87`
- Current Agent Sessions version/protocol: `0.2.4` / `3`
- Source host: Linux amd64 `linux-qwen-ef4`
- Hub: `10.2.20.132:7520`

Each call below used an explicit host and product `qwen`. Every failing start supplied a unique
`must-not-create` name and input that would have been visible had a target run. The current source
already retained one archived record from the credited macOS-to-Linux positive cell; across every
negative its total Qwen record count remained `1 -> 1`, its active count remained `0 -> 0`, and the
exact current runtime had zero Qwen lane manager/worker processes. No selected target acquired a Qwen
lane record, lane socket, native transcript, or managed process. No alternate host or local lane was
selected.

## Disconnected source agent

A disposable protocol-3 host `neg-disconnected-ef4` ran with no hub configured. Its status was:

```json
{"runtime_version":"0.2.4","protocol_version":3,"host_id":"neg-disconnected-ef4","hub":"","connected":false,"local_peers":0,"remote_peers":0,"remote_hosts":0}
```

A live fixture parent named/sessioned `neg-disconnected-source` was registered only on that host in
explicit group `qwen-fed-negative`. Its real remote Qwen start returned exit `1` with:

```text
peer-federator: hub is disconnected
```

Rows stayed `1 -> 1`, keys `1 -> 1`, and Qwen lane records stayed `0 -> 0` during the call. The
fixture then exited and the agent was terminated only after exact executable and process-start
re-attestation. Final rows, keys, sockets, and processes were zero. Durable source preferences remain
in `/tmp/qwen-fed-neg-disconnected-ef4` as evidence; no target state exists.

## Connected host without Qwen capability

The exact current `peer-federator` binary, SHA-256
`e18f88cdf2ac791133163007a25c4402a8bd4068721ead30927d7269157a7c7e`, joined as
`neg-noncapable-ef4`. It used an isolated `HOME`, `PATH`, runtime, state, and Claude registry so no
native lane launcher was discoverable. Its live advertised capability list was exactly empty. The
source parent `neg-noncapable-source2` was live and explicitly identified to the lane call.

The remote start returned exit `1` with:

```text
peer-federator: remote host neg-noncapable-ef4 does not advertise qwen-lane
```

Source records remained `1 -> 1`, active records `0 -> 0`, target records `0 -> 0`, and exact Qwen
lane processes remained zero. The destination agent was PID `1198224`, process start `1798675979`;
it was TERM-stopped only after those exact values and its executable were rechecked. It left zero
service rows and sockets and disappeared from the hub before the next cell.

One setup attempt is discarded: before isolating `HOME`, the resolver correctly found the owner's
installed launchers under `~/.local/bin` and advertised all four capabilities. No lane call was made
against that misconfigured attempt; it was exact-identity stopped and receives no acceptance credit.

## Intentionally unready Qwen destination

The current agent was started with explicit exact-current `qwen-peer-lane`, native Qwen `0.22.0`,
and fresh isolated `QWEN_HOME`/`QWEN_RUNTIME_DIR` roots containing neither provider configuration nor
the Agent Sessions extension. Startup failed before publication with exit `1` and the specific
readiness evidence:

```text
configured Qwen lane launcher is not ready: qwen lane readiness failed:
parser_dual_output: ... No auth type is selected ...;
integration_probe: qwen readiness file is not a bounded regular file:
/tmp/qwen-fed-neg-unready-ef4/qwen-home/extensions/agent-sessions/plugin.json;
credential_configuration: Qwen provider configuration is not ready
```

The host never appeared on the hub and created zero service rows, agent sockets, or Qwen lane
records. An explicit source start to `neg-unready-ef4` then returned exit `1` with:

```text
peer-federator: remote host "neg-unready-ef4" is not connected to the hub
```

Source records remained `1 -> 1`, active records `0 -> 0`, and exact Qwen lane processes remained
zero. The isolated profile contained no credentials and no owner profile was consulted or mutated.

## Mixed Agent Sessions protocol

The mixed endpoint used the signed `v0.1.1` source commit
`8dd8307de7a29b39f215cfd095da5794adece6d9` (tree
`32d0142733f60e8e1afcb0014d6b17e9f1c94614`) and its locally built protocol-2 federator, SHA-256
`5652d7f0613a31a01fd1fe3839b72db5097c3d25414a1083de2b7280a2ceffbc`. It ran as PID
`1201115`, process start `1798690407`, under isolated runtime and registry roots.

The protocol-2 status remained `connected=false`. Its scoped doctor returned exit `1`,
`hub_reachable=true`, `hub_compatible=false`, and summary:

```text
hub protocol is incompatible
```

The protocol-3 hub rejected every connection before host publication; `neg-mixed-v2-ef4` was never
visible. An explicit current-source start therefore returned exit `1` with:

```text
peer-federator: remote host "neg-mixed-v2-ef4" is not connected to the hub
```

Again source records remained `1 -> 1`, active records `0 -> 0`, mixed-target records remained zero,
and exact Qwen lane processes remained zero. The protocol-2 agent was TERM-stopped after exact PID,
process-start, and executable re-attestation, leaving zero rows and sockets.

## Final cleanup and boundary

The shared fixture parent was PID `1199608`, process start `1798681345`. TERM withdrew its exact
registration and socket. Because the test-only FIFO held a second read/write descriptor, its scanner
remained blocked; one blank line written to that test-owned FIFO woke the cancelled scanner and the
same exact process then exited without KILL. The current Linux agent returned to `local_peers=0`, one
service row, one agent socket, zero active Qwen lanes, and only the already-credited archived positive
record. The hub again listed only `mac-qwen-ef4`.

Credential values or credential-bearing files were never read, copied, printed, logged, diffed, or
hashed. All configured negative roots were disposable and product-specific; no owner daemon,
profile, permission setting, or plugin was changed.
