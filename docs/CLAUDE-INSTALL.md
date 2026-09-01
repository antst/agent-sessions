# Claude-side installation

The Claude payload is the `agent-sessions` plugin under [`claude/`](../claude). It contains four
target-product lane skills, the canonical `agent-sessions` routing skill, one read-only preflight command, and the
process-attested structured messaging MCP declaration. It ships no runtime, hooks, binary, settings,
or permission grants. Its native dependencies are installed by the Codex/runtime side.

Install the Codex side first — see [INSTALL.md](./INSTALL.md). Without it there is no lane runtime
to drive, and the preflight will say so.

## Marketplace installation

```bash
claude plugin marketplace add https://github.com/antst/agent-sessions.git
claude plugin install agent-sessions@agent-sessions
```

From a local checkout instead:

```bash
claude plugin marketplace add ~/agent-sessions
claude plugin install agent-sessions@agent-sessions
```

The default user-scope installation is available to every new interactive Claude session and to
non-interactive `claude -p` sessions on that host. Restart Claude Code afterwards, or use
`/reload-plugins` in a development session. From a source checkout, `make install-all` installs
the shared runtime and every integration whose native client is present; missing products are
reported and skipped. `make install` is the same atomic transaction, and there is no separate
Claude-only Make installer. Installation stages each cache-busted Claude plugin version under
`~/.local/share/agent-sessions/claude-marketplaces/`; the active version is immutable, so a later
native-runtime-only install cannot change code beneath an already running Claude session.
After the replacement is verified, the installer removes the historical `codex-peer` plugin IDs
from the current and legacy marketplaces.

Verify:

```bash
claude plugin list
claude plugin validate ~/agent-sessions/claude --strict
```

In a managed Claude session, invoke `/agent-sessions:agent-sessions list peers`
to select Agent Sessions rather than Claude's native agent/team discovery.
Claude namespaces plugin-provided skills as `/PLUGIN:SKILL`; the plugin and
skill intentionally share the same name, so the repeated form is expected.

## Installation without plugins

The skill is self-contained, so it can be copied into the user skills directory:

```bash
cp -r ~/agent-sessions/claude/skills/codex-lane ~/.claude/skills/
```

It then loads as `codex-lane@skills-dir` on the next session. The `/agent-sessions:doctor` command is
not included in this route; run the copied
`~/.claude/skills/codex-lane/scripts/lane-preflight` directly instead.

## Verifying the runtime

```bash
claude
> /agent-sessions:doctor
```

Or directly, from the checkout:

```bash
./claude/skills/codex-lane/scripts/lane-preflight
```

The preflight is read-only: it starts, installs, and changes nothing. It resolves and validates the
installed `codex-peer-lane` alias directly. It prints one
JSON object and exits non-zero unless the runtime satisfies **adapter contract 2**. Interpretation:

| Field | Meaning |
|---|---|
| `summary` | `"ready"`, or the reason orchestration should not proceed |
| `runtime_found` | Whether a lane runtime could be resolved at all |
| `launcher_found` | Whether `codex-peer-lane` is on `PATH` |
| `invocation` | Exact validated `codex-peer-lane` alias to use |
| `contract_version` / `contract_ok` | Whether the CLI implements contract 2 |
| `runtime_ready` / `doctor_exit` | Whether the unified daemon and Codex App Server path are ready |
| `list_supported`, `doctor_supported` | The contract-1 subcommands, probed individually |
| `peer_discovery_cli` | Only whether the `claude` executable is on `PATH` |

## How the runtime is located

Preflight resolves `codex-peer-lane` from `PATH`, probes its contract, and reports that exact path as
`invocation`. The alias is installed transactionally with the unified host runtime, so there is no
separate runtime marker or launcher bootstrap step.

## Permissions

`claude-peer` creates a lifecycle-owned settings overlay that allows exactly the
four Agent Sessions MCP operations: peer listing, direct/multicast send, group
broadcast, and lane lifecycle. The overlay disables only a project `.mcp.json`
server named `agent_sessions` and approves the installed plugin's exact,
origin-qualified tool identifiers. Claude lanes receive the same narrow policy.
This prevents a repository from impersonating the trusted connector to inherit
its approval, while avoiding a prompt on every structured Agent Sessions call.
The user's global settings and unrelated tools are not changed. Bare `claude`,
native model/tool permissions, and explicit deny/removal policy remain unchanged.

Lane lifecycle is sent through the structured `lane` MCP tool; managed Claude
does not need a `Bash(codex-peer-lane:*)` permission for normal orchestration.

## Hosts and portability

The plugin is text only, so it carries no platform requirements of its own; the runtime targets
Linux and macOS on x86-64 and arm64.

A colleague on another host needs read access to this repository for the SSH marketplace URL. Where
that is not available, the local-checkout and skills-directory routes above work from any copy of
the tree.

## Contract version

The skill targets adapter **contract version 2**: `list`, `doctor --json`, and `contract_version` in
`lane.ready` and `doctor` output. A runtime that predates it is reported as a version gap rather
than being driven on a guess. The contract is [CLAUDE-ADAPTER.md](./CLAUDE-ADAPTER.md).
