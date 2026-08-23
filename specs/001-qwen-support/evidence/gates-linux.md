# Qwen Linux rehearsal gate

- Timestamp: 2026-08-23T21:34:27Z
- Branch: `feature/qwen-support`
- Evidence class: pre-candidate frozen-worktree rehearsal; this is not final tagged-commit evidence
- Version under test: `0.2.4`
- Base commit before the feature worktree: `4402e9abe2499d1d99e81ad68ec7935b3c6665b9`

## Exact toolchain and native clients

- Go: `go version go1.26.5 linux/amd64`
- Repository-managed golangci-lint: `2.12.2`, built with Go 1.26.5
- Qwen Code: `0.22.0`
- Codex CLI: `0.149.0`
- Claude Code: `2.1.240`
- Grok: `1.0.5 (5115b46bc9) [stable]`

## Credited gates

All commands below completed with exit status 0 on the same frozen behavior:

1. `make test`
   - all Go packages passed;
   - marketplace and plugin validation passed;
   - the prebuilt release-install contract passed;
   - grouped federation integration reported `PASS`.
2. `make test-race`
   - all race-enabled Go packages passed;
   - zero `WARNING: DATA RACE` reports;
   - grouped federation integration reported `PASS`.
3. `go vet ./...`
4. `make lint`
   - used `bin/tools/golangci-lint`, not a system linter;
   - configuration verification passed;
   - result: `0 issues`.
5. `go test ./internal/bridge ./internal/federator ./internal/launcher ./internal/qwenprofile ./internal/qwenreadiness ./internal/releaseevidence ./internal/releasepkg -count=1`
6. `git diff --check`

## Four-platform build and package matrix

The same `make build-release-platform` entrypoint used by CI built eleven target
executables and packaged all four platforms successfully. Cross-target archives
were created by a host-native `agent-session-runtime release-package` helper;
the archives contain the target-native executables.

| Platform | Result | Rehearsal archive SHA-256 |
|---|---:|---|
| linux-x64 | PASS | `27531c7f9518a51ef6479388fd33a146636534eda741f74fc2b32e2b27ebd19f` |
| linux-arm64 | PASS | `66d11a13c660837a57e5dcd39586a8090fac2773add4a93ce8b81a23e590d27d` |
| darwin-x64 | PASS | `ecb4832f24d9527b2fd31880d95fee1ee105afde626b57116ad0e6a031cf6d57` |
| darwin-arm64 | PASS | `0fa737b176bdca52ebd7d6bee223db56bb99b1ba9fe08e3d25b075d45c6b91b0` |

These are disposable rehearsal hashes, not release artifacts.

## RCA closed before the credited run

The first lint rehearsal exposed new-code findings rather than a tool mismatch.
They were remediated without disabling any linter: wrapped errors now use
`errors.As`, state switches are exhaustive, dead symbols were removed, path
operations have local ownership justification, diagnostics follow Go error
conventions, and explicit state machines have function-local complexity
annotations. The final repository-managed lint was run twice consecutively and
reported zero issues both times.

The first cross-package rehearsal also exposed two real portability defects:

1. Darwin requires an explicit conversion of `Stat_t.Dev` where Linux does not.
   The portable conversion was restored with a narrow `unconvert` explanation.
2. The Ubuntu CI matrix attempted to execute target ARM/Mach-O runtimes while
   packaging. `build-release-platform` now builds a host-native packager and
   passes it explicitly to `scripts/package-release`; all four matrix entries
   then passed.

No failed intermediate run is credited as green.
