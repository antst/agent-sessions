# Codex lane caller-owned policy

Every policy flag is optional, and every omitted flag inherits the user's normal Codex and
`CODEX_HOME` configuration. Explicit flags are overlays on top of that configuration, never a
replacement for it.

**The rule: pass through what the caller stated, omit everything else.** An orchestrator that adds
a flag the caller did not ask for has silently changed the user's model, spend, or blast radius.

## The flag map

Map a caller's stated policy 1:1 onto flags. Nothing here has a default supplied by this skill.

| Caller states | Flag |
|---|---|
| lane name (required) | `-n`, `--name NAME` |
| working directory | `-C`, `--cd DIR` |
| model | `-m`, `--model MODEL` |
| reasoning effort | `--effort LEVEL` |
| sandbox | `--sandbox read-only \| workspace-write \| danger-full-access` |
| approval policy | `--approval-policy POLICY` |
| web access | `--web` or `--no-web` |
| config overlay | `-c KEY=VALUE` (repeatable, dotted keys) |
| lane deadline on `run`, `start`, or `resume` | `--timeout SECONDS` (0 means none) |
| survive the owning orchestrator | `--persistent` |
| custom terminal retention grace | `--auto-archive-after SECONDS` (minimum 0.001; default 60) |
| keep a completed lane beyond its configured grace period | `--no-auto-archive` |
| structured output | `--schema FILE` |
| isolated checkout | `--worktree` |

Prompts go on stdin (`-`) or in `--prompt-file FILE`. Never on argv.

`wait --timeout SECONDS` is not lane policy: it only bounds that collector call and never
interrupts or relabels the lane. An orchestrator may choose a bounded wait to stay within its own
tool-call limit; exit 124 means it should wait again or inspect status.

Lifecycle and notification routing are not model policy. Ordinary lanes belong to the launching
orchestrator, notify their immediate Agent Sessions parent automatically, and are interrupted/archived when
that owner exits. `lane.ready.owner_session_id` exposes the parent attachment; the unified daemon
does not use `notify_target`, `--notify`, or `--no-notify`. Pass `--persistent` only when requested. Auto-archive is enabled by default and
retires an idle lane one minute after its latest final terminal turn; a newer turn cancels the timer.
Pass `--auto-archive-after SECONDS` only when a different grace is requested. Pass
`--no-auto-archive` only for indefinite retention, never together with a custom grace, and pair it
with `--persistent` for a permanent lane.
The configured grace sets an exact not-before deadline; actual cleanup normally follows within five
seconds on the supervisor's next reconciliation tick.

## Prohibitions

- Do not infer policy from the repository's language, size, contents, or the task's apparent risk.
- Do not add `-c` overlays on the caller's behalf.
- Do not enable `--worktree`, `--schema`, or `--web` because they seem useful.
- Do not copy policy from a previous lane in the session unless the caller said to reuse it.
- `--cd` may default to the current working directory when the caller named no directory — say
  which directory was used.

## Approval is a decision, not a default

A detached lane has no TUI, so it cannot answer an interactive approval prompt. A lane that needs
tool access and inherits a prompting approval policy can stall until its deadline.

`--approval-policy never` is therefore often the right choice — and it is exactly the kind of
choice a skill must not make silently, because it removes a human checkpoint. Ask the caller,
offering the two real options:

- pass `--approval-policy never` so the lane runs unattended, or
- leave the approval policy inherited and accept that tool calls may block.

Sandboxing is independent of approval. Peer messaging is client-side and keeps working under a
`read-only` sandbox, so a read-only lane is still fully messageable.

## What the wrapper does on its own

The wrapper overlays exactly one internal setting, `features.code_mode_host=false`, because a
detached lane has no attached TUI to act as an external code-mode host. That is execution plumbing,
not agent policy. Do not try to restore or override it.

## Permissions on the Claude side

Every lane command runs through the `Bash` tool and prompts for permission unless the user has
allowed it. This plugin ships no permission grants and no settings. The optional, user-owned
launcher allowlist is documented in `references/install.md`.
