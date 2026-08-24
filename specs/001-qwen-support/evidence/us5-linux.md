# US5 Linux: no-Go prebuilt install and smoke

- Date: 2026-08-24
- Platform: Linux amd64, kernel `6.17.4-2-pve`
- Commit: `1524e3e22645e8a5f471b093a7a91218066dda0c`
- Tree: `845050aa198b13ed9faa3ecd761465b6112d403c`
- Parent: `fc8bd380a4087ccd0a93081d9bc880c760e311c5`
- Signature: good SSH signature from `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Version: `0.2.4`
- Qwen Code: `0.22.0`
- Credited root: `/tmp/qwen-us5-linux.b2tEVA` (private `0700`, removed after exact cleanup)
- Verdict: RED — the smoke below passed, but the subsequent full interactive
  contract could not discover the installed `agent_sessions` MCP tools. This
  evidence remains diagnostic only until the live MCP-registration regression
  is fixed and the complete contract is rerun.

## Package and no-Go installation

The credited archive was produced twice from the clean commit through the same
`make build-release-platform` entrypoint used by CI. Both clean Linux-x64
archives had SHA-256
`b767012ccbe30459f596208658a455aff39f5d53f187d6272ae1eb84c9e86621`.
The earlier archive that differed was not a reproducibility failure: Go build
metadata proved that it came from dirty parent commit `fc8bd380...`, whereas
both credited archives came from clean `1524e3e...`.

The archive was extracted into the credited root. `PATH` was set to
`$HOME/.local/bin:/usr/bin:/bin`, and `command -v go` returned no result.
The extracted prebuilt marker selected the packaged binaries; `make build` and
`make install` completed without Go into an isolated prefix and install root.
All 11 authoritative executables were present, executable, and linked from the
isolated prefix to the isolated install root.

| Executable | SHA-256 |
| --- | --- |
| `agent-session-runtime` | `22f76262036fd0ea6f6a2cdae39cbbefdae95168d72ffbea43d221f49d5f91f1` |
| `peer` | `4d84e362286d2d90b310a20d6976af5f97ac01cb3113a22a9f314855a337d21a` |
| `codex-peer` | `c8239a9eddabbdba8bb80562c974a5cae8b5777f9af5479e62ebe770f9da637b` |
| `claude-peer` | `e4c45cbc6faea06bed9d7ce8d5d7282bff90412f72e895bcb3579cfbebec82d9` |
| `grok-peer` | `cc39efd128481e9f64383ff7033f488957a7f80ec00b096cbc8424bfa0c66402` |
| `qwen-peer` | `6db33a0948a4e8cf2c2948f100e510acac1945ab4eedf35c4b3ded60da56a4f5` |
| `codex-peer-lane` | `ae7bf16d4542ac0b90a3377cd7b125c36a2658ed1479358e301ca618fb0bdd2a` |
| `claude-peer-lane` | `c08a2f8e4da3d17fa35c3045f3bf596d2cc7c56d695cb3e74b1308865d4c33ef` |
| `grok-peer-lane` | `a35fa48323e54bdb8d203ca9f6a19c7714edc16c52d51f1c4f94fe0030cfe63b` |
| `qwen-peer-lane` | `18ade8d9168142c256f949e08fb7cd1e67e8e4d388f1dc3431b0f05038cb65df` |
| `peer-federator` | `f4e445f0fca1929cf031704640d546072a347de269df09ef4298a26919b2e544` |

The packaged Qwen integration was installed with the packaged runtime, not a
source-built helper. Native inventory reported exact enabled extension
`agent-sessions (0.2.4)`. Its MCP command was `./scripts/native-entry`; the entry point was a regular
executable file with mode `0755`. No bare PATH-resolved runtime remained.

## Doctor, peer, and lane smoke

The isolated host agent advertised `runtime_version: 0.2.4`, protocol 3, host
`qwen-us5-linux`, no hub, and the isolated runtime/state/registry roots. The
packaged `qwen-peer-lane doctor --json` returned `ok: true`, Qwen `0.22.0`,
`auth_state: ready`, `integration_ready: true`, all parser contracts ready,
archive/interactive/ACP contracts ready, and the exact explicit selected
profile plus dedicated `QWEN_RUNTIME_DIR`.

The packaged interactive peer published as `qwen-us5-prebuilt-peer`, produced
`QWEN_US5_PREBUILT_PEER_OK`, remained interactive, and exited through native
`/quit`. The agent then reported `local_peers: 0`; its test-owned native session
was `e673181e-f939-431e-a0a7-6d9c39edd9c7`.

The packaged persistent lane published wrapper session
`563e5352-9337-4995-9406-8ab9b57f75a6`, native Qwen session
`a4117523-c436-4d13-bb8f-13519e918387`, and turn
`6ec6f99d-39e7-4648-91ff-0788388bf14c`. It returned exactly
`QWEN_US5_PREBUILT_LANE_OK`, exited 0, and archived with no cleanup debt. The
token occurred in exactly one test-owned transcript under the dedicated Qwen
runtime.

One peer launch was discarded as harness-confounded because its `--state-dir`
mistakenly named the lifecycle state root instead of the running agent's state
root. It failed before child launch with the exact mismatch diagnostic and
created no native transcript. The corrected command used the agent's exact
state root and is the only peer launch credited above.

## Profile and cleanup boundary

The already-authorized authenticated default Qwen profile supplied readiness;
no credential file or value was read, printed, copied, diffed, or hashed.
Before and after the cell, `settings.json` retained inode `2103632`, size 922,
mode `0664`, and mtime `2026-08-23T17:24:25.241460602Z`; the owner `projects/`
directory retained inode `2104072`, size 4, mode `0775`, and mtime
`2026-08-23T17:25:14.909241811Z`. Test transcripts were written only below the
dedicated `QWEN_RUNTIME_DIR`. Native extension-store counters/journals remained
Qwen-owned bookkeeping.

After the smoke, the lane was archived, the peer exited natively, the exact
isolated agent was terminated, and the supported supervisor and App Server
stop commands returned stopped. There were zero sockets and zero processes
rooted in the credited directory. The only escaping symlink, the isolated
Codex-home `packages/standalone` link, was unlinked explicitly; the validated
uid-owned, non-symlink, same-device test root was then removed with a bounded
depth-first delete. The authorized source-tree Qwen extension was restored and
remains enabled at version `0.2.4`.

## Post-cell regression gate

After this smoke, `scripts/test-qwen-contract` launched a managed Qwen peer at
the same commit but timed out waiting for `QWEN_DIRECT_FROM_QWEN`. Its
test-owned transcript shows the Agent Sessions skill loaded, while every
`mcp__agent_sessions__*` ToolSearch returned no tools. Static doctor readiness
therefore did not prove that Qwen had registered the live MCP server. The
contract failed before acceptance, so this smoke does not complete T076.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
