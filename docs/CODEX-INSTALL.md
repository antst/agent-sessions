# Codex-side installation

The Codex side owns the native Agent Sessions runtime, launchers, managed App Server integration,
lifecycle hooks, and process-attested MCP server. Install it before the optional Claude and Grok
integrations.

## Install

From a source checkout:

```bash
git clone https://github.com/antst/agent-sessions.git ~/agent-sessions
cd ~/agent-sessions
make test
codex app-server daemon stop
make install
```

From a release archive, verify `SHA256SUMS`, extract the archive for the current OS and
architecture, stop every Codex client and managed App Server, then run `make install`. The target
installs the canonical host executable and its command aliases under the selected prefix, registers
the `agent-sessions` marketplace, and installs `agent-sessions@agent-sessions` into Codex.

Use `make install-all` to install the shared runtime plus every product integration whose native
client is present. Missing Claude, Grok, or Qwen clients are reported and skipped. The explicit
`make install-claude`, `make install-grok`, and `make install-qwen` targets remain strict.

## Activate and verify

Start a new managed Codex TUI after installation. On the first managed Codex launch, the unified
daemon invokes Codex's supported `app-server daemon start` operation for the selected profile, waits
for the vendor endpoint, and reuses that App Server for subsequent sessions. Approve the one-time
`agent-sessions@agent-sessions` plugin hook prompt; the approval enables the installed lifecycle
hooks but does not change Codex sandbox or tool-approval policy.

Verify the installed side from a host terminal:

```bash
codex plugin list
codex app-server daemon version
codex-peer -n reviewer -g project-a
codex-peer-lane doctor --json
agent-sessions status --json
```

An already-running TUI retains its old plugin snapshot and must be restarted. The installer refuses
to replace a live App Server or an unverifiable managed Grok process tree; it does not silently
restart active product sessions. The App Server remains an external vendor process: Agent Sessions
starts it through the supported Codex command when first needed, but does not replace its native
implementation, state, or service contract.

## Development installs

`make dev-install` deliberately points the installed native runtime and Codex marketplace at a
mutable checkout. `make reinstall` refreshes the plugin cachebuster, rebuilds, and reinstalls. Use
the repository's `make lint`, `make test`, and `make test-race` targets rather than an unrelated
system-linter invocation.

The default locations can be changed with `PREFIX`, `INSTALL_ROOT`, and `CODEX`. Product state is
profile-scoped by `CODEX_HOME`; runtime socket and lifecycle locations also honor the documented
XDG and Agent Sessions overrides.

See the cross-product [INSTALL.md](INSTALL.md) for release archives, platform paths, safe update
ordering, runtime ownership, and recovery procedures. See [CODEX-ADAPTER.md](CODEX-ADAPTER.md) for
interactive peers and [CODEX-LANES.md](CODEX-LANES.md) for worker lanes.
