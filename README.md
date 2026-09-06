# Agent Sessions

Agent Sessions lets coding agents discover one another, exchange live messages,
and run named worker sessions across products and machines. It works with the
products you already use: each product keeps ownership of its models,
credentials, settings, names, transcripts, and native session IDs.

Eight products can run as interactive peers. All eight, plus DeepSeek Harness
(DSH), can run as worker lanes.

| Product | Peer | Lane |
| --- | :---: | :---: |
| Codex | yes | yes |
| Claude Code | yes | yes |
| Grok Build | yes | yes |
| Qwen Code | yes | yes |
| OpenCode | yes | yes |
| Kilo Code | yes | yes |
| Pi | yes | yes |
| Oh My Pi (OMP) | yes | yes |
| DeepSeek Harness (DSH) | — | yes |

Agent Sessions is useful when one agent should implement while another reviews,
when specialists should keep their native conversation history across tasks, or
when agents on separate build hosts need one shared collaboration surface.

## Quick start

Install from a source checkout:

```sh
git clone https://github.com/antst/sessionbus.git
cd agent-sessions
make install
```

The installer adds the available product integrations, command aliases under
`~/.local/bin`, and one user service. Start two peers in the same group:

```sh
codex-peer --name lead --group demo
claude-peer --name reviewer --group demo
```

Inside either managed session, use the installed Agent Sessions skill and ask
it to list peers or send a message. The model calls the structured
`agent_sessions` tools; it does not need to invoke a shell command.

For example:

```text
List the Agent Sessions peers, then ask reviewer to inspect the current diff.
```

The receiving product gets a labeled message containing the sender's native
session ID, product-owned name, product, and groups. Delivery is synchronous:
success means the destination product accepted the message; otherwise the
sender receives the exact rejection.

## Peers

Managed peer aliases follow one pattern:

```text
codex-peer      claude-peer     grok-peer       qwen-peer
opencode-peer   kilo-peer       pi-peer         omp-peer
```

Common wrapper options include:

```sh
PRODUCT-peer --name NAME --group GROUP [--group GROUP] [--yolo] [NATIVE_ARGS...]
PRODUCT-peer --resume [NATIVE_SELECTOR] [--group GROUP] [OPTIONS...]
```

`--resume` is uniform only at the wrapper boundary. Agent Sessions translates
the flag and passes its optional selector verbatim; the product owns names,
IDs, duplicate handling, native pickers, and errors. Agent Sessions never lists
or matches product sessions to resolve a peer resume.

Every invocation is complete. Omitting a group, model, agent, effort, or
permission override on resume does not carry it forward from a prior Agent
Sessions invocation. The product receives no replacement value and applies its
own default. Product-specific arguments remain available as native passthrough
arguments unless they conflict with fields the wrapper must own.

See [Products](docs/PRODUCTS.md) for the exact native surfaces and supported
wrapper translations.

## Groups and messaging

Groups are the visibility and routing boundary. Two sessions can discover and
message one another when their effective group sets intersect. `--group` is
repeatable, and membership is defined entirely by the current invocation.
Codex interactive peers instead use the group array configured in their one
installed MCP entry; see [Groups](docs/GROUPS.md).

Each peer also receives a private group derived from its native identity:

```text
session:<host-id>/<native-session-id>
```

A lane receives its parent's private group and its own derived child anchor.
This makes parent/child replies possible without copying a global membership
database. Group membership does not grant instruction authority and does not
weaken a recipient's sandbox, approval policy, or higher-priority instructions.

The structured tools cover peer listing, direct, multicast, and group sends,
identity, supported native rename operations, and lane lifecycle.
There is no mailbox: the destination must be live and the caller owns any retry.

See [Groups](docs/GROUPS.md) for namespace and lane-handover rules.

## Worker lanes

A managed parent can run any supported lane product, including its own. DSH is
lane-only. Inside a managed agent, use the `agent_sessions.lane` MCP tool. The
shell aliases are the equivalent operator and scripting surface:

```text
codex-peer-lane       claude-peer-lane      grok-peer-lane
qwen-peer-lane        opencode-peer-lane    kilo-peer-lane
pi-peer-lane          omp-peer-lane         dsh-peer-lane
```

A blocking run looks like:

```sh
codex-peer-lane run --name reviewer --group demo/child --yolo \
  --prompt-file review-brief.md
```

A detached turn uses `start`; later calls use the native session ID or unique
name:

```sh
codex-peer-lane start --name reviewer --yolo --prompt-file review-brief.md
codex-peer-lane wait reviewer --timeout 300
codex-peer-lane resume reviewer --yolo --prompt-file follow-up.md
codex-peer-lane interrupt reviewer
codex-peer-lane archive reviewer
codex-peer-lane list --all
```

`run` and `resume` return their result synchronously. A terminal notice says
`collection=required` and includes a structured MCP `wait` hint only for a
detached result that still needs a collector; otherwise it says
`collection=none` and carries no hint.

The product's native session ID is the lane's only identity. Archive drops the
live worker and routing handle while preserving the product session. After a
daemon restart, daemon-owned lanes are not live; `list --all` reads the one
durable candidate table, asks the product to confirm each eligible native ID,
and exposes only confirmed sessions. Resume then opens that exact product
session again.

See [Lanes](docs/LANES.md) for lifecycle, messaging, restart, and reparenting
semantics.

## Inspect and operate a host

Useful commands are:

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
agent-sessions roster --json
agent-sessions catalog --json
```

A healthy local installation has exactly one service-managed
`agent-sessions daemon` per operating-system user. Product-launched
`agent-sessions connector ...` processes are short-lived clients of that
daemon, not additional daemons.

On Linux:

```sh
systemctl --user status agent-sessions.service --no-pager
journalctl --user -u agent-sessions.service -n 100 --no-pager
```

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
```

See [Installation](docs/INSTALL.md) and
[Troubleshooting](docs/TROUBLESHOOTING.md).

## Multiple machines

Each host daemon may connect to one optional `agent-sessions-hub`. The hub
relays complete live rosters, messages, and remote lane calls; it owns no
product credentials, transcripts, or lane sessions. Shared groups work across
hosts, and remote lifecycle calls use the same MCP operation with a `host`
field or the CLI `--host` option.

See [Federation](docs/FEDERATION.md).

## Trust and state

Agent Sessions is designed for a trusted local environment: the user's own
agents on infrastructure the user controls. Deployment boundaries such as
separate accounts, hosts, VLANs, or hubs provide isolation.

The daemon does not persist peers, messages, turns, results, names, product
metadata, or live presence. Its sole durable data is an immutable lane
discovery candidate row. Products remain the authority for whether a session
exists and for every product-owned field returned to users.

The wire contract for native clients is the
[Native Agent Sessions Presence Protocol](docs/specs/NATIVE-PEER-PROTOCOL.md).
The shorter [adapter architecture](docs/ADAPTER-PROTOCOL.md) explains how the
shipped integrations apply it.

## Documentation

- [Documentation index](docs/README.md)
- [Products](docs/PRODUCTS.md)
- [Lanes](docs/LANES.md)
- [Groups](docs/GROUPS.md)
- [Installation](docs/INSTALL.md)
- [Federation](docs/FEDERATION.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Acceptance matrix](docs/ACCEPTANCE-MATRIX.md)
- [Native peer protocol](docs/specs/NATIVE-PEER-PROTOCOL.md)

## License

See [LICENSE](LICENSE).
