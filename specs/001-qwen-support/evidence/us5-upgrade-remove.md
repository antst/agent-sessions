# US5 Qwen upgrade and removal safety

- Date: 2026-08-24
- Platform: Linux amd64
- Final gate commit/tree: `b8bc0136ca37de484588d2e3ce4a978f186a19a7` /
  `2e314a145d99f907ccfe71b568f27c1417395805`
- Qwen Code / integration: `0.22.0` / `agent-sessions 0.2.4`
- Verdict: GREEN

The selected authenticated profile was the owner's already-authorized
`/home/antst/.qwen`; no credential material was inspected or copied. Default
selection and explicit `QWEN_HOME=/home/antst/.qwen` plus the dedicated
`QWEN_RUNTIME_DIR=/tmp/qwen-us5-linux.b2tEVA/qwen-runtime` were both exercised.

## Credited operations

1. A no-Go default-profile install from the extracted prebuilt archive replaced
   the source-selected same-version extension through native scoped
   uninstall/install. Exact post-verification required the packaged source,
   version, manifest, contained `./scripts/native-entry` MCP command,
   executable native entry, and full skill inventory.
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

The original mutation cell ran at `1524e3e...`; its static verifier accepted a
native-Qwen `${extensionPath}` command that Agent Plugins v1 silently skipped.
Successor `5f881f7...` corrected the command and exact verifier, and final
`b8bc013...` reran the packaged install plus real interactive and lane smokes.
Both complete Qwen contracts passed after the correction, so the earlier
mutation evidence is no longer isolated from live MCP-registration proof.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
