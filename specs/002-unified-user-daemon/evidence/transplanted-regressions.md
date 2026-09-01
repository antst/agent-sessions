# Transplanted Legacy Regressions

This evidence freezes the working `c056fbc5015d4ab0a673f66cac5404206f7bcee6` product behavior
before any shared daemon adapter, attachment transaction, or product lifecycle extraction. The
production Go tree remains the working baseline. The new parity harness resolves the exact public
port-map entries and launches every named legacy test in its original package, preserving that
package's `TestMain`, helper-process argv, environment, process ancestry, and native fixture behavior.

The harness rejects a missing, renamed, duplicate, skipped, or failing test. It removes only transient
parent-peer variables (`AGENT_SESSIONS_AGENT_RUNTIME_DIR`, `AGENT_SESSIONS_PRODUCT`,
`AGENT_SESSIONS_SESSION_ID`, and `CLAUDE_PEER_CLAUDE_CONFIG_DIR`) from child test processes so a
validator running inside a managed peer cannot contaminate baseline product observation. It does not
replace product binaries with acceptance credit: these are source regressions only, and all installed
or interactive cells still require the real-product recipes in `baseline-functional-cells.md`.

## Combined gate

- Platform: Linux x86_64
- Go: 1.26.5
- Baseline commit: `c056fbc5015d4ab0a673f66cac5404206f7bcee6`
- Baseline production tree: `bb61de9f4ba4399cf0c62fb5b7a78a1896251189`
- Command:

  ```bash
  PATH=/usr/local/go/bin:$PATH go test ./internal/bridge \
    -run '^(TestCodexDaemonParity|TestClaudeDaemonParity|TestGrokDaemonParity|TestQwenDaemonParity|TestHookDaemonParity)$' \
    -count=1 -v
  ```

- Result: exit 0, package PASS in 21.740 seconds.
- No production extraction or product cutover occurred.
- The intentionally failing acceptance-result contract in `internal/acceptance` is outside this
  focused product-parity command and remains RED until T040.

## Shared wrapper scanner

- Replacement gates: `TestWrapperOptionScannerPreservesBytesOrderDelimiterAndRepeatedSemanticsAcrossProducts`,
  `TestWrapperOptionScannerRejectsInternalOrMalformedOptionsWithoutPartialForwarding`, and
  `TestWrapperOptionTablesRetainEveryBaselineArityEdge`.
- The new scanner has one shared Agent Sessions option layer and four explicit native option-arity
  tables. It does not interpret or normalize vendor options.
- Exact mapped legacy gates were rerun beside the replacement tests: peer context separation,
  native-option-value protection, all-product short groups, transient parent environment removal,
  Codex native argv, Claude yolo/delimiter ordering, Grok managed prefix ordering, and Qwen managed
  argument behavior.
- Result: PASS on Linux x86_64 with Go 1.26.5; package completed in 0.007 seconds.
- Port-map entry advanced: `SH-CLI` at manifest revision 4.

## Shared process and filesystem identity

- Replacement gates: exact PID/coarse-start/strong-start/state classification, real capture and
  ancestry, canonical no-follow type/mode capture, and intermediate/leaf symlink rejection in
  `internal/procinfo/identity_test.go` and `internal/pathidentity/identity_test.go`.
- The bridge exact-identity authorization and capture paths now delegate to `internal/procinfo`; the
  baseline bridge classification and Linux `/proc` observation tests pass beside the replacements.
- Linux result: PASS with Go 1.26.5. `internal/procinfo` and `internal/pathidentity` completed in
  0.003 and 0.004 seconds; the mapped bridge identity tests completed in 0.007 seconds.
- Darwin source uses the same shared classifier over its existing kernel snapshot and retains its
  platform-specific start/strong-start implementation, but real Darwin execution remains pending T012.
- Port-map entry advanced: `SH-IDENTITY` at manifest revision 5.

## Codex

- Replacement gate: `TestCodexDaemonParity`
- Exact mapped legacy tests: 51 across `internal/bridge` and `internal/launcher`
- Named result: PASS in 1.36 seconds (bridge 1.01s, launcher 0.35s)
- Event gate: `TestHookDaemonParity`, including bare `SessionStart`/`UserPromptSubmit`/`Stop`, managed
  outputs, permission refresh, runtime mismatch, rollback, and exact inbox delivery.
- Port-map entries advanced: `C-LAUNCH`, `C-SUP`.

## Claude

- Replacement gate: `TestClaudeDaemonParity`
- Exact mapped legacy tests: 36 across `internal/bridge`, `internal/federator`, and `internal/launcher`
- Named result: PASS in 6.33 seconds (bridge 0.46s, federator 0.58s, launcher 5.29s)
- Event gate: `TestHookDaemonParity`, retaining Claude's native registry/permission/rollback evidence
  rather than inventing Codex-style hooks.
- Port-map entries advanced: `CL-LAUNCH`, `CL-MCP`.

## Grok

- Replacement gate: `TestGrokDaemonParity`
- Exact mapped legacy tests: 42 across `internal/bridge` and `internal/launcher`
- Named result: PASS in 6.16 seconds (bridge 5.74s, launcher 0.42s)
- Event gate: `TestHookDaemonParity`, retaining ACP roster generation, permission refresh, failed
  publication retry, and old-generation rejection.
- Port-map entries advanced: `G-LAUNCH`, `G-HOST`.

## Qwen

- Replacement gate: `TestQwenDaemonParity`
- Exact mapped legacy tests: 30 across `internal/bridge`, `internal/federator`, and `internal/launcher`
- Named result: PASS in 1.50 seconds (bridge 0.64s, federator 0.46s, launcher 0.39s)
- Event gate: `TestHookDaemonParity`, retaining exact first `session_start`, input cursor evidence,
  lifecycle identity rejection, and prepared-launch rollback.
- Port-map entries advanced: `Q-LAUNCH`, `Q-HOST`.

## Cleanup and addressability amendment

The same closed 202-cell matrix now states the negative half of cleanup explicitly without adding or
removing IDs: ended peers and archived/non-addressable lanes must disappear from `list_peers`; direct
send to the former address must return the canonical no-live-target result; and no delivery may be
queued. This applies whenever nobody currently owns the address: a later resume or replacement peer
must not receive messages attempted during that absence. The strengthened assertions are C-09, CL-07,
G-16/G-21, Q-09, and L-10. Real installed credit for those assertions remains pending.

## Unified install owner

The supported lifecycle is one staged host transaction (`make install` / `make install-all`), one
independent hub transaction (`make install-hub`), exact host/hub removal, and explicit host purge.
The old per-product Make targets and executable-specific installers are absent. Available native
products are validated before the release link changes; unavailable products are reported and
skipped without weakening the host transaction.

Replacement source gates on Linux x86_64:

```bash
PATH=/usr/local/go/bin:$PATH go test ./internal/releaseinstall ./internal/releasepkg -count=1
PATH=/usr/local/go/bin:$PATH go test ./internal/bridge -count=1 -timeout=15m
PATH=/usr/local/go/bin:$PATH ./scripts/test-install-host-transaction
PATH=/usr/local/go/bin:$PATH ./scripts/test-install-hub-transaction
PATH=/usr/local/go/bin:$PATH ./scripts/test-removal-transaction
```

Result on 2026-08-30: PASS. The release-install and release-package packages completed in 0.260s and
0.057s; the complete bridge package completed in 40.594s; and all three isolated shell transactions
reported PASS. The gates cover validation before commit, partial-commit and same-version rollback,
prior selection preservation, one activation per successful install, role-scoped removal, all four
optional native connector installers with reverse rollback and no credential/history access, and
two-pass byte-stable archives for all four platforms with exactly two executable images.

An additional staged `scripts/package-release` check produced the Linux x64 archive from the positive
payload allowlist, found 187 normalized entries, proved both repository-only transition artifacts
absent, extracted the archive, removed Go from `PATH`, and completed `make build` using only the two
packaged prebuilt binaries. The `INSTALL` entry remains at its already-achieved cumulative
`daemon-backed` status, which is later than `transplanted`; real installed-product credit remains
separate from this source gate.

The reviewed `c056fbc` cleanup inventory contains 37 stable IDs after adding the two exact systemd
enablement symlinks and two exact launchd output logs that the first draft omitted. The direct utility
gate is also PASS:

```bash
PATH=/usr/local/go/bin:$PATH go test ./internal/releaseevidence -run 'TestPreUnificationCleanup' -count=1 -v
```

It completed in 0.842s and proves deterministic plan-only output, stale-complete-plan zero mutation,
apply-time per-target revalidation, tool-dependency-safe deletion order, FIFO/opaque deletion without
content reads, four-vendor credentials/history/settings/ordinary-file and unrelated-plugin canaries,
unrelated-process preservation, repeat-safe convergence, fixture confinement, exact closed inventory,
no discovery primitives, and absence of both repository-only cleanup artifacts from all four actual
release archives.

The current dirty-tree Linux release-candidate gate was then rerun after the inventory hardening. All
four target builds succeeded and `file` identified the expected Linux ELF x86-64/arm64 and macOS
Mach-O x86-64/arm64 images. Two independent package runs produced byte-identical archives:

```text
darwin-arm64  1f0d9a2ceb7d8f5c11eddf647ee9a6771467de4cd7761552de39efe1f4e3453f
darwin-x64    a0116c6c163b43a52c61e1a505ad54e7ed15a93357462aeb6bcce3ff868b7f42
linux-arm64   4d5f8cb7f58865381e0a9156240fe87c641c4358b0bcff9213beddf5dbf5976f
linux-x64     17ceca565e5f29d394e093d29b925edf5a2eb7abb6dd65c2365bae27c5f5c29f
```

Each archive contained 187 normalized entries and exactly `agent-sessions` plus
`agent-sessions-hub` below its platform binary directory; neither repository-only cleanup artifact
was present. The extracted Linux x64 archive completed its prebuilt `make build` and isolated host
install/rollback transaction with `PATH=/usr/bin:/bin`, where `go` was unavailable. The dedicated
`TestReleaseInventoryAliasesAndArchiveImagesAreExact` additionally proves every installed host alias
resolves to `agent-sessions`, the hub alias resolves to `agent-sessions-hub`, and the same exact
four-archive image/exclusion contract is enforced from the authoritative release inventory.

## Shared messaging and durable delivery

The local daemon messaging path now constructs one product-neutral `AgentFrame`, applies one shared
group and target-admission contract, and records one metadata-only delivery lifecycle per admitted
destination. Direct and multicast sends reject duplicate resolution; discovery and routing ignore
hidden name collisions; broadcast snapshots membership at admission; and the immediate parent/private
group anchors are derived from identities rather than caller-supplied names. Product- and
network-specific presentation remains behind a callback, but an `accepted` result is emitted only
after that destination callback succeeds.

Delivery retries reuse a stable message-and-destination ID. A presentation failure becomes retryable,
while an acknowledged replay does not dispatch again. Persisted state contains routing metadata,
state, acknowledgement, and a non-sensitive retry classification; it never stores message content or
vendor error text.

Source gate on Linux x86_64:

```bash
PATH=/usr/local/go/bin:$PATH go test \
  ./internal/federation ./internal/daemon ./cmd/agent-sessions -count=1
```

Result on 2026-08-30: PASS. The coordinator's `send_message` and `broadcast` handlers now route
through this engine. The `SH-MSG` port-map row advances to `shared` at manifest revision 9. This is
source replacement credit; the installed eight-by-eight messaging matrix remains a separate
acceptance gate.

## Daemon-owned federation

The protocol-3 network engine now lives in `internal/federation`; it does not import the legacy
`internal/federator` package. One `internal/daemon.Federation` component owns the outbound host loop
for each daemon generation, while `agent-sessions-hub` runs the independent product-neutral hub.
The host and hub share exact-version handshake, bounded framing, host-generation replacement,
validated snapshots, destination-acknowledged delivery, remote lane attestation/streaming, and
disconnect cleanup. Build strings remain diagnostic and do not gate equal-protocol interoperability.

The replacement suite covers silent-hub heartbeat reconnect, daemon snapshot changes during a hub
outage, hub restart, remote delivery and Qwen lane transport, malformed roster retention, stale host
generation refusal, duplicate pending and accepted delivery, stale broadcast rejection, destination
disconnect, lane concurrency, and a durable acknowledged replay after reopening daemon state. The
federation release gate now runs the new owner rather than using the retained legacy package as its
proof surface.

Source gates on Linux x86_64, 2026-08-31:

```bash
PATH=/usr/local/go/bin:$PATH RACE=1 ./scripts/federation/test
PATH=/usr/local/go/bin:$PATH ./scripts/test
```

Both passed. This is local source and daemon-integration credit for T093-T096. Cross-host `X-01..X-08`
and remote product/messaging/lane cells on Linux and macOS, plus the unrelated binary-pair upgrade
gate, remain separate T097/T098 evidence and are not claimed here.

The production-binary harness has also passed its same-tree four-binary smoke mode; see
`protocol-compatibility.md`. That result verifies the real command paths but deliberately does not
advance T098, whose unrelated prebuilt Linux and macOS runs remain pending.

## Shared lane engine

The daemon catalog now owns lane creation, queued and explicit turn acceptance, native dispatch,
terminal evidence, chronological exactly-once collection, auto-archive deadlines, restart
reconciliation, stable terminal-notice delivery state, and cleanup debt. The coordinator no longer
performs ad hoc catalog mutation for these transitions. Explicit resume rejects outstanding
collection debt; work accepted behind a running turn remains serialized and each terminal is still
collected once in sequence order. Identical terminal replay is idempotent, while conflicting terminal
evidence and a second collection are rejected.

Restart recovery is product-neutral except for one adapter callback deciding whether an active native
turn can be reattached. Terminal notices retain a stable delivery identity across retry and are not
emitted after acknowledgement. Cleanup debt must be resolved from `cleanup-debt` to a proven terminal
state before the lane can be reported cleanly archived.

## Product lane differences

The shared engine consumes product facts without erasing them. The focused replacement suite retains
Codex App Server thread/turn identity and setup-before-timeout semantics; Claude presence-sensitive
PID-row/socket ancestry and ordinary-query wrapping for inbound peer frames; Grok TUI/private-leader
authorization; and Qwen dual-output artifact revision plus native ancestry. Parent detach,
permission-mode projection, native identity publication, interrupt, archive, and product-specific
readiness remain coordinator/adapter responsibilities.

Source gates on Linux x86_64:

```bash
PATH=/usr/local/go/bin:$PATH go test ./internal/bridge \
  -run '^(TestClaudeLane.*|TestGrokLane.*|TestQwenLane.*|TestEveryParentProductComposesWithEveryTargetGroupLayer|TestRemoteParentTargetThenNestedLaneUsesImmediateParent|TestQwenParentMCPPublishesAllFourLaneTargets)$' \
  -count=1
PATH=/usr/local/go/bin:$PATH go test ./internal/daemon ./cmd/agent-sessions \
  -run '^(TestLaneEngine.*|Test.*Lane.*|Test.*Adapter.*)$' -count=1
```

The exact closed test lists recorded in the port map were used for the actual run; both commands
passed on 2026-08-30. `L-SHARED` advances to `shared` and `L-PRODUCT` to `transplanted` at manifest
revision 10. Installed native-product cells remain separate acceptance credit.

The next product-callback cutover added and integrated the four explicit native lane adapters. Codex
now reaches App Server thread start/resume, effective permission policy, turn start/wait, interrupt,
and archive only through `CodexLaneAdapter`, including fail-closed resumed-thread and terminal-identity
checks. Claude's supported native command is built by `ClaudeLaneAdapter`, which preserves transcript
resume and permission arguments and allows only one active worker for a lane. Grok and Qwen execute
through persistent daemon adapter instances that admit only one ACP driver/client per lane and own
their cancellation/archive exclusion; Grok normalizes its supported headless mode and Qwen preserves
resume plus yolo/bypass equivalence.

After production integration, the exact mapped product-lane regression command and complete
`internal/daemon` plus `cmd/agent-sessions` packages passed again on Linux x86_64. `L-PRODUCT`
advances to `shared` at manifest revision 11. This does not substitute for installed authenticated
`L-*`, `P-*`, or `A-*` cells.

## User service control, bounded diagnostics, and runtime recovery

Explicit daemon start, stop, and restart now cross a typed user-service boundary: Linux invokes only
the exact `agent-sessions.service` systemd user unit, and macOS invokes only the exact
`gui/<uid>/net.antst.agent-sessions` launchd job. Candidate validation precedes upgrade reload or
bootout, so a rejected candidate leaves the running service untouched. The source-level asset checks
also retain `Restart=on-failure` plus `KillMode=process` on Linux and `RunAtLoad` plus `KeepAlive` on
macOS; neither controller names a vendor process, hub, or unrelated service.

The admin endpoint now emits `agent-sessions.admin.v1`. Status and doctor include only normalized
service/readiness enums, booleans, monotonic revisions, and aggregate record counts. Raw host values,
paths, product errors, attachment names, message routing strings, acknowledgements, turn results,
diagnostics, cleanup causes, credentials, prompts, and transcripts never enter the diagnostic encoder.
The report is capped at 32 KiB even when supplied a large untrusted readiness map. Product absence is
reported independently and does not make the single daemon unavailable.

A four-product runtime recovery regression creates four attached peers, two idle lanes, and two busy
lanes; models process death; starts one successor generation; refreshes each product exactly once; and
reconciles the busy native dispatches without resubmission. It proves unchanged native session and
dispatch IDs, unchanged turn sequence, exactly four attachments/lanes/turns, and zero cleanup debt.

Source gates on Linux x86_64 on 2026-08-31:

```bash
PATH=/usr/local/go/bin:$PATH go test \
  ./internal/servicecontrol ./internal/diagnostics ./internal/daemon \
  ./internal/clihelp ./cmd/agent-sessions -count=1
PATH=/usr/local/go/bin:$PATH go test -race \
  ./internal/servicecontrol ./internal/diagnostics ./internal/daemon \
  ./cmd/agent-sessions -count=1
PATH=/usr/local/go/bin:$PATH GOOS=darwin GOARCH=amd64 go test -c \
  ./internal/servicecontrol
PATH=/usr/local/go/bin:$PATH GOOS=darwin GOARCH=amd64 go test -c \
  ./cmd/agent-sessions
```

All native Linux tests, race tests, and Darwin cross-compiles passed. The `INSTALL` row retains its
`daemon-backed` status with the new replacement symbols and tests at manifest revision 12. Live
systemd/launchd upgrade and four-real-product recovery remain separate acceptance tasks.
