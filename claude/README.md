# agent-sessions

The Claude Code side of this repository. It gives a Claude orchestrator the same
daemon-backed Agent Sessions lifecycle surface as Codex, Grok, and Qwen: start a
named Codex, Claude, Grok, or Qwen lane, collect its final answer, message or steer
it while it runs, resume the same transcript, and archive it.

## Contents

```text
.claude-plugin/plugin.json     plugin manifest
.mcp.json                      process-attested messaging and lane lifecycle control
skills/codex-lane/             the Codex lane skill and its references
skills/claude-lane/            the Claude self-lane skill
skills/grok-lane/              the Grok lane skill and its references
skills/qwen-lane/              the Qwen lane skill
skills/agent-sessions/         canonical discovery, messaging, and lane router
commands/doctor.md             /agent-sessions:doctor — read-only preflight
skills/codex-lane/scripts/     portable POSIX preflight included with the skill
```

Install it from the repository marketplace with:

```bash
claude plugin marketplace add https://github.com/antst/sessionbus.git
claude plugin install agent-sessions@agent-sessions
```

Or drop either skill directory into `~/.claude/skills/`. The Agent Sessions runtime must be
installed separately; each skill's references carry its host setup and verification commands.

A user-scope marketplace installation is loaded by new interactive sessions and non-interactive
`claude -p` sessions. Existing sessions need `/reload-plugins` or a restart.

## What it deliberately does not do

- **One process-attested MCP server.** Managed Claude peers get structured grouped discovery,
  messaging, and local or federated lifecycle control for all four lane products. The server
  remains inactive unless its process descends from the exact live Claude adapter and the host
  agent corroborates the same UUID, socket, and lifecycle owner. The daemon, not Claude's MCP
  process or shell, owns background workers.
- **No hooks.** Nothing runs on SessionStart, UserPromptSubmit, Stop, or SessionEnd.
- **No subagent.** A subagent boundary would hide a running lane from the orchestrator that needs to
  message it.
- **No global settings mutation.** Plugin installation never edits the operator's Claude profile.
  `claude-peer` disables only a same-named project `.mcp.json` entry and merges the four exact,
  plugin-qualified Agent Sessions tool approvals into its lifecycle-owned settings overlay. Claude
  lanes receive the same narrow policy. This prevents an arbitrary project from inheriting the
  installed connector's approval. Bare `claude` and unrelated MCP tools are unchanged; explicit
  tool removal or denial remains caller-owned.
- **No binary and no adapter code.** Runtime dependencies are the four product lane adapters;
  protocol, wake, and portability logic stay in the Go runtime where fixes are
  centralized.
- **No policy defaults.** Model, effort, sandbox, approval, web access, config overlays, output
  schema, and worktree isolation are supplied by the caller or not at all.

The MCP entry invokes the separately installed Agent Sessions runtime from `PATH`; the plugin still
ships no binary and remains portable across supported hosts.

## Contract

All four lane skills target adapter **contract version 2** and require `list`, `doctor --json`,
and `contract_version` in `lane.ready` and doctor output.
Their `references/` directories are self-contained operational contracts loaded from Claude's
plugin cache. The source repository also publishes the human-facing adapter specifications under
`docs/`.
