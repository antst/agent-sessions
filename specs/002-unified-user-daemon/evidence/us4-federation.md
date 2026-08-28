# US4 Federation Acceptance Evidence

## Status

**INCOMPLETE — T074 remains open.**

This record distinguishes repository fixtures from installed multi-host evidence. A passing logical
contract is useful regression evidence, but it is not credited as a real systemd/launchd service, an
unrelated-commit binary pair, a three-machine deployment, or a macOS run.

The worktree under test is based on commit
`2e54b62a94b4c309070061222fd38dcb88df778e` and contains concurrent uncommitted feature work. It is not
a release-candidate identity. The initial Linux run used Go 1.26.5, Python 3.12.3, and
`golangci-lint` 2.12.2 on Linux amd64.

## Acceptance command

```sh
scripts/federation/test
RACE=1 scripts/federation/test
```

The script treats each row below as a closed cell. For every Go cell it consumes `go test -json` and
fails unless the exact named root tests both run and pass without a skip. It then builds the canonical
two-binary inventory and starts one isolated, script-owned `agent-sessions-hub` binary to exercise its
real status, doctor, and protocol-probe surfaces. On Linux normal mode it additionally invokes the
isolated nested-systemd installed-role cell and the independent-root binary-pair cell described below;
race mode retains the in-process service/lifecycle race contracts without starting nested services.

## Historical first-run diagnostic matrix (superseded)

The result columns in this detailed audit preserve the first diagnostic run only. They are not the
current acceptance status and must not be used to classify a cell as pending. The authoritative
current Linux result is the superseding normal/race matrix and the independent binary-pair and
installed-systemd sections below; those results passed every named test plus the executable hub,
doctor, reinstall, purge, and independent-lifecycle boundaries.

| Cell | Authoritative contract exercised by the script | Linux normal | Linux race | macOS | Remaining installed/live evidence |
|---|---|---:|---:|---:|---|
| Hub help and unavailable status are read-only | `TestHubStatusUnavailableDoesNotCreateStateOrStartService` | PASS | Not run | Not run | Installed binary exit and residue census |
| Running hub status and protocol probe | `TestHubServePublishesIndependentStatusAndSupportsProtocolProbe` | PASS | Not run | Not run | Final isolated binary probe did not run because the concurrent two-binary build was RED |
| Running hub doctor | `TestHubStatusAndDoctorHaveStableSharedEnvelopeWithoutHostAuthority` plus the isolated binary doctor probe | Fixture PASS; binary pending | Not run | Not run | Real installed service doctor on both platforms |
| Hub-only role and service identity | `TestHubRoleDescriptorUsesDistinctSelectedBinaryAndSharedServiceEngine` | PASS | Not run | Not run | Installed service census proving no host alias/connector/runtime root |
| Hub install and immutable selection | `TestInstallStagesImmutableReleaseCommitsPointerAndRequiresReadiness` | PASS | Not run | Not run | Real `make install-hub` on Linux and macOS |
| Normal hub removal | `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` and `TestRoleNeutralRemovalPreservesStateAndPurgeIsRevisionBound` | PASS | Not run | Not run | Real `make remove-hub` and process/service residue |
| Hub reinstall restores configuration | The co-located fixture proves initial install and preserved configuration across removal | Partial | Not run | Not run | An explicit post-removal reinstall and restored hub identity/configuration discriminator |
| Revision-bound hub purge | `TestRoleNeutralRemovalPreservesStateAndPurgeIsRevisionBound` | PASS | Not run | Not run | Real offline inspect/apply commands |
| Exact hub deletion and host exclusion | `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` | PASS | Not run | Not run | Installed filesystem inventory before/after purge |
| Preserved non-secret hub configuration | `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` | PASS | Not run | Not run | Byte comparison of a disposable installed configuration |
| Co-located host/hub disjoint roots, selections, locks, and journals | `TestRoleLayoutsAreDisjointAndReleaseIdentityIncludesContent` and `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` | PASS | Not run | Not run | Real co-located installation |
| Different host/hub releases | `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` | PASS | Not run | Not run | Separately built installed images |
| Hub upgrade leaves host selection/service unchanged | `TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval` | PASS | Not run | Not run | Exact live host PID/start/build identity |
| Hub readiness failure rolls back without host mutation | `TestReadinessFailureRestoresExactPriorSelectionAndHookState` and the co-located fixture | PASS | Not run | Not run | Installed fault injection |
| Host generation upgrade preserves stable federation identity | `TestEmbeddedFederationSuccessorKeepsHostIdentityAndPublishesNewRuntimeGeneration` | PASS | Not run | Not run | Exact live hub PID/start/build identity |
| Hub listener injection and wildcard matching | `TestHubInstalledListenerIsStableInjectionSafeAndMatchesBoundWildcard` | PASS | Not run | Not run | Installed service asset and bound listener |
| Hub normal/debug/error/crash outputs exclude content | `TestEveryHubCoreObservabilitySinkUsesMetadataOnlyContentCanary` | PASS | Not run | Not run | Real service outputs on both platforms |
| Hub status human/JSON exclude content | `TestHubStatusAndDoctorHaveStableSharedEnvelopeWithoutHostAuthority` | PASS | Not run | Not run | Real installed status captures |
| Hub doctor human/JSON exclude content and retain remediation | `TestHubStatusAndDoctorHaveStableSharedEnvelopeWithoutHostAuthority` and `TestHubDoctorRetainsCauseSpecificBoundedRemediation` | PASS | Not run | Not run | Real installed failure captures |
| Hub metric and trace exclude content | `TestEveryHubCoreObservabilitySinkUsesMetadataOnlyContentCanary` | PASS | Not run | Not run | Real configured/disabled exporter boundary |
| Hub journal/stdout/stderr capture excludes content | `TestHubServiceManagerCapturedOutputUsesTheSameContentPolicy` and the applicable service-control canary test | PASS | Not run | Not run | Actual systemd journal and launchd files |
| Hub disk failure precedes acceptance | `TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance/disk` | PASS | Not run | Not run | Real constrained-resource fixture |
| Hub memory failure precedes acceptance | `TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance/memory` | PASS | Not run | Not run | Real constrained-resource fixture |
| Hub file-descriptor failure precedes acceptance | `TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance/file_descriptor` | PASS | Not run | Not run | Real constrained-resource fixture |
| Hub process-resource failure precedes acceptance | `TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance/process` | PASS | Not run | Not run | Real constrained-resource fixture |
| Durable commit precedes success and no artificial quota exists | `TestHubAdmissionReportsSuccessOnlyAfterDurableCommit` and `TestHubAdmissionHasNoAgentSessionsQuota` | PASS | Not run | Not run | None beyond final platform rerun |
| Three-host registry and ready capability publication | `TestCapabilityPublicationAndHubRegistryAreProtocolAndGenerationBound` | PASS | Not run | Not run | Three real host daemons |
| Three-host global groups, host suffixes, and duplicate admission | `TestRouterUsesGlobalGroupsHostSuffixesAndIdempotentMessageAdmission` | PASS | Not run | Not run | Cross-machine peer delivery/reply |
| Hub outcome ownership fails closed | `TestHubDeliveryAdmissionAndOutcomeOwnershipFailClosed` | PASS | Not run | Not run | Cross-machine ACK/error transport |
| Codex remote lane dispatch | `TestRemoteLaneDispatchCallsDestinationAuthorityDirectlyForEveryProduct/codex` | PASS | Not run | Not run | Real Codex target and result |
| Claude remote lane dispatch | `TestRemoteLaneDispatchCallsDestinationAuthorityDirectlyForEveryProduct/claude` | PASS | Not run | Not run | Real Claude target and result |
| Grok remote lane dispatch | `TestRemoteLaneDispatchCallsDestinationAuthorityDirectlyForEveryProduct/grok` | PASS | Not run | Not run | Real Grok target and result |
| Qwen remote lane dispatch | `TestRemoteLaneDispatchCallsDestinationAuthorityDirectlyForEveryProduct/qwen` | PASS | Not run | Not run | Real Qwen target and result |
| Remote parent/group/permission propagation | `TestRemoteLaneDispatchPropagatesExactAttestedParentGroupsAndPermission` | PASS | Not run | Not run | Real source-parent inspection |
| Remote lane idempotency and one terminal notice | `TestRemoteLaneDispatchIsIdempotentWithoutDuplicateDestinationWork` and `TestRemoteLaneResultPublishesOneContentFreeParentNotice` | PASS | Not run | Not run | Restarted cross-machine transport |
| No lane-watch process boundary | `TestRemoteLaneDispatchHasNoLaneWatchProcessBoundary` | PASS | Not run | Not run | Final process census after T071 |
| Host reconnect keeps identity and readvertises current products | `TestEmbeddedFederationAdvertisesStableHostIdentityAndOnlyReadyProducts` and `TestEmbeddedFederationRecoversConnectionGenerationAndReadvertisesCurrentReadyProducts` | PASS | Not run | Not run | Independent restart of each real host |
| Hub outage refuses before acceptance without local fallback | `TestFederationHubOutageIsRetryableBeforeAcceptanceWithoutLocalFallback` | PASS | Not run | Not run | Live hub stop/start |
| Sleep/wake reconnect | Applicable service-control sleep/wake test is included on macOS | N/A | N/A | Not run | Real macOS sleep/wake |
| Unrelated SHA/release labels interoperate at equal protocol | `TestArbitrarySHABuildsInteroperateByProtocolVersionOnly` | PASS fixture | Not run | Not run | Actual unrelated-commit host/hub binaries |
| Capabilities gate operations without release coupling | `TestCapabilitiesGateOperationsWithoutCouplingReleases` | PASS | Not run | Not run | Installed missing-product host |
| Protocol-matching host reconnect | `TestHostAgentReconnectsWithTheSameAdvertisement` | PASS | Not run | Not run | Protocol-preserving installed host/hub upgrades |
| Protocol mismatch fails before snapshot/registration/work | `TestProtocolMismatchNamesRequiredVersionBeforeRegistration` and `TestHostAgentRejectsMismatchedHelloBeforeSnapshot` | PASS | Not run | Not run | Actual unequal-protocol binary pair |

## Initial observed Linux run

All ten named logical cells emitted `unified.federation.cell.passed` in normal mode. The run then
stopped before the executable probe because concurrent T071/T072 edits temporarily left the legacy
`internal/federator` package uncompilable (`remoteHostSnapshot`, `normalizeCapabilities`, and
`randomLaneRequestID` were missing). This is a genuine overall RED, so the normal column above credits
only the individually completed Go cells and not the full command.

## Superseding complete Linux normal/race result

After T071/T072 restored the shared build, both exact commands completed successfully:

```text
scripts/federation/test
RACE=1 scripts/federation/test
```

| Closed script cell | Normal | Race |
|---|---:|---:|
| `hub-cli-runtime` | PASS | PASS |
| `hub-role-lifecycle` | PASS | PASS |
| `hub-release-preservation` | PASS | PASS |
| `hub-observability` | PASS | PASS |
| `hub-resource-preflight` | PASS | PASS |
| `three-host-peer-routing` | PASS | PASS |
| `all-product-remote-lanes` | PASS | PASS |
| `host-restart-reconnect` | PASS | PASS |
| `protocol-interoperability` | PASS | PASS |
| `hub-service-capture` | PASS | PASS |
| Fresh canonical two-binary build | PASS | PASS |
| Isolated canonical hub status | PASS | PASS |
| Isolated canonical hub doctor | PASS | PASS |
| Isolated protocol-3 probe | PASS | PASS |

The executable discriminator reported one script-owned hub, zero production host daemons, no second
user daemon, healthy status/doctor, and `probe_ok` at protocol 3. The race run produced no race report.

The complete logical Linux fixture is green. Installed Linux and independent binary-pair evidence is
recorded separately below. macOS and the one-hub/three-real-host matrix remain uncredited, so T074
remains unchecked.

## Independent-root binary-pair result

`scripts/federation/test-binary-pairs` created two disposable Git repositories from the exact current
functional source, added a different non-runtime marker to each tree, and made one root commit in each
repository. The script verified that neither object database could resolve the other repository's
commit before building both canonical host and hub binaries. It then exercised both cross-pair
directions. A third hub was compiled from the first disposable source with the single authoritative
protocol constant changed from 3 to 4.

The completed Linux amd64 run reported:

```json
{"type":"unified.federation.binary_pairs.passed","equal_protocol_forward":{"host_state":"connected","connected_hosts":1,"host_runtime_version":"commit-cfefe318df82","hub_runtime_version":"commit-eb08efbc34e5"},"equal_protocol_reverse":{"host_state":"connected","connected_hosts":1,"host_runtime_version":"commit-eb08efbc34e5","hub_runtime_version":"commit-cfefe318df82"},"protocol_mismatch":{"host_state":"incompatible","connected_hosts":0,"host_runtime_version":"commit-cfefe318df82","hub_runtime_version":"protocol-4-acceptance"}}
```

This credits actual independently built binaries and a real TCP handshake. It does not claim that
either disposable commit is a historical release commit or that this local pair is the required
three-machine deployment.

## Installed Linux systemd-user result

`scripts/federation/test-installed-hub` ran under a disposable D-Bus session and a real nested
`systemd --user` manager. `HOME`, every XDG root, and `TMPDIR` were distinct owner-only directories
under one test-owned root, so the same-UID host's real legacy runtime inventory was outside the cell.
The runner used the supported immutable host and hub lifecycle transactions and reported:

```json
{"type":"unified.federation.installed_hub.passed","platform":"linux","systemd_user":true,"co_located_roles":true,"independent_host_upgrade":true,"hub_resource_preflight":true,"hub_crash_restart":true,"hub_rollback":true,"hub_removal_reinstall":true,"hub_purge":true,"metadata_only_observability":true}
```

The completed cell proves:

- co-located host and hub services selected disjoint role roots and content-distinct immutable
  releases; a host-only upgrade changed the host release and exact PID without changing the hub
  selection or PID;
- successive protocol-preserving hub candidates did not restart or repoint the host;
- four acceptance-only hub images returned real `ENOSPC`, `ENOMEM`, `EMFILE`, and `EAGAIN` causes
  through the production pre-admission hook of a systemd-owned TCP listener; every request was
  refused before registry commit and `connected_hosts` remained zero;
- an exact hub `SIGKILL` was restarted by systemd with a different PID and emitted a bounded crash
  recovery record;
- a runtime-version readiness mismatch rolled the hub back to its exact prior selection and running
  version without changing the host PID;
- normal hub removal preserved the complete non-secret configuration record byte-for-byte and left
  the host untouched; reinstall preserved its semantic value while advancing the statestore CAS
  revision, as required by the durable record contract;
- revision-bound purge removed the hub configuration root, state root, and systemd definition while
  the host configuration/state, selection, and exact PID survived; and
- captured systemd journal/manager output plus human and JSON status/doctor contained all six
  declared output kinds (`normal`, `debug`, `error`, `crash_report`, `metric`, `trace`), all four
  cause-specific error codes, and no injected private-content canary.

The outer cleanup found no retained acceptance root or process referring to one. This is one real
Linux user-manager cell on one host; it does not credit launchd, sleep/wake, three physical hosts, or
real vendor-backed remote-lane turns.
