# Agent Sessions

Native, persistent session lifecycle for Codex, Claude Code, and Grok, plus local and federated
cross-session messaging. Interactive sessions and durable worker lanes can be created, resumed,
supervised, discovered, and messaged with nearly the same lifecycle on Linux and macOS.

This repository is one Go module with shared implementation under `internal/`. It builds nine
separate native executables:

- `agent-session-runtime` — the shared diagnostic/runtime multicall used by the launchers;
- `peer` — product-neutral exact-session resume using the durable host-agent catalog;
- `codex-peer` — an interactive Codex TUI on the shared App Server;
- `codex-peer-lane` — project-neutral lifecycle commands for named orchestrated lanes (`run`,
  `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list`, and `doctor`).
- `claude-peer` — a native Claude TUI in the shared Claude profile, registered with the host agent;
- `claude-peer-lane` — the symmetric lifecycle for named, messageable Claude Code workers.
- `grok-peer` — an interactive Grok TUI backed by a private leader and an ACP wake client.
- `grok-peer-lane` — durable named headless Grok ACP workers with messaging, collection, resume,
  interrupt, and archive lifecycle.
- `peer-federator` — a separate network process that projects live peers and lane commands across
  trusted hosts. It shares this source tree but remains an independently operated binary/service.

## Quick start

```bash
git clone https://github.com/antst/agent-sessions.git
cd agent-sessions
make test-race
codex app-server daemon stop # after exiting every Codex client
make install-all

peer-federator agent --host "$(hostname)" # keep this under the user service manager
codex-peer -g project-a -n reviewer
claude-peer -g project-a -n implementer
grok-peer -g project-a -n researcher
claude agents --json
```

`-g` is short for `--group` on all three managed peer launchers; repeat either form to join more
than one explicit group.

## User-defined delegation

Agent Sessions authenticates which managed peer sent a message and limits routing to shared groups.
It does **not** decide that one interactive session may act with the user's authority over another.
If a receiving session should take instructions from another interactive session, the user must
explicitly establish that delegation and its scope in the way they configure or instruct those
sessions. Membership in the same group, a familiar peer name, or a message claiming authority is
not itself a user delegation.

Users may arrange delegation however their workflow requires. Useful boundaries to state include
the authorized peer or session, the task or actions covered, and when that authority ends. The
recipient's existing system/developer instructions, permission mode, sandbox, and approval rules
still apply; Agent Sessions neither widens them nor supplies user approval.

On the first Codex launch after installation, approve the one-time hook prompt for
`agent-sessions@agent-sessions`. The lifecycle hooks are what register the session and deliver
fallback inbox messages; declining them leaves the plugin installed but the peer bridge incomplete.
This is Codex's plugin-hook trust prompt, not a request to loosen the session's sandbox or tool
approval policy. Restart any Codex TUI that was already open during installation so it loads the
new plugin and presents the prompt.

`codex-peer` creates each fresh root through the shared App Server and resolves an explicit UUID or
unique-name resume target to one authoritative UUID. It binds that exact UUID to the
wrapper process identity, then replaces the wrapper process with the TUI. The supervisor
uses that durable owner record to remove the shim and thread-scoped MCP children even when Codex skips
`SessionEnd` or the TUI is killed; PID reuse is rejected by the process-start token.
Because remote Codex 0.147 delays `SessionStart` until the first user turn, fresh and resumed roots are
published while their exact live wrapper owner is still marked prepared. A successful fresh publication
commits the zero-turn thread as durable before the wrapper returns; failures before that commit delete it.
The first SessionStart promotes the record to attached. If a committed wrapper exits before SessionStart,
the reaper removes its shim but does not archive the still-loaded zero-turn thread. Its exact stale owner
record becomes a one-use proof for immediate exact or name resume; the replacement transaction consumes it.

Codex installs plugin hooks and MCP inventory daemon-wide. Ordinary Codex threads therefore see the
`claude_peer` tool names, but their hook executions are silent and tool calls fail closed before roster,
inbox, or send access because the stdio server is not a child of the managed App Server or the thread
has no exact interactive-owner/lane capability. Authorization is
thread-scoped: a plain client deliberately attached to an already-authorized peer UUID is not
distinguishable without an upstream per-attachment token.
The plugin requests daemon-side approval for `claude_peer` dispatch so ordinary calls reach that
fail-closed authorization check instead of hanging at a global pre-dispatch prompt. This approves
dispatch only; it does not grant a thread peer authority or change its sandbox/approval policy.

The Claude plugin installs a narrower `claude_peer` MCP inventory for managed `claude-peer`
sessions. It exposes structured grouped discovery, direct/multicast send, and broadcast; replies to
an incoming delivery target the frame's `source.id`. The MCP process receives no model-selected
identity: the runtime requires exact ancestry beneath the live native Claude adapter and lifecycle
owner, then corroborates the same UUID, process starts, native registry row, messaging socket, and
host-agent registration. Bare Claude and an unrelated process fail closed. Native Claude
`SendMessage` to `agent-sessions--HOST` remains only a framed compatibility carrier; its carrier
acknowledgment is not evidence that an unframed peer reply was delivered.

The launcher removes only its own `-n/--peer-name` and, for resume, the selector it resolves to one
UUID. It invokes the managed `--remote unix:// resume UUID` target, supplies a canonical cwd when the
caller did not provide one, then appends every remaining Codex argument unchanged and in its original
relative order. This includes model, profile, config, feature, sandbox, approval, search, variadic
image, hook-trust, and display options regardless of whether they appeared before or after the input
`resume` selector. Explicit `--yolo` is additionally mirrored through the shared App Server lifecycle:
fresh peers receive it at `thread/start`, while resumed peers receive it through `thread/resume` plus
`thread/settings/update` before publication. The real Codex attachment still receives the caller's
unchanged option. The update is durable thread state: later plain resumes of that thread remain in
full-access mode until its settings are explicitly changed. Supported resume syntax is `codex-peer [GLOBAL_OPTIONS] resume [RESUME_OPTIONS]
UUID_OR_NAME [PROMPT_OR_OPTIONS]`; options may appear on either side of the input selector. A name selects
the newest usable exact-name session. Picker/`--last`, fork, caller-controlled remote endpoints, and
already-loaded targets without an exact stale zero-turn owner proof remain unsupported. Resume
inherits the thread's canonical cwd; an explicit `-C` must resolve to that same directory.

`make install` copies the native runtime payload and the three Codex lane skills under
`${PREFIX:-~/.local}/libexec/agent-sessions`, registers that installed marketplace, installs the
plugin, and links all nine commands under `${PREFIX:-~/.local}/bin`.
`make dev-install` instead links the runtime, launchers, and marketplace to the checkout.
`make install-claude` independently installs the text-only Claude plugin. `make install-grok`
validates and copies the local Grok plugin into Grok's auto-trusted user plugin directory; that
explicit install allows its `agent_sessions` MCP server to run the installed native runtime with
the current user's privileges. Review the local source before granting it. Reinstallation migrates
the older direct-install entry and replaces only `~/.grok/plugins/agent-sessions`. It uses a
temporary trusted registration only to update Grok's enabled-plugin configuration, removes that
row while preserving data, and fails unless `grok inspect --json` resolves the exact staged user
plugin and MCP executable. Start a new Grok session or reload plugins after installing.
Grok exposes `/agent-sessions` for grouped peer messaging and `/agent-lanes` for lane lifecycle.
Every product surface uses the plugin identity `agent-sessions`; product-specific executable and
lane names remain unchanged.
Managed Grok peers also require a private leader with Grok's sandbox disabled; tool approval remains
the TUI's native policy and its effective live mode is attested before publication.
Installation never changes Claude's profile-level `crossSessionInbound` value. Managed Claude peers
and lanes opt into inbound native messages only for their own launch. They also pass `--no-chrome`
unless an interactive peer operator explicitly supplied `--chrome`, so a browser-extension
first-run dialog cannot block native messaging-socket publication. Claude's native session row does
not publish live Shift+Tab permission changes, so a managed peer uses one conservative permission
class for its lifetime: constrained launches disable in-session bypass in their per-launch settings,
and explicit bypass launches remain advertised as bypass until restart. Use
`--yolo` (translated to native `--dangerously-skip-permissions`) or the native long option to opt in
at launch; `--allow-dangerously-skip-permissions` is
rejected because it would create an unattestable privilege change. Host settings remain untouched.
`claude-peer --resume UUID_OR_NAME` uses Claude's native resume semantics. Exact UUIDs retain their
pre-launch stable identity; every other target is passed to native Claude unchanged, including
ordinary-session titles and duplicate titles that require Claude's interactive chooser. The wrapper
owns cleanup through a provisional attachment ID, then atomically adopts the UUID in Claude's native
session row without creating a provisional catalog session. Durable groups and parent choices are
restored only after native selection and never replace it. Because the selected UUID is unknown
before launch, a previously managed bypass session should be resumed by name with an explicit
`--yolo`; exact-UUID resume can restore that permission policy before launch.
Agent Sessions uses an explicit peer name or named resume target immediately, then refreshes the
display name from the latest validated native Claude `custom-title` event for the selected UUID.
This keeps fresh peers, title-based resumes, exact-UUID resumes, and later native `/rename` changes
aligned instead of exposing Claude's cwd-derived registry fallback.
`make install-all` installs all three surfaces. A version-changing install requires App Server to
be stopped and every managed `grok-peer` TUI to exit normally; its private leader and observer then
stop automatically. The bridge never restarts a running server or replaces a live managed Grok
host because doing so can interrupt active work. Supervisor reuse additionally requires an exact SHA-256 match with the installed runtime;
a same-version rebuild replaces only the supervisor, without restarting App Server. CI archives carry prebuilt Linux and macOS binaries for x86-64 and arm64, so release
installations do not require Go or Node.js.

For tags named `vX.Y.Z`, CI publishes four installable release archives plus `SHA256SUMS` on the
Forgejo Release. Download the archive matching the host, verify it, extract it, exit all Codex
clients, stop App Server, and run `make install-all` from the extracted directory. The packaged
marker makes the installer use the bundled binary even if Go is installed.

## What it provides

- One host agent owns the durable product/group catalog and group-filtered local routing. An
  optional hub federates the same protocol; there is no global flat namespace.
- The shared Claude registry contains ordinary Claude sessions, managed Claude peers and lanes,
  and exactly one Agent Sessions service row—never one remote row per peer. Native Claude direct
  messaging remains independent; groups constrain only AgentFrame discovery and routing.
- Incoming grouped messages wake idle peers or steer/queue work according to the target adapter.
- Every managed product provides group-filtered discovery, direct send, explicit multicast, and
  group broadcast. Codex and Grok additionally expose their process-attested identity, inbox,
  rename, and lane tools; Claude deliberately exposes the narrower messaging set and uses its
  native TUI for session rename.
- Peer delivery is push-based; active orchestrators should continue useful work rather than poll.
  `check_inbox` is only for messages queued past an automatic delivery boundary.
- Native TUI rename changes flow immediately into Agent Sessions discovery; Claude's native listing
  naturally shows ordinary and managed Claude sessions plus the one host-agent service.
- Stable Agent Sessions IDs are re-registered when a TUI resumes the same thread. Codex and Grok
  adapters keep session-scoped sockets; each Claude attachment uses a preparation-scoped socket
  that rotates on resume without depending on PID reuse. Normal TUI exit removes the live
  registration and owned children.
- A native transcript can move between ordinary and peer mode by exact UUID. Use the product
  wrapper once to adopt an ordinary session into the catalog; later `peer resume UUID` restores its
  product/groups while an ordinary native resume remains an unregistered Agent Sessions opt-out.
- Dead shim transports are replaced and garbage-collected without deleting queued messages.
- Child Codex subagents remain private to their parent while the root is a published peer.
- Generic lanes inherit normal user configuration and impose no model, reasoning, sandbox,
  approval, web, or project policy.
- A lane is owned by its launching orchestrator by default. When that owner exits, active work is
  interrupted and the lane is archived, stopping its discovery shim while retaining resumable
  transcript history. For a corroborated Claude caller, the Claude session process is the owner—not
  a short-lived Bash or Python wrapper that invokes the CLI. `--persistent` explicitly creates a
  lane that survives its owner.
- Completed lanes can take follow-up turns on the same transcript. Parent-owned Claude lanes notify
  their corroborated owner automatically; persistent lanes may nominate a peer with `--notify`.
  By default a terminal lane remains available for one minute, then auto-archives if no newer turn
  started. `--auto-archive-after SECONDS` configures that grace and `--no-auto-archive` disables it;
  combine the latter with `--persistent` for a
  permanently idle, messageable lane.
  JSON-Schema output enforcement, detached worktree isolation, and terminal accounting are
  available to orchestrators.
- Versioned Codex, Claude, and Grok skills let every supported parent product select every
  supported Codex, Claude, or Grok target lane without duplicating target lifecycle logic.
- A versioned Claude Code plugin teaches any local orchestrator this generic lane contract without
  copying bridge logic or choosing model, effort, sandbox, approval, web, or project policy.
- Parent groups are not propagated by default. Every lane gets its own private group and its
  immediate parent anchor; `--inherit-groups` is the parent’s explicit opt-in for the rest.
- With the separate `peer-federator` protocol-3 agent/hub installed, all skills can run their native
  lane CLI on a named connected host. Remote lifecycle traffic and ordinary peer messages remain
  hub-only; remote execution is an explicit destination opt-in, the cleanup fuse cannot be disabled
  remotely, the destination exposes no direct spawn listener, and there is no SSH fallback.

## Repository layout

```text
.codex-plugin/              plugin manifest
.agents/plugins/            repository-local marketplace
.claude-plugin/             Claude Code marketplace catalog
claude/                     self-contained Claude Code plugin and orchestration skill
grok/                       Grok plugin manifest and MCP registration
.mcp.json                   MCP registration
hooks/                      Codex lifecycle hook registration
skills/                     Codex skills for orchestrating Codex, Claude, and Grok lanes
cmd/                        nine executable entry points
internal/bridge/            local session lifecycle and messaging runtime
internal/launcher/          native launcher argument and bootstrap logic
internal/federator/         independent cross-host federation runtime
deploy/peer-federator/      systemd and launchd service templates
scripts/                    hook/MCP trampoline, maintenance, packaging, and test tooling
docs/                       installation, lane integration, and protocol notes
.forgejo/workflows/         tests and four-platform release builds
```

The command packages contain only process entry points. Shared implementation stays private under
`internal/`; local session semantics remain in `internal/bridge`, launcher policy in
`internal/launcher`, and host federation in `internal/federator`. Federation does not run inside
the local session supervisor.

## Development

```bash
make lint
make test
make test-race
make build
make dev-install        # source-linked Codex/runtime development install
make dev-install-claude # source-linked Claude orchestration skill
make dev-install-grok   # source-linked trusted Grok MCP plugin
make install-claude     # Claude skill from the stable installed runtime tree
make install-grok       # trusted Grok MCP plugin from the stable installed runtime tree
make install-all        # native runtime plus Claude and Grok integrations
make reinstall   # refresh cachebuster, rebuild, and reinstall the local plugin
make repair-projection THREAD_ID=<uuid>         # inspect known Codex 0.147 projection damage
make repair-projection THREAD_ID=<uuid> APPLY=1 # back up and repair the exact known shape
```

The lint target verifies `.golangci.yml` before running `golangci-lint`. Forgejo runs lint, normal
tests, race tests, and all four architecture builds concurrently; release publication remains gated
on every one of those jobs.

See the [documentation index](docs/README.md), [cross-product installation](docs/INSTALL.md), the
normative [acceptance matrix](docs/ACCEPTANCE-MATRIX.md), and the product-specific Codex, Claude,
and Grok adapter/install/lane guides linked from the index. Shared implementation details are in
the reverse-engineered [native adapter protocol](docs/ADAPTER-PROTOCOL.md).

The Claude-side wire format is not a public Anthropic API. Final v0.2.0 live validation included
Codex CLI 0.148.0, Claude Code 2.1.237, and Grok 1.0.4 across Linux and macOS hosts; CI also builds
all four supported OS/architecture combinations. The bridge follows a trusted-local model:
managed Codex, Claude, and Grok peers plus the host agent running as the same operating-system user
are mutually trusted. It is not a cross-user authorization boundary; private runtime directories
and sockets protect against other local users, while federation requires an explicitly configured
trusted hub and host agents.
