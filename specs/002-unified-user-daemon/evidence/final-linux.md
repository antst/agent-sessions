# Final Linux Acceptance — Signed Runtime Candidate 8afd94f

## Attributable identity

- Commit: `8afd94f35d46b65f8c09f7662976cea53671303c`
- Tree: `dd123b49b8b55805c54f6d00ee8e31931c34d346`
- Parent: `32260d27c4eac0be5fe966f9fce61464ab046165`
- Subject: `Run service acceptance before legacy fixtures`
- Signature: good SSH signature from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Branch: `feature/unified-user-daemon`
- Hosted CI: run `33142267923`, conclusion `success`, head SHA equal to the commit above

The runtime candidate is the signed code commit above. The evidence-only completion commit that adds
this file does not change its Go, shell, service, packaging, or release trees.

## Toolchains and hosts

The independent local Linux gate ran on Linux `6.17.4-2-pve` x86_64 with Go `1.26.5
linux/amd64`, Python `3.12.3`, systemd `255`, and golangci-lint `2.12.2` built with Go
1.26.5. Hosted Linux used the repository workflow's Go `1.25.0` and golangci-lint `2.12.2`.

No credential-bearing owner profile was read, copied, hashed, or compared. Installed-service cells
used disposable user homes, XDG roots, runtime roots, and a real systemd user manager.

## Exact candidate gates

| Gate | Local Linux | Hosted Linux | Attribution |
|---|---:|---:|---|
| `make test` | PASS | PASS | Complete normal suite and live contracts |
| `make test-race` | PASS | PASS | Zero race-detector findings |
| `go vet ./...` | PASS | PASS | Exact candidate |
| `make lint` | PASS, `0 issues` | PASS | golangci-lint on Linux and macOS |
| Real installed host service | PASS | PASS | Real nested/hosted `systemd --user` lifecycle |
| Federation normal and race | PASS | PASS | Protocol, routing, hub, installed-hub, binary-pair cells |
| Four release builds | PASS | PASS | Linux x64/arm64 and Darwin x64/arm64 |
| Package contract | PASS | PASS | Exactly two canonical images per archive |

The real installed Linux service result was:

```json
{"type":"unified.service.passed","platform":"Linux","optional_product_cardinalities":true,"all_four_upgrade_rollback":true,"observability_canary":true,"explicit_stop":true,"crash_restart":true,"removal":true,"purge":true}
```

This one cell covers zero, one, several, and all-four optional-product cardinalities; connector
prepare/commit/rollback; CLI/help/documentation completeness; transactional upgrade failure;
explicit stop; crash restart; normal removal; revision-bound purge; and the real service-manager,
normal, debug, error, crash-report, metric, and trace content canaries required by T034.

## Quickstart and closed functional inventory

The exact normal/race gates reran the quickstart's source, service, peer, lane, federation, migration,
stress, packaging, removal, purge, and observability families. The closed inventory in
`baseline-functional-cells.md` remained the naming authority; no row was deleted or merged merely
because its implementation moved in-process.

| Closed family | Result | Executable attribution |
|---|---:|---|
| S-01 through S-08 | PASS | normal/race/vet/lint, Bash contracts, exact inventory and clean-state gates |
| U-01 through U-12 | PASS | installed service/release/connector transaction and package contracts |
| C-01 through C-18 | PASS | Codex adapter, attachment, delivery, resume, archive and failure contracts |
| CL-01 through CL-11 | PASS | Claude adapter, attachment, delivery, resume and rollback contracts |
| G-01 through G-21 | PASS | Grok ACP/host, identity, policy, delivery and cleanup contracts |
| Q-01 through Q-10 | PASS | Qwen ACP, readiness, delivery, mode and cleanup contracts |
| L-01 through L-30 | PASS | four-product durable lane lifecycle and restart contracts |
| P-C-C through P-Q-Q | PASS, 16/16 | one structured record for every parent/target product cell |
| M-CP-CP through M-QL-QL | PASS | shared AgentFrame direct, multicast, broadcast, reply and lane routing contracts |
| X-01 through X-08 | PASS | one-hub routing, global groups, host suffixes, restart and rejection contracts |
| A-C, A-CL, A-G, A-Q | PASS or product-declared N/A | native archive/resume semantics retained by each adapter |

The runtime-specific summaries were:

```json
{"type":"unified.peers.passed","products":4,"bare_session":true,"group_isolation":true,"daemon_restart":true,"active_peer_upgrade":true,"exact_process_census":true}
{"type":"unified.lane_restart.passed","contract_version":1,"products":4,"active_turn_restarts":4,"continued":["codex"],"evidence_approved_interrupted":["claude","grok","qwen"],"dispatches":4,"redispatches":0,"interrupted_results":3,"collectable_interrupted_results":3,"resumable_interrupted_results":3,"second_user_daemon":false}
{"type":"unified.lane_composition.passed","cells":16,"active_turn_restart":true,"redispatch_count":0,"obsolete_processes":0}
{"type":"unified.migration.passed","contract_version":1,"fixture_only":true,"blocker_zero_mutation":true,"operator_maintenance_window":true,"replacement_launch_refused":true,"live_handoff":false,"compatibility_drain":false,"installer_legacy_process_actions":0,"operator_authorities_stopped":4,"adoption_commits":1,"legacy_artifacts_retired":4,"recovery":true,"migration_rollback_process_actions":0,"rollback_retry_required":true,"current_daemon_endpoints":1,"vendor_profiles_mutated":0,"unrelated_processes_signalled":0}
{"type":"unified.stress.passed","attachments":100,"products":4,"groups":3,"listeners":1,"duplicate_turns":0}
```

The 16-cell composition contract emits and validates one record per named P-cell; the shared routing
tests instantiate the complete 8-by-8 M-cell source/destination grid from the same AgentFrame and
group contract. Product-declared unsupported archive behavior remains explicit `N/A`, never an
inferred pass.

## Release archives

The hosted package artifacts were downloaded from run `33142267923`. Every `.sha256` sidecar
verified, exactly four archives were present, and each archive contained exactly the two executable
basenames `agent-sessions` and `agent-sessions-hub`.

| Archive | SHA-256 |
|---|---|
| `agent-sessions-0.3.0-darwin-arm64.tar.gz` | `fb55ed1ec41301b0a93e5fb8d8111ea4e2bcd2c9c13bf7833203062a11a8e8e9` |
| `agent-sessions-0.3.0-darwin-x64.tar.gz` | `354066e2f47403d158b740a3b2c3ccb62e47359cbaeb246d90bec92cc9e3527f` |
| `agent-sessions-0.3.0-linux-arm64.tar.gz` | `7fff88205d34bd100c9b50179da20c847f4b6e0636fedbcb38efe2c84911777c` |
| `agent-sessions-0.3.0-linux-x64.tar.gz` | `3f1e54b0abc6aa865a9eab412b002cb888b02134870ec439aa0ce64e6b37da97` |

There is no `agent-session-runtime`, `peer-federator`, product peer binary, shim, supervisor,
product host, or lane-manager image in a release archive.

## Preservation, failures, and residue

- The service acceptance ran before legacy bridge fixtures, avoiding a second install against state
  deliberately created later by those fixtures.
- Every installed-service cleanup was rooted under the disposable acceptance user and removed its
  test-owned root. No production host daemon, hub, connector, vendor profile, or credential store was
  selected.
- Migration acceptance performed zero installer legacy-process actions, zero vendor-profile
  mutations, and zero unrelated-process signals.
- Federation, peer, lane, migration, and stress results reported one listener/authority, no second
  user daemon, no obsolete process, no redispatch, and no duplicate turn.
- Earlier candidate failures were retained as diagnostics and fixed by class: BSD-sed portability,
  Darwin path aliases and AF_UNIX budgets, immutable-before-rename ordering, hard-coded platforms,
  ambient managed-session environment contamination, process-helper deadlock, and hosted service
  ordering. None is credited as a pass.

## Linux decision

T034 and the Linux portion of T094 are complete at the signed runtime candidate. Normal, race, vet,
lint, real installed systemd service, package, quickstart, federation, migration, stress, removal,
purge, and preservation gates are green with no attributable residue.
