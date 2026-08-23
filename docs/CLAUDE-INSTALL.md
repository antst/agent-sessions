# Claude-side installation

The Claude payload is the `agent-sessions` plugin under [`claude/`](../claude). It contains the
three lane skills, grouped messaging instructions, one read-only preflight command, and the
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
reported and skipped. If that runtime tree is already populated, `make install-claude` updates only
Claude and remains strict when Claude Code is absent. Use
`make dev-install-claude` only when the marketplace should deliberately follow the checkout. Normal
installation stages each cache-busted Claude plugin version under
`~/.local/share/agent-sessions/claude-marketplaces/`; the active version is immutable, so a later
native-runtime-only install cannot change code beneath an already running Claude session.
After the replacement is verified, the installer removes the historical `codex-peer` plugin IDs
from the current and legacy marketplaces.

Verify:

```bash
claude plugin list
claude plugin validate ~/agent-sessions/claude --strict
```

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

The preflight is read-only: it starts, installs, and changes nothing. It invokes the native binary
recorded in `native-runtime-path` directly rather than the bootstrapping launcher. It prints one
JSON object and exits non-zero unless the runtime satisfies **adapter contract 2**. Interpretation:

| Field | Meaning |
|---|---|
| `summary` | `"ready"`, or the reason orchestration should not proceed |
| `runtime_found` | Whether a lane runtime could be resolved at all |
| `launcher_found` | Whether `codex-peer-lane` is on `PATH`; preflight never executes it |
| `invocation` | Exact `<validated-native-runtime> lane` command to use |
| `contract_version` / `contract_ok` | Whether the CLI implements contract 2 |
| `runtime_ready` / `doctor_exit` | Whether App Server and the peer supervisor are reachable |
| `list_supported`, `doctor_supported` | The contract-1 subcommands, probed individually |
| `peer_discovery_cli` | Only whether the `claude` executable is on `PATH` |

## How the runtime is located

Preflight reads the first line of
`${CLAUDE_PEER_DATA_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/claude-code-peer}/native-runtime-path`
and invokes that exact binary as `<path> lane …`. `launcher_found` separately reports whether
`codex-peer-lane` is on `PATH`, but preflight never substitutes it: a PATH launcher could belong to
a different installation root and select another runtime while bootstrapping.

## Permissions

Lane commands run through the `Bash` tool and prompt for approval like any other command. This
plugin ships no permission grants — that decision belongs to the user, not to a plugin.

A user who wants fewer prompts can add this to their own `settings.json`:

```json
{
  "permissions": {
    "allow": ["Bash(codex-peer-lane:*)"]
  }
}
```

Scope it to project settings when the lanes are project-specific. This allowlist covers explicit
launcher use; the skill follows preflight's exact native invocation, which should be approved as
shown. Note that `wait` blocks, so a long collection is best run as a bounded background `Bash`
call regardless of permissions.

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
