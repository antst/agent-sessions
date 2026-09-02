# Codex interactive peer adapter

`codex-peer` runs the ordinary Codex TUI while giving its exact root thread a managed Agent
Sessions identity. Bare `codex` remains outside Agent Sessions: it receives no discovery row,
group membership, messaging authority, or lifecycle owner record.

## Invocation and identity

Start a fresh managed peer with an optional name and one or more groups:

```bash
codex-peer -n reviewer -g project-a
codex-peer --peer-name reviewer --group project-a --group release
```

`-g` and `--group` are equivalent and repeatable on every interactive peer launcher. The adapter
selects a native thread UUID before publication, starts the managed App Server, and publishes only
after the exact TUI owner, thread, cwd, shim, process starts, and delivery socket agree.

Resume by exact UUID or product-owned name through either the uniform wrapper
flag or Codex's native spelling:

```bash
codex-peer --resume reviewer
codex-peer resume 019fe660-1c86-7700-b462-6ff16de00fc5
codex-peer resume reviewer
```

Codex can resolve a name natively, but its external parent cannot learn which
thread the TUI selected ([upstream issue #35676](https://github.com/openai/codex/issues/35676)).
Until Codex exposes that selection, the terminal-owning wrapper reads Codex's
own thread list before launch so presence can report the exact UUID. One match
is selected; multiple matches use an interactive picker, while a headless call
prints the same product-provided UUID, cwd, and update details and exits. Exact
UUID resume goes directly to Codex without listing. This exception is deleted
when Codex exposes the selected thread to its parent.

Groups are supplied anew on every start or resume invocation. They are never
copied from an earlier run: omitting `-g` on resume intentionally leaves only
the peer's derived private group, while adding or omitting group arguments is
the complete add/remove operation.

## Permissions

Normal launches inherit Codex policy. `--yolo` is the managed shorthand for Codex's native
`--dangerously-bypass-approvals-and-sandbox` behavior and is also mirrored through App Server before
publication so the advertised permission class matches the durable thread setting:

```bash
codex-peer --yolo -n isolated-reviewer -g project-a
```

This is an explicit full-access opt-in. Group membership and messaging do not grant tool approval,
change the sandbox, or widen a thread's permission mode.

## Messaging

The installed `agent-sessions` Codex plugin supplies the process-attested `agent_sessions` MCP tools.
Only a live managed root thread can list visible peers, send or broadcast messages, or launch a
lane through those tools. A model-provided session ID never grants authority; App Server thread
identity, the TUI owner, hook context, shim, and host registration must corroborate it.

Messages are routed through the local host agent and filtered by shared groups. A peer always has a
private session anchor in addition to its explicit groups. Native Claude registry rows are a local
carrier and presentation surface, not a global namespace or a substitute for Agent Sessions group
checks.

## Lifecycle

The TUI owns the interactive peer lifetime. Normal exit removes the bridge-owned shim, row, key,
and sockets while retaining the Codex transcript for resume. Exact process-start identities let the
supervisor perform the same scoped cleanup after `SIGKILL` without acting on a recycled PID.
Ordinary Codex threads and child subagents are never adopted heuristically.

See [CODEX-INSTALL.md](CODEX-INSTALL.md) for installation,
[CODEX-LANES.md](CODEX-LANES.md) for durable worker lanes, and
[ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md) for the shared carrier and authorization details.
