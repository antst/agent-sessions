# Linux Acceptance — Greenfield Runtime Candidate 08e915d

## Attributable identity

- Commit: `08e915da6b91ddb54eb4790054d1fc818d4b4dec`
- Tree: `dd448d5e9c3d0a412fee88f001cf15ed03cfe755`
- Parent: `e3a52b141617674f36a3a564bd944ad43486720c`
- Subject: `Establish greenfield unified daemon install`
- Signature: good SSH signature from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Branch: `feature/unified-user-daemon`

The evidence-only completion commit containing this file does not change the runtime, service,
connector, package, or test tree above.

## Host and toolchain

- Linux `6.17.4-2-pve` x86_64
- Go `1.26.5 linux/amd64`
- systemd `255`
- golangci-lint `2.12.2`

Credential-bearing vendor files were not read, copied, hashed, or compared. Public connector
manifests and bounded non-secret daemon metadata were inspected only where named below.

## Exact clean-tree gates

| Gate | Result |
|---|---:|
| `make test` | PASS |
| `make test-race` | PASS; zero race-detector findings |
| `go vet ./...` | PASS |
| `make lint` | PASS; `0 issues` |
| Installed service contract | PASS |
| Federation integration, installed hub, and binary pairs | PASS |
| Unified peers | PASS |
| Four-by-four lane composition | PASS, 16/16 |
| Four-product active-turn restart | PASS, zero redispatches |
| 100-attachment stress | PASS, zero duplicate turns |
| Four release builds | PASS |
| Exact package inventory | PASS; four archives, two binaries each |

The real installed-service result was:

```json
{"type":"unified.service.passed","platform":"Linux","optional_product_cardinalities":true,"all_four_upgrade_rollback":true,"observability_canary":true,"explicit_stop":true,"crash_restart":true,"removal":true,"purge":true}
```

This exercises absent, partial, and all-four native-product inventories; connector
prepare/apply/rollback; exact service restart; crash recovery; explicit stop; removal; purge; and the
metadata-only observability canaries. Missing optional vendor executables do not fail aggregate
installation.

## Greenfield local cutover

The owner closed the old peer sessions and authorized complete removal of pre-0.3 Agent
Sessions-owned runtime state. Exact old runtime processes were stopped; no vendor profile, native
history, credential store, settings file, or transcript was selected. The old Agent Sessions roots
remain recoverably archived at:

`/home/antst/.local/state/agent-sessions-pre-0.3.R0iCWq`

The exact candidate was then installed through `make install-all`. The same-revision transaction
completed at daemon generation 5 with:

- one active `systemd --user` service and one process;
- endpoint `/run/user/1000/agent-sessions/daemon.sock`;
- runtime version `0.3.0` and identity
  `sha256:f33d59b32cd2a3d51a5a7e7098f35e98fe6abb47418c4f2cf798a3c2ba9873de`;
- zero attachments, zero lanes, and zero lifecycle debt;
- all four native executables discovered;
- a committed four-product connector journal whose source and payload paths are inside the selected
  immutable release; and
- healthy service-manager, runtime-identity, state-schema, product-inventory, federation-protocol,
  and lifecycle-debt doctor checks.

The exact process census contained only the unified `agent-sessions daemon`. No
`agent-session-runtime`, supervisor, shim, product manager, lane manager, or `peer-federator`
process remained. The unrelated signing `ssh-agent` was not touched.

Grok inspection reported exactly one enabled user-scope `agent-sessions` plugin and exactly one
plugin-sourced `agent_sessions` stdio MCP. The repository workspace's own `.mcp.json` row is ignored
for installed user-plugin cardinality.

## Release archives

Exactly four archives were built from the clean signed tree. Each contains exactly the executable
basenames `agent-sessions` and `agent-sessions-hub`:

| Archive | SHA-256 |
|---|---|
| `agent-sessions-0.3.0-darwin-arm64.tar.gz` | `6c628c497f994c57d9a28fef37254b2319b03cea9be1eecfa4204cf35ef4ee3e` |
| `agent-sessions-0.3.0-darwin-x64.tar.gz` | `9b171dda9a3fd37c8f38812900db362ad0f9ac6c51874e3d8d0f94733b6d46d1` |
| `agent-sessions-0.3.0-linux-arm64.tar.gz` | `5e2afa59480ec321f35e9246d1c4e1bbfb948ec05215784f2ae5b49dedf3fd6e` |
| `agent-sessions-0.3.0-linux-x64.tar.gz` | `703adb2c4110c66267fd39252f3abca49bd877bec11fa8dd1562635b8f25fc08` |

There is no obsolete runtime, product-peer image, supervisor, shim, product host, lane manager, or
host federation-agent image in a release archive.

## Linux decision

The Linux candidate is green. Cross-platform final acceptance remains pending until the same signed
runtime candidate is exercised by hosted CI and independent macOS validation; no result from the
older candidate is carried forward as exact-candidate credit.
