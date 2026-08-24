# US5 Linux: no-Go prebuilt install and smoke

- Date: 2026-08-24
- Platform: Linux amd64, kernel `6.17.4-2-pve`
- Commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Parent: `5f881f771d349cc4d8f1c51c61dfcedc17a0adb2`
- Signature: good SSH signature from `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Version: `0.2.4`
- Qwen Code: `0.22.0`
- Credited root: `/tmp/qwen-us5-b8bc.tVh6EE` (private `0700`, removed after exact cleanup)
- Verdict: GREEN

## Package and no-Go installation

The credited archive was produced from the exact clean commit through the same
`make build-release-platform` entrypoint used by CI. Its SHA-256 was
`96a4a26aeb63b9107a2ecaded7544a254390357b69bfd5c54b0471778816173f`.

The archive was extracted into the credited root. `PATH` was set to
`$HOME/.local/bin:/usr/bin:/bin`, and `command -v go` returned no result.
The extracted prebuilt marker selected the packaged binaries; `make build` and
`make install` completed without Go into an isolated prefix and install root.
All 11 authoritative executables were present, executable, and linked from the
isolated prefix to the isolated install root.

| Executable | SHA-256 |
| --- | --- |
| `agent-session-runtime` | `18b5c0b36da1ee2256f3dfadba215c2fa92a682a48f7a125a054e90fb8ccd4f0` |
| `peer` | `6d729f66a4428eb7b3eb01a6d2b9fe3fba9260c9d38c46b1b55ee1981b13ac93` |
| `codex-peer` | `277fdf10419b68b0917e4bf2c545d6f58c57e708b751fa6c10e345a467cd4f51` |
| `claude-peer` | `1d4989afcc0f2c26dabcebb74564216bef48b7e2656fee82554745c1f80edfdd` |
| `grok-peer` | `31c97d42eab872f983ba05406c5b73f8fe79cd5a67b371a3e69c059137b00dc5` |
| `qwen-peer` | `8af8d9d96cf4d2dac337831a9c63410e3ed96989bd1f59ca3af122449284531d` |
| `codex-peer-lane` | `743662262c00dd8375f74e31dcc1c607a943ff6721934ad8b40aa04f0cfb79f4` |
| `claude-peer-lane` | `d4b4afcadf7675065e58510a4a36e57136414423760bf609b5d73d22e1155ff8` |
| `grok-peer-lane` | `e2c316e9902e49ce1c71efc98b4e878df9eeb53701a7e17a86b45df782dc25d7` |
| `qwen-peer-lane` | `ba7b2309f8ba4093754736399f14cba382b081f3a3b71b53047e779838817f6a` |
| `peer-federator` | `81fab1b0506b404bd81a62cb91a97717311cb52b740b62cc188616e01055d235` |

The packaged Qwen integration was installed with the packaged runtime, not a
source-built helper. Native inventory reported exact enabled extension
`agent-sessions (0.2.4)`. Its MCP command was `./scripts/native-entry`; the entry point was a regular
executable file with mode `0755`. No bare PATH-resolved runtime remained.

## MCP, peer, and lane smoke

The isolated host agent advertised `runtime_version: 0.2.4`, protocol 3, host
`qwen-us5-b8bc`, no hub, and the isolated runtime/state/registry roots. From a
clean workspace outside the repository, `qwen mcp list` resolved the packaged
entry to the installed extension's absolute `scripts/native-entry` and
reported `agent_sessions` connected; no repository `.mcp.json` could mask the
extension result.

The packaged interactive peer published as `qwen-us5-b8bc-peer`, successfully
discovered and called `mcp__agent_sessions__identity`, produced
`QWEN_US5_B8BC_PEER_OK`, remained interactive, and exited through native
`/quit`. The agent then reported `local_peers: 0`; its test-owned native session
was `4204530d-4493-4df2-a1ba-fd9c40d6fc06`.

The packaged persistent lane published wrapper session
`3be61810-8e89-44c0-8fa5-f2686890f282`, native Qwen session
`46b7f120-3413-4cd2-948e-ff931288475b`, and turn
`eb0d091d-9672-43c4-b4f4-2522473614d4`. It returned exactly
`QWEN_US5_B8BC_LANE_OK`, exited 0, and archived with no cleanup debt. The
token occurred in exactly one test-owned transcript under the dedicated Qwen
runtime.

Two lane-collection assertions were discarded as harness-confounded because
they treated a JSON event stream as one JSON document. The already-running
lane was collected and archived by its exact wrapper UUID; its terminal event
and exact token were unaffected.

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

## Rejected predecessor and regression closure

Predecessor `1524e3e...` used native-Qwen `${extensionPath}` syntax inside an
Agent Plugins v1 `mcp.json`. Qwen silently skipped that server in arbitrary
workspaces; a repository `.mcp.json` had masked the defect. Successor
`5f881f7...` changed the command to the vendor-required contained
`./scripts/native-entry` form and added an exact rejection regression. At the
final Linux gate, the previously failing real interactive contract
passed with session `99ca05f7-720d-482f-aab8-0b513df056bd`, a real Unix
delivery socket, zero hub round trips, and the full seven-cell Qwen lane
contract also passed.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
