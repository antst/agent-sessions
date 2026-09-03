# Acceptance matrix

The manifest at `specs/002-unified-user-daemon/contracts/acceptance-matrix.yml` expands the stable
cell IDs below and validates every result independently. Real-product evidence is mandatory for
native behavior; unit or mock evidence cannot receive that credit.

## Source and packaging (`S`)

| ID | Assertion |
| --- | --- |
| S-01 | Normal, race, vet, lint, JavaScript, and shell integration tests pass uncached. |
| S-02 | Stock macOS Bash runs the shell gate. |
| S-03 | Every platform archive contains its declared binaries and integration payloads exactly once. |
| S-04 | Product validation never launches an unvalidated same-name executable or GUI. |
| S-05 | PID reuse, stale socket, duplicate selector, provisional identity, and process-cleanup regressions pass. |
| S-06 | Supported source, package, install, service, help, plugin, and executable surfaces contain only current names. |
| S-07 | Gates preserve the checkout and user-owned files outside the tested patch. |
| S-08 | Codex argument forwarding passes against the supported CLI boundary and preserves unknown native arguments byte-for-byte. |

## Installation and upgrade (`I/U`)

| ID | Platforms | Assertion |
| --- | --- | --- |
| U-01 | Linux, macOS | Clean install produces release-owned links, integrations, runtime paths, and one service. |
| U-02 | Linux, macOS | Same-release reinstall is idempotent and preserves product-owned data. |
| U-03 | Linux, macOS | Upgrade restarts only the Agent Sessions user service and leaves product session stores intact. |
| U-04 | Linux, macOS | Install does not reload another user's service, the hub, or unrelated product processes. |
| U-05 | Linux, macOS | Product trust and login prompts remain product-owned and are never approved by installation. |
| U-06 | Linux, macOS | Integration validation precedes atomic selection of the new release. |
| U-07 | macOS | Same-name desktop applications are rejected as CLI candidates without execution. |
| U-08 | Linux, macOS | Every installed integration resolves the exact selected host runtime. |
| U-09 | Release | Platform archives install without Go and match their published checksums. |
| U-10 | Linux, macOS | Malformed product launch inventory fails with its exact diagnostic. |
| U-11 | Linux, macOS | Qwen installation uses the selected profile and preserves settings, credentials, other extensions, and transcripts. |
| U-12 | Linux, macOS | The release contains exactly `agent-sessions` and `agent-sessions-hub` executable images; host and hub targets select only their role. |

## Codex interactive peer (`T/F`)

| ID | Cell |
| --- | --- |
| C-01 | Fresh named launch reaches the native composer and exits normally. |
| C-02 | A real prompt returns output while retaining one product thread UUID. |
| C-03 | Name and exact-ID resume reuse the product thread; ambiguity uses the shared picker because external native choice is not observable. |
| C-04 | Global flags before or after the resume target project to equivalent native argv. |
| C-05 | Model, sandbox, approval, config, display, image, attached-value, and `--` arguments remain intact. |
| C-06 | Repeated native permission booleans retain product last-value semantics. |
| C-07 | Explicit and invocation cwd resume use the product's own project behavior. |
| C-08 | One native thread maps to one live peer without an alias identity. |
| C-09 | Normal quit sends no archive request and removes the live presence row without product deletion. |
| C-10 | Identity, peer listing, rename, messaging, and lane calls use host-supplied thread metadata. |
| C-11 | Missing or non-live host thread metadata is refused without exposing another roster. |
| C-12 | Idle inbound starts and busy inbound steers through the App Server exactly once. |
| C-13 | Delivery carries the exact sender UUID, product-owned name, product, and groups. |
| C-14 | Plain and permissive start/resume agree across native argv and live projection. |
| C-15 | A zero-option resume sends no remembered launch selection and uses product defaults. |
| C-16 | Quit, signal, daemon replacement, and App Server replacement converge without duplicate actors. |
| C-17 | Explicit archive is idempotent; resume unarchives only when Codex itself reports the thread archived. |
| C-18 | Missing sockets, stale processes, and interrupted publication do not affect another thread. |

## Claude interactive peer (`T/F`)

| ID | Platforms | Cell |
| --- | --- | --- |
| CL-01 | Linux, macOS | Bare Claude remains outside Agent Sessions. |
| CL-02 | Linux, macOS | `claude-peer` runs the native TUI while ordinary peers and lanes coexist under one daemon. |
| CL-03 | Linux, macOS | Two peers hold distinct native UUIDs and use group-filtered protocol-v1 routing. |
| CL-04 | Linux, macOS | Native name/ID resume retains the selected transcript and uses the current invocation's groups and options. |
| CL-05 | Linux, macOS | Native transcript title changes appear on the next roster and route-by-name query. |
| CL-06 | Linux, macOS | A retained live connector re-reports the same native session after daemon replacement. |
| CL-07 | Linux, macOS | Exit and connection failure retire only the exact live row and preserve Claude-owned data. |
| CL-08 | Linux, macOS | Structured list/send/broadcast and replies work with host-supplied identity and protocol-v1 delivery. |
| CL-09 | Linux, macOS | A Claude parent can launch each catalogued lane product with correct parent context. |
| CL-10 | Linux, macOS | Invalid launch, native startup failure, and selector failure leave no false live row or altered product session. |
| CL-11 | Linux, macOS with Claude in Chrome installed | Explicit Chrome choices remain native while managed headless lanes never wait on Chrome interaction. |

## Grok interactive peer (`T/F`)

| ID | Platforms | Cell |
| --- | --- | --- |
| G-01 | Linux, macOS | Discovery selects the validated Grok Build CLI, not another same-name product. |
| G-02 | macOS | Application-bundle and case-varied candidates are rejected before execution. |
| G-03 | Linux, macOS | An explicit executable override is validated and never falls through silently. |
| G-04 | Linux, macOS | A cold peer reaches the native TUI with configured MCP services. |
| G-05 | Linux, macOS | Cold launch creates no duplicate leader, actor, connector, or daemon. |
| G-06 | Linux, macOS | TUI, private leader, primary connection, observer, and MCP share one product session ID. |
| G-07 | Linux, macOS | The observer attaches to the resident session without prompting it. |
| G-08 | Linux, macOS | Product authentication and third-party MCP errors remain product-attributed. |
| G-09 | Linux, macOS | Idle native interject starts a turn and acknowledges acceptance. |
| G-10 | Linux, macOS | Busy native interject changes the current run exactly once. |
| G-11 | Linux, macOS | Duplicate and conflicting request IDs preserve exact-once response correlation in memory. |
| G-12 | Linux, macOS | Reconnect never invents or durably replays an ambiguous product input. |
| G-13 | Linux, macOS | Native name/ID resume retains the selected UUID, title, cwd behavior, tools, and messaging. |
| G-14 | Linux, macOS | Native title and mode changes appear in live roster projections. |
| G-15 | Linux, macOS | Product refusal reaches the caller verbatim without stale permission data. |
| G-16 | Linux, macOS | Normal exit removes only the owned leader, primary connection, observer, and connector tree. |
| G-17 | Linux, macOS | Administrative leader commands are never used to discover or kill an unrelated client. |
| G-18 | Linux, macOS | Owner, daemon, or leader failure preserves unrelated Grok sessions. |
| G-19 | Linux, macOS | Readiness proves the actual peer tool path without recursive self-admission. |
| G-20 | Linux, macOS | A running turn may call Agent Sessions without blocking on a second roster owner. |
| G-21 | Linux, macOS | Product events project live working, waiting, idle, and gone states without a durable copy. |

## Qwen interactive peer (`T/F`)

| ID | Platforms | Cell |
| --- | --- | --- |
| Q-01 | Linux, macOS | Bare Qwen remains outside Agent Sessions; `qwen-peer` uses the selected product profile. |
| Q-02 | Linux, macOS | Readiness proves the version, profile, extension inventory, and native protocol without creating a session. |
| Q-03 | Linux, macOS | Fresh and exact resume retain the native UUID, title, cwd behavior, transcript, and current groups. |
| Q-04 | Linux, macOS | Default and permissive launches map to the product's native approval mode; omission sends nothing. |
| Q-05 | Linux, macOS | The native session-start event supplies the exact product UUID before live publication. |
| Q-06 | Linux, macOS | The held protocol-v1 presence path is private and addresses the exact live session. |
| Q-07 | Linux, macOS | Structured discovery, direct/multicast/broadcast, reply, and native busy behavior are exact once. |
| Q-08 | Linux, macOS | Reconnect republishes the same live product UUID without duplicating it. |
| Q-09 | Linux, macOS | Exit, signal, crash, and publication failure remove only Agent Sessions-owned live state. |
| Q-10 | Linux, macOS | Ordinary and managed use share product credentials, settings, extensions, and transcripts without migration. |

## Pairwise peer messaging (`T/X`)

Each cell requires destination-visible acceptance and an exact return marker. The same matrix is
exercised locally and across supported host pairs.

| Source \ destination | Codex peer | Claude peer | Grok peer | Qwen peer | Codex lane | Claude lane | Grok lane | Qwen lane |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Codex peer | M-CP-CP | M-CP-CLP | M-CP-GP | M-CP-QP | M-CP-CL | M-CP-CLL | M-CP-GL | M-CP-QL |
| Claude peer | M-CLP-CP | M-CLP-CLP | M-CLP-GP | M-CLP-QP | M-CLP-CL | M-CLP-CLL | M-CLP-GL | M-CLP-QL |
| Grok peer | M-GP-CP | M-GP-CLP | M-GP-GP | M-GP-QP | M-GP-CL | M-GP-CLL | M-GP-GL | M-GP-QL |
| Qwen peer | M-QP-CP | M-QP-CLP | M-QP-GP | M-QP-QP | M-QP-CL | M-QP-CLL | M-QP-GL | M-QP-QL |
| Codex lane | M-CL-CP | M-CL-CLP | M-CL-GP | M-CL-QP | M-CL-CL | M-CL-CLL | M-CL-GL | M-CL-QL |
| Claude lane | M-CLL-CP | M-CLL-CLP | M-CLL-GP | M-CLL-QP | M-CLL-CL | M-CLL-CLL | M-CLL-GL | M-CLL-QL |
| Grok lane | M-GL-CP | M-GL-CLP | M-GL-GP | M-GL-QP | M-GL-CL | M-GL-CLL | M-GL-GL | M-GL-QL |
| Qwen lane | M-QL-CP | M-QL-CLP | M-QL-GP | M-QL-QP | M-QL-CL | M-QL-CLL | M-QL-GL | M-QL-QL |

## Codex, Claude, Grok, and Qwen lanes (`L/F/X`)

These stable IDs now apply to every catalogued lane product where the capability is applicable.

| ID | Cell |
| --- | --- |
| L-01 | Doctor proves the installed native runtime, integration, and required capability. |
| L-02 | Foreground run completes a real product inference and returns its terminal result. |
| L-03 | Detached start, status, and wait correlate one exact native turn. |
| L-04 | Wait timeout does not interrupt or consume a later result. |
| L-05 | Idle and busy inbound follow the product's one native input path. |
| L-06 | The lane lists and messages its owner and visible destinations through protocol v1. |
| L-07 | Resume reuses one exact native session ID. |
| L-08 | Interrupt reports the product terminal and the session accepts a later turn. |
| L-09 | Execution timeout and collection timeout remain distinct. |
| L-10 | Archive removes live addressability without deleting the product session. |
| L-11 | Offline resume product-confirms and reopens the exact native session. |
| L-12 | Default, custom, and disabled auto-archive apply to the current invocation. |
| L-13 | Duplicate names and invalid launch input create no second product session. |
| L-14 | User files and product-owned histories survive lane lifecycle operations. |
| L-15 | Parent EOF archives idle nonpersistent lanes and releases persistent ownership correctly. |
| L-16 | Daemon replacement leaves workers non-live and sessions recoverable through product confirmation. |
| L-17 | Permission comes from the current invocation and is never widened implicitly. |
| L-18 | Remote lifecycle preserves result, stderr, exit, cwd, identity, and cleanup semantics. |
| L-19 | Oversized input, missing capability, and hub loss fail before unbounded native work. |
| L-20 | Every driver exposes one product-native identity after Open. |
| L-21 | Native message receipt/rejection is returned synchronously without a daemon queue. |
| L-22 | Headless policy either resolves noninteractively or fails the product turn truthfully. |
| L-23 | Installed skills and aliases name the structured lane contract accurately. |
| L-24 | Status/list expose session ID, state, result, ownership, persistence, and current auto-archive facts. |
| L-25 | Archive and service stop remove only the exact daemon-owned worker tree. |
| L-26 | Failure before the first turn leaves a truthful lane that remains explicitly archivable. |
| L-27 | Product-generated identity is atomically re-keyed; caller-supplied identity is accepted exactly. |
| L-28 | Product-specific archive behavior is invoked only where the product actually provides it. |
| L-29 | Worker and connector crashes preserve unrelated processes and product sessions. |
| L-30 | Model, agent, effort, permission, and group choices are invocation-owned; omission restores product defaults. |

## Federation (`X/F`)

| ID | Cell |
| --- | --- |
| X-01 | Local discovery and messaging work with one daemon and no hub. |
| X-02 | Duplicate visible names are ambiguous while group-hidden identities do not leak. |
| X-03 | Peer messaging and lane lifecycle cross Linux/macOS host pairs through one hub. |
| X-04 | A complete roster carries one valid host/native identity and private anchor per peer. |
| X-05 | Hub and daemon reconnect replace rosters without duplicate product sessions. |
| X-06 | Invalid source, product, group, target, and duplicate multicast are rejected before delivery. |
| X-07 | Remote parent context retains the source private anchor and explicit inheritance choice. |
| X-08 | Tests and install do not stop another user's host daemon or the central hub. |

## Parent layer × target layer (`P/L/X`)

Each cell runs the target lane from the named live parent and verifies native identity, parent groups,
tools, messaging, result, and cleanup.

| Parent | Codex lane | Claude lane | Grok lane | Qwen lane |
| --- | --- | --- | --- | --- |
| Codex | P-C-C | P-C-CL | P-C-G | P-C-Q |
| Claude | P-CL-C | P-CL-CL | P-CL-G | P-CL-Q |
| Grok | P-G-C | P-G-CL | P-G-G | P-G-Q |
| Qwen | P-Q-C | P-Q-CL | P-Q-G | P-Q-Q |

## Archive and unarchive contract

| ID | Product |
| --- | --- |
| A-C | Codex peer archive and product-confirmed unarchive. |
| A-CL | Claude peer native archive behavior or explicit not-applicable evidence. |
| A-G | Grok peer archive and exact native resume. |
| A-Q | Qwen peer archive and exact native resume. |

## Current credit

Peer mode is credited for Codex, Claude, Grok, Qwen, OpenCode, Kilo, Pi, and OMP. Lane mode is
credited for those eight products plus DSH.

Qwen lane credit remains provisional until the quota-blocked outbound peer and fresh lane cells are
rerun after 2026-09-04 16:07 UTC. Claude accepts `--effort` natively
(`low|medium|high|xhigh|max`) and records it per session; the product exposes no effort echo in
transcripts or results, so its runtime effect is product-owned and not asserted here.
