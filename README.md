# Agent Sessions

Agent Sessions lets Codex, Claude Code, Grok Build, and Qwen Code work together.

Start normal interactive sessions, place them in a shared group, and they can discover and message
one another. Any of them can also launch durable worker sessions—called lanes—in any supported
product, locally or on another machine.

The result is a product-neutral collaboration layer over the AI tools you already use. It does not
replace their models, interfaces, permissions, configuration, credentials, or conversation history.

## What can I use it for?

- Give an implementation to Codex and ask Claude to review it independently.
- Let Grok research an unfamiliar subsystem while Qwen writes tests.
- Keep a specialist worker alive for later follow-up questions instead of starting from scratch.
- Hand work between long-running interactive sessions with clear source information.
- Use the same orchestration instructions whether the lead session is Codex, Claude, Grok, or Qwen.
- Coordinate Linux, macOS, and build-host sessions while developing and validating multi-platform
  software.
- Run cross-model acceptance, critique, and adversarial-review workflows without flattening every
  product into the same security or model settings.

Agent Sessions works especially well when one model should not be both author and final judge.

## A five-minute local example

First install Agent Sessions, as described below. Then open two terminals in the same project:

```sh
# Terminal 1
codex-peer -n lead -g my-project

# Terminal 2
claude-peer -n reviewer -g my-project
```

Inside either session, invoke the installed `agent-sessions` skill and ask it to list peers. You
should see the other session. Then ask, for example:

```text
Send reviewer this message: review the authentication changes and report only concrete risks.
```

The receiving session gets a labeled message containing the sender's Agent Sessions identity. You
can reply, multicast to selected peers, or broadcast to a shared group.

You can also ask the lead session to start a worker:

```text
Use Agent Sessions to start a persistent Grok lane named auth-research.
Ask it to inspect current OAuth guidance and send its conclusions back here.
```

The skill uses the Agent Sessions MCP tools to create and supervise the lane in the background. The
worker's terminal status is delivered back to its immediate parent automatically; the parent then
collects any still-owed result through the structured lane tool. The same lane can receive follow-up
turns.

## The basic ideas

- A **peer** is an interactive Codex, Claude, Grok, or Qwen session started with a `*-peer`
  command.
- A **group** controls which peers can discover and message one another. Add one with
  `-g NAME`; repeat it to join several groups.
- A **lane** is a supervised worker session. It may finish one task and exit, or remain persistent
  for follow-up work.
- The **host daemon** is one background user service on each machine. It owns local registrations,
  routing, lane state, and recovery.
- The optional **hub** connects host daemons so group-visible peers and lanes can work across
  machines.

Groups restrict participant discovery and delivery. They do not grant one session authority to act
for the user, and they do not weaken a recipient's system instructions, sandbox, or approval rules.

### Registrations and connector processes

`agent-sessions roster` lists managed peers and lanes. It deliberately does not list every helper or
MCP server process. Claude, Grok, and Qwen normally start a connector under each managed interactive
product process. Codex is different: Codex owns one durable App Server and that App Server hosts one
shared Agent Sessions MCP connector for all of its threads.

Consequently, the Codex App Server and its `agent-sessions connector auto` child may remain running
when no Codex peer is registered. That is expected. The shared connector gets its authority from
the native thread metadata on each call; without a matching live managed peer or lane it has no
session authority and appears nowhere in `roster`. Agent Sessions disconnects its own App Server
client during shutdown but does not stop the user-managed Codex App Server.

A Claude process tree, by contrast, normally contains its own `agent-sessions connector claude`
child, and that connector corresponds to the Claude row only while the daemon has adopted the exact
native process/session evidence. Process presence alone is never a registration.

## Requirements

Agent Sessions supports Linux and macOS on x86-64 and arm64. You may install any subset of the four
native products; missing products are skipped without disabling the others.

| Product | Minimum supported | Current acceptance version |
|---|---:|---:|
| Codex CLI | 0.151.0 | 0.151.0 |
| Claude Code | 2.1.251 | 2.1.251 |
| Grok Build | 1.0.13 | 1.0.13 |
| Qwen Code | 0.21.15 | 0.22.3 |

Log in to each native product normally before using its peers or lanes. Qwen readiness enforces its
version floor. Codex, Claude, and Grok validate the native capabilities they need; older versions
are unsupported even if they happen to launch.

Building from a source checkout requires Go 1.22 or newer. Packaged release archives contain
prebuilt binaries and do not require Go.

## Install

From a source checkout:

```sh
git clone https://github.com/antst/agent-sessions.git
cd agent-sessions
make install
```

`make install-all` performs the same unified installation. It installs every available product
integration, command aliases under `~/.local/bin`, and one enabled user service:

- Linux: `agent-sessions.service`
- macOS: `net.antst.agent-sessions`

It restarts only Agent Sessions. It does not restart vendor applications, the Codex App Server, or
the optional hub.

Restart any Codex, Claude, Grok, or Qwen sessions that were already open during installation so
they load the installed plugin and MCP inventory. On the first managed Codex launch, approve the
one-time hook prompt for `agent-sessions@agent-sessions`; this enables session registration and
message delivery, not broader sandbox access.

Managed Claude launches add exact approvals for the Agent Sessions MCP tools they expose. Installing
the plugin or running ordinary `claude` does not change Claude's global approval policy.

Verify the installation:

```sh
agent-sessions doctor
agent-sessions status
agent-sessions roster
```

On Linux, also check the service:

```sh
systemctl --user status agent-sessions.service --no-pager
```

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
```

No local configuration file is required for local-only use.

## Start and resume peers

Use the managed wrapper for each product:

```sh
codex-peer  -n lead        -g project-a
claude-peer -n reviewer    -g project-a
grok-peer   -n researcher  -g project-a
qwen-peer   -n test-writer -g project-a
```

The wrapper launches the native interactive product with its normal configuration. Agent Sessions
adds identity, groups, lifecycle registration, and message delivery. Product arguments such as
model, sandbox, effort, approval, project directory, and display options continue to belong to the
native product.

Use each wrapper's documented `--resume` form to adopt or return to a native conversation. Exact
native UUIDs are durable identities; supported wrappers also accept their documented unique-name or
native-title selectors.

Every peer receives a private `session:<host>/<session>` group in addition to explicit groups. This
lets child lanes talk to their immediate parent without making that private relationship globally
visible.

## Discover and message peers

Every product installs the same semantic skill named `agent-sessions`:

| Product | Typical invocation |
|---|---|
| Codex | `$agent-sessions list peers` |
| Claude | `/agent-sessions:agent-sessions list peers` |
| Grok | `/agent-sessions list peers` |
| Qwen | invoke the `agent-sessions` skill and ask to list peers |

Claude writes plugin skill names as `/PLUGIN:SKILL` to prevent collisions. Both names are
`agent-sessions` here, which is why Claude's form repeats the name; an installed plugin cannot claim
the unnamespaced `/agent-sessions` command without mutating the user's global command directory.

The invocation UI differs, but the skill content and MCP operations are the same. It supports:

- listing local and remote peers that share a group with the caller;
- sending to one peer or an explicit set of peers;
- broadcasting within a shared group;
- replying to the verified source of an incoming message;
- renaming the current managed registration, with later product-native renames also propagated; and
- starting, supervising, resuming, interrupting, and archiving lanes.

Product-native agent or session lists are not substitutes for Agent Sessions discovery. If a
structured Agent Sessions operation fails, the skill reports the failure instead of silently using
a different native messaging channel.

## Use worker lanes

Every supported parent can launch every supported target, including its own product. A Claude
orchestrator can therefore launch a Claude Agent Sessions lane, a Codex lane, a Grok lane, or a Qwen
lane through the same operation. This is separate from any product-native subagent system.

The recommended interface inside a managed AI session is the shared skill, which calls
`agent_sessions.lane` through MCP. The `*-peer-lane` commands intentionally remain supported: they
are the operator, automation, CI, recovery, and third-party integration surface, and the daemon and
federation adapters use the same product launchers. MCP is the agent-facing control plane over that
CLI contract, not a replacement for it.

Shell commands are therefore also available directly:

```sh
codex-peer-lane doctor --json
claude-peer-lane start --name review - < brief.md
grok-peer-lane wait review
qwen-peer-lane list --all
```

All four products implement lane contract version 2 with the same lifecycle:

```text
run  start  resume  wait  status  interrupt  archive  list  doctor
```

Lane behavior is intentionally predictable:

- `run`, `start`, and `resume` accept prompt input. Control commands never read terminal stdin.
- A lane belongs to its immediate parent unless `--persistent` is explicit.
- Parent groups are private by default. Use `--inherit-groups` or explicit `--group` values when a
  worker should join broader collaboration.
- Terminal status is pushed automatically to the immediate owner and can also be collected with
  `wait`. There is no `notify_target` field or `--notify` flag.
- Finished lanes auto-archive after 60 seconds by default. Change this with
  `--auto-archive-after SECONDS`. `--persistent` changes parent-exit ownership
  only; it does not disable this grace. Use `--persistent --no-auto-archive` for
  a durable idle worker, then archive it explicitly.
- There is no default Agent Sessions cap on model, reasoning, tokens, dollar budget, sandbox,
  approvals, web access, or project policy. Limits apply only when you or the native product
  configuration set them.
- A persistent completed lane can take follow-up turns on the same native conversation.

See [Codex lanes](docs/CODEX-LANES.md), [Claude lanes](docs/CLAUDE-LANES.md),
[Grok lanes](docs/GROK-LANES.md), and [Qwen lanes](docs/QWEN-LANES.md) for target-specific options.

## See what is running

Peer discovery is group-restricted, but the user who owns the daemon needs to diagnose the whole
installation. The operator roster provides that view:

```sh
agent-sessions roster
agent-sessions roster --json
```

It shows every current local peer and lane, product, state, group, permission mode, owner, connected
federated host, and live remote registration. It does not expose prompts, messages, results,
credentials, capability tokens, or native evidence. The machine-readable schema is
`agent-sessions.roster.v1`.

Use the available inventories according to their purpose:

- `agent_sessions.list_peers`: participants sharing a group with the managed caller;
- `*-peer-lane list`: one product's lanes visible to or owned by the caller;
- `agent-sessions status` and `doctor`: host-wide counts and health;
- `agent-sessions roster`: unrestricted same-user operational metadata across groups and connected
  hosts.

## Connect multiple machines

Federation is optional. Install one hub on a machine reachable by every host:

This is useful for more than moving a worker to a faster machine. For cross-platform development,
you can put Linux and macOS peers in the same project group and let them coordinate while each uses
its own native checkout, compiler, package manager, test environment, and platform APIs. One session
can implement a portable change, another can reproduce or fix the macOS behavior, and a third can
run Linux integration tests or review the combined result.

Other multi-host patterns include:

- keeping an interactive lead on a laptop while durable implementation and test lanes run on a
  workstation;
- assigning hardware-, operating-system-, network-, or credential-specific validation to the host
  that actually has that environment;
- coordinating release checks across architectures without pretending one machine can faithfully
  emulate every target; and
- keeping specialized peers close to large repositories, local services, or restricted test data
  while sharing only task messages and results with the wider group.

Agent Sessions coordinates identities, work requests, status, and messages. It does not synchronize
source trees or artifacts; use Git, your CI system, or another explicit file-transfer workflow so
each host is working from the intended revision.

Install the hub on the central machine:

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

The installer enables and starts `agent-sessions-hub.service` on Linux or
`net.antst.agent-sessions-hub` under launchd on macOS. The default listen address is TCP port 7419.
A central machine may run both the hub service and its own normal host daemon.

On Linux, an optional `~/.config/agent-sessions/hub.env` changes the listen address:

```sh
AGENT_SESSIONS_HUB_LISTEN=:7419
```

On each participating host, copy and edit the installed example:

```sh
mkdir -p ~/.config/agent-sessions
cp -n ~/.config/agent-sessions/service.env.example \
  ~/.config/agent-sessions/service.env
${EDITOR:-vi} ~/.config/agent-sessions/service.env
```

Set:

```sh
AGENT_SESSIONS_HUB=hub.example:7419
AGENT_SESSIONS_HOST_NAME=workstation-a
```

Restart that host's Agent Sessions service and inspect the connection:

```sh
systemctl --user restart agent-sessions.service
agent-sessions roster
```

On macOS restart with:

```sh
launchctl kickstart -k "gui/$(id -u)/net.antst.agent-sessions"
```

Group-visible remote peers now appear through the same skill; no separate remote-discovery command
is needed. To place a lane on a specific machine, choose its `host` in the MCP request or use:

```sh
grok-peer-lane --host workstation-b doctor --json
grok-peer-lane --host workstation-b start --name remote-review - < brief.md
```

Remote work never falls back to SSH or silently runs on the source host.

The current federation protocol assumes a trusted network. It does not provide TLS,
authentication, a policy language, or hub-side offline storage. Expose port 7419 only where every
connected host daemon is trusted. See [Federation](docs/FEDERATION.md) for the full model.

## Update or uninstall

Update with the same transaction:

```sh
git pull
make install
```

It stages and validates the new release before switching, then restarts one Agent Sessions service.
Restart already-open product sessions when plugin files changed.

Remove Agent Sessions while preserving unified state and all vendor-owned data:

```sh
make remove-all
```

Remove the independent hub with:

```sh
make remove-hub
```

Delete Agent Sessions state and configuration too only with the explicit destructive target:

```sh
make purge-all
```

The repository-only `scripts/cleanup-pre-unification` tool exists for controlled development hosts
migrating from the old unreleased split stack. It is not a normal uninstall or recovery command.

## Troubleshooting

Start here:

```sh
agent-sessions doctor
agent-sessions status
agent-sessions roster --json
```

On Linux, inspect the one user service:

```sh
systemctl --user status agent-sessions.service --no-pager
journalctl --user -u agent-sessions.service -n 100 --no-pager
```

Restart it through the service manager:

```sh
agent-sessions daemon restart
```

Lane `doctor`, `list`, `status`, `wait`, `interrupt`, and `archive` do not consume stdin;
`</dev/null` is unnecessary. If Codex readiness fails after upgrading Codex, check its App Server:

```sh
codex app-server daemon version
codex app-server daemon restart
agent-sessions daemon restart
codex-peer-lane doctor --json
```

Do not kill healthy vendor sessions or start a second Agent Sessions daemon to work around a stale
connector. See the [troubleshooting guide](docs/TROUBLESHOOTING.md) for process census, macOS logs,
and safe recovery.

## Security and ownership

Managed peers and the daemon running as the same operating-system user are mutually trusted.
Private runtime directories and sockets protect against other local users, but Agent Sessions is
not a cross-user security boundary. Native product permissions remain authoritative.

Groups restrict participant discovery and message delivery. They deliberately do not hide
operational metadata from the same-user `roster` command and do not establish user delegation.
Federation adds trusted-network routing; it does not turn groups into cryptographic authorization.

## More documentation

The [documentation index](docs/README.md) links the installation, groups, federation,
troubleshooting, product-specific, protocol, and acceptance guides. Start with:

- [Installation and service behavior](docs/INSTALL.md)
- [Groups and visibility](docs/GROUPS.md)
- [Federation](docs/FEDERATION.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Acceptance matrix](docs/ACCEPTANCE-MATRIX.md)

## License

Agent Sessions is licensed under the [Mozilla Public License 2.0](LICENSE). Changes to covered
source files remain available under MPL-2.0, while the license permits combining Agent Sessions
with separately licensed software in a larger work.
