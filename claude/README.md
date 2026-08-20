# agent-sessions

The Claude Code side of this repository. It teaches a Claude orchestrator how to drive
`codex-peer-lane`, `claude-peer-lane`, and `grok-peer-lane`: start a named lane, collect its final answer, message or
steer it while it runs, resume the same transcript, and archive it.

## Contents

```text
.claude-plugin/plugin.json     plugin manifest
.mcp.json                      process-attested structured peer messaging
skills/codex-lane/             the Codex lane skill and its references
skills/claude-lane/            the Claude self-lane skill
skills/grok-lane/              the Grok lane skill and its references
skills/agent-sessions/         grouped messaging through the one host-agent service
commands/doctor.md             /agent-sessions:doctor — read-only preflight
skills/codex-lane/scripts/     portable POSIX preflight included with the skill
```

Install it from the repository marketplace with:

```bash
claude plugin marketplace add https://github.com/antst/agent-sessions.git
claude plugin install agent-sessions@agent-sessions
```

Or drop either skill directory into `~/.claude/skills/`. The Agent Sessions runtime must be
installed separately; each skill's references carry its host setup and verification commands.

A user-scope marketplace installation is loaded by new interactive sessions and non-interactive
`claude -p` sessions. Existing sessions need `/reload-plugins` or a restart.

## What it deliberately does not do

- **One process-attested MCP server.** Managed Claude peers get structured grouped discovery and
  messaging. The server remains inactive unless its process descends from the exact live Claude
  adapter and the host agent corroborates the same UUID, socket, and lifecycle owner.
- **No hooks.** Nothing runs on SessionStart, UserPromptSubmit, Stop, or SessionEnd.
- **No subagent.** A subagent boundary would hide a running lane from the orchestrator that needs to
  message it.
- **No settings and no permission grants.** Lane commands prompt like any other `Bash` call.
- **No binary and no adapter code.** Runtime dependencies are the three product lane launchers;
  protocol, wake, and portability logic stay in the Go runtime where fixes are
  centralized.
- **No policy defaults.** Model, effort, sandbox, approval, web access, config overlays, output
  schema, and worktree isolation are supplied by the caller or not at all.

The MCP entry invokes the separately installed Agent Sessions runtime from `PATH`; the plugin still
ships no binary and remains portable across supported hosts.

## Contract

The Codex skill targets adapter **contract version 2**; Claude and Grok target **contract version
1**. All require `list`, `doctor --json`, and `contract_version` in `lane.ready` and doctor output.
Their `references/` directories are self-contained operational contracts loaded from Claude's
plugin cache. The source repository also publishes the human-facing adapter specifications under
`docs/`.
