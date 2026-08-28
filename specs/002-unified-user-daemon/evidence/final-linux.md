# Linux Acceptance — Greenfield Runtime Candidate 37e1977

## Attributable identity

- Commit: `37e1977356299f6cd741685df566835e80c96abf`
- Tree: `4bd400c4e8b1494e1cd0ea3b10c9de1e85307bbd`
- Parent: `f46a4d7d3adc27293a23947f6a1cd1cf9ed56aa2`
- Subject: `Restore lazy Codex App Server startup`
- Signature: good SSH signature from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Branch: `feature/unified-user-daemon`

The hosted run used this exact runtime head. No result from an earlier candidate is substituted for
the runtime, service, connector, package, or test tree.

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

The initial greenfield candidate was installed through `make install-all`, then the same supported
transaction upgraded the host to the exact candidate above. The latter transaction completed at
daemon generation 6 with:

- one active `systemd --user` service and one process;
- endpoint `/run/user/1000/agent-sessions/daemon.sock`;
- runtime version `0.3.0` and identity
  `sha256:a84e0f2c66c204e9190feca1e748e6642a74922c8104a1e1849cd4fac870ef53`;
- zero attachments, zero lanes, and zero lifecycle debt;
- all four native executables discovered;
- a committed four-product connector journal whose source and payload paths are inside the selected
  immutable release; and
- healthy service-manager, runtime-identity, state-schema, product-inventory, federation-protocol,
  and lifecycle-debt doctor checks.

Before the live Codex discriminator, the exact Agent Sessions process census contained only the
unified `agent-sessions daemon`. No `agent-session-runtime`, supervisor, shim, product manager, lane
manager, or `peer-federator` process remained. The unrelated signing `ssh-agent` was not touched.

The Codex App Server socket and process were then stopped and verified absent. Starting a fresh
managed Codex peer caused the unified daemon's `attachment.prepare` operation to invoke Codex's
supported vendor daemon start, publish the profile socket, and open the TUI through
`codex --remote unix://`. The App Server process was a direct child of the unified daemon; the peer
wrapper did not bootstrap it. The throwaway peer exited cleanly, no remote Codex process remained,
and the vendor App Server stayed available for reuse as intended.

Grok inspection reported exactly one enabled user-scope `agent-sessions` plugin and exactly one
plugin-sourced `agent_sessions` stdio MCP. The repository workspace's own `.mcp.json` row is ignored
for installed user-plugin cardinality.

## Release archives

Exactly four archives were built from the clean signed tree. Each contains exactly the executable
basenames `agent-sessions` and `agent-sessions-hub`:

| Archive | SHA-256 |
|---|---|
| `agent-sessions-0.3.0-darwin-arm64.tar.gz` | `fa1eb4d15e9fa828ac1364fc268fc78b28f411a31c397be0e96a8fb1e0d0162f` |
| `agent-sessions-0.3.0-darwin-x64.tar.gz` | `9c5cb1da35adabed9e98b2f7cc5e93a969d7f1a7c25e03f57ca3ac3c4618f3dd` |
| `agent-sessions-0.3.0-linux-arm64.tar.gz` | `caf1a370e03b7098dfc7263eb93343f7ce9f19ec2fe622b199a97b862f1dffb6` |
| `agent-sessions-0.3.0-linux-x64.tar.gz` | `a2b40adbe5eb9c83590fd8d518c042b8e44c5c8bae9257e4e23a3fbb1e9b5174` |

There is no obsolete runtime, product-peer image, supervisor, shim, product host, lane manager, or
host federation-agent image in a release archive.

## Linux decision

The Linux candidate is green locally and in hosted CI. Workflow run
[`33181867525`](https://github.com/antst/agent-sessions/actions/runs/33181867525) completed successfully
at exact runtime head `37e1977`. Its Linux normal, race, vet, lint, installed-service fixture, package
contract, inventory, and both Linux release-build jobs passed. No result from an older runtime
candidate is carried forward as exact-candidate credit.
