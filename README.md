# Agent Sessions

Native, persistent session lifecycle for Codex, Claude Code, Grok, and Qwen Code, plus local and federated
cross-session messaging. Interactive sessions and durable worker lanes can be created, resumed,
supervised, discovered, and messaged with nearly the same lifecycle on Linux and macOS.

This repository is one Go module with shared implementation under `internal/`. It builds exactly
two native executables:

- `agent-sessions` — the sole per-user host daemon plus its administrative, peer, lane, and
  stateless connector modes;
- `agent-sessions-hub` — the independently installed central federation hub.

Compatibility command names such as `peer`, `codex-peer`, and `codex-peer-lane` are links to the
same `agent-sessions` image. They are not additional runtimes or authorities.

## Quick start

```bash
git clone https://github.com/antst/agent-sessions.git
cd agent-sessions
make test-race
make install-all

agent-sessions status --json # the installed user service owns daemon lifetime
codex-peer -g project-a -n reviewer
claude-peer -g project-a -n implementer
grok-peer -g project-a -n researcher
qwen-peer -g project-a -n analyst
claude agents --json
```

`-g` is short for `--group` on all four managed peer launchers; repeat either form to join more
than one explicit group.

## User-defined delegation

Agent Sessions authenticates which managed peer sent a message and limits routing to shared groups.
It does **not** decide that one interactive session may act with the user's authority over another.
Delivered content is labeled only as `Message from <peer>:` with factual provenance metadata; the
transport does not characterize its reliability or inject a delegation decision.
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

The installed user service is the sole host authority. Peer launchers ask that daemon to prepare an
exact attachment, then replace themselves with the native TUI using the daemon-issued launch
handoff. The daemon's product adapter corroborates the resulting native session before it becomes
discoverable. Resume by UUID or unambiguous durable name reuses the same Agent Sessions session
identity; native selectors that cannot be resolved exactly fail before catalog mutation.

Product connectors are stateless relays into the fixed per-user control endpoint. They do not own a
listener, roster, session store, or fallback daemon. Each call is authorized from the
inherited attachment identity and capability, so a bare native session may see installed tool names
but cannot read the roster or send messages. PID/process-start, native identity, profile, and socket
evidence are checked by the relevant adapter; stale or mismatched evidence fails closed.

Native behavior remains native. Codex arguments retain their relative order and App Server is used
inside the Codex adapter for exact thread start/resume/reconnect. Claude preserves exact UUID and
native title/chooser resume behavior, does not change profile-level `crossSessionInbound`, and uses
launch-scoped settings for explicit permission choices. Grok preserves its UUID/title/picker and
ACP/leader semantics while the daemon owns the durable attachment. Qwen uses its selected
presence-sensitive profile and native extension manager. Agent Sessions records the effective
permission class without widening a product's sandbox, approval, or tool policy.

`make install-all` installs or upgrades the host role and every locally available product
integration. Product-specific targets remain strict; absent optional clients are reported and
skipped only by the aggregate target. Install is a role-scoped transaction: it validates the exact
release manifest, checksums, executable role, service assets, and connector inventory before
switching the selected release. Active or ambiguous migration resources produce named blockers
rather than being killed or silently adopted. Reinstall preserves product credentials, settings,
transcripts, and unrelated plugins.

`make install-hub` independently installs the `agent-sessions-hub` role. Host and hub have disjoint
release roots, service definitions, configuration, logs, and lifecycle operations even when they
run under the same user on one machine. Neither role installation starts a second host daemon.

CI release archives contain the two role images for Linux and macOS on x86-64 and arm64 plus a
schema-validated manifest and checksums, so archive installation does not require Go or Node.js.
After verifying the archive checksum, run the desired host or hub install target from the extracted
directory; the installer selects only the executable and service assets for that role.

## What it provides

- One per-user host daemon owns the durable attachment, lane, name, group, delivery, notice,
  migration, and federation-client catalogs. Product adapters share that authority; they do not
  create parallel product runtimes.
- An optional central hub joins connected hosts into one uniform multi-host space. Groups are
  global routing and visibility boundaries, and duplicate display names remain ambiguous unless a
  host-qualified identity such as `<peer>--<host>` is used.
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
  naturally shows ordinary and managed Claude sessions plus the one Agent Sessions host service.
- Stable Agent Sessions IDs are reattached when a TUI resumes the same native session. Normal TUI
  exit removes live visibility while retaining only the metadata required for exact future resume.
- A native transcript can move between ordinary and peer mode. Exact UUIDs work for every product;
  Claude and Grok wrappers also preserve their native title and chooser selectors. Use the product
  wrapper once to adopt an ordinary session into the catalog; later managed resume restores its
  product/groups while an ordinary native resume remains an unregistered Agent Sessions opt-out.
- Child Codex subagents remain private to their parent while the root is a published peer.
- Generic lanes inherit normal user configuration and impose no model, reasoning, sandbox,
  approval, web, or project policy.
- A lane is owned by its launching orchestrator by default. When that owner exits, the daemon applies
  the lane's cleanup policy while retaining supported resumable transcript history. `--persistent`
  explicitly creates a lane that survives its owner.
- Completed lanes can take follow-up turns on the same transcript. Parent-owned Claude lanes notify
  their corroborated owner automatically; persistent lanes may nominate a peer with `--notify`.
  By default a terminal lane remains available for one minute, then auto-archives if no newer turn
  started. `--auto-archive-after SECONDS` configures that grace and `--no-auto-archive` disables it;
  combine the latter with `--persistent` for a
  permanently idle, messageable lane.
  JSON-Schema output enforcement, detached worktree isolation, and terminal accounting are
  available to orchestrators.
- Versioned Codex, Claude, Grok, and Qwen skills let every supported parent product select every
  supported Codex, Claude, Grok, or Qwen target lane without duplicating target lifecycle logic.
- Versioned product skills teach the same daemon lane contract without copying lifecycle logic or
  choosing model, effort, sandbox, approval, web, or project policy.
- Parent groups are not propagated by default. Every lane gets its own private group and its
  immediate parent anchor; `--inherit-groups` is the parent’s explicit opt-in for the rest.
- With the host daemon connected to an independently installed `agent-sessions-hub`, all skills can
  run `agent-sessions lane --host HOST --product PRODUCT -- ...` on a named connected host. Remote
  lifecycle traffic and ordinary peer messages remain
  hub-only; remote execution is an explicit destination opt-in, the cleanup fuse cannot be disabled
  remotely, the destination exposes no direct spawn listener, and there is no SSH fallback.

## Repository layout

```text
.codex-plugin/              plugin manifest
.agents/plugins/            repository-local marketplace
.claude-plugin/             Claude Code marketplace catalog
claude/                     self-contained Claude Code plugin and orchestration skill
grok/                       Grok plugin manifest and MCP registration
qwen/                       Qwen Agent Plugins v1 manifest, MCP, and skills
.mcp.json                   MCP registration
hooks/                      Codex lifecycle hook registration
skills/                     Codex skills for orchestrating all four lane products
cmd/                        the two canonical executable entry points
internal/bridge/            product adapter and stateless connector mechanics
internal/launcher/          native launcher argument and daemon handoff policy
internal/federation/        shared hub protocol, routing, identity, and hub runtime
internal/daemon/            the one per-user host authority, including federation client state
deploy/agent-sessions/      host systemd and launchd service templates
deploy/agent-sessions-hub/  independent hub systemd and launchd service templates
scripts/                    hook/MCP trampoline, maintenance, packaging, and test tooling
docs/                       installation, lane integration, and protocol notes
.github/workflows/          tests, two-OS release gates, evidence, and four-platform release builds
```

The command packages contain only process entry points. Shared implementation stays private under
`internal/`; adapter semantics remain in `internal/bridge`, launcher policy in `internal/launcher`,
the host authority in `internal/daemon`, and logical hub protocol/routing in `internal/federation`.

## Development

```bash
make lint
make test
make test-race
make build
make dev-install        # source-linked host and Codex development install
make dev-install-claude # source-linked Claude orchestration skill
make dev-install-grok   # source-linked trusted Grok MCP plugin
make dev-install-qwen   # source Qwen Agent Plugins payload in the selected profile
make install-claude     # Claude skill from the stable selected host release
make install-grok       # trusted Grok MCP plugin from the stable selected host release
make install-qwen       # Qwen plugin from the stable selected host release
make remove-qwen        # remove only Agent Sessions from the selected Qwen profile
make install-all        # host role plus every locally available product integration
make reinstall   # refresh cachebuster, rebuild, and reinstall the local plugin
make repair-projection THREAD_ID=<uuid>         # inspect known Codex 0.147 projection damage
make repair-projection THREAD_ID=<uuid> APPLY=1 # back up and repair the exact known shape
```

The lint target verifies `.golangci.yml` before running `golangci-lint`. Forgejo runs lint, normal
tests, race tests, and all four architecture builds concurrently; release publication remains gated
on every one of those jobs.

See the [documentation index](docs/README.md), [cross-product installation](docs/INSTALL.md), the
normative [acceptance matrix](docs/ACCEPTANCE-MATRIX.md), and the product-specific Codex, Claude,
Grok, and Qwen adapter/install/lane guides linked from the index. Shared implementation details are in
the reverse-engineered [native adapter protocol](docs/ADAPTER-PROTOCOL.md).

The Claude-side wire format is not a public Anthropic API. Qwen support is frozen against Qwen Code
0.21.15 or newer and validated on real Linux and macOS hosts together with Codex, Claude, and Grok;
CI also builds
all four supported OS/architecture combinations. The bridge follows a trusted-local model:
managed Codex, Claude, Grok, and Qwen peers plus the host agent running as the same operating-system user
are mutually trusted. It is not a cross-user authorization boundary; private runtime directories
and sockets protect against other local users, while federation requires an explicitly configured
trusted hub and host agents.
