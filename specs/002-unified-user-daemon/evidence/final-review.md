# Final Constitution Review — Acceptance Draft

**Review status**: **NOT FINAL — T034, T074, T094, and T095 remain open.**

The implementation is complete in the feature worktree. This draft records the source, fixture, and
installed Linux evidence available before the signed cross-platform release candidate is accepted.
It deliberately does not bind results to the dirty worktree's base commit: final credit requires a
clean signed commit/tree and the real Linux/macOS gates described below.

## Constitution review matrix

| Required review item | Current evidence | Current result | Evidence still required |
|---|---|---|---|
| Shared contracts and one implementation | Authoritative product, binary, protocol, CLI, release, service, state, diagnostics, attachment, delivery, lane, and migration contracts are exercised from shared `internal/` packages. The obsolete `internal/federator` tree, standalone host commands, services, supervisors, shims, product hosts, lane managers, and lane-watch implementation are absent. | Source and fixture **PASS**. | Reconfirm the exact installed process/asset census on macOS and the final signed tree. |
| Exact identity and fail-closed safety | Control, attachment, delivery, lane, release, service, filesystem, migration, and cleanup tests cover recycled/unknown process identity, changed type, nofollow access, ownership, revision CAS, ambiguous replies, and replacement launch. Independent T084 review is **ACCEPTED**. | Source, race, fixture, and independent review **PASS**. | Bind the same rules to final installed Linux/macOS identities. |
| Lifecycle ordering and single host authority | Runtime lifecycle, crash restart, explicit stop, one-listener admission, host install/upgrade rollback, normal removal, purge, and installed nested-systemd acceptance pass. | Linux/source **PASS**. | Real launchd and final exact-commit Linux/macOS service acceptance. |
| Rollback, recovery, and cleanup debt | Release/service/connector transactions restore exact prior selections and enabled/running states; crash-ambiguous operations reobserve before retry; migration rollback leaves unified and legacy authorities stopped and restores only installer-owned surfaces; cleanup debt is durable and retryable. | Source, race, fixture, and independent review **PASS**. | Final installed fault injection on both platforms. |
| Permissions and model-facing authority | Role-scoped local control, exact native ancestry, inherited product permissions, bare-session refusal, parent/group propagation, and absence of daemon admin tools on model-facing MCP surfaces pass. | Fixture **PASS**. | Final real-product cells on Linux and macOS. |
| Existing global groups and host-suffixed peer names | Local and federated discovery, direct send, multicast, broadcast, reply, duplicate admission, group isolation, and host suffix routing use the existing global group contract; no namespace or new visibility boundary exists. | Source and fixture **PASS**. | One-hub/three-real-host Linux/macOS matrix. |
| Canonical CLI/help/environment/JSON/exit contracts | Generated descriptors, instantiated parsers, help, checked `docs/CLI.md`, environment bindings, JSON fields, and numeric exits pass parity gates for both binaries and all host aliases. | Source and packaged-fixture **PASS**. | Inspect final installed host and hub binaries on both platforms. |
| Exactly two binaries and four archives | Release inventory and packaging tests require one `agent-sessions` image, one distinct `agent-sessions-hub` image, the exact alias set, connector payloads, role-specific service assets, manifests, checksums, and no obsolete binaries/assets. All four 0.3.0 archives build. | Source/package **PASS**. | Rebuild and install from the final signed commit. |
| One hub with multiple embedded host agents | The host embeds its outbound agent; the distinct hub contains no host connectors or host lifecycle. Hub-only status/doctor/install/remove/reinstall/purge and co-located disjoint-role lifecycle pass the Linux systemd-user fixture. | Linux/source **PASS**. | Real one-hub/three-host matrix including macOS. |
| Protocol-only interoperability | Independently rooted unrelated commits interoperate in both directions at protocol 3; a protocol-4 build fails before registration. Runtime SHA/release labels are diagnostics only. | Real binary/TCP fixture **PASS**. | Repeat with separately identified final network artifacts and real hosts. |
| Independent host/hub lifecycle | Disjoint roots, selections, locks, journals, services, rollback, removal, reinstall, and purge pass fixture and installed nested-systemd tests; upgrading either role leaves the other role's selection and PID unchanged. | Linux installed fixture **PASS**. | Final macOS lifecycle and real network upgrade cells. |
| No pre-unification interoperability obligation | The first migration requires an operator-held maintenance window, no live handoff or drain, zero installer legacy lifecycle actions, exact absence proof, adoption, and artifact retirement. | Contract, fixture, and independent review **PASS**. | Final installed maintenance-window evidence. |
| No artificial quota | Pre-admission disk, memory, file-descriptor, process, and native-dependency failures retain their real cause. The stress cell passes with 100 attachments, four products, three groups, one listener, and zero duplicate turns. | Source and fixture **PASS**. | Repeat at the signed candidate. |
| Metadata-only observability | Registered normal/debug/error/crash/service/status/doctor/metric/trace sinks pass secret/content canaries. Installed nested-systemd output contains the required output kinds and no private canary. | Linux/source **PASS**. | Real launchd output and final exact-commit rerun. |
| No obsolete Agent Sessions processes | Source and archive inventories reject every obsolete entrypoint and service asset; peer/lane/migration fixtures report zero obsolete runtime processes and one current endpoint/listener. | Source and fixture **PASS**. | Final installed Linux/macOS process, socket, and service censuses. |
| Linux and macOS parity | CI includes Linux/macOS lint, test, race, vet, build, package, and service-fixture gates. The coherent Linux worktree passes normal, race, vet, managed lint, quickstart fixtures, and all four builds. | Linux **PASS**, macOS pending. | Full Darwin S-tier and installed launchd acceptance at the same signed candidate. |

## Current coherent Linux evidence

The current feature worktree passes:

```text
make test
make test-race
go vet ./...
make lint                                  # 0 issues
scripts/test-unified-service
scripts/test-unified-peers
scripts/test-unified-lane-restart
scripts/test-unified-lane-composition
scripts/test-unified-migration
scripts/test-unified-stress
scripts/federation/test
scripts/federation/test-binary-pairs
scripts/federation/test-installed-hub
```

All four `build-release-platform` cells produced 0.3.0 archives for Linux x64/arm64 and Darwin
x64/arm64. The installed hub fixture uses a real nested `systemd --user` manager and proves host/hub
independence, crash restart, resource refusal, rollback, removal/reinstall, purge, and metadata-only
observability. The binary-pair runner builds independent repository histories and proves both equal-
protocol directions plus mismatch refusal. These results remain provisional until rebound to a clean
signed commit.

The unified migration fixture passes after replacing global process-table observation with bounded
exact-PID reads. Independent review ran five consecutive additional passes and returned **ACCEPTED**;
production enumeration remains unchanged and fail closed.

## Remaining final gates

1. Create one clean signed candidate and record its commit, tree, signature, Go/tool versions, and
   four archive identities.
2. Run the complete Darwin normal/race/vet/lint/build and launchd installed-service acceptance at that
   exact candidate.
3. Run the final installed Linux acceptance at the same candidate.
4. Exercise one hub and three real hosts, including Linux/macOS peer and remote-lane traffic,
   independent host/hub restarts and protocol-preserving upgrades, unrelated-commit equal-protocol
   artifacts, and protocol mismatch.
5. Record exact process/service/socket/state baselines, preserved state, failures, cleanup, and residue
   in `final-linux.md`, `final-macos.md`, and `us4-federation.md`.
6. Re-run this review against those attributable artifacts and then mark T034/T074/T094/T095 complete.

## Decision

No known source, fixture, lifecycle, migration, packaging, or Linux gate remains RED. The feature is
not yet release-accepted because the constitution requires real macOS parity and the final real
multi-host matrix. **T095 remains unchecked until those results are bound to the signed candidate.**
