# US5 Qwen upgrade and removal safety

- Date: 2026-08-24
- Platform: Linux amd64
- Commit/tree: `1524e3e22645e8a5f471b093a7a91218066dda0c` /
  `845050aa198b13ed9faa3ecd761465b6112d403c`
- Qwen Code / integration: `0.22.0` / `agent-sessions 0.2.4`
- Verdict: RED — the native mutation operations below passed, but the
  subsequently launched managed Qwen peer had no discoverable
  `agent_sessions` MCP tools. Upgrade/removal credit is held until the live
  MCP-registration regression is fixed and the full contract is rerun.

The selected authenticated profile was the owner's already-authorized
`/home/antst/.qwen`; no credential material was inspected or copied. Default
selection and explicit `QWEN_HOME=/home/antst/.qwen` plus the dedicated
`QWEN_RUNTIME_DIR=/tmp/qwen-us5-linux.b2tEVA/qwen-runtime` were both exercised.

## Credited operations

1. A no-Go default-profile install from the extracted prebuilt archive replaced
   the source-selected same-version extension through native scoped
   uninstall/install. Exact post-verification required the packaged source,
   version, manifest, extension-rooted MCP command, executable native entry,
   and full skill inventory.
2. Repeating explicit-profile `upgrade-qwen` against that exact payload exited
   0 idempotently and performed no replacement.
3. While managed peer `qwen-us5-live-refusal` was live on that exact profile,
   explicit-profile `remove-qwen` failed closed with
   `refuse Qwen plugin mutation while managed Qwen process 1272510 uses the selected profile`.
   The extension remained present. An exact already-current `upgrade-qwen`
   returned idempotently because no mutation was required; it is not credited
   as a live mutation attempt.
4. After the peer returned `QWEN_US5_LIVE_REFUSAL_READY` and exited through
   native `/quit`, explicit-profile removal succeeded and the extension root
   was absent.
5. Explicit-profile packaged installation succeeded from the empty extension
   state. Default-profile removal then succeeded and again left the extension
   absent.
6. `make dev-install-qwen` from the exact source tree restored the authorized
   extension. Native inventory reports version `0.2.4`, enabled at user and
   workspace scopes, with source `/home/antst/agent-sessions/qwen`.

The selected profile's `settings.json` and owner transcript-root metadata were
unchanged across the cell. Native session output was confined to the dedicated
runtime. Qwen legitimately updated only native extension-store bookkeeping
while uninstalling/installing the extension; no Agent Sessions code rewrote a
credential, provider setting, transcript, or unrelated extension.

Cleanup reused the exact US5 Linux boundary: local peers returned to zero, the
test lane was archived, supported supervisor/App Server stops completed, the
isolated agent stopped, no test-owned socket or process survived, and the
validated temporary root was removed.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
