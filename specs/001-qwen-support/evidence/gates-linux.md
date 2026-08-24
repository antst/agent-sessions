# Qwen Linux rehearsal gate

- Date: 2026-08-24
- Platform: Ubuntu 24.04.4 LTS, Linux `6.17.4-2-pve`, amd64
- Commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Signature: good SSH signature from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Version: `0.2.4`
- Evidence class: pre-candidate rehearsal, not tagged-commit evidence
- Verdict: GREEN

## Toolchain and native clients

| Component | Exact version |
| --- | --- |
| Go | `go1.26.5 linux/amd64` |
| Repository-managed golangci-lint | `2.12.2`, built with Go 1.26.5 |
| Codex | `0.149.0` |
| Claude Code | `2.1.241` |
| Grok | `1.0.5 (5115b46bc9) [stable]` |
| Qwen Code | `0.22.0` |

## Source gate

Every command ran from the exact clean commit and exited 0:

```text
make test
make test-race
go vet ./...
make lint
```

The normal and race suites emitted zero top-level failures; the race suite
emitted zero data-race reports. `make lint` used the repository-managed binary
and reported `0 issues.` The grouped federation integration passed in both
test modes. `git status --porcelain` was empty at the end of the gate.

All four calls to the same repository-owned `build-release-platform`
entrypoint used by CI exited 0 with `RELEASE_VERSION=0.2.4`:

| Platform | Rehearsal archive SHA-256 |
| --- | --- |
| linux-x64 | `96a4a26aeb63b9107a2ecaded7544a254390357b69bfd5c54b0471778816173f` |
| linux-arm64 | `44fb8f10261e8f81e757a8a34d901ff4d0fac68a7921e6538c9b1c8b07a8f360` |
| darwin-x64 | `3fc0ae45af76f9d1b5f1ef00645079c9ca38ec9d45e4826823c6f7127f1649c1` |
| darwin-arm64 | `47a26b6500abd33a0c7966c711d7bd71147df7ed9236b68392ce2c6f4905e012` |

These are disposable rehearsal packages, not release assets.

## Real Qwen contracts

The previously failing full interactive contract passed after the Agent
Plugins v1 MCP command was corrected to `./scripts/native-entry`:

```json
{"delivery_socket_type":"unix","elapsed_seconds":105.959,"hub_round_trips":0,"session_id":"99ca05f7-720d-482f-aab8-0b513df056bd","type":"qwen.contract.passed"}
```

The complete seven-cell Qwen lane lifecycle contract also printed
`Qwen lane contract: PASS`. It covered publish, collect, follow-up, interrupt,
persistent resume, archive/idempotency, crash cleanup, and exact native
transcript state. No contract-owned process or socket survived either run.

## No-Go packaged discriminator

The exact linux-x64 archive was extracted into a private test root. With
`PATH=$HOME/.local/bin:/usr/bin:/bin`, `command -v go` returned no result, and
the packaged marker drove `make install` from the prebuilt eleven-binary
inventory.

From a clean workspace outside the repository, with no project `.mcp.json`,
Qwen reported the packaged `agent_sessions` MCP connected at the installed
extension's absolute `scripts/native-entry`. Packaged interactive peer
`qwen-us5-b8bc-peer` discovered and called
`mcp__agent_sessions__identity`, returned `QWEN_US5_B8BC_PEER_OK`, and exited
through `/quit`. Packaged persistent lane
`3be61810-8e89-44c0-8fa5-f2686890f282` returned
`QWEN_US5_B8BC_LANE_OK`, completed with exit 0, and archived cleanly.

The isolated host agent had no hub, returned to zero local peers, and was
stopped by exact process ownership. The test supervisor and isolated Codex App
Server were stopped through supported controls. The only escaping symlink was
unlinked explicitly, then every validated uid-owned test root was removed by
an exact-device depth-first delete. The authorized source Qwen and Codex
plugin selections were restored afterward.

## RCA and harness classification

The rejected predecessor used native-Qwen `${extensionPath}` syntax inside an
Agent Plugins v1 manifest. Qwen silently skipped the server outside the
repository; the repository's own `.mcp.json` had masked it. The final verifier
now requires the vendor-supported contained relative command and has a
regression rejecting the old syntax.

Two no-Go lane assertions mistakenly parsed a JSON event stream as one JSON
document. They failed after the product had already started/completed the
single lane; exact UUID collection and archive closed it without a retry.
Those parser assertions receive no credit.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
