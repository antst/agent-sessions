# Agent Sessions acceptance matrix

This is the release acceptance contract for every peer and lane product. A
source test, fake protocol server, successful enqueue, or clean build proves
only its own cell. It must never be reported as installed product acceptance.

Each run uses unique names (`mx-<run>-...`) and records the exact source SHA,
installed VCS revision, OS/architecture, Codex/Claude/Grok/Qwen versions, commands,
exit codes, live argv, session IDs, registry/owner state, protocol counts, TUI
output, and cleanup inventory. Tests communicate only with sessions created by
that run. Unrelated peers, lanes, host daemons, hubs, worktrees, and application
processes must remain untouched.

## Verdicts and tiers

- `GREEN`: every applicable mandatory cell passed with evidence.
- `GREEN WITH N/A`: all supported cells passed and unsupported capabilities
  are explicitly identified.
- `BLOCKED`: environment, credentials, platform, or required CLI unavailable;
  this is not a product pass or failure.
- `RED`: a product, test, cleanup, or safety assertion failed.

Evidence tiers are `S` source/fake-protocol, `I` exact installed runtime, `T`
real interactive TUI/model, `L` real local lane, `X` cross-host federation,
`F` fault/recovery, and `U` install/upgrade/release. A changed adapter is not
release-green without every applicable tier on Linux and macOS. Federation
changes additionally require at least two Linux hosts and one macOS host.

## Evidence required for every live cell

1. Exact source SHA and installed binary revision are identical.
2. Literal command, cwd, environment overrides, and exit code are saved.
3. Final child argv is captured from the live process, not inferred from UI.
4. TUI output proves startup, response, status/permission mode, and exit.
5. UUID, name, canonical cwd, archive state, permission class, PID, and
   process-start identity are recorded.
6. Registry, owner, adapter, daemon attachment/lane catalog, federation, and group state are captured
   before, during, and after.
7. Protocol method counts prove no duplicate archive, unarchive, prompt, turn,
   wake, or delivery.
8. Cleanup proves no owned TUI, native worker, adapter resource, private endpoint, socket,
   lock, temporary worktree, or tmux server remains.
9. Destination-visible receipt plus an exact acknowledgement is required for
   messaging. Sender-side `accepted` or `queued` is not a pass.
10. A phase that starts an in-process daemon fixture records its test-owned config, state, runtime,
    product profiles, and `PATH`. It never starts, stops, or replaces the installed user service;
    later phases use a new isolated daemon generation when different inputs are required.
11. On a host with another Agent Sessions install on `PATH`, in-session lane
    acceptance uses the attested `agent_sessions.lane` tool
    and verifies the loaded plugin/runtime revision. A shell-resolved lane
    executable from another install is not exact-revision evidence.

## Source and packaging (`S`)

Run uncached with the CI Go toolchain:

```bash
go clean -testcache
/bin/bash ./scripts/test
go clean -testcache
RACE=1 /bin/bash ./scripts/test
go vet ./...
make lint

VERSION="$(cat deploy/agent-sessions/VERSION)"
GOOS=linux  GOARCH=amd64 make build-release-platform RELEASE_VERSION="$VERSION" RELEASE_OUTPUT_DIR=dist/one
GOOS=linux  GOARCH=arm64 make build-release-platform RELEASE_VERSION="$VERSION" RELEASE_OUTPUT_DIR=dist/two
GOOS=darwin GOARCH=amd64 make build-release-platform RELEASE_VERSION="$VERSION" RELEASE_OUTPUT_DIR=dist/three
GOOS=darwin GOARCH=arm64 make build-release-platform RELEASE_VERSION="$VERSION" RELEASE_OUTPUT_DIR=dist/four
```

| ID | Assertion |
|---|---|
| S-01 | Normal, race, vet, lint, and shell integration pass uncached. |
| S-02 | Stock macOS Bash 3.2 runs `scripts/test`. |
| S-03 | All four archives contain every documented binary and plugin payload exactly once. |
| S-04 | Plugin validation never launches a GUI or unvalidated same-name product. |
| S-05 | PID reuse, process-start mismatch, stale lock/socket/record, duplicate name, and provisional cleanup regressions pass. |
| S-06 | Repository and install plans contain no obsolete canonical-host or binary names. |
| S-07 | Gates leave the checkout clean except for the tested patch; user files and stashes are unchanged. |
| S-08 | Launcher drift runs against both the oldest supported and current accepted Codex CLI versions, records each exact version, and passes every discovered non-interactive command through byte-for-byte. An upstream Codex version change invalidates later live evidence until this cell is rerun. |

## Installation and upgrade (`I/U`)

Before mutation, require a clean source tree and exact migration inventory for every legacy
authority, live attachment, lane, native adapter resource, service, endpoint, and cleanup debt.
Installation must refuse named blockers and never infer safety from scalar counts.

| ID | Platforms | Assertion |
|---|---|---|
| U-01 | Linux, macOS | Clean install produces exact-revision role links, service assets, and plugin payloads. |
| U-02 | Linux, macOS | Same-revision reinstall is idempotent and preserves plugin data. |
| U-03 | Linux, macOS | Upgrade refuses named active attachment/lane or migration blockers before release mutation. |
| U-04 | Linux, macOS | Host or hub install does not stop/reload the other role or an unrelated peer. |
| U-05 | Linux, macOS | Codex hook/project trust prompts are surfaced; automation never approves them. |
| U-06 | Linux, macOS | Grok plugin trust is explicit; validation precedes replacement of only `agent-sessions`. |
| U-07 | macOS | Chat/desktop same-name CLIs are skipped/rejected without launching a GUI or updater. |
| U-08 | Linux, macOS | Plugin entry points select the exact same host image revision as their owner release. |
| U-09 | Release | Every release archive installs without Go and matches its published checksum. |
| U-10 | Linux, macOS | Malformed/unreadable Grok launch inventory fails closed with an inventory diagnostic, not false "peer still running" advice. |
| U-11 | Linux, macOS | Qwen install/upgrade/remove uses the exact selected profile, refuses a live managed user of that profile, verifies manifest/version/enabled/MCP/skill inventory, and preserves credentials, settings, other extensions, and transcripts. |
| U-12 | Linux, macOS | Two role images and four product plugin payloads install from a prebuilt archive without Go; a second build from the same tree is byte-identical. |

## Codex interactive peer (`T/F`)

| ID | Cell |
|---|---|
| C-01 | Fresh named zero-turn launch reaches composer and exits normally. |
| C-02 | Real prompt returns visible output; normal quit preserves one unarchived UUID. |
| C-03 | Resume by exact UUID and durable name reuses UUID after prompt changes DB title. |
| C-04 | Global flags before command and after target produce byte-identical native argv. |
| C-05 | Model, sandbox, approval, config, display, multi-value `--image`, attached values, and bare `--` remain intact. |
| C-06 | Repeated permission booleans follow native last-value semantics without classification drift. |
| C-07 | Renamed project without compatibility symlink resumes implicitly and with explicit cwd, then retains canonical cwd. |
| C-08 | Stale loaded zero-turn takeover unsubscribes before cwd override and never duplicates owner/thread/index row. |
| C-09 | Normal quit sends no archive/unarchive request, detaches exact live visibility, and preserves durable resume metadata. |
| C-10 | `identity`, `list_peers`, `rename_session`, `check_inbox`, and `send_message` work only for an attested caller. |
| C-11 | Wrong identity/token/owner ancestry fails closed before roster or message access. |
| C-12 | Idle message wakes; busy message steers/queues; ordered burst is exact-once. |
| C-13 | Destination returns exact acknowledgement with correct sender name, UUID, host, and `from-mode`. |
| C-14 | Plain/YOLO launch and resume agree across argv, App Server state, `/status`, registry, and outgoing label. |
| C-15 | Sticky resumed YOLO is verified; never-YOLO control stays constrained. |
| C-16 | Normal quit, Ctrl+C, generation interrupt, TUI SIGTERM, daemon restart, and App Server transport reconnect converge cleanly. |
| C-17 | Explicit archive is idempotent; archived behavior is correct; resume performs one unarchive and retains transcript. |
| C-18 | Missing sockets, stale owner, PID reuse, and interrupted publication recover without affecting another thread. |

## Claude interactive peer (`T/F`)

| ID | Platforms | Cell |
|---|---|---|
| CL-01 | Linux, macOS | Bare `claude` remains an opt-out and changes neither the Agent Sessions catalog nor registration set. |
| CL-02 | Linux, macOS | `claude-peer` starts a real native TUI in the configured shared Claude profile; ordinary, peer, and lane rows coexist with exactly one Agent Sessions host service row. |
| CL-03 | Linux, macOS | Two Claude peers use distinct preparation-bound sockets and discover one another through group-filtered AgentFrame routing; native Claude direct messaging remains independently available. |
| CL-04 | Linux, macOS | Exact UUID ordinary→peer→ordinary→generic-peer resume retains one shared transcript plus explicit groups, inheritance snapshot, name, cwd, and effective durable YOLO choice. `claude-peer --resume NATIVE_TARGET` passes non-UUID targets unchanged to Claude, including ordinary titles and duplicate-title chooser flows, ignores any transient boot UUID, then atomically promotes a cleanup-owned provisional attachment only after the selected transcript title/UUID is authoritative. The attachment alias still attests structured MCP and local/remote lane calls as the selected UUID. Named resumes publish the requested title immediately; exact-UUID resumes recover the latest validated native transcript title. No provisional or transient catalog row survives; explicit overrides replace only selected fields. |
| CL-05 | Linux, macOS | Explicit launch names and native transcript title/status changes refresh the daemon attachment without being replaced by Claude's cwd-derived row fallback or duplicated. Permission class is a stable launch decision: constrained peers disable the unobservable in-session bypass surface, while explicit bypass peers remain conservatively advertised as bypass until restart. |
| CL-06 | Linux, macOS | Daemon generation restart re-corroborates the exact idle Claude attachment without restarting Claude or creating a second service row. |
| CL-07 | Linux, macOS | Normal exit, Ctrl+C, SIGTERM, connector crash, stale native row, key/socket-before-row startup failure, PID reuse, and socket mismatch retire only the exact owned Claude attachment/artifacts, preserving unrelated shared rows and the service. |
| CL-08 | Linux, macOS | Managed Claude's structured MCP discover/direct/multicast/broadcast returns correlated group-filtered results and replies to an incoming delivery through Agent Sessions. Exact adapter/lifecycle ancestry is accepted without a model-supplied Codex ID; copied environment, unrelated registered process, bare caller, and native unframed service prose fail closed. The framed native carrier remains a compatibility path, while independent native direct traffic can cross Agent Sessions groups. |
| CL-09 | Linux, macOS | Claude→Codex, Claude→Claude, Claude→Grok, and Claude→Qwen lane launches all bind the immediate Claude parent, including nested and persistent children. |
| CL-10 | Linux, macOS | Profile/credential mismatch, live exact-UUID attachment, invalid launch settings, launcher crash, native startup failure, and failure before or after native named-target selection do not adopt or alter the ordinary session's catalog row; a durable gated-launch journal survives agent restart and rolls back groups and YOLO before retry. |
| CL-11 | Linux, macOS with Claude in Chrome installed | Ordinary shared-profile use may cache extension discovery; a later managed peer or lane still publishes its native row/socket without an interstitial. Peers preserve explicit `--chrome`/`--no-chrome`, while the managed default and all headless lanes use `--no-chrome`; host profile settings remain unchanged. |

## Grok interactive peer (`T/F`)

| ID | Platforms | Cell |
|---|---|---|
| G-01 | Linux, macOS | Chat Grok first and Grok Build later/fallback selects only Grok Build with fixed help probe. |
| G-02 | macOS | Direct, symlinked, dangling, and case-varied `*.app/Contents` candidates are rejected before execution. |
| G-03 | Linux, macOS | Explicit override is identically validated and never silently falls through. |
| G-04 | Linux, macOS | Genuinely cold `grok-peer` reaches usable TUI with all configured MCP servers started. |
| G-05 | Linux, macOS | Cold logs contain no reclaimed process scope, duplicate actor, or observer ownership failure. |
| G-06 | Linux, macOS | TUI, host, private leader, ACP observer, and MCP server identities match one launch record. |
| G-07 | Linux, macOS | Observer initializes/authenticates, sees one resident row, and sends neither `session/load` nor `session/prompt`. |
| G-08 | Linux, macOS | Ordinary Grok and `grok-peer` use equivalent auth/config for the same cwd; a foreign MCP OAuth failure remains server-attributed in Grok's MCP status/events and private leader/observer diagnostics never render as an unattributed fatal in the interactive TUI. |
| G-09 | Linux, macOS | Idle `x.ai/interject` visibly starts a turn without typing and returns exact acknowledgement. |
| G-10 | Linux, macOS | Busy/generating/tool-active interjection is received once without replacing the TUI actor. |
| G-11 | Linux, macOS | Burst, duplicate ID, conflicting reuse, reconnect, rejection, response-before-echo, missing actor echo, and ambiguous EOF preserve the documented local dedup/never-replay contract. |
| G-12 | Linux, macOS | Restart restores queued records but never replays ambiguous `in_flight` interjection. |
| G-13 | Linux, macOS | Exact-UUID, title, and bare-picker resume preserve native selection behavior, selected UUID/title, cwd, plugin access, structured messageability, and local/remote lane ownership. A cleanup-owned attachment alias is promoted only after one authoritative resident roster row; no provisional catalog row survives and an explicit peer name wins. |
| G-14 | Linux, macOS | Launch/config/admin policy and in-TUI changes converge across roster, record, registry, status, federation, and label. |
| G-15 | Linux, macOS | Failed permission publication retries; background refresh reports retryable busy instead of stale authority. |
| G-16 | Linux, macOS | Before exit, the host process group differs from the TUI's and the private leader/observer remain isolated; normal TUI exit then terminates only those owned processes and removes the MCP, sockets, locks, launch record, private directory, registry, and empty state directory. |
| G-17 | Linux, macOS | Ordinary `grok leader list`/`kill` is tested only on run-owned shared leaders, never used for a healthy private peer. |
| G-18 | Linux, macOS | Owner/host/leader death, auth failure, roster ambiguity, PID reuse, and stale records clean up without killing unrelated clients. |
| G-19 | Linux, macOS | Direct `agent_sessions.list_peers` readiness failure blocks first publication; catalog omission and unrelated MCP failures do not, and the already-running server never rejects itself through a recursive readiness check. |
| G-20 | Linux, macOS | After actor acceptance, a Grok-deferred roster request cannot block the interjected turn's own `agent_sessions` calls; the labelled pre-interjection permission snapshot retires on the first successful post-turn roster refresh and expires after 30 minutes if recovery remains broken. |
| G-21 | Linux, macOS | Global roster pushes plus one-second reconciliation publish `working`/`needs_input`/`idle` as busy/waiting/idle; initial non-idle state is retained, old observer generations and turn-boundary stale polls cannot overwrite it, and removed/terminal/nonresident/ambiguous actors withdraw the peer. |

## Qwen interactive peer (`T/F`)

| ID | Platforms | Cell |
|---|---|---|
| Q-01 | Linux, macOS | Bare `qwen` remains an opt-out; a cold `qwen-peer` uses the exact selected profile and reaches a usable native TUI. |
| Q-02 | Linux, macOS | Session-free readiness proves package/version, presence-sensitive profile, parser semantics, ACP initialize/capabilities, native archive, trusted cwd, and exact plugin inventory without creating a transcript or reading secrets. |
| Q-03 | Linux, macOS | Fresh and exact managed resume retain UUID, canonical cwd, native transcript, groups, name, and selected profile; continue/fork and ambiguous managed selectors fail before mutation. |
| Q-04 | Linux, macOS | Native-default, exact `--no-yolo`→initial `default`, `--yolo`, and admitted native `--approval-mode` launches match argv and durable launch preference; conflicting flags fail before a child exists. Native in-session mode changes remain Qwen-owned. |
| Q-05 | Linux, macOS | Dual-output admission requires the exact session-start UUID/cwd/version/protocol/event inventory; truncation, replacement, path-type change, native authentication failure, and malformed/out-of-order events fail closed. |
| Q-06 | Linux, macOS | The published session-stable endpoint is a real `0600` Unix socket, not a symlink. Direct native delivery succeeds at the advertised path with no caller-side resolution or extra hub round trip. |
| Q-07 | Linux, macOS | Attested Qwen MCP discovery, direct send/reply, atomic multicast, named-group broadcast, idle/busy delivery, and deduplication work; copied environment, bare Qwen, stale process, wrong profile, wrong capability, and model-supplied ID fail closed. |
| Q-08 | Linux, macOS | Agent restart republishes the same live peer without restarting Qwen or duplicating the service/participant. |
| Q-09 | Linux, macOS | Normal exit, Ctrl+C, SIGTERM, wrapper/native crash, failure before/after publication, recycled PID, replaced file/socket, and legacy symlink cleanup remove only exact owned state and retain retryable debt on ambiguity. |
| Q-10 | Linux, macOS | Ordinary→managed→ordinary transcript use requires no credential, settings, extension, skill, or transcript migration and leaves no Agent Sessions authority in the ordinary attachment. |

## Pairwise peer messaging (`T/X`)

For each applicable pair, send a unique marker while destination is idle and
busy, then require the exact return marker:

| Source | Destinations |
|---|---|
| Codex peer | Codex, Claude, Grok, Qwen peer/lane |
| Claude peer | Codex, Claude, Grok, Qwen peer/lane |
| Grok peer | Codex, Claude, Grok, Qwen peer/lane |
| Qwen peer | Codex, Claude, Grok, Qwen peer/lane |
| Codex lane | Codex, Claude, Grok, Qwen peer/lane |
| Claude lane | Codex, Claude, Grok, Qwen peer/lane |
| Grok lane | Codex, Claude, Grok, Qwen peer/lane |
| Qwen lane | Codex, Claude, Grok, Qwen peer/lane |

Repeat same-host, Linux→macOS, macOS→Linux, and Linux→Linux. Repeat with the
destination offline then resumed and across daemon/hub reconnect. Enqueue-only
evidence is `RED`.

## Codex, Claude, Grok, and Qwen lanes (`L/F/X`)

Run every applicable cell for local and remote Codex, Claude, Grok, and Qwen lanes, owned in turn by a
live Codex peer, a live Claude peer, a live Grok peer, and a live Qwen peer. Grok and Qwen lane federation are explicitly
implemented. Use real turns and unique names.

| ID | Cell |
|---|---|
| L-01 | Local `doctor` reports exact product, `ready: true`, and authority `daemon`; remote doctor reports `remote-daemon`, exact host/product, and an advertised ready-product capability. |
| L-02 | Foreground `run` completes a real inference turn, thereby proving end-to-end authentication, and emits coherent JSONL with the exact expected final answer. |
| L-03 | `start` returns `lane.ready`; `status` is busy; `wait` collects matching turn once. |
| L-04 | `wait --timeout` does not interrupt; later wait collects the turn. |
| L-05 | Idle peer message starts one collectable follow-up; busy message follows product semantics. |
| L-06 | Lane replies to owner and messages every applicable peer/lane destination. |
| L-07 | `resume` reuses exact identity and refuses while prior work is owed. |
| L-08 | `interrupt` yields correct raw status, normalized outcome, exit, collection, and a notice where that product supports notices. |
| L-09 | Execution timeout yields `timed_out`/124; collection timeout does not mutate work. |
| L-10 | Archive is idempotent, removes discovery, retains transcript, and reports dropped notices where notices are supported. |
| L-11 | Archived resume performs supported unarchive exactly once and preserves identity/transcript. |
| L-12 | Auto-archive deadline, cancellation, custom grace, and no-auto-archive match docs. |
| L-13 | Duplicate name and invalid owner/capability/product remove provisional worktree and adapter resources. |
| L-14 | User dirty worktree and archived lane worktree are preserved. |
| L-15 | Owner exit interrupts/archives parent-owned work; persistent lane and unrelated lanes survive. |
| L-16 | Native worker/adapter/daemon-generation recovery restores terminal items, cursor, supported notices, and cleanup once without redispatch. |
| L-17 | Permission inheritance requires exact local owner; explicit lane policy always wins. |
| L-18 | Remote run/start/resume/wait/status/interrupt/archive/list preserve streams, exit, cwd, notify, and cleanup fuse. |
| L-19 | Remote stdin cap, unsupported `--prompt-file`, hub loss, and disabled destination capability fail closed. |
| L-20 | Grok persists its ACP-created native UUID beside the stable lane UUID; `session/load` reuses that exact native identity under one sole ACP driver and never attaches to an interactive Grok conversation. |
| L-21 | Grok idle/busy inbound messages serialize into collectable turns; duplicate message IDs are idempotent and conflicting reuse fails closed. |
| L-22 | Grok headless policy is explicitly always-approve, published as bypass, and never inferred from an uncorroborated owner or downgraded to an unusable prompting mode. |
| L-23 | Codex and Claude plugins ship a valid Grok-lane skill; its executable Claude preflight and linked contract/install references survive staging and packaging and require the exact daemon doctor authority/product. |
| L-24 | Grok `lane.status`/`lane.list` report stable and native identities, lifecycle state, collection debt, owner/persistence, and auto-archive policy; every collected terminal result is `turn.completed` with explicit status/outcome/exit. |
| L-25 | Grok normal archive and daemon-generation recovery retire exact adapter-owned ACP/native resources on Linux and macOS while preserving unrelated vendor processes; ambiguous process ancestry becomes cleanup debt and is never guessed or killed heuristically. |
| L-26 | A Codex lane that fails before its first rollout exists remains archivable by exact thread ID. Only an authoritative missing-rollout response plus a local failed record with no turn evidence permits `thread/delete`; transport ambiguity or any turn evidence fails closed. |
| L-27 | Qwen persists its ACP-created native UUID beside the stable lane UUID and uses one sole ACP client for initialize/new-or-resume, serialized prompt, mode request, and cancel. |
| L-28 | Qwen native archive/unarchive uses one bounded token-authenticated loopback helper, exact workspace/UUID, idempotent conflict handling, compensation, and zero helper/preheated-child residue. |
| L-29 | Qwen native resource crash, PID reuse, daemon restart, and cleanup retry preserve unrelated processes and converge durable cleanup debt. |
| L-30 | Qwen launch preference, expected initial mode, and observed current mode or `unknown` remain distinct; Qwen-native later mode changes are neither blocked nor treated as routing authority. |

## Federation (`X/F`)

| ID | Cell |
|---|---|
| X-01 | Local-only startup publishes exactly one host service row; grouped discovery/direct/multicast/broadcast work without a hub. |
| X-02 | Same-name visible peers are ambiguous, same-name peers in disjoint groups do not interfere, and hidden identities do not leak. |
| X-03 | Pairwise messaging and lane lifecycle pass Linux↔macOS and Linux↔Linux without per-peer shadows. |
| X-04 | Every snapshot peer has an exact host/session identity, protocol, product, instance, groups, and its own private anchor; malformed replacement snapshots retain the last valid roster. |
| X-05 | Hub and host-daemon restart restore one service row and attachment snapshot without duplicates or peer restart. |
| X-06 | Legacy flat delivery, duplicate resolved multicast recipients, invalid source/product/group, and a broadcast to a non-member group are rejected before delivery. |
| X-07 | Remote parent context retains the source-host private anchor; optional parent groups propagate only after explicit `--inherit-groups`. |
| X-08 | Install/tests never stop or reload an unrelated live host agent or hub. |

## Parent layer × target layer (`P/L/X`)

Run every local pair and every supported federated pair, not just different-product pairs:

| Parent | Codex lane | Claude lane | Grok lane | Qwen lane |
|---|---|---|---|---|
| Codex | required | required | required | required |
| Claude | required | required | required | required |
| Grok | required | required | required | required |
| Qwen | required | required | required | required |

For every cell prove the child gets its own private group plus the immediate
parent anchor, does not inherit other parent groups by default, inherits them
only with explicit `--inherit-groups`, and restores that choice on resume. A
three-level chain must bind a grandchild to its immediate parent rather than
the original root. Each cell also covers parent↔child messaging, terminal
notice/collection, interrupt, archive, exact resume, parent exit,
`--persistent`, and target-owned cleanup. The target adapter remains
product-specific; the parent/group layer is shared.

## Archive and unarchive contract

Every product declares archive as native, daemon-owned through its adapter, or `N/A`. For supported
products, record visibility and exact archive/unarchive counts for normal quit,
explicit/repeated archive, message to archived target, resume-triggered
unarchive, repeated resume, same-name reuse, and remote propagation. Normal
interactive quit is not archive unless that product explicitly documents it.

## Parked adapters

An adapter that cannot wake a genuinely idle interactive prompt without human
input is `PARKED`, even if enqueue and next-invocation delivery work. It may be
tested for CLI/GUI resolution, plugin safety, identity, MCP, queued delivery,
lane ownership, and cleanup, but it is not an interactive peer. Requiring the
user to type `.` for each message is a failed wake cell.

## Minimum evidence before saying “ready”

For a new peer adapter: exact installed cold launch, visible real response,
idle wake, busy wake, two-way messaging, all four lane targets and parent roles,
message flow, resume, permission observation, normal shutdown, crash recovery,
and cleanup on Linux and macOS. All source gates must also pass. Any missing
live cell is `BLOCKED` or pending—not green.
