# Baseline Functional Cells

Baseline: `c056fbc5015d4ab0a673f66cac5404206f7bcee6`.

This index freezes the working pre-unification behavior. It grants no acceptance credit. Every row
binds one immutable cell to exact legacy test symbols through the named entries in
`contracts/baseline-port-map.yml`, one exact installed recipe below, an artifact/cleanup contract,
and the cell-specific assertion from `docs/ACCEPTANCE-MATRIX.md`.

A scripted fake vendor proves only framing or orchestration. Recipes marked **interactive** require a
human-visible authenticated vendor session on the named platform, including visible history/output;
unit tests, fixture JSON, process order, and aggregate summaries cannot credit those cells.

## Baseline recipe ledger

- **R-SOURCE (repeatable)**: run every `old_tests` symbol in `SH-CLI`, `SH-IDENTITY`, and
  `INSTALL`; run `PATH=/usr/local/go/bin:$PATH make test`, `make test-race`,
  `go vet ./...`, `make lint`, four explicit
  `RELEASE_VERSION=<version> make build-release-platform GOOS=<os> GOARCH=<arch>` commands,
  `scripts/release-inventory`, `scripts/package-release`, and `scripts/release-final-gate`.
- **R-INSTALL (installed; trust cells interactive)**: run every `INSTALL.old_tests` symbol and
  `make install-all`, `make reinstall`, `make install-claude`, `make install-grok`,
  `make install-qwen`, and `make remove-qwen` against the exact selected profile. U-05/U-06/U-07
  stop for native trust UI; automation never approves it.
- **R-CODEX (authenticated interactive)**: run all `C-LAUNCH.old_tests` and `C-SUP.old_tests`;
  visibly run `codex-peer --name acceptance-codex --yolo`,
  `codex-peer resume <exact-uuid> --yolo`, and
  `codex-peer resume acceptance-codex --yolo` from the asserted cwd, including a real prompt,
  visible output, structured tools, quit, interrupt, archive, restart, and fault cells.
- **R-CLAUDE (authenticated interactive)**: run all `CL-LAUNCH.old_tests` and
  `CL-MCP.old_tests`, `scripts/test-claude-contract`, visible
  `claude-peer --name acceptance-claude`, and ordinary/peer
  `claude --resume <target>` / `claude-peer --resume <target>` transitions in the exact shared
  profile.
- **R-GROK (authenticated interactive)**: run all `G-LAUNCH.old_tests` and `G-HOST.old_tests`;
  visibly run `grok-peer --name acceptance-grok`, UUID/title/bare-picker resume, and structured
  Agent Sessions calls from the actual Grok actor. Private leader/observer evidence never substitutes
  for TUI-visible output.
- **R-QWEN (authenticated interactive)**: run all `Q-LAUNCH.old_tests` and `Q-HOST.old_tests`;
  run `scripts/test-qwen-contract` with explicit test-owned `QWEN_TEST_HOME` and
  `QWEN_TEST_RUNTIME_DIR`, plus visible `qwen-peer` start/resume and native mode changes in the
  selected authenticated profile.
- **R-LANES (authenticated interactive per target)**: run all `L-SHARED.old_tests` and
  `L-PRODUCT.old_tests`; execute each of `codex-peer-lane`, `claude-peer-lane`,
  `grok-peer-lane`, and `qwen-peer-lane` with `doctor`, `run`, `start`, `status`,
  `wait`, `resume`, `interrupt`, `archive`, and `list`. L-02 requires a real inference
  response and native transcript.
- **R-COMPOSE (authenticated 4×4 interactive)**: run
  `internal/bridge/group_agent_test.go:TestEveryParentProductComposesWithEveryTargetGroupLayer`
  and `scripts/test-qwen-composition` with four authenticated explicit test profiles. Credit each
  P-cell only from its destination-native token/transcript, never from matrix order.
- **R-MESSAGE (authenticated two-ended interactive)**: run all `SH-MSG.old_tests`; from the source
  invoke structured `agent_sessions.send_message` for the exact destination, visibly receive and
  reply at the destination with the same correlation ID, idle and busy. Record UUIDs, host suffixes,
  groups, acknowledgement, and native presentation for each M-cell.
- **R-FED (installed cross-host)**: run all `FED.old_tests` and `SH-MSG.old_tests`,
  `scripts/federation/test`, then the same authenticated message/lane command Linux↔macOS and
  Linux↔Linux through the central hub. Restart only run-owned host/hub instances.
- **R-ARCHIVE (authenticated interactive)**: run matching product and lane archive tests; invoke the
  real native archive, inspect native inventory, and resume/unarchive by exact UUID. A product without
  native peer archive receives explicit capability-based `N/A`, never simulated file deletion.

## Artifact and cleanup contracts

- **E-SOURCE**: exact build/archive payload, checksum, and clean Git state; only test-owned output is
  removed.
- **E-INSTALL**: exact release links and plugin payloads; preserve credentials, settings, transcripts,
  other plugins/users, the hub, and vendor processes.
- **E-CODEX**: one App Server thread/rollout/index identity and owner/shim state; retain history and
  remove only Agent Sessions owner/shim/socket/inbox state; after retirement require discovery absence
  and no-live-target direct-send rejection with no queued delivery.
- **E-CLAUDE**: one native transcript row, preparation-bound socket, profile, and attachment alias;
  preserve native Claude and unrelated shared rows; after retirement require discovery absence and
  no-live-target direct-send rejection with no queued delivery.
- **E-GROK**: one launch record binding TUI, host, private leader, observer, MCP, roster, and ACP actor;
  remove exact owned helpers/sockets/locks/private directory only; after withdrawal require discovery
  absence and no-live-target direct-send rejection with no queued delivery.
- **E-QWEN**: exact dual-output files, native UUID/profile, real 0600 socket, preparation, and
  attachment; retain typed cleanup debt on ambiguity; after retirement require discovery absence and
  no-live-target direct-send rejection with no queued delivery.
- **E-LANE**: stable lane and product-native UUID/transcript, ordered turns/notices/cursor, worktree,
  owner/persistence, permission, and archive state; exact run-owned cleanup only; archived or otherwise
  non-addressable lanes are absent from peer discovery and reject direct send without queuing.
- **E-MESSAGE**: durable correlation ID plus destination acknowledgement/presentation; no duplicate or
  hidden group/identity leakage.
- **E-FED**: exact protocol, host/service/peer roster, remote delivery/lane IDs; restart restores one
  row without peer restart or duplication.

## Per-cell baseline ledger

| Cell | Exact legacy test ledger | Recipe | Artifact/cleanup contract | Cell-specific baseline assertion |
|---|---|---|---|---|
| S-01 | INSTALL | R-SOURCE | E-SOURCE | Normal, race, vet, lint, and shell integration pass uncached. |
| S-02 | INSTALL | R-SOURCE | E-SOURCE | Stock macOS Bash 3.2 runs `scripts/test`. |
| S-03 | INSTALL | R-SOURCE | E-SOURCE | All four archives contain every documented binary and plugin payload exactly once. |
| S-04 | INSTALL | R-SOURCE | E-SOURCE | Plugin validation never launches a GUI or unvalidated same-name product. |
| S-05 | SH-IDENTITY | R-SOURCE | E-SOURCE | PID reuse, process-start mismatch, stale lock/socket/record, duplicate name, and provisional cleanup regressions pass. |
| S-06 | INSTALL | R-SOURCE | E-SOURCE | Active production source, build, package, standard install/update/remove, service, help, plugin, and executable dependency surfaces contain no obsolete canonical-host or binary names. Historical baseline maps, deletion evidence, tests, and the repository-only cleanup script plus its authoritative allowlist contract are excluded from this active-surface assertion. |
| S-07 | INSTALL | R-SOURCE | E-SOURCE | Gates leave the checkout clean except for the tested patch; user files and stashes are unchanged. |
| S-08 | SH-CLI | R-SOURCE | E-SOURCE | Launcher drift runs against both the oldest supported and current accepted Codex CLI versions, records each exact version, and passes every discovered non-interactive command through byte-for-byte. An upstream Codex version change invalidates later live evidence until this cell is rerun. |
| U-01 | INSTALL | R-INSTALL | E-INSTALL | Clean install produces exact-revision links, plugin payloads, and runtime paths. |
| U-02 | INSTALL | R-INSTALL | E-INSTALL | Same-revision reinstall is idempotent and preserves plugin data. |
| U-03 | INSTALL | R-INSTALL | E-INSTALL | The first greenfield install uses operator-provided old-stack quiescence. Later unified upgrades restart only the exact Agent Sessions user daemon and preserve live vendor peers and vendor infrastructure. |
| U-04 | INSTALL | R-INSTALL | E-INSTALL | Install does not stop or reload another OS user's Agent Sessions service, the central hub, or an unrelated vendor peer. |
| U-05 | C-LAUNCH, C-SUP, INSTALL | R-INSTALL | E-INSTALL | Codex hook/project trust prompts are surfaced; automation never approves them. |
| U-06 | G-LAUNCH, INSTALL | R-INSTALL | E-INSTALL | Grok plugin trust is explicit; validation precedes replacement of only `agent-sessions`. |
| U-07 | G-LAUNCH, INSTALL | R-INSTALL | E-INSTALL | Chat/desktop same-name CLIs are skipped/rejected without launching a GUI or updater. |
| U-08 | INSTALL | R-INSTALL | E-INSTALL | Plugin entry points select the exact same runtime revision as their owner host. |
| U-09 | INSTALL | R-INSTALL | E-INSTALL | Every release archive installs without Go and matches its published checksum. |
| U-10 | G-HOST, INSTALL | R-INSTALL | E-INSTALL | Malformed/unreadable Grok launch inventory fails closed with an inventory diagnostic, not false "peer still running" advice. |
| U-11 | Q-LAUNCH, Q-HOST, INSTALL | R-INSTALL | E-INSTALL | Qwen install/upgrade/remove uses the exact selected profile, refuses a live managed user of that profile, verifies manifest/version/enabled/MCP/skill inventory, and preserves credentials, settings, other extensions, and transcripts. |
| U-12 | INSTALL | R-INSTALL | E-INSTALL | A prebuilt archive contains exactly two executable images—`agent-sessions` and `agent-sessions-hub`—plus four product plugin payloads; the independent host-only and hub-only install targets select only their role, require no Go, and a second build from the same tree is byte-identical. |
| C-01 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Fresh named zero-turn launch reaches composer and exits normally. |
| C-02 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Real prompt returns visible output; normal quit preserves one unarchived UUID. |
| C-03 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Resume by exact UUID and durable name reuses UUID after prompt changes DB title. |
| C-04 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Global flags before command and after target produce byte-identical native argv. |
| C-05 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Model, sandbox, approval, config, display, multi-value `--image`, attached values, and bare `--` remain intact. |
| C-06 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Repeated permission booleans follow native last-value semantics without classification drift. |
| C-07 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Renamed project without compatibility symlink resumes implicitly and with explicit cwd, then retains canonical cwd. |
| C-08 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Stale loaded zero-turn takeover unsubscribes before cwd override and never duplicates owner/thread/index row. |
| C-09 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Normal quit sends no archive/unarchive request; the attachment becomes detached, disappears from `list_peers`, rejects direct send to its former address as no live target without queuing, and leaves no per-session Agent Sessions process or owned artifact. |
| C-10 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | `identity`, `list_peers`, `rename_session`, `check_inbox`, and `send_message` work only for an attested caller. |
| C-11 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Wrong identity/token/owner ancestry fails closed before roster or message access. |
| C-12 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Idle message wakes; busy message steers/queues; ordered burst is exact-once. |
| C-13 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Destination returns exact acknowledgement with correct sender name, UUID, host, and `from-mode`. |
| C-14 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Plain/YOLO launch and resume agree across argv, App Server state, `/status`, registry, and outgoing label. |
| C-15 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Sticky resumed YOLO is verified; never-YOLO control stays constrained. |
| C-16 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Normal quit, Ctrl+C, generation interrupt, TUI SIGTERM, unified daemon restart, and App Server restart converge cleanly. |
| C-17 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Explicit archive is idempotent; archived behavior is correct; resume performs one unarchive and retains transcript. |
| C-18 | C-LAUNCH, C-SUP | R-CODEX | E-CODEX | Missing sockets, stale owner, PID reuse, and interrupted publication recover without affecting another thread. |
| CL-01 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Bare `claude` remains an opt-out and changes neither the Agent Sessions catalog nor registration set. |
| CL-02 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | `claude-peer` starts a real native TUI in the configured shared Claude profile; ordinary, peer, and lane rows coexist with exactly one unified daemon service row. |
| CL-03 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Two Claude peers use distinct preparation-bound sockets and discover one another through group-filtered AgentFrame routing; native Claude direct messaging remains independently available. |
| CL-04 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Exact UUID ordinary→peer→ordinary→generic-peer resume retains one shared transcript plus explicit groups, inheritance snapshot, name, cwd, and effective durable YOLO choice. `claude-peer --resume NATIVE_TARGET` passes non-UUID targets unchanged to Claude, including ordinary titles and duplicate-title chooser flows, ignores any transient boot UUID, then atomically promotes a cleanup-owned provisional attachment only after the selected transcript title/UUID is authoritative. The attachment alias still attests structured MCP and local/remote lane calls as the selected UUID. Named resumes publish the requested title immediately; exact-UUID resumes recover the latest validated native transcript title. No provisional or transient catalog row survives; explicit overrides replace only selected fields. |
| CL-05 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Explicit launch names and native transcript title/status changes refresh the daemon-owned attachment registration without being replaced by Claude's cwd-derived row fallback and without duplicating the service row. Permission class is a stable launch decision: constrained peers disable the unobservable in-session bypass surface, while explicit bypass peers remain conservatively advertised as bypass until restart. |
| CL-06 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Unified daemon restart republishes one service row and reattests every idle Claude attachment without restarting Claude. |
| CL-07 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Normal exit, Ctrl+C, SIGTERM, connector or daemon-side attachment failure, stale native row, key/socket-before-row startup failure, PID reuse, and socket mismatch retire only the exact owned Claude registration/artifacts, preserving the native Claude process, unrelated shared rows, and the service. Every retired attachment disappears from `list_peers` and direct send to its former address fails as no live target without queuing. |
| CL-08 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Managed Claude's structured MCP discover/direct/multicast/broadcast returns correlated group-filtered results and replies to an incoming delivery through Agent Sessions. Exact adapter/lifecycle ancestry is accepted without a model-supplied Codex ID; copied environment, unrelated registered process, bare caller, and native unframed service prose fail closed. The framed native carrier remains a compatibility path, while independent native direct traffic can cross Agent Sessions groups. |
| CL-09 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Claude→Codex, Claude→Claude, Claude→Grok, and Claude→Qwen lane launches all bind the immediate Claude parent, including nested and persistent children. |
| CL-10 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Profile/credential mismatch, live exact-UUID attachment, invalid launch settings, launcher crash, native startup failure, and failure before or after native named-target selection do not adopt or alter the ordinary session's catalog row; a durable gated-launch journal survives unified daemon restart and rolls back groups and YOLO before retry. |
| CL-11 | CL-LAUNCH, CL-MCP | R-CLAUDE | E-CLAUDE | Ordinary shared-profile use may cache extension discovery; a later managed peer or lane still publishes its native row/socket without an interstitial. Peers preserve explicit `--chrome`/`--no-chrome`, while the managed default and all headless lanes use `--no-chrome`; host profile settings remain unchanged. |
| G-01 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Chat Grok first and Grok Build later/fallback selects only Grok Build with fixed help probe. |
| G-02 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Direct, symlinked, dangling, and case-varied `*.app/Contents` candidates are rejected before execution. |
| G-03 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Explicit override is identically validated and never silently falls through. |
| G-04 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Genuinely cold `grok-peer` reaches usable TUI with all configured MCP servers started. |
| G-05 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Cold logs contain no reclaimed process scope, duplicate actor, or observer ownership failure. |
| G-06 | G-LAUNCH, G-HOST | R-GROK | E-GROK | TUI, daemon attachment, private leader, ACP observer, and MCP server identities match one launch record. |
| G-07 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Observer initializes/authenticates, sees one resident row, and sends neither `session/load` nor `session/prompt`. |
| G-08 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Ordinary Grok and `grok-peer` use equivalent auth/config for the same cwd; a foreign MCP OAuth failure remains server-attributed in Grok's MCP status/events and private leader/observer diagnostics never render as an unattributed fatal in the interactive TUI. |
| G-09 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Idle `x.ai/interject` visibly starts a turn without typing and returns exact acknowledgement. |
| G-10 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Busy/generating/tool-active interjection is received once without replacing the TUI actor. |
| G-11 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Burst, duplicate ID, conflicting reuse, reconnect, rejection, response-before-echo, missing actor echo, and ambiguous EOF preserve the documented local dedup/never-replay contract. |
| G-12 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Restart restores queued records but never replays ambiguous `in_flight` interjection. |
| G-13 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Exact-UUID, title, and bare-picker resume preserve native selection behavior, selected UUID/title, cwd, plugin access, structured messageability, and local/remote lane ownership. A cleanup-owned attachment alias is promoted only after one authoritative resident roster row; no provisional catalog row survives and an explicit peer name wins. |
| G-14 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Launch/config/admin policy and in-TUI changes converge across roster, record, registry, status, federation, and label. |
| G-15 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Failed permission publication retries; background refresh reports retryable busy instead of stale authority. |
| G-16 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Before exit, the private leader and observer process scopes remain isolated from the TUI and unified daemon; normal TUI exit then withdraws the peer from `list_peers`, makes direct send to its former address fail as no live target without queuing, terminates only those owned helpers, and removes the MCP relay, sockets, locks, launch record, private directory, registry, and empty state directory. |
| G-17 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Ordinary `grok leader list`/`kill` is tested only on run-owned shared leaders, never used for a healthy private peer. |
| G-18 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Owner/daemon/leader death, auth failure, roster ambiguity, PID reuse, and stale records clean up without killing unrelated clients. |
| G-19 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Direct `agent_sessions.list_peers` readiness failure blocks first publication; catalog omission and unrelated MCP failures do not, and the already-running server never rejects itself through a recursive readiness check. |
| G-20 | G-LAUNCH, G-HOST | R-GROK | E-GROK | After actor acceptance, a Grok-deferred roster request cannot block the interjected turn's own `agent_sessions` calls; the labelled pre-interjection permission snapshot retires on the first successful post-turn roster refresh and expires after 30 minutes if recovery remains broken. |
| G-21 | G-LAUNCH, G-HOST | R-GROK | E-GROK | Global roster pushes plus one-second reconciliation publish `working`/`needs_input`/`idle` as busy/waiting/idle; initial non-idle state is retained, old observer generations and turn-boundary stale polls cannot overwrite it, and removed/terminal/nonresident/ambiguous actors withdraw the peer from discovery and reject later direct send as no live target without queuing. |
| Q-01 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Bare `qwen` remains an opt-out; a cold `qwen-peer` uses the exact selected profile and reaches a usable native TUI. |
| Q-02 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Session-free readiness proves package/version, presence-sensitive profile, parser semantics, ACP initialize/capabilities, native archive, trusted cwd, and exact plugin inventory without creating a transcript or reading secrets. |
| Q-03 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Fresh and exact managed resume retain UUID, canonical cwd, native transcript, groups, name, and selected profile; continue/fork and ambiguous managed selectors fail before mutation. |
| Q-04 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Native-default, exact `--no-yolo`→initial `default`, `--yolo`, and admitted native `--approval-mode` launches match argv and durable launch preference; conflicting flags fail before a child exists. Native in-session mode changes remain Qwen-owned. |
| Q-05 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Dual-output admission requires the exact session-start UUID/cwd/version/protocol/event inventory; truncation, replacement, path-type change, native authentication failure, and malformed/out-of-order events fail closed. |
| Q-06 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | The published session-stable endpoint is a real `0600` Unix socket, not a symlink. Direct native delivery succeeds at the advertised path with no caller-side resolution or extra hub round trip. |
| Q-07 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Attested Qwen MCP discovery, direct send/reply, atomic multicast, named-group broadcast, idle/busy delivery, and deduplication work; copied environment, bare Qwen, stale process, wrong profile, wrong capability, and model-supplied ID fail closed. |
| Q-08 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Unified daemon restart republishes the same live peer without restarting Qwen or duplicating the service/participant. |
| Q-09 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Normal exit, Ctrl+C, SIGTERM, wrapper/native crash, failure before/after publication, recycled PID, replaced file/socket, and legacy symlink cleanup remove only exact owned state and retain retryable debt on ambiguity. After retirement the peer is absent from `list_peers`, and direct send to its former address fails as no live target without queuing. |
| Q-10 | Q-LAUNCH, Q-HOST | R-QWEN | E-QWEN | Ordinary→managed→ordinary transcript use requires no credential, settings, extension, skill, or transcript migration and leaves no Agent Sessions authority in the ordinary attachment. |
| L-01 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | `doctor` proves CLI/runtime readiness, the CLI's locally reported authentication state, and a supported contract major; it must fail when Claude reports logged out. |
| L-02 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Foreground `run` completes a real inference turn, thereby proving end-to-end authentication, and emits coherent JSONL with the exact expected final answer. |
| L-03 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | `start` returns `lane.ready`; `status` is busy; `wait` collects matching turn once. |
| L-04 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | `wait --timeout` does not interrupt; later wait collects the turn. |
| L-05 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Idle peer message starts one collectable follow-up; busy message follows product semantics. |
| L-06 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Lane replies to owner and messages every applicable peer/lane destination. |
| L-07 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | `resume` reuses exact identity and refuses while prior work is owed. |
| L-08 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | `interrupt` yields correct raw status, normalized outcome, exit, collection, and a notice where that product supports notices. |
| L-09 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Execution timeout yields `timed_out`/124; collection timeout does not mutate work. |
| L-10 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Archive is idempotent, removes the lane from peer discovery/addressability, makes direct send to its former address fail as no live target without queuing, retains transcript, and reports dropped notices where notices are supported. |
| L-11 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Archived resume performs supported unarchive exactly once and preserves identity/transcript. |
| L-12 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Auto-archive deadline, cancellation, custom grace, and no-auto-archive match docs. |
| L-13 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Duplicate name and invalid owner/token/product remove provisional worktree/process/socket. |
| L-14 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | User dirty worktree and archived lane worktree are preserved. |
| L-15 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Owner exit interrupts/archives parent-owned work; persistent lane and unrelated lanes survive. |
| L-16 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Worker or unified daemon crash recovery restores terminal items, cursor, supported notices, and cleanup once. |
| L-17 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Permission inheritance requires exact local owner; explicit lane policy always wins. |
| L-18 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Remote run/start/resume/wait/status/interrupt/archive/list preserve streams, exit, cwd, notify, and cleanup fuse. |
| L-19 | L-SHARED, L-PRODUCT | R-LANES | E-LANE | Remote stdin cap, prompt-file semantics, hub loss, and disabled destination capability fail closed. |
| L-20 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Grok persists its ACP-created native UUID beside the stable lane UUID; `session/load` reuses that exact native identity under one sole ACP driver and never attaches to an interactive Grok conversation. |
| L-21 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Grok idle/busy inbound messages serialize into collectable turns; duplicate message IDs are idempotent and conflicting reuse fails closed. |
| L-22 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Grok headless policy is explicitly always-approve, published as bypass, and never inferred from an uncorroborated owner or downgraded to an unusable prompting mode. |
| L-23 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Codex and Claude plugins ship a valid Grok-lane skill; its executable Claude preflight and linked contract/install references survive staging and packaging; the Grok plugin ships an agent-lanes skill that distinguishes Codex contract 2 from Claude contract 1. |
| L-24 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Grok `lane.status`/`lane.list` report stable and native identities, lifecycle state, collection debt, owner/persistence, and auto-archive policy; every collected terminal result is `turn.completed` with explicit status/outcome/exit. |
| L-25 | L-SHARED, L-PRODUCT, G-HOST | R-LANES | E-LANE | Grok normal archive and crash reconciliation remove the real ACP worker and all attributable MCP/tool descendants on Linux and macOS. The macOS cell must exercise a registered restricted shell in its own session across unified daemon SIGKILL and restart. Unmanaged restricted daemons that create a new session and reparent after their registered shell has exited are explicitly unsupported and excluded from a green cleanup claim, never guessed or killed heuristically. |
| L-26 | L-SHARED, L-PRODUCT, C-LAUNCH, C-SUP | R-LANES | E-LANE | A Codex lane that fails before its first rollout exists remains archivable by exact thread ID. Only an authoritative missing-rollout response plus a local failed record with no turn evidence permits `thread/delete`; transport ambiguity or any turn evidence fails closed. |
| L-27 | L-SHARED, L-PRODUCT, Q-HOST | R-LANES | E-LANE | Qwen persists its ACP-created native UUID beside the stable lane UUID and uses one sole ACP client for initialize/new-or-resume, serialized prompt, mode request, and cancel. |
| L-28 | L-SHARED, L-PRODUCT, Q-HOST | R-LANES | E-LANE | Qwen native archive/unarchive uses one bounded token-authenticated loopback helper, exact workspace/UUID, idempotent conflict handling, compensation, and zero helper/preheated-child residue. |
| L-29 | L-SHARED, L-PRODUCT, Q-HOST | R-LANES | E-LANE | Qwen worker/tool-root/helper crash, PID reuse, unified daemon restart, and cleanup retry preserve unrelated processes and converge durable cleanup debt. |
| L-30 | L-SHARED, L-PRODUCT, Q-HOST | R-LANES | E-LANE | Qwen launch preference, expected initial mode, and observed current mode or `unknown` remain distinct; Qwen-native later mode changes are neither blocked nor treated as routing authority. |
| P-C-C | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-C-CL | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-C-G | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-C-Q | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-CL-C | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-CL-CL | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-CL-G | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-CL-Q | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-G-C | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-G-CL | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-G-G | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-G-Q | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-Q-C | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-Q-CL | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-Q-G | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| P-Q-Q | L-SHARED, L-PRODUCT | R-COMPOSE | E-LANE | Immediate parent anchor, private group, opt-in inheritance, messaging, collection, interrupt, archive/resume, parent exit, persistence, exact cleanup. |
| M-CP-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CP-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLP-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GP-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QP-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CL-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-CLL-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-GL-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-CP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-CLP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-GP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-QP | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-CL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-CLL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-GL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| M-QL-QL | SH-MSG | R-MESSAGE | E-MESSAGE | Unique idle and busy marker, exact return marker, offline/resume and reconnect; enqueue-only is RED. |
| X-01 | FED, SH-MSG | R-FED | E-FED | Local-only startup publishes exactly one unified daemon host service row; grouped discovery/direct/multicast/broadcast work without a hub. |
| X-02 | FED, SH-MSG | R-FED | E-FED | Same-name visible peers are ambiguous, same-name peers in disjoint groups do not interfere, and hidden identities do not leak. |
| X-03 | FED, SH-MSG | R-FED | E-FED | Pairwise messaging and lane lifecycle pass Linux↔macOS and Linux↔Linux without per-peer shadows. |
| X-04 | FED, SH-MSG | R-FED | E-FED | Every snapshot peer has an exact host/session identity, protocol, product, instance, groups, and its own private anchor; malformed replacement snapshots retain the last valid roster. |
| X-05 | FED, SH-MSG | R-FED | E-FED | Hub and host-daemon restart restore one service row and peer registrations without duplicates or peer restart. |
| X-06 | FED, SH-MSG | R-FED | E-FED | Legacy flat delivery, duplicate resolved multicast recipients, invalid source/product/group, and a broadcast to a non-member group are rejected before delivery. |
| X-07 | FED, SH-MSG | R-FED | E-FED | Remote parent context retains the source-host private anchor; optional parent groups propagate only after explicit `--inherit-groups`. |
| X-08 | FED, SH-MSG | R-FED | E-FED | Install/tests never stop or reload another OS user's Agent Sessions service or the central hub. |
| A-C | C-LAUNCH, C-SUP | R-ARCHIVE | E-CODEX | Codex peer archive/unarchive |
| A-CL | CL-LAUNCH, CL-MCP | R-ARCHIVE | E-CLAUDE | Claude peer archive/unarchive or explicit native `N/A` |
| A-G | G-LAUNCH, G-HOST | R-ARCHIVE | E-GROK | Grok peer archive/unarchive |
| A-Q | Q-LAUNCH, Q-HOST | R-ARCHIVE | E-QWEN | Qwen peer archive/unarchive |

## Existing convergence predicates and deadlines

These are the baseline predicates/deadlines where a cell asserts restart, reconnect, or eventual
cleanup. Ownership may move; the implementation may not replace them with fixed sleeps, unbounded
polling, or a weaker predicate.

| Cells | Exact baseline predicate | Existing deadline/source |
|---|---|---|
| U-03 | Exact legacy App Server/Grok activity inventory reaches empty before replacement; unrelated processes remain unchanged. | Operator-bounded preflight in the baseline install path; no implicit force timeout. |
| C-01, C-03, C-08 | Prepared owner publishes only after exact App Server thread/cwd/owner corroboration. | `preparedPublicationTimeout = 60s`, `internal/bridge/launch.go`. |
| C-12, C-16, C-18 | Exact thread resumes and the wake ledger reaches one terminal delivery; request failures stay typed. | App Server connect 15s; resume/turn list 30s; turn start 60s; `internal/bridge/supervisor.go`. |
| CL-06, CL-07, CL-10 | Native row/socket and exact process-start reappear, or exact cleanup debt remains; unrelated rows stay unchanged. | Claude lane publication 35s; focused reconciliation tests use one-second bounded waits. |
| G-06, G-09, G-10, G-11, G-12, G-18, G-19, G-20, G-21 | One resident roster row and MCP readiness authorize publication; interjection becomes delivered/rejected/ambiguous exactly once; cleanup verifies exact exit. | ACP startup 15s, control 20s, interject 30s, cleanup 5s; `internal/bridge/grok.go`. |
| Q-01, Q-05, Q-06, Q-07, Q-08, Q-09 | Exact session-start/event inventory admits the host; exact registration/socket appears or typed cleanup debt remains. | Host admission 20s; test lifecycle 10s; installed runner `TIMEOUT=120s`. |
| L-03, L-04, L-05, L-06, L-07, L-08, L-09, L-10, L-11, L-12, L-13, L-14, L-15, L-16, L-20, L-21, L-22, L-23, L-24, L-25, L-26, L-27, L-28, L-29, L-30 | Durable cursor/status/notices reach one collectable terminal outcome without redispatch; cleanup stays identity-bound. | Claude manager 35s; Grok manager 60s; Qwen manager 75s; composition `TIMEOUT=180s`. |
| X-03, X-05 | Exact remote roster equals expected sessions and each host has one service row after hub/agent restart. | Default 10s, restart 15s; `scripts/federation/integration_test.py:wait_for`. |

## Reviewed topology deltas

| Cell | Baseline observation | Target observation | Preserved invariant | Review |
|---|---|---|---|---|
| S-06 | repository and install plans contain no obsolete canonical-host or binary names | active production, build, package, standard lifecycle, help, plugin, and executable dependency surfaces contain no obsolete canonical-host or binary names while historical evidence, tests, and the repository-only one-time cleanup script plus its authoritative allowlist contract may name them | no obsolete Agent Sessions authority or binary remains reachable from a supported build, package, installation, update, removal, service, or runtime path | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| U-03 | upgrade waits for Codex App Server and managed Grok peers to stop | first greenfield install assumes operator quiescence; unified upgrades restart only the Agent Sessions daemon and preserve vendor peers | upgrade never creates mixed Agent Sessions authority or signals unrelated processes | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| U-04 | install preserves an unrelated standalone federator or peer | install preserves another OS user's Agent Sessions service, the central hub, and unrelated vendor peers | install mutates only the exact selected user service and release | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| U-12 | prebuilt archive installs eleven Agent Sessions binaries | prebuilt archive contains exactly agent-sessions and agent-sessions-hub images and independent host-only and hub-only targets select their role | prebuilt no-Go installation, four host product payloads, and reproducible bytes | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| C-09 | normal quit converges the per-session shim count to zero | normal quit converges the managed attachment to detached with no per-session Agent Sessions process | normal quit does not archive and leaves no Agent Sessions-owned residue | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| C-16 | supervisor restart participates in Codex recovery | unified daemon restart participates in Codex recovery | every exit, interrupt, restart, and App Server recovery path converges exactly once | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| CL-02 | Claude rows coexist with one standalone host-agent service row | Claude rows coexist with one unified daemon service row | ordinary, peer, and lane Claude identities coexist without duplication | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| CL-05 | native title and status refresh the host-agent registration | native title and status refresh the daemon-owned attachment registration | native state wins over cwd fallback without duplicating the service row | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| CL-06 | host-agent restart causes idle supershims to re-register | daemon restart reattests idle Claude attachments without restarting Claude | one service row and every live native Claude identity are restored | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| CL-07 | supershim crash and stale native artifacts retire exact Claude ownership | connector or daemon-side attachment failure and stale native artifacts retire exact Claude ownership | cleanup preserves unrelated Claude rows, processes, and service state | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| CL-10 | gated launch journal survives standalone agent restart | gated launch journal survives unified daemon restart | failed selection never adopts or mutates the ordinary Claude session | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| G-06 | TUI, product host, private leader, observer, and MCP share one launch record | TUI, daemon attachment, private leader, observer, and MCP share one launch record | every Grok actor is bound by exact ancestry and launch identity | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| G-16 | standalone product-host process group is isolated from the TUI | private leader and observer process scopes are isolated from the TUI and daemon | normal exit terminates only exact owned Grok helpers and removes exact owned artifacts | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| G-18 | owner, standalone host, or leader death reconciles Grok state | owner, unified daemon, or leader death reconciles Grok state | fault cleanup never kills unrelated Grok clients | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| Q-08 | standalone agent restart republishes Qwen | unified daemon restart republishes Qwen | the same native Qwen peer returns without restart or duplication | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| L-16 | worker, manager, or supervisor crash restores lane state | worker or unified daemon crash restores lane state | terminal items, cursor, notices, and cleanup restore exactly once | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| L-25 | macOS restricted-shell cleanup is exercised after manager SIGKILL | macOS restricted-shell cleanup is exercised across daemon SIGKILL and restart | exact Grok lane workers and descendants are removed without heuristic collateral | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| L-29 | Qwen manager, worker, helper, agent, or supervisor failure is reconciled | Qwen worker, tool root, helper, or unified daemon failure is reconciled | retry preserves unrelated processes and converges durable cleanup debt | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| X-01 | local startup publishes one standalone host service row | local startup publishes one unified daemon host service row | grouped local collaboration works without a hub | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| X-05 | hub and standalone host-agent restart restore registrations | hub and host-daemon restart restore registrations | service and peer identities return without duplicate or peer restart | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |
| X-08 | install and tests preserve unrelated standalone host agents and hubs | install and tests preserve another OS user's Agent Sessions service and the central hub | test and install scope never mutates unrelated authorities | Reviewed: Agent Sessions topology only; native/functional assertion unchanged. |

All substitutions above are confined to Agent Sessions processes, service rows, packages, or
ownership records. None authorizes changing vendor-native selection, history, profile, permission,
messaging, lane, or archive behavior.
