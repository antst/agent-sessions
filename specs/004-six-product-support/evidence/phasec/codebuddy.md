# Phase C CodeBuddy product-slice audit

## Verdict

**GREEN — product-local CodeBuddy slice on Linux.**

This verdict is limited to T040, T044, T048, T052, the CodeBuddy-owned portion
of T054, T057, and T061. It is not a release/GA verdict and does not award
credit for the central deferred-binding transaction, physical macOS, or the
Tencent-authenticated model-turn cell.

Audit checkout:

- branch: `feature/six-product-support`
- tested HEAD: `c29466af8d77f975214ed0c8bd813240614d3466`
- tested HEAD tree: `f347198b315fd93f126874479fb0f19fae023d69`
- CodeBuddy slice commit: `a8130476ffb2af2c08bbfac24a03a81b76132f96`
- slice tree: `43fb1768f937b548b7a4d672c4e8d12c49d9194e`
- `internal/products/codebuddy/` and `integrations/codebuddy/` are unchanged
  between the slice commit and tested HEAD (`git diff --exit-code` returned 0).

## Task audit

| Task | Product-local result | Evidence and boundary |
|---|---|---|
| T040 | GREEN | `peer_test.go`, `registry_test.go`, `socketowner_linux_test.go`, and `socketowner_lsof_test.go` cover wrapper launch/adopt/refresh, constant CSRF with no Authorization credential, idle/busy reply, rename, registry mutation/ambiguity/symlink/credential rejection, exact socket PID, stale/closed/recycled port, cross-target live-session mismatch, entrypoint matching, and daemon-restart reconstruction followed by `Refresh`. The Linux socket test uses a real listening socket and kernel ownership lookup. |
| T044 | GREEN | `client.go` keeps `NewPeerClient` (constant `X-CodeBuddy-Request: 1`) distinct from authenticated `NewLaneClient`; `peer.go` re-attests registry session/PID/URL, cwd, strong process identity, executable/argv, ancestry, exact socket ownership, live native session, unchanged row, and final process identity before use. No peer password or component/sidecar is created. |
| T048 | GREEN | Lane tests exercise dispatch, reply, event stream wake plus polling, stop, saved-reply respawn, terminal wait, exact resume/recovery, archive, archive retry, substitution rejection, concurrent wait/steer ordering, cross-lane non-blocking, and ambiguous failures. |
| T052 | GREEN locally | `lane.go` owns a separately supervised, bearer-authenticated `codebuddy --serve` process with a generated memory-only secret. Fresh `Open` is explicitly unbound; first `StartTurn` obtains the product session, rejects reused/substituted identity, and reconciles only one exact marker-bearing possible write without replay. Bound recovery/resume/wait/interrupt/archive stay exact. The central durable CAS/ledger boundary is excluded below. |
| T054 (CodeBuddy portion) | GREEN behavior; layout deviation | `permission.go` preserves native default prompts, rejects implicit bypass, permits bypass only under explicit sandbox authorization, and rejects unknown modes with `ErrUnsupportedPolicy`. `TestPermissionMapperNeverWidensImplicitly` passes. The test is in `lane_test.go`, not the task's named `permission_test.go`; this is a literal file-layout deviation, not a missing assertion or behavior. |
| T057 | GREEN locally | `parent_test.go` proves kernel peer/process identity plus per-session ancestry, rejects a forged native-session claim, component-sidecar authority, and ambiguous cross-target ancestry. `assets_test.go` proves the one CodeBuddy MCP connector and checks fail-closed/terminal-notice collection instructions. |
| T061 | GREEN locally | `integrations/codebuddy/mcp.json`, command/skill/CODEBUDDY assets, `PeerDriver.BuildLaunch`, and `ParentAttester` provide product-side connector injection and exact ancestry attestation. Shared daemon connector authorization/tool dispatch is central integration and is not credited here. |

The package also pins CodeBuddy `2.143.0`, exports only an experimental runtime,
checks OpenAPI drift and an offline job round trip, and explicitly refuses to
mark `tencent-model-turn` ready.

## Commands and exact results

The repository CI pins Go 1.25.0. The shell initially had no unqualified Go in
`PATH`:

```text
$ go test -count=1 ./internal/products/codebuddy
/bin/bash: line 1: go: command not found
exit 127
```

The definitive gates therefore used the available CI-pinned binary directly:

```text
$ /home/antst/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go version
go version go1.25.0 linux/amd64
exit 0
```

Focused normal package gate (all integration-asset, config, doctor, lane,
permission, parent, peer, registry, Linux socket-owner, and lsof tests):

```text
$ /home/antst/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./internal/products/codebuddy
ok  	github.com/antst/agent-sessions/internal/products/codebuddy	0.189s
exit 0
```

The verbose rerun reported 35 top-level tests and every listed subtest PASS,
ending:

```text
$ /home/antst/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./internal/products/codebuddy
PASS
ok  	github.com/antst/agent-sessions/internal/products/codebuddy	0.125s
exit 0
```

Focused race gate:

```text
$ /home/antst/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./internal/products/codebuddy
ok  	github.com/antst/agent-sessions/internal/products/codebuddy	1.241s
exit 0
```

Attributable integration-payload test:

```text
$ /home/antst/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -run '^TestParentAssetsInjectOneCodeBuddyConnectorAndExplainTerminalNotice$' -v ./internal/products/codebuddy
=== RUN   TestParentAssetsInjectOneCodeBuddyConnectorAndExplainTerminalNotice
--- PASS: TestParentAssetsInjectOneCodeBuddyConnectorAndExplainTerminalNotice (0.00s)
PASS
ok  	github.com/antst/agent-sessions/internal/products/codebuddy	0.004s
exit 0
```

Slice identity check:

```text
$ git diff --exit-code a813047 HEAD -- internal/products/codebuddy integrations/codebuddy; status=$?; echo "exit=$status"
exit=0
```

## Explicitly uncredited or pending

- **Central deferred binding and reconciliation:** credited locally are the
  CodeBuddy driver's deferred `Open`, exact first `StartTurn`, one-candidate
  native possible-write reconciliation, and no-replay ambiguity behavior.
  Not credited are `productruntime` validation, daemon lane/input atomic
  persistence, coordinator `SetNativeSessionID`/first-acceptance CAS, or daemon
  restart reconciliation. Those central files were introduced outside
  `a813047` (notably `89fce56`) and no central suite was used to make this
  product-slice verdict.
- **Physical macOS:** not run. `TestDarwinSocketOwnerUsesExactListeningPID` is
  guarded by `//go:build darwin` and did not execute on this Linux host. The
  portable lsof parser/reuse tests passed, but they do not earn physical macOS
  kernel/process credit. This remains a Phase-E cell.
- **Tencent model-turn GA:** pending, never PASS. No Tencent credential or live
  authenticated model turn was available or attempted. The passing doctor
  test asserts that offline protocol success leaves `tencent-model-turn=false`
  and support experimental.
- **Native-product/live acceptance:** the focused Go suite uses typed fake HTTP
  product endpoints except for the real Linux kernel socket-owner test. It
  proves the committed adapter contracts, not a fresh real CodeBuddy 2.143.0
  end-to-end run; existing Phase-0 spike evidence is not re-awarded here.
- Full-repository normal/race, vet/lint, four-build, install/removal,
  federation, and release gates are outside this focused product-slice audit.
