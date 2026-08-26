# Qwen pre-candidate quickstart rehearsal

- Date: 2026-08-24
- Frozen behavior commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Frozen tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Intended version: `0.2.4`
- Evidence class: pre-candidate rehearsal; no tag, release, or published asset
- Verdict: GREEN for the pre-candidate steps; final exact-commit publication remains T088

This report maps every section of `specs/001-qwen-support/quickstart.md` to
the credited evidence. It does not turn earlier rejected/confounded attempts
green, and it does not claim that a release commit, workflow artifact, tag, or
GitHub release already exists.

## Evidence map

| Quickstart section | Linux evidence | macOS evidence | Result |
| --- | --- | --- | --- |
| 1. Prerequisites and baseline | `gates-linux.md`; metadata-only selected-profile baselines in `us1-linux.md` through `us5-linux.md` | `gates-macos.md`; metadata-only profile/process baselines in `us1-macos.md` through `us5-macos.md` | GREEN |
| 2. Source gate | exact `b8bc013` normal/race/vet/repository-lint and four builds in `gates-linux.md` | exact same commit/tree and gates under stock Darwin TMPDIR in `gates-macos.md` | GREEN |
| 3. Isolated install and doctor | selected-profile, negative readiness, package, and no-Go cells in `us1-linux.md`, `us2-linux.md`, and `us5-linux.md` | selected-profile reconciliation and clean-workspace registration in `gates-macos.md`; packaged cell in `us5-macos.md` | GREEN |
| 4. Managed interactive peer | full real contract at exact `b8bc013`, session `99ca05f7-720d-482f-aab8-0b513df056bd`, real socket, zero hub trips | full real contract in `us1-macos.md`; exact-`b8bc013` runtime/MCP discriminator in `gates-macos.md` | GREEN |
| 5. Durable Qwen lane | full seven-cell exact-`b8bc013` contract in `gates-linux.md`; package lane in `us5-linux.md` | full seven-cell contract in `us2-macos.md`; package lane in `us5-macos.md` | GREEN |
| 6. Crash/adversarial lifecycle | `us1-linux.md`, `us2-linux.md`, exact current normal/race regressions | `us1-macos.md`, `us2-macos.md`, `us3-macos.md`, and exact current normal/race regressions | GREEN |
| 7. Four-product composition | all 16 real edges in `us3-linux.md` | all 16 real edges in `us3-macos.md` | GREEN |
| 7. Federation | `us4-linux-to-macos.md`, `us4-macos-to-linux.md`, `us4-negative.md` | independent source/destination audits in the same reports | GREEN |
| 8. Rehearsal packages | exact archive/build/schema/collision evidence in `release-archives.md`; no-Go install in `us5-linux.md` | no-Go install in `us5-macos.md` | GREEN |
| 8. Final workflow/tag/release | deliberately not run during a pre-candidate rehearsal | deliberately not run during a pre-candidate rehearsal | NOT RUN — owned by T088 |

## Accepted current-tree evidence

Both operating systems ran the full source gate at exact `b8bc013` after the
last behavior correction. Linux then reran the complete real interactive and
seven-cell lane contracts. macOS reran the corrected same-version install,
proved registration outside the repository, and proved by descendant
executable path that the managed parent's structured MCP used the exact
`b8bc013` runtime without an ambient owner-runtime fallback.

The complete macOS interactive/lane, 16-edge composition, external-termination,
and bidirectional federation cells predate only the extension-root selection
and Agent Plugins v1 command-form corrections. Those later commits do not
change Qwen ACP turns, archive semantics, stable-socket ownership, groups,
remote lifecycle, or target dispatch. Exact-current normal/race coverage and
the current live parent/package discriminators exercise the changed boundary;
the earlier accepted native cells continue to evidence the unchanged
boundaries. T088 nevertheless requires the final workflow to repeat the real
OS gates at the exact signed release commit.

## Rejected and confounded evidence retained

- A repository `.mcp.json` masked missing installed Qwen MCP registration.
  Clean-workspace probes exposed it; `5f881f7` switched to the vendor-supported
  contained command and added a regression rejecting native-Qwen
  `${extensionPath}` syntax.
- An earlier Qwen entry used an ambient bare runtime and could select stale
  installed code. `fc8bd38` binds the selected managed runtime explicitly;
  exact-current Linux and macOS descendant-process evidence proves it.
- Two Linux packaged-lane harness assertions parsed a JSON event stream as one
  document. They receive no credit; exact UUID collection/archive inspected
  the already-created single lane without retrying it.
- Early macOS composition runs accidentally relied on repository or installed
  MCP fallback, rewrote `HOME` too broadly, or hid Grok ACP `error.data`.
  Clean target workspaces, product-scoped environment, session MCP injection,
  and specific-error retention closed those classes before the credited
  16-edge run.
- The macOS verifier initially computed forbidden digests for
  credential-bearing profile files. Those comparisons were withdrawn, the
  files containing them were deleted under exact gates, and no acceptance
  claim in this report uses them. Current profile evidence is metadata-only.
- An external-termination attempt stranded a supervisor. The cleanup unwind
  and exact-process stop were repaired and the exact discriminator later
  retired the process, socket, state, and temporary root.
- A linked-worktree archive comparison lacked the VCS build metadata embedded
  by normal checkouts. It is classified as harness-confounded and is not used
  for byte-reproducibility credit.

## Release boundary

The rehearsal packages are disposable and must not be uploaded as release
assets. Local and remote tag preflight reported `v0.2.4` absent. T088 must
create a signed release commit on `main`, run the exact workflow at that
unchanged commit, validate the immutable canonical evidence artifact, and only
then create the signed tag and GitHub release. None of those publication steps
has occurred here.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
