# Installing the Qwen Code integration

Agent Sessions supports Qwen Code 0.21.15 or newer. Install or authenticate
Qwen through Qwen's own supported flow first; Agent Sessions never copies,
migrates, or reads credential values.

## Selected profile

The integration is installed into exactly the profile selected by the current
environment:

- with `QWEN_HOME` unset, Qwen's native `$HOME/.qwen` default is preserved;
- with `QWEN_HOME` set, it must be a non-empty canonical absolute path with no
  mutable symlink ambiguity; fixed macOS `/tmp` and `/var` system aliases are
  resolved to their `/private/...` targets;
- `QWEN_RUNTIME_DIR`, when set, has the same presence-sensitive rule.

Run install, doctor, the host daemon, peers, and lanes with the same values. An
unset variable and a variable explicitly set to the native-looking path are
different identities by design.

## Install, upgrade, and remove

From a source checkout or an unpacked prebuilt release:

```bash
make install-qwen
make upgrade-qwen       # same verified, idempotent transaction
make remove-qwen
```

`make install-all` installs the host role and each Codex, Claude, Grok, or Qwen integration whose
native client is present. Missing products are reported and skipped; `make install-qwen` remains
strict when requested directly. A prebuilt archive needs no Go toolchain: its two role images,
manifest, checksums, service assets, and four product payloads are validated before installation.

The Qwen operation uses native `qwen extensions install/update/uninstall` at
user scope. A version change uses native update. Because Qwen treats a
same-version local source as already current even when its files changed,
same-version drift or source reconciliation uses a scoped native
uninstall/install of `agent-sessions` after the live-profile refusal gate.
Before that replacement transaction, Agent Sessions verifies the prior recorded local source and
plugin identity. If the new native install or exact post-install verification fails, it removes the
failed replacement and restores the prior enabled plugin through Qwen's installer. If no usable
rollback source exists, the command fails before uninstalling the current extension.
Before and after a mutation it verifies the exact Agent Plugins v1
manifest, version, enabled policy, one `agent_sessions` stdio MCP server, and
five direct-child skills (`agent-sessions` plus four lane skills). Drift yields
a cause-specific error. The recorded native extension source must also equal
the selected stable plugin root: a same-version `dev-install-qwen` is therefore
reconciled back to `INSTALL_ROOT/qwen` by a later release install instead of
leaving future native updates attached to a checkout. Exact already-current
install and already-absent remove are idempotent.

The installed MCP command uses the Agent Plugins v1 contained
`./scripts/native-entry` form and enters the exact canonical `agent-sessions`
host image selected by the installation pointer; it never resolves an ambient
runtime from `PATH`. Managed Qwen parents receive their exact daemon-issued
connector identity. A missing or stale selected image fails closed with an
installation diagnostic instead of silently loading a different release.

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

For remote Qwen lanes, enable remote execution only through a trusted configured hub. Use
`agent-sessions lane --host HOST --product qwen -- ...`; there is no product-specific listener,
executable override, or fallback transport. The host daemon advertises `qwen-lane` only when its
Qwen adapter is ready in the selected profile and republishes its ready-product set on federation
recovery. Remote doctor must report `ready: true`, authority `remote-daemon`, the exact requested
host, and product `qwen`. If readiness is absent, inspect host status/doctor and the selected Qwen
profile; do not substitute a caller-chosen binary or restart an unrelated hub.

See [INSTALL.md](INSTALL.md), [QWEN-ADAPTER.md](QWEN-ADAPTER.md), and
[QWEN-LANES.md](QWEN-LANES.md).
