# Final macOS Acceptance — Signed Runtime Candidate 8afd94f

## Attributable identity

- Commit: `8afd94f35d46b65f8c09f7662976cea53671303c`
- Tree: `dd123b49b8b55805c54f6d00ee8e31931c34d346`
- Parent: `32260d27c4eac0be5fe966f9fce61464ab046165`
- Subject: `Run service acceptance before legacy fixtures`
- Signature: good SSH signature from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Hosted CI: run `33142267923`, head SHA exact, conclusion `success`

The physical independent validator used real Darwin arm64 hardware with Go `1.26.5
darwin/arm64`, Python 3, BSD userland tools, stock Bash `3.2.57`, and golangci-lint `2.12.2`.
Hosted macOS used the workflow's Go `1.25.0` and golangci-lint `2.12.2`.

## Hosted real-install evidence

The exact candidate's macOS workflow jobs all passed:

| Job | Result | Boundary |
|---|---:|---|
| `test (macos-latest)` | PASS | normal suite and non-destructive live contracts |
| `race (macos-latest)` | PASS | zero race-detector findings |
| `vet (macos-latest)` | PASS | `go vet ./...` |
| `lint (macos-latest)` | PASS | Darwin-tagged files included; `0 issues` |
| `service-fixture (macos-latest)` | PASS | real launchd install/bootstrap/KeepAlive/crash/explicit-stop/remove/purge cell |
| Darwin arm64 build | PASS | canonical two-binary archive |
| Darwin x64 build | PASS | canonical two-binary archive |
| Package contract | PASS | four archives, exact two images each |

The service fixture used a dedicated clean hosted user boundary and the real launchd controller. It
proved login/bootstrap enablement, one authoritative daemon, KeepAlive after unexpected exit,
explicit stop remaining stopped, optional product cardinalities, all-four connector rollback,
upgrade rollback, normal removal, revision-bound purge, and metadata-only normal/debug/error/crash,
service-manager, metric, trace, status, and doctor output. This is the authoritative real launchd
credit for T034; cross-compilation is not used as runtime evidence.

## Independent physical-Mac discriminator

At the exact candidate, an independent validator ran ordinary `make test` successfully on the owner
Mac. The candidate deliberately skips the destructive clean-user service cell on an ordinary Darwin
workstation unless `AGENT_SESSIONS_CLEAN_ACCEPTANCE_USER=1`; direct
`scripts/test-unified-service` remains strict. Before and after the run:

- `net.antst.agent-sessions` remained unloaded;
- `~/Library/LaunchAgents/net.antst.agent-sessions.plist`, `~/.config/agent-sessions`, and
  `~/.local/libexec/agent-sessions/host` remained absent;
- the existing Agent Sessions state-root metadata and LaunchAgents entry count were unchanged; and
- every baselined owner runtime, Claude child, and production federator process remained alive.

Thus the normal workstation gate neither borrowed the hosted clean-user result nor mutated the
owner's live launchd/profile estate.

The same physical validator had already run the byte-identical Go tree at parent `32260d2` through
vet, lint (`0 issues`), all four release builds, exact archive inventory, unified peers, all 16 lane
composition cells, all four restart cells, migration, stress, federation, and focused Darwin
regressions. The only `32260d2..8afd94f` changes are `Makefile` and `scripts/test`; the exact-candidate
normal run validates that reordering on Darwin.

## Race-flake disclosure

The physical Mac saw two full-suite `internal/bridge` timing failures across seven race attempts at
`32260d2`/`8afd94f`. Five subsequent targeted, full-package, or full-suite race runs passed and every
run had zero Go race-detector findings. At least one failing run named four lane lifecycle/setup tests;
all four passed individually with and without `-race`, and the complete bridge package passed twice
under `-race`. The exact hosted macOS race job and one exact-candidate physical-Mac full race rerun
passed.

This failed evidence is not counted as a pass and is not hidden. It is classified as a recurring
full-suite load/timing flake without a reproducible product-state failure or race report; the exact
candidate still has two independent complete green race results. No timeout was lengthened and no
assertion was weakened to obtain them.

## Quickstart and baseline closure

The exact hosted normal/race jobs ran the same closed quickstart families recorded in
`final-linux.md`: S-01..S-08, U-01..U-12, C-01..C-18, CL-01..CL-11, G-01..G-21,
Q-01..Q-10, L-01..L-30, all 16 P-cells, all 64 M-cells, X-01..X-08, and the four
product-specific A-cells. Darwin-specific coverage included BSD shell behavior, `/var` to
`/private/var` canonicalization, AF_UNIX budgets, process-session observation, launchd lifecycle,
sleep/wake, service output, filesystem immutability ordering, and Darwin-tagged lint.

The hosted artifacts and checksums are recorded in `final-linux.md`; both Darwin archives contain
exactly `agent-sessions` and `agent-sessions-hub` as executable images.

## Preservation and residue

- Hosted service/install state lived under the dedicated runner account and was removed by the
  acceptance transaction.
- The physical validator did not install, bootstrap, unload, kill, signal, or purge owner state.
- No credential value or credential-bearing file was read, copied, hashed, printed, or compared.
- The validator's scratch checkout was clean and detached at the exact candidate after the run.
- No production launchd job, owner connector, native profile, or existing session was credited as
  test-owned evidence.

## macOS decision

T034 and the macOS portion of T094 are complete at the signed runtime candidate. Hosted real-launchd
normal/race/vet/lint/build/package gates and the independent physical-Mac nonmutation discriminator
are green. The disclosed race timing flake remains an observed non-release-blocking reliability item,
not an undisclosed acceptance result.
