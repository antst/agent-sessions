# Portable Codex lane installation

The Claude plugin contains instructions only. The host also needs the native Codex messaging
runtime that provides `codex-peer-lane`.

## Install both sides from source

Run these commands yourself in a host terminal, outside any Codex turn:

```bash
git clone https://github.com/antst/sessionbus.git
cd sessionbus
make test-race
codex app-server daemon stop
make install-all
```

Exit every Codex client before stopping App Server. The installer refuses to replace a live server
because doing so can interrupt an active rollout. Restart Claude Code after installation so it
loads the new plugin.

To install only the runtime, use `make install`. To add only the Claude plugin to a host that
already has a compatible runtime, use `make install-claude`.

## Install the Claude plugin from its marketplace

```bash
claude plugin marketplace add https://github.com/antst/sessionbus.git
claude plugin install sessionbus@sessionbus
```

For a local checkout, pass its path to `claude plugin marketplace add` instead. A colleague using
the SSH URL needs read access to the repository.

## Verify

Start a new Claude Code session, then run:

```text
/sessionbus:doctor
```

Proceed only when its report has `summary: "ready"`, `contract_version: 2`, and
`runtime_ready: true`. The skill never installs, updates, or starts host services itself.

## Optional manual-launcher permission

The plugin grants no tools or permissions. A user who intentionally drives the PATH launcher can
reduce repeated prompts with this entry in their own Claude settings:

```json
{
  "permissions": {
    "allow": ["Bash(codex-peer-lane:*)"]
  }
}
```

The portable preflight reports an exact native runtime invocation instead. Approve that command
when prompted; do not broaden the permission merely to hide prompts.
