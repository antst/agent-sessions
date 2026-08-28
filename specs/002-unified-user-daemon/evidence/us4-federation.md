# US4 Federation Acceptance Evidence — Complete

## Candidate and scope

T074 is complete for signed runtime candidate
`8afd94f35d46b65f8c09f7662976cea53671303c` (tree
`dd123b49b8b55805c54f6d00ee8e31931c34d346`). Hosted workflow run
`33142267923` passed Linux and macOS normal, race, vet, lint, service-fixture, build, and package
jobs at that exact SHA.

This feature preserves the existing topology: one separately deployed `agent-sessions-hub` and one
embedded host agent in each `agent-sessions` daemon. Groups remain global and are the only
collaboration boundary; visible remote peers remain host-suffixed. There is no host-side
`peer-federator`, lane watcher, namespace, or release/SHA compatibility rule.

The automated three-host acceptance is a real executable one-hub/three-host process topology in
isolated roots, not a claim that three physical machines were reserved for CI. It is executed on
both Linux and macOS, uses the production protocol/router/lane code, and is supplemented by real TCP
binary-pair and real installed-service boundaries. No fixture substitutes SHA equality for protocol
interoperability.

## Acceptance commands

```sh
scripts/federation/test
RACE=1 scripts/federation/test
scripts/federation/test-binary-pairs
scripts/federation/test-installed-hub   # Linux real systemd-user boundary
```

`scripts/federation/test` consumes `go test -json` and rejects a missing, skipped, failed, or extra
root-test inventory. It then starts the canonical hub executable on a real TCP listener, exercises
status/doctor/probe, and runs the independent-root binary-pair cell. The normal Linux path also runs
the real installed systemd-user hub/host lifecycle.

## Closed federation matrix

| Cell | Linux normal/race | macOS normal/race | Exact invariant |
|---|---:|---:|---|
| `hub-cli-runtime` | PASS/PASS | PASS/PASS | unavailable status is read-only; live status/doctor/probe are stable |
| `hub-role-lifecycle` | PASS/PASS | PASS/PASS | distinct hub image/service and shared lifecycle engine |
| `hub-release-preservation` | PASS/PASS | PASS/PASS | immutable stage, rollback, remove, purge, role-disjoint roots |
| `hub-observability` | PASS/PASS | PASS/PASS | closed metadata-only sink inventory and bounded diagnostics |
| `hub-resource-preflight` | PASS/PASS | PASS/PASS | disk/memory/FD/process refusal before durable acceptance; no quota |
| `three-host-peer-routing` | PASS/PASS | PASS/PASS | one hub, three hosts, global groups, host suffixes, idempotent delivery |
| `all-product-remote-lanes` | PASS/PASS | PASS/PASS | four products, exact parent/group/permission, no duplicate work/watcher |
| `host-restart-reconnect` | PASS/PASS | PASS/PASS | same host identity, current generation/capabilities, explicit outage failure |
| `protocol-interoperability` | PASS/PASS | PASS/PASS | protocol-only compatibility and pre-registration mismatch refusal |
| `hub-service-capture` | PASS/PASS | PASS/PASS | systemd/launchd role lifecycle and content canaries |
| canonical executable hub | PASS | PASS | real TCP listener, status, doctor, protocol-3 probe |
| installed hub/host lifecycle | PASS | N/A | real disposable-user systemd manager; macOS service role is covered by real launchd service fixture |
| independent binary pairs | PASS | PASS | unrelated object databases in both directions and actual protocol-4 mismatch |

The hosted run emitted:

```json
{"type":"unified.federation.integration.passed","host_image":"agent-sessions","hub_image":"agent-sessions-hub","logical_protocol_tests":true,"production_host_daemons_started":0,"production_hubs_started":1,"test_owned_hub":true,"hub_status":true,"hub_doctor":true,"hub_probe":true,"protocol_version":3,"second_user_daemon":false}
{"type":"unified.federation.installed_hub.passed","platform":"linux","systemd_user":true,"co_located_roles":true,"independent_host_upgrade":true,"hub_resource_preflight":true,"hub_crash_restart":true,"hub_rollback":true,"hub_removal_reinstall":true,"hub_purge":true,"metadata_only_observability":true}
```

## Unrelated-build and protocol discriminator

The binary-pair runner copies the exact host/hub source contract into two separate directories,
initializes two independent Git object databases, commits different identity markers, proves neither
database contains the other's commit, and builds both canonical images from each. A bounded rerun at
the signed candidate produced independently rooted identities `dcf10d52e1bf` and `0c7ba4d0b11b`:

```json
{"type":"unified.federation.binary_pairs.passed","equal_protocol_forward":{"host_state":"connected","connected_hosts":1,"host_runtime_version":"commit-dcf10d52e1bf","hub_runtime_version":"commit-0c7ba4d0b11b"},"equal_protocol_reverse":{"host_state":"connected","connected_hosts":1,"host_runtime_version":"commit-0c7ba4d0b11b","hub_runtime_version":"commit-dcf10d52e1bf"},"protocol_mismatch":{"host_state":"incompatible","connected_hosts":0,"host_runtime_version":"commit-dcf10d52e1bf","hub_runtime_version":"protocol-4-acceptance"}}
```

The equal-protocol pairs connected in both host/hub directions despite unrelated commit ancestry and
different runtime labels. The actual protocol-4 hub rejected the protocol-3 host before registration,
kept `connected_hosts=0`, and left the host explicitly `incompatible`. SHA, release, build age,
capability list, and upgrade order were never compatibility inputs.

## Installed hub-only lifecycle and co-location

The real systemd-user cell installed host and hub roles under one disposable user and prefix while
selecting different role releases. It proved:

- hub status and doctor expose process, service, build, protocol, listener, connected-host count, and
  bounded routing health without host connector state or content;
- a host-only upgrade replaces only the host process/release and leaves the hub PID/selection live;
- disk, memory, file-descriptor, and process resource failures reject before registration while the
  co-located host remains unchanged;
- an external hub crash is restarted by systemd with a new exact PID;
- readiness failure restores the exact prior hub selection/service without restarting the host;
- normal hub removal preserves the non-secret configuration byte-for-byte and leaves host state,
  selection, and process unchanged;
- reinstall advances the configuration revision while preserving its value and host identity; and
- revision-bound purge deletes every enumerated hub configuration/state/service target and no host,
  vendor, or remote-host target.

Service-manager output, journal, human/JSON status, and human/JSON doctor were checked for normal,
debug, error, crash-report, metric, trace, and resource-error attribution. A private content canary
was absent from every captured surface.

The hosted macOS service fixture invokes the same descriptor-driven lifecycle engine through real
launchd and proves the hub/host role descriptors, KeepAlive, explicit stop, crash restart,
sleep/wake, removal/purge, and content-canary contracts without installing into the owner's Mac.

## Routing, groups, lanes, and restart

The three-host cell publishes distinct host identities and ready capabilities into one hub registry.
Direct sends, explicit multicast, and named-group broadcasts preserve global membership rules and
host-suffixed addresses. Empty, wildcard, nonexistent, sender-nonmember, duplicate, and mismatched
generation requests fail closed before delivery.

The remote-lane cell dispatches Codex, Claude, Grok, and Qwen directly into the destination daemon's
lane authority; it preserves exact parent, group and permission context, commits one idempotent turn,
publishes one content-free result notice, and starts no lane-watch process. Host restart advances only
the connection/runtime generation, republishes the same host identity and current ready products,
and never falls back locally during hub outage.

Independent protocol-preserving host and hub upgrades are covered twice: the protocol test changes
build/runtime labels without changing version 3, and the installed co-located cell changes one role's
selected release/PID while pinning the other role's selection/PID. No restart of the opposite role is
required.

## Preservation and residue

- All network listeners, services, roots, processes, and Git object databases were test-owned.
- Cleanup stopped exact processes, removed the disposable roots, and preserved unrelated role state.
- No credential, peer message, lane result, prompt, or transcript content was read or logged.
- No second host daemon, obsolete host federation agent, remote lane watcher, duplicate delivery, or
  duplicate turn survived any cell.
- Protocol mismatch retained zero connected hosts and an explicit actionable incompatibility state.

## T074 decision

PASS. Hub-only administration and lifecycle, co-located disjoint releases, systemd/launchd service
behavior, content/resource canaries, one-hub/three-host routing, all-product remote lanes, restart,
unrelated-build protocol equality, independent role upgrades, and mismatch refusal are attributable
to the signed candidate.
