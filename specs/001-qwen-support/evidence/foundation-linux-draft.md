# T017 Linux shared-foundation evidence — draft

## 1. Cell identity and verdict

| Field | Exact value |
| --- | --- |
| Requirement/task/cell | `T017` — product/group/lifecycle/help/race/cleanup regressions and stable delivery socket foundation |
| Attempt ID | `foundation-linux-20260821T183706Z`; repaired rerun `foundation-linux-20260821T183940Z` |
| Credited attempt | `partial — Linux automated and isolated process cells are green; managed-Claude and macOS cells remain` |
| Verdict | `BLOCKED-ENVIRONMENT` |
| Started/finished UTC | `2026-08-21T18:37:06Z` / `2026-08-21T18:46:12Z` |
| Host role | local Linux test host |
| OS/architecture | Ubuntu 24.04.4 LTS; Linux 6.17.4-2-pve; x86_64 |
| Operator boundary | Repository tests and dedicated `/tmp/t017-linux.*`/`/tmp/t017-grok.*` roots only. No installation, live owner profile mutation, credential read, or unrelated process signal was authorized or performed. Test-owned shims were signalled only after exact PID/start rechecks; only validated dedicated temporary roots were deleted. |
| First genuine RED | Initial extracted-prebuilt install lacked `qwen/`. The packager and all three archive/install assertions were repaired, and the exact full command passed on rerun. |

The narrow and full Linux regressions, race suite, vet, pinned lint, grouped federation integration,
and two isolated source-built process/socket cells passed after the packaging contract was repaired.
The delegated boundary did not provide an isolated authenticated managed Claude profile, and no real
macOS host ran the same final tree, so T017 as a cross-platform live requirement remains open.

## 2. Source and toolchain identity

| Field | Exact value |
| --- | --- |
| Commit SHA | `cafce25ec0239a8c743ca90c6bb8bf9c5b8e044a` |
| Commit tree | `31050c469729dcf074ab20eaeab25e158ba0b883` |
| Parent SHA | `6ec4c4606b0a1c00dbbace5b8e4ffb51562b7acd` |
| Commit subject | `Merge pull request #11 from antst/release/v0.2.0-grok-messaging-skill` |
| Branch | `develop` |
| Worktree | shared implementation worktree; 46 tracked/untracked status entries at finish; `git diff --check` passed |
| Go | `/usr/local/go/bin/go`; `go version go1.26.5 linux/amd64` |
| Repository-managed linter | `/home/antst/agent-sessions/bin/tools/golangci-lint`; `2.12.2`, built with Go 1.26.5 |
| Git | `/usr/bin/git`; `git version 2.43.0` |

Because this was a concurrently changing implementation worktree rather than a committed source tree,
this draft is diagnostic evidence only. Final Linux/macOS evidence must use one identical committed
tree and the repository-pinned linter required by the release gate.

## 3. Credited focused regressions

All commands ran from `/home/antst/agent-sessions` with `-count=1`.

### Product, group, route, and lifecycle registry

```text
go test ./internal/federator -run 'Test(ProductDescriptors|SessionCatalog|Grouped|PrepareRemoteLane|PeerRegistration|Reconcile|CommittedClaudePeerCleanupDebt|CurrentRegistryFormat)' -count=1
ok github.com/antst/agent-sessions/internal/federator 0.415s
```

This covers the authoritative four-product inventory, exact group/catalog behavior, grouped routing,
remote-lane parent construction, registration, reconciliation, cleanup debt, and current registry
format.

### Launcher product/group/native cleanup

```text
go test ./internal/launcher -run 'Test(LauncherProductProjection|PeerLaunchContext|CleanupClaudePeerArtifacts|ReadClaudeNativePeerRecord|GenericResumeInvocation)' -count=1
ok github.com/antst/agent-sessions/internal/launcher 0.009s
```

### Bridge help, lifecycle, stable socket, and cleanup

```text
go test ./internal/bridge -run 'Test(BridgeProductProjection|ExistingLaneParsedGroupOptions|.*LaneUsageAdvertisesGroupOptions|NativeShimPublishesPrivateStablePeerAndQueuesMessage|CleanupPreservesNativeClaudeAndUnownedFiles|CleanupRemovesOrphanedBridgeSocketAliases|.*LaneCleanup|.*OwnerDeath|ToolRootLedgerCrashRetryRetainsCleanupDebt)' -count=1
ok github.com/antst/agent-sessions/internal/bridge 2.395s
```

`TestNativeShimPublishesPrivateStablePeerAndQueuesMessage` created a real Linux Unix listener and
asserted with `os.Lstat` that `socketPath == backendSocketPath`, the published path had
`os.ModeSocket`, lacked `os.ModeSymlink`, rejected `os.Readlink`, was mode `0600`, and accepted a
native-framed delivery directly at that path. No caller-side path resolution occurred.

`TestCleanupRemovesOrphanedBridgeSocketAliases` exercised the local upgrade path for an exactly owned
legacy stable symlink plus its dead PID-bound backend. `TestCleanupPreservesNativeClaudeAndUnownedFiles`
and the lane/ledger cleanup cases verified that unrelated controls survive.

## 4. Full automated Linux boundary

After the repair described below, both repository-managed suites passed:

```text
make test
exit 0
grouped federation integration: PASS

make test-race
exit 0
grouped federation integration: PASS
```

The normal bridge package completed in `16.498s`; the race bridge package completed in `24.906s`.
Every other Go package passed, including `internal/qwenprofile` and `internal/qwenreadiness`. No race
finding was emitted. The extracted prebuilt release selected its packaged Linux binaries and its real
`make install` completed with the full Qwen payload.

Additional gates:

```text
go vet ./...
exit 0

make lint
/home/antst/agent-sessions/bin/tools/golangci-lint config verify
/home/antst/agent-sessions/bin/tools/golangci-lint run
0 issues.
```

## 5. Initial RED, RCA, and repaired rerun

Exact command:

```text
make test
```

Exit: `2`.

Relevant output:

```text
./scripts/test
Validating marketplace manifest: /home/antst/agent-sessions/.claude-plugin/marketplace.json
Validation passed
Validating plugin manifest: /home/antst/agent-sessions/claude/.claude-plugin/plugin.json
Validation passed
{"liveSessionIds":[],"stopped":true}
sed: can't read qwen/plugin.json: No such file or directory
sed: can't read qwen/plugin.json: No such file or directory
Using packaged prebuilt binaries in /tmp/codex-peer-test.7yzBP2/release-extract/agent-sessions-v0.1.0-linux-x64/bin/linux-x64
cp: cannot stat 'qwen': No such file or directory
make[1]: *** [Makefile:203: install] Error 1
make: *** [Makefile:105: test] Error 2
```

The failure is deterministic from the contract visible in the same tree:

- `Makefile` derives `QWEN_PLUGIN_VERSION` from `qwen/plugin.json` and unconditionally copies `qwen`
  during `install`.
- `scripts/test` builds a slim prebuilt archive, extracts it, and exercises the extracted archive's
  real `make install` path.
- The archive inventory assertions cover Claude and Grok plugin payloads but not `qwen/`, so the
  release fixture presented an installable archive missing a newly mandatory install payload.

This was not a timing failure. The repair added `qwen/` to `scripts/package-release` and added exact
Qwen manifest, MCP, messaging skill, and four lane-skill assertions to the source install, release
inventory, and extracted-release install cells in `scripts/test`. The repair diff for
`scripts/package-release` plus `scripts/test` had SHA-256
`59845c0e1cca8df2b43927d59decde41a081ff4fcd7106ce64cbfe9f0eae6648` at the final capture. The exact
`make test` command then passed; no retry-only workaround was used.

## 6. Real Linux source-built stable-socket discriminators

Two credited process-level cells built `./cmd/agent-session-runtime` with `-trimpath` into dedicated
private temporary roots. Both binaries had SHA-256
`bdacf3b52b86518559a255a11e8270c632d2cb3b1108b62c66970f0e3b5a081b`. Each shim used only isolated
state, Claude-registry, Codex-home, and runtime directories.

### Codex-shaped bridge

```text
root: /tmp/t017-linux.BXw1zc
owner: pid 2969594, Linux start token 1771958284
shim: pid 2969673, Linux start token 1771958331
published: /tmp/t017-linux.BXw1zc/run/codex-claude-peer-1000/session-d3ce0d5bc9977b86d0ce.sock
lstat: type=socket device=121 inode=520955 uid=1000 mode=600
direct frame: id=t017-direct-native, message=T017_DIRECT_NATIVE
shim exit: 0
remaining runtime sockets: 0
preserved control SHA-256: 47ffc8f5e3cbd88fa4922e46091cc2a174650a37ad05fce27f641c534587828d
temporary root removed: true
```

The sender wrote directly to the published path with `nc -N -U`; no `readlink`, path rewrite, or
resolved backend path was supplied. The inbox record carried the exact correlated ID and message.

### Grok-shaped bridge

```text
root: /tmp/t017-grok.cdPk2E
owner: pid 2970233, Linux start token 1771960920
shim: pid 2970310, Linux start token 1771960964
published: /tmp/t017-grok.cdPk2E/run/codex-claude-peer-1000/session-27d66586380ed7242930.sock
lstat: type=socket device=121 inode=520687 uid=1000 mode=600
direct frame: id=t017-direct-grok, message=T017_DIRECT_GROK
shim exit: 0
remaining runtime sockets: 0
preserved control SHA-256: 7ea16b60b1a81342409ed50b192e7a63c81acbc055aeff3c9aafeac3ae021d7f
temporary root removed: true
```

Immediately before each `SIGTERM`, PID plus Linux start token were re-read and matched. Normal shutdown
removed only the run-owned socket, native row, and state record; the unrelated control hash remained
identical. Each dedicated root was revalidated as a real uid-1000 directory before exact-device
`find <root> -xdev -depth -delete` removal.

One earlier Codex-shaped attempt had identical product assertions but used `find -xdev <root>` in the
wrong argument order during final harness cleanup. It exited 1 and was discarded as
`HARNESS-CONFOUNDED`; the exact remaining `/tmp/t017-linux.VrzojS` root was checked as a real uid-1000
directory with no sockets or open files, then removed with the corrected syntax before the credited
attempt.

## 7. Remaining blocked/not-run cells

- Live managed-Claude-to-source-built Codex and Grok delivery. The process cells above prove that a
  Claude-shaped native frame reaches each exact real published socket without resolution, but the
  sender was the bounded harness rather than a managed Claude TUI. A true native `SendMessage` cell
  requires an isolated,
  authenticated managed Claude profile; mutating or reusing an owner profile was outside this
  delegated boundary;
- Crash cleanup at process level beyond the existing cleanup/race integrations. Normal process-level
  exit cleanup is proven above;
- supported cross-build/package commands, left to the parent implementation gate because they create
  repository build artifacts and this delegation allowed no workspace writes beyond this evidence;
- all macOS evidence, which requires this exact final committed tree on a real macOS host. Cross-builds
  cannot replace that gate.

## 8. Safety statement

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
Owner processes signalled: NO
Owner sessions or registries mutated: NO
```

No task marker is justified by this draft. The release-fixture RED is closed and the Linux automated
and process/socket boundary is green, but T017 remains open until the identical committed-tree macOS
gate and managed-Claude direct-path cell are green.
