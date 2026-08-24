# Installing the Qwen Code integration

Agent Sessions supports Qwen Code 0.21.15 or newer. Install or authenticate
Qwen through Qwen's own supported flow first; Agent Sessions never copies,
migrates, or reads credential values.

## Selected profile

The integration is installed into exactly the profile selected by the current
environment:

- with `QWEN_HOME` unset, Qwen's native `$HOME/.qwen` default is preserved;
- with `QWEN_HOME` set, it must be a non-empty absolute non-symlink path;
- `QWEN_RUNTIME_DIR`, when set, has the same presence-sensitive rule.

Run install, doctor, the host agent, peers, and lanes with the same values. An
unset variable and a variable explicitly set to the native-looking path are
different identities by design.

## Install, upgrade, and remove

From a source checkout or an unpacked prebuilt release:

```bash
make install-qwen
make upgrade-qwen       # same verified, idempotent transaction
make remove-qwen
```

`make install-all` installs the shared runtime and each Codex, Claude, Grok, or Qwen integration
whose native client is present. Missing products are reported and skipped; `make install-qwen`
remains strict when requested directly. A prebuilt archive needs no Go toolchain: its eleven
platform binaries are validated before installation.

The Qwen operation uses native `qwen extensions install/update/uninstall` at
user scope. Before and after a mutation it verifies the exact Agent Plugins v1
manifest, version, enabled policy, one `agent_sessions` stdio MCP server, and
five direct-child skills (`agent-sessions` plus four lane skills). Drift yields
a cause-specific error. The recorded native extension source must also equal
the selected stable plugin root: a same-version `dev-install-qwen` is therefore
reconciled back to `INSTALL_ROOT/qwen` by a later release install instead of
leaving future native updates attached to a checkout. Exact already-current
install and already-absent remove are idempotent.

Upgrade or removal refuses while a managed Qwen peer or lane uses that exact
profile. Stop/archive it and retry. The gate reads process identity and
presence-sensitive profile environment; it does not trust stale catalog rows.
Only the `agent-sessions` extension is changed. Native settings, credentials,
other extensions, skills, and transcripts are outside the transaction.

## Verify

```bash
qwen-peer-lane doctor --json
qwen-peer --help
qwen-peer-lane --help
```

Doctor reports `ready: true` only when executable, parser, ACP, archive,
workspace trust, profile identity, and installed integration evidence agree.
Credential/provider configuration is reported as `ready`, `unknown`, or
`unready` without secret values and without pretending a provider login was
performed.

For remote Qwen lanes, enable remote lanes only on a trusted hub. The agent
auto-discovers `qwen-peer-lane`; override it with
`PEER_FEDERATOR_QWEN_LANE=/absolute/path/qwen-peer-lane`. The agent advertises
`qwen-lane` only after the same readiness engine passes in its selected
profile. Capabilities are fixed when the agent connects to its hub. If Qwen is
installed after an already-running agent withheld `qwen-lane`, restart that
agent after installation; product installation does not stop an unrelated
federation process automatically.

See [INSTALL.md](INSTALL.md), [QWEN-ADAPTER.md](QWEN-ADAPTER.md), and
[QWEN-LANES.md](QWEN-LANES.md).
