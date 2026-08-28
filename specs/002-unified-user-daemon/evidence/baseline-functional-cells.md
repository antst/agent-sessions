# Pre-unification functional-cell inventory

This is the closed, individually addressable functional baseline frozen by T001 at `develop` commit
`c056fbc5015d4ab0a673f66cac5404206f7bcee6`. The source authority is
[`docs/ACCEPTANCE-MATRIX.md`](../../../docs/ACCEPTANCE-MATRIX.md); the executable source/fake-protocol
aggregate is [`scripts/test`](../../../scripts/test), and installed/live cells use the product contract
scripts and literal commands named below.

Each ID is one reportable cell. A family command runs only the applicable mechanics: final acceptance
must report every ID separately, distinguish `PASS`, `N/A`, `BLOCKED`, and `RED`, and may not infer an
individual pass from an aggregate exit code. Unless a row says otherwise, its expected result is the
same assertion stated for that ID in the acceptance matrix, with exact identities, destination-visible
receipt where messaging is involved, and exact owned cleanup. Process/service topology assertions are
the only assertions expected to change under this feature.

## Commands that bind this inventory to executable evidence

| Evidence family | Existing exact command or live surface |
|---|---|
| Source/fake protocol | `go clean -testcache && ./scripts/test`; repeat as `RACE=1 ./scripts/test`; `go vet ./...`; `make lint`; four `make build-release-platform` calls from the acceptance matrix |
| Claude contract | [`scripts/test-claude-contract`](../../../scripts/test-claude-contract) plus installed `claude`/`claude-peer` TUIs and `agent_sessions` MCP calls |
| Qwen interactive | [`scripts/test-unified-peers`](../../../scripts/test-unified-peers), the Qwen daemon attachment tests, and installed `qwen`/`qwen-peer` TUIs. Q-01 through Q-10 below remain individually reportable; the unified runner's aggregate result is not evidence for an unnamed cell. |
| Qwen lane | [`scripts/test-unified-lane-restart`](../../../scripts/test-unified-lane-restart), the Qwen daemon lane lifecycle tests, and [`scripts/test-unified-lane-composition`](../../../scripts/test-unified-lane-composition). L-01 through L-30 below remain individually reportable wherever Qwen is applicable. |
| 4x4 lane composition | [`scripts/test-unified-lane-composition`](../../../scripts/test-unified-lane-composition), which parses and validates one structured record for each of the 16 P-* cells below; [`scripts/test-unified-lane-restart`](../../../scripts/test-unified-lane-restart) separately binds each target's restart discriminator. |
| Federation | [`scripts/federation/test`](../../../scripts/federation/test), [`scripts/federation/integration_test.py`](../../../scripts/federation/integration_test.py), and installed multi-host peers/lanes |
| Codex peer | Installed `codex-peer [OPTIONS] [resume TARGET]`, native `codex`, App Server control, and attested `agent_sessions` MCP calls |
| Grok peer | Installed `grok-peer [OPTIONS] [resume TARGET]`, native `grok`, ACP/leader observations, and attested `agent_sessions` MCP calls |
| Lanes | Installed `<product>-peer-lane doctor|run|start|resume|wait|status|list|interrupt|archive` and attested `agent_sessions.lane` calls |
| Install/release | `make install-all`, product-specific install/remove targets, same-revision reinstall, and archive install through the Makefile/release scripts |

## Source and packaging cells

| ID | Individually named cell |
|---|---|
| S-01 | Normal, race, vet, lint, and shell-integration gate |
| S-02 | Stock macOS Bash 3.2 shell gate |
| S-03 | Four-archive complete binary/plugin inventory |
| S-04 | Plugin validation no-GUI/no-unvalidated-product safety |
| S-05 | PID-reuse, process-start, stale artifact, duplicate-name, and provisional-cleanup regressions |
| S-06 | Obsolete canonical-name absence |
| S-07 | Clean checkout and untouched user-state gate |
| S-08 | Oldest/current Codex launcher argv passthrough drift |

## Install and upgrade cells

| ID | Individually named cell |
|---|---|
| U-01 | Clean exact-revision install on Linux and macOS |
| U-02 | Same-revision idempotent reinstall and plugin-data preservation |
| U-03 | Live Codex App Server/Grok peer upgrade refusal |
| U-04 | Unrelated federator/peer preservation during install |
| U-05 | Codex trust prompt surfaced without automation approval |
| U-06 | Explicit Grok trust and validate-before-replace |
| U-07 | macOS desktop same-name CLI rejection |
| U-08 | Plugin entrypoint/runtime revision equality |
| U-09 | No-Go archive installation and checksum match |
| U-10 | Malformed Grok launch inventory fail-closed diagnostic |
| U-11 | Exact-profile Qwen install/upgrade/remove and owner-state preservation |
| U-12 | Eleven-binary/four-payload prebuilt install and reproducible build |

## Interactive Codex peer cells

The live surface is `codex-peer`, native `codex`, App Server state, and attested MCP.

| ID | Individually named cell |
|---|---|
| C-01 | Fresh named zero-turn composer and normal exit |
| C-02 | Real prompt/output and one unarchived UUID after quit |
| C-03 | Exact-UUID and durable-name resume after native title change |
| C-04 | Global flag placement argv equivalence |
| C-05 | Native model/sandbox/approval/config/display/image/`--` passthrough |
| C-06 | Repeated permission boolean native last-value semantics |
| C-07 | Renamed-project canonical-cwd resume |
| C-08 | Stale loaded zero-turn takeover without duplication |
| C-09 | Normal quit without archive/unarchive and zero shim convergence |
| C-10 | Attested identity/roster/rename/inbox/send tools |
| C-11 | Wrong identity/token/ancestry fail closed |
| C-12 | Idle wake, busy steer/queue, ordered exact-once burst |
| C-13 | Destination acknowledgement with exact sender attribution |
| C-14 | Plain/YOLO launch/resume agreement |
| C-15 | Sticky resumed YOLO and never-YOLO control |
| C-16 | Quit/interrupt/SIGTERM/supervisor/App Server recovery and cleanup |
| C-17 | Idempotent archive and one-unarchive transcript-preserving resume |
| C-18 | Missing socket/stale owner/PID reuse/interrupted publication isolation |

## Interactive Claude peer cells

| ID | Individually named cell |
|---|---|
| CL-01 | Bare Claude opt-out |
| CL-02 | Real shared-profile TUI and single host-service row |
| CL-03 | Two distinct managed sockets and group-filtered discovery |
| CL-04 | Ordinary/peer resume, late native selection, exact title/UUID and no provisional residue |
| CL-05 | Name/status refresh and durable permission classification |
| CL-06 | Host-agent restart re-registration without Claude restart |
| CL-07 | Exit/crash/stale-row/PID/socket cleanup isolation |
| CL-08 | Structured MCP group operations, reply path, and exact attestation failures |
| CL-09 | Claude parent to all four lane products |
| CL-10 | Profile/live-attachment/settings/startup failure rollback journal |
| CL-11 | Chrome-discovery cache and managed `--no-chrome` behavior |

## Interactive Grok peer cells

| ID | Individually named cell |
|---|---|
| G-01 | Grok Build selection over Chat Grok/fallback |
| G-02 | macOS app-bundle direct/symlink/case rejection |
| G-03 | Explicit executable override validation |
| G-04 | Genuine cold usable TUI with MCP servers |
| G-05 | Cold log ownership cleanliness |
| G-06 | TUI/host/leader/observer/MCP launch identity |
| G-07 | Observer initialize/authenticate without load/prompt |
| G-08 | Ordinary/peer auth-config equivalence and attributed foreign MCP failure |
| G-09 | Idle interjection visible turn and acknowledgement |
| G-10 | Busy interjection exact-once without actor replacement |
| G-11 | Dedup/reuse/reconnect/rejection/echo/EOF never-replay contract |
| G-12 | Restart queued recovery without ambiguous in-flight replay |
| G-13 | UUID/title/picker resume and authoritative alias promotion |
| G-14 | Permission/policy convergence across every projection |
| G-15 | Permission publication retry and retryable-busy refresh |
| G-16 | Process-group isolation and complete normal-exit cleanup |
| G-17 | Run-owned shared-leader list/kill safety |
| G-18 | Owner/host/leader/auth/roster/PID/stale-record failure isolation |
| G-19 | Agent Sessions readiness gating without recursive self-rejection |
| G-20 | Deferred roster non-blocking turn and permission snapshot retirement |
| G-21 | Roster/reconciliation status projection and stale-generation withdrawal |

## Interactive Qwen peer cells

| ID | Individually named cell |
|---|---|
| Q-01 | Bare Qwen opt-out and cold managed TUI |
| Q-02 | Session-free readiness without transcript or secret read |
| Q-03 | Fresh/exact resume and pre-mutation unsupported-selector refusal |
| Q-04 | Native-default/default/YOLO/approval-mode permission contract |
| Q-05 | Dual-output exact admission and malformed/auth/path failure closure |
| Q-06 | Real `0600` stable socket and direct native delivery |
| Q-07 | Attested MCP discovery/send/reply/multicast/broadcast/dedup and negative cases |
| Q-08 | Host-agent restart re-publication without Qwen restart/duplication |
| Q-09 | Exit/crash/publication/PID/path/legacy cleanup and retryable ambiguity |
| Q-10 | Ordinary-managed-ordinary use without profile/transcript migration |

## Shared lane lifecycle cells

Each L row is run for Codex, Claude, Grok, and Qwen targets wherever applicable, locally and remotely,
with each of the four peer products as parent. Product-specific `N/A` remains explicit.

| ID | Individually named cell |
|---|---|
| L-01 | Doctor readiness/authentication/contract major |
| L-02 | Foreground real turn and coherent JSONL final answer |
| L-03 | Start/ready, busy status, one matching wait collection |
| L-04 | Non-mutating wait timeout then later collection |
| L-05 | Idle and busy inbound peer-message behavior |
| L-06 | Owner reply and all peer/lane destinations |
| L-07 | Exact-identity resume and owed-work refusal |
| L-08 | Interrupt raw/normalized/exit/collection/notice contract |
| L-09 | Execution timeout 124 and non-mutating collection timeout |
| L-10 | Idempotent archive, discovery removal, transcript/notice result |
| L-11 | One-unarchive exact resume with transcript preservation |
| L-12 | Auto-archive deadline/cancel/grace/disabled behavior |
| L-13 | Duplicate-name and invalid owner/token/product provisional cleanup |
| L-14 | Dirty user and archived lane worktree preservation |
| L-15 | Owner-exit behavior for parent-owned, persistent and unrelated lanes |
| L-16 | Worker/manager/supervisor crash terminal/cursor/notice/cleanup recovery |
| L-17 | Exact-owner permission inheritance and explicit override |
| L-18 | Remote lifecycle streams/exit/cwd/notify/cleanup fuse |
| L-19 | Remote stdin/prompt-file/hub-loss/capability failures |
| L-20 | Grok stable/native identity and sole ACP driver |
| L-21 | Grok serialized idle/busy messages and dedup/conflict |
| L-22 | Grok explicit always-approve/bypass headless policy |
| L-23 | Grok lane skill/preflight/contract/package inventory |
| L-24 | Grok status/list and terminal result schema |
| L-25 | Grok archive/crash descendant cleanup on Linux/macOS |
| L-26 | Pre-rollout failed Codex lane exact archive safety |
| L-27 | Qwen stable/native identity and sole ACP client |
| L-28 | Qwen bounded archive/unarchive helper and zero residue |
| L-29 | Qwen crash/PID/restart/cleanup-debt convergence |
| L-30 | Qwen launch/initial/current mode separation |

## Parent-product x target-product composition cells

[`scripts/test-unified-lane-composition`](../../../scripts/test-unified-lane-composition) is the
daemon-backed 4x4 harness. It emits and validates one `unified.lane_composition.cell` record for each
P-* cell, including exact parent attachment/session, lane/turn, native PID/process-start, generation,
dispatch/reconnect/redispatch, collection, resume, and evidence fields. The closed acceptance report
must still name separately, for every P-* cell, the real target turn and unique marker in the target's
native store; child private group, immediate parent anchor, default non-inheritance, explicit
`--inherit-groups`, exact resume, parent/child messaging, terminal collection/notice, interrupt,
archive, parent exit, persistence, and target-owned cleanup. A runner summary alone remains
insufficient. [`scripts/test-unified-lane-restart`](../../../scripts/test-unified-lane-restart) binds
the no-redispatch restart outcome for each target product without creating a second user daemon.

| Parent → target | Codex | Claude | Grok | Qwen |
|---|---|---|---|---|
| Codex | P-C-C: Codex→Codex | P-C-CL: Codex→Claude | P-C-G: Codex→Grok | P-C-Q: Codex→Qwen |
| Claude | P-CL-C: Claude→Codex | P-CL-CL: Claude→Claude | P-CL-G: Claude→Grok | P-CL-Q: Claude→Qwen |
| Grok | P-G-C: Grok→Codex | P-G-CL: Grok→Claude | P-G-G: Grok→Grok | P-G-Q: Grok→Qwen |
| Qwen | P-Q-C: Qwen→Codex | P-Q-CL: Qwen→Claude | P-Q-G: Qwen→Grok | P-Q-Q: Qwen→Qwen |

## Pairwise messaging cells

Each cell below sends a unique marker while the destination is idle and busy and requires the exact
return marker plus sender name, UUID, host and permission label. Each cell repeats same-host,
Linux→macOS, macOS→Linux and Linux→Linux where applicable, then offline/resumed delivery and
federator reconnect. An enqueue-only result is `RED`.

| Source \ destination | Codex peer | Claude peer | Grok peer | Qwen peer | Codex lane | Claude lane | Grok lane | Qwen lane |
|---|---|---|---|---|---|---|---|---|
| Codex peer | M-CP-CP | M-CP-CLP | M-CP-GP | M-CP-QP | M-CP-CL | M-CP-CLL | M-CP-GL | M-CP-QL |
| Claude peer | M-CLP-CP | M-CLP-CLP | M-CLP-GP | M-CLP-QP | M-CLP-CL | M-CLP-CLL | M-CLP-GL | M-CLP-QL |
| Grok peer | M-GP-CP | M-GP-CLP | M-GP-GP | M-GP-QP | M-GP-CL | M-GP-CLL | M-GP-GL | M-GP-QL |
| Qwen peer | M-QP-CP | M-QP-CLP | M-QP-GP | M-QP-QP | M-QP-CL | M-QP-CLL | M-QP-GL | M-QP-QL |
| Codex lane | M-CL-CP | M-CL-CLP | M-CL-GP | M-CL-QP | M-CL-CL | M-CL-CLL | M-CL-GL | M-CL-QL |
| Claude lane | M-CLL-CP | M-CLL-CLP | M-CLL-GP | M-CLL-QP | M-CLL-CL | M-CLL-CLL | M-CLL-GL | M-CLL-QL |
| Grok lane | M-GL-CP | M-GL-CLP | M-GL-GP | M-GL-QP | M-GL-CL | M-GL-CLL | M-GL-GL | M-GL-QL |
| Qwen lane | M-QL-CP | M-QL-CLP | M-QL-GP | M-QL-QP | M-QL-CL | M-QL-CLL | M-QL-GL | M-QL-QL |

## Federation, groups and host-suffix cells

| ID | Individually named cell |
|---|---|
| X-01 | Local-only one-service-row grouped direct/multicast/broadcast |
| X-02 | Same-name ambiguity, disjoint-group independence and hidden-identity non-leakage |
| X-03 | Linux/macOS/Linux pairwise messaging and lane lifecycle without shadows |
| X-04 | Exact snapshot identity/private anchor and last-valid-roster retention |
| X-05 | Hub/agent restart without duplicate or peer restart |
| X-06 | Legacy/duplicate/invalid/group-nonmember rejection before delivery |
| X-07 | Remote private parent anchor and explicit-only parent group inheritance |
| X-08 | Install/test preservation of unrelated host agent and hub |

These eight rows are the closed global-group and host-suffix baseline: groups are global, are the sole
visibility/access boundary, and do not introduce per-product/profile/test namespaces. Visible remote
names remain `<peer>--<host>`. One hub serves multiple host agents.

## Archive, collection, permission, resume and cleanup closure

The following rows name the four product archive declarations that the acceptance matrix requires in
addition to the lane rows. Every row checks normal quit, explicit/repeated archive, message to archived
target, resume-triggered unarchive, repeated resume, same-name reuse, and remote propagation; unsupported
operations are explicit `N/A`, never an inferred pass.

| ID | Product cell | Expected result |
|---|---|---|
| A-C | Codex peer archive/unarchive | Native/bridge contract from C-09 and C-17, exact counts and transcript retention |
| A-CL | Claude peer archive/unarchive | Native Claude contract or explicit `N/A`, with no Agent Sessions-owned false archive |
| A-G | Grok peer archive/unarchive | Native Grok resume/visibility behavior from G-13 and exact cleanup |
| A-Q | Qwen peer archive/unarchive | Native Qwen archive/resume behavior from Q-03/Q-10 and exact cleanup |

Cross-cutting closure is therefore explicit rather than implied: permissions are C-06/C-11/C-14/C-15,
CL-05/CL-08/CL-10, G-14/G-15/G-20, Q-04/Q-05/Q-07, L-08/L-17/L-22/L-30 and every P cell;
resume is C-03/C-07/C-08/C-14/C-15/C-17, CL-04/CL-10, G-13, Q-03/Q-10, L-07/L-11/L-18/L-20/L-27,
every P cell and A-C/A-CL/A-G/A-Q; collection is L-03 through L-11, L-16, L-18, L-21, L-24 and every P
cell; cleanup is S-05/S-07, U-04/U-11, C-08/C-09/C-16/C-18, CL-07/CL-10, G-11/G-12/G-16/G-18,
Q-05/Q-09, L-10 through L-16, L-18/L-25/L-26/L-28/L-29, X-08, every P cell and every M cell.

## Closure statement

The complete pre-unification functional set is exactly S-01..S-08, U-01..U-12, C-01..C-18,
CL-01..CL-11, G-01..G-21, Q-01..Q-10, L-01..L-30, P-C-C through P-Q-Q (the 16 explicitly
named grid entries), M-CP-CP through M-QL-QL (the 64 explicitly named grid entries), X-01..X-08,
and A-C/A-CL/A-G/A-Q. The parked-adapter rule and minimum-evidence rule in the acceptance matrix remain
gates on these cells, not additional unnamed cells. No cell may be dropped, merged into an aggregate
result, silently skipped, or weakened because implementation responsibilities move into one daemon.
