# US5: Operator-Stopped Split-Runtime Migration Acceptance

## Scope

This acceptance cell exercises the bounded first-migration contracts on a
disposable split-runtime fixture. It does not inspect or mutate an installed
Agent Sessions estate, a vendor profile, or a live user session. The fixture
uses only children, sockets, state, and canary files created beneath the
script-owned `/tmp/asm.*` root.

The accepted migration is deliberately maintenance-window-only. The operator fixture must close every
peer and lane, stop every responsive legacy supervisor, product manager, and federation authority
through the simulated old supported lifecycle, and prevent new legacy launches until installation
completes. The installer fixture must perform no legacy stop, signal, restart, live handoff, or
compatibility drain.

## Command

Run from the repository root:

```sh
scripts/test-unified-migration
```

The script creates a compact private test root, enables the otherwise-skipped fixture discriminator,
requires the named Go test to run and pass exactly once, requires every field of its closed structured
result, and removes the test root on exit.

## Required assertions for renewed acceptance

- The closed legacy inventory is resolved through `LegacyInventorySources`;
  no home-directory or process-name scan is used.
- A live Qwen lane-manager fixture yields its exact lane blocker. The blocked
  gate performs zero lifecycle mutations and leaves a complete test-owned tree
  snapshot unchanged.
- A stale shim's uncorroborated scalar active count does not block migration,
  while a native Qwen path is classified as excluded.
- After the operator closes the fixture blocker but leaves a responsive zero-shim authority running,
  installation fails before mutation, names that authority, and sends it no signal.
- The operator fixture stops the legacy supervisor, product manager, and host federation authority
  through their old supported lifecycle and holds every replacement launch path closed.
- The installer repeatedly verifies all legacy authorities absent and performs no legacy process
  lifecycle action, live handoff, or compatibility drain.
- A replacement legacy launch at any pre-ready checkpoint fails closed and is named without being
  stopped or signalled.
- Agent Sessions-owned session/name/group preferences, completed Qwen lane and
  turn state, collection cursor, terminal notice, hub configuration, and debt
  are staged and atomically adopted. An identical commit retry is idempotent.
- The authoritative `migration/current` selector and
  `CommitFirstMigrationAuthority` commit the exact successor identity and
  generation before legacy endpoints are re-attested and retired.
- Recovery reaches `complete`; exactly one unified daemon socket remains and four exact disposable
  legacy artifacts are retired after successor authority commit.
- A separate pre-ready crash stops the unified candidate, leaves every legacy authority stopped,
  restores only installer-changed release/state/connector/service surfaces, and reports both supported
  next actions: retry unified installation or manually relaunch the old supported lifecycle.
- Four fixture vendor-profile canaries remain byte-identical, the unrelated fixture process remains
  alive, the operator-closed blocker is retired exactly once, and the excluded native vendor candidate
  is never selected for retirement.

## Current Linux fixture evidence

The revised fixture ran successfully on 2026-08-28 from the current shared feature worktree:

```text
--- PASS: TestUnifiedMigrationAcceptance
{"type":"unified.migration.passed","contract_version":1,"fixture_only":true,"blocker_zero_mutation":true,"operator_maintenance_window":true,"replacement_launch_refused":true,"live_handoff":false,"compatibility_drain":false,"installer_legacy_process_actions":0,"operator_authorities_stopped":4,"adoption_commits":1,"legacy_artifacts_retired":4,"recovery":true,"migration_rollback_process_actions":0,"rollback_retry_required":true,"current_daemon_endpoints":1,"vendor_profiles_mutated":0,"unrelated_processes_signalled":0}
```

This is valid fixture evidence for T086. The production migration and shared lifecycle implementation
completed independent T084 review with an **ACCEPTED** verdict. The review confirmed exact bounded
inventory, absence-only retirement, operator-owned legacy shutdown, crash-safe adoption and rollback,
secure release/service transactions, and unchanged fail-closed production process enumeration. After
the acceptance fixture replaced process-table scans with bounded exact-PID reads, eight root/integrator
runs and five additional independent runs completed consecutively without a failure. Installed
Linux/macOS maintenance-window evidence remains part of T034/T094.

The adoption families that are broader than the top-level fixture summary were also run explicitly:

```sh
go test ./internal/daemon -run '^(TestLegacyAdoptionStagesThenAtomicallyCommitsCatalogGroupsNamesHubAndDebt|TestLegacyAdoptionPreservesCompletedLaneCursorNoticeAndArchiveWithoutRedispatch|TestProductionLegacyAdoptsNoStatePendingInboxAndWakeExactlyOnce|TestProductionLegacyWakeOutcomesKeepTerminalMetadataAndDebtWithoutContent|TestAdoptedDispatchClaimRestartBecomesDebtWithoutDuplicate|TestProductionLegacyPreparationAndUncollectedTurnAreMetadataOnlyExactDebt|TestProductionLegacyStoppedConfigurationProjectsClosedKnownValues|TestProductionLegacyDormantHostWithoutConfigurationIsDebtButNoAgentIsClean|TestReflectLegacyAdoptionRequestEmptyRejectsDeliveryMetadata)$' -count=1
```

The package returned `ok`. These named tests cover accepted deliveries and delivery cursors, native
wake outcomes, notices, ambiguous dispatch recovery without duplicate work, preparation journals,
cleanup/service provenance, terminal uncollected work references, stopped host/hub configuration,
dormant-host debt, and rejection of silently omitted delivery metadata. They complement rather than
replace the end-to-end maintenance-window fixture.

An additional read-only Linux discriminator ran from the same dirty source worktree with:

```sh
go run ./cmd/agent-sessions migrate inspect --json
```

Host: Linux 6.17.4-2-pve x86_64, Go 1.26.5 linux/amd64. The inspection named five exact live blockers
(host federation authority, supervisor authority, Qwen host peer, interactive-owner peer record, and
shim peer) plus one retryable malformed-record identity debt. A metadata-only before/after census of
the unified state root—type, path, mode, uid, gid, size, mtime, and inode, without file contents—was
identical. This proves the current source-built inspect is read-only on a real legacy estate; it is not
release evidence because HEAD remained `2e54b62a94b4c309070061222fd38dcb88df778e` with 335 dirty/untracked
worktree entries rather than an independently identified immutable binary.

## Superseded evidence

The previously recorded Linux fixture is superseded because it exercised installer-owned legacy
lifecycle actions and a rollback path that could restore prior legacy authority. Its former passing
output is intentionally removed from this evidence artifact and must not be cited as acceptance.

The historical zero-mutation hashes covered only disposable fixture files, and no credential-bearing or
owner-profile file was read, copied, hashed, or compared. T084/T086 are complete; the required
installed-host release acceptance remains tracked by T034/T094.
