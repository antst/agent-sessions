# Baseline Linux Evidence

Date: 2026-08-29 UTC.

## Identity

- Commit: `c056fbc5015d4ab0a673f66cac5404206f7bcee6`
- Tree: `bb61de9f4ba4399cf0c62fb5b7a78a1896251189`
- Host: `Linux 6.17.4-2-pve x86_64 GNU/Linux`
- Toolchain: `go version go1.26.5 linux/amd64`
- Checkout: detached test-owned worktree at `/tmp/agent-sessions-v2-baseline-linux`
- Before/after tracked status: clean (`git status --porcelain=v1` emitted zero lines)
- Managed-parent contamination variables explicitly removed:
  `AGENT_SESSIONS_AGENT_RUNTIME_DIR`, `AGENT_SESSIONS_PRODUCT`,
  `AGENT_SESSIONS_SESSION_ID`, and `CLAUDE_PEER_CLAUDE_CONFIG_DIR`

## Exact baseline gates

| Gate | Literal command | Exit | Result |
|---|---|---:|---|
| Normal | `PATH=/usr/local/go/bin:$PATH make test` | 0 | all packages green; `grouped federation integration: PASS` |
| Race | `PATH=/usr/local/go/bin:$PATH make test-race` | 0 | all packages green; no `DATA RACE`; grouped federation green |
| Vet | `PATH=/usr/local/go/bin:$PATH go vet ./...` | 0 | no output |
| Lint | `PATH=/usr/local/go/bin:$PATH make lint` | 0 | `0 issues.` |
| Mapped packages | `PATH=/usr/local/go/bin:$PATH go test ./internal/launcher ./internal/bridge ./internal/federator ./internal/procinfo ./internal/pathidentity ./internal/releasepkg -count=1` | 0 | all six mapped package groups green |
| New manifest validators | `PATH=/usr/local/go/bin:$PATH go test ./internal/releaseevidence -count=1` in the feature worktree | 0 | checked 202-cell manifest, port-map gates/references, evidence ledger, and topology deltas |

The normal and race commands invoked the baseline `scripts/test`, validated the Claude marketplace and
plugin manifests, used a packaged prebuilt release fixture, ran the mapped Go packages, and completed
the grouped federation integration. No install target or authenticated real-product recipe was run in
this source-gate cell.

## Log identities and pre-existing failures

| Log | SHA-256 |
|---|---|
| `make test` | `a3c0f0e280f67bc5746469507706266d4261e13b3af0c7e7c4537e02e04c06f4` |
| `make test-race` | `2d25bb1cc0dcd4ec118881383efde9757a2cf2e94797ce7e4dbbcc05802dbd74` |
| `go vet` | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` (empty successful log) |
| `make lint` | `2685cb40121026677126f6f6466efdcf4c6cfbc794a3f89eb22be255f5a63aad` |
| mapped packages | `054be389503ca354ca27bf81637053377f057c3e958ef0760787235f658c01cf` |

Search across all five baseline logs for `FAIL`, `DATA RACE`, make failure lines, and numbered Make
errors returned no matches. Therefore this Linux baseline run has **zero recorded pre-existing source
gate failures**. This does not grant any of the installed or interactive acceptance cells; those remain
subject to the per-cell recipes in `baseline-functional-cells.md`.
