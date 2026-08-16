# codex-peer

The Claude Code side of this repository. It teaches a Claude orchestrator how to drive
`codex-peer-lane` and `grok-peer-lane`: start a named lane, collect its final answer, message or
steer it while it runs, resume the same transcript, and archive it.

## Contents

```text
.claude-plugin/plugin.json     plugin manifest
skills/codex-lane/             the Codex lane skill and its references
skills/grok-lane/              the Grok lane skill and its references
commands/doctor.md             /codex-peer:doctor — read-only preflight
skills/codex-lane/scripts/     portable POSIX preflight included with the skill
```

Install it from the repository marketplace with:

```bash
claude plugin marketplace add https://github.com/antst/agent-sessions.git
claude plugin install codex-peer@agent-sessions
```

Or drop either skill directory into `~/.claude/skills/`. The Agent Sessions runtime must be
installed separately; each skill's references carry its host setup and verification commands.

A user-scope marketplace installation is loaded by new interactive sessions and non-interactive
`claude -p` sessions. Existing sessions need `/reload-plugins` or a restart.

## What it deliberately does not do

- **No MCP server.** Claude discovers and messages Codex lanes through its own native local session
  registry. Nothing needs to be brokered.
- **No hooks.** Nothing runs on SessionStart, UserPromptSubmit, Stop, or SessionEnd.
- **No subagent.** A subagent boundary would hide a running lane from the orchestrator that needs to
  message it.
- **No settings and no permission grants.** Lane commands prompt like any other `Bash` call.
- **No binary and no adapter code.** Runtime dependencies are `codex-peer-lane` and
  `grok-peer-lane`; protocol, wake, and portability logic stay in the Go runtime where fixes are
  centralized.
- **No policy defaults.** Model, effort, sandbox, approval, web access, config overlays, output
  schema, and worktree isolation are supplied by the caller or not at all.

That leaves the plugin as documentation plus one read-only preflight script, which is what makes it
portable to any repository or host.

## Contract

The Codex skill targets adapter **contract version 2** and the Grok skill targets **contract version
1**. Both require `list`, `doctor --json`, and `contract_version` in `lane.ready` and doctor output.
Their `references/` directories are self-contained operational contracts loaded from Claude's
plugin cache. The source repository also publishes the human-facing adapter specifications under
`docs/`.
