# Final Constitution Review — Unified User Daemon

**Runtime candidate**: signed commit
`8afd94f35d46b65f8c09f7662976cea53671303c`, tree
`dd123b49b8b55805c54f6d00ee8e31931c34d346`.

**Review status**: **PASS**. T034, T074, T094, and T095 have attributable Linux/macOS evidence.
No constitution exception is used.

## Review matrix

| Required review item | Evidence and conclusion |
|---|---|
| Shared contracts, one implementation | PASS. One `agent-sessions` host composition root owns attachments, delivery, lanes, embedded federation, recovery, and diagnostics. Logical packages provide shared state, service, release, protocol, groups, routing, identity, and diagnostics. Product/role files contain only genuine native/host/hub differences. |
| Exact identity and fail-closed safety | PASS. Kernel peer credentials, PID plus process start, native session/profile/ancestry, socket/file type, owner, revision, generation, and request digest are corroborated at the mutation boundary. Unknown, recycled, stale, malformed, changed-type, or conflicting evidence refuses without collateral action. |
| Root-cause and class-closing fixes | PASS. The feature removes the independently versioned supervisor/shim/product-host/lane-manager/host-federator authority class. Acceptance also closed observed portability classes: BSD tools, Darwin aliases and socket budgets, immutable rename ordering, platform literals, ambient managed-session variables, deadlocked helpers, Darwin lint coverage, and service-test ordering. Failed/confounded evidence is not credited. |
| Lifecycle ordering | PASS. Durable admission precedes publication; generation startup is exclusive; install validates and stages before transition; service restart/crash/explicit-stop semantics are platform-owned; peer/lane workflows never manage daemon lifetime. |
| Rollback and cleanup debt | PASS. Release, connector, service, lane, delivery, and migration transactions use revision/identity checks. Ambiguous cleanup retains retryable debt. Upgrade rollback restores exact prior installer-owned surfaces; first-migration rollback leaves legacy and unified authorities stopped. |
| Permissions and administration | PASS. The owning OS user is the administrative boundary. Model-facing MCP/peer/lane roles expose only attested operation surfaces. Native permission, cancellation, transcript, archive, and resume semantics remain product-owned. |
| Groups and addressing | PASS. Existing global groups are the only collaboration boundary in one uniform multi-host space. Host suffixes disambiguate names. No profile/test/product namespace or ambient roster was added. |
| Canonical CLI/help/environment/JSON/exits | PASS. One descriptor inventory drives both binaries, host aliases, parser/options, help, environment inputs, JSON schemas, semantic exit classes, and checked `docs/CLI.md`. Every parsed option is documented and model-facing tools contain no admin modes. |
| Exactly two binaries | PASS. `cmd/` contains only `agent-sessions` and `agent-sessions-hub`; all host aliases resolve to the first image. Four 0.3.0 archives contain exactly those two executable basenames and validated payloads/checksums. No obsolete command/service asset is shipped. |
| One-hub topology | PASS. One central hub serves multiple embedded host agents. The hub is a separate deployment role and never a host authority. Global groups, AgentFrame, host suffixes, direct/multicast/broadcast, and remote lanes preserve existing semantics. |
| Network interoperability | PASS. Exact hub protocol equality is the sole software-version input. Independent Git object databases interoperate in both host/hub directions at protocol 3; actual protocol 4 fails before registration. SHA, release, binary identity, build age, capabilities, and upgrade order are diagnostic only. |
| Independent host/hub lifecycle | PASS. Co-located roles use disjoint release roots, selections, locks, journals, services, rollback, removal, and purge. Upgrading, crashing, rolling back, removing, reinstalling, or purging one role leaves the other's PID, selection, readiness, and state unchanged. |
| No pre-unification compatibility obligation | PASS. First migration is an operator maintenance window: all peers/lanes close and old authorities stop through their own supported lifecycle. The installer only inventories, verifies absence, adopts durable metadata, and retires exact artifacts. It implements no live handoff, drain, signalling, or legacy restart. |
| No artificial quotas | PASS. The daemon has no Agent Sessions count limits. One hundred simultaneous attachments across four products and three groups pass with one listener and zero duplicate turns. Disk, memory, FD, process, and native-dependency failures retain their real pre-acceptance causes. |
| Metadata-only observability | PASS. Closed normal/debug/error/crash/service/status/doctor/metric/trace sink manifests use bounded metadata envelopes. Content/secret canaries pass in daemon, hub, systemd, and launchd cells. No message, prompt, result, transcript, or credential content is an operational field. |
| Zero obsolete processes | PASS. Source, package, runtime census, lane composition, restart, migration, and installed-service gates find no supervisor, per-session shim, product host, lane manager, lane watcher, or host federation-agent authority. Vendor-required children are stateless and daemon-generation-owned. |
| Linux/macOS parity | PASS. Exact hosted candidate jobs are green for normal, race, vet, lint, real systemd/launchd service fixtures, both architectures, and package contracts. Independent Linux and physical Darwin gates corroborate runtime behavior. Toolchains, identity, preservation, and residue are in `final-linux.md` and `final-macos.md`. |

## Evidence index

- `final-linux.md`: exact Linux identity/toolchain, normal/race/vet/lint, real systemd service,
  quickstart/baseline families, archive hashes, preservation, and residue.
- `final-macos.md`: exact hosted real-launchd and physical-Darwin evidence, owner nonmutation, and the
  disclosed race timing flake.
- `us4-federation.md`: hub lifecycle, three-host routing, remote lanes, independent builds/upgrades,
  protocol mismatch, content/resource canaries, and residue.
- `us5-migration.md`: operator maintenance window, exact blockers, zero installer legacy lifecycle
  action, adoption, artifact retirement, rollback, and collateral exclusion.
- `us3-lanes.md`: four-product lane ownership, restart, composition, collection, resume, and cleanup.
- `baseline-runtime-inventory.md` and `baseline-functional-cells.md`: closed pre-change topology and
  individually named functional cells used as the acceptance naming authority.

## Exact release and CI decision

GitHub Actions run `33142267923` at the exact runtime candidate concluded `success`. Linux and macOS
normal, race, vet, lint, service-fixture, four build, inventory, and package-contract jobs passed.
Release jobs were correctly skipped because the workflow's publication stage is main-only; no tag or
release was created by feature acceptance.

Downloaded hosted artifacts provided exactly four archives. Every sidecar verified and every archive
contained exactly `agent-sessions` plus `agent-sessions-hub`. The authoritative hashes are recorded in
`final-linux.md`.

## Known observation

The independent physical Mac saw two non-deterministic full-suite bridge timing failures across seven
race attempts, with zero race-detector reports. Five targeted/full-package/full-suite retries passed,
including one exact-candidate full race run, and the exact hosted macOS race job passed. This evidence
is disclosed rather than credited. No deterministic product-state failure was reproduced, and no
timeout/assertion was weakened. It remains a reliability observation outside the accepted invariant,
not a hidden constitution exception.

## Final decision

The signed runtime candidate satisfies the feature specification, plan, all 95 tasks, and every
applicable constitution principle. It ships one user-managed host daemon plus one independently
versioned central-hub binary, preserves existing product/group/federation semantics, requires only
protocol equality across the network, and leaves no pre-unification runtime authority in the shipped
or accepted topology.
