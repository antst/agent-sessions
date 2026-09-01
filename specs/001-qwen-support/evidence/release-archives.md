# v0.2.4 rehearsal archive evidence

- Date: 2026-08-24
- Commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Version source: `deploy/peer-federator/VERSION` = `0.2.4`
- Evidence class: disposable rehearsal packages; nothing published
- Verdict: GREEN

## Authoritative build and reproducibility

The same repository-owned `build-release-platform` target used by
`.github/workflows/ci.yml` built all four platforms twice. The first run used
the clean feature branch. The second used a fresh normal clone checked out to
the same branch and exact commit. All archive bytes matched:

| Platform | SHA-256, both runs |
| --- | --- |
| linux-x64 | `96a4a26aeb63b9107a2ecaded7544a254390357b69bfd5c54b0471778816173f` |
| linux-arm64 | `44fb8f10261e8f81e757a8a34d901ff4d0fac68a7921e6538c9b1c8b07a8f360` |
| darwin-x64 | `3fc0ae45af76f9d1b5f1ef00645079c9ca38ec9d45e4826823c6f7127f1649c1` |
| darwin-arm64 | `47a26b6500abd33a0c7966c711d7bd71147df7ed9236b68392ce2c6f4905e012` |

Each archive contained exactly the authoritative eleven executables:

```text
agent-session-runtime
peer
codex-peer
claude-peer
grok-peer
qwen-peer
codex-peer-lane
claude-peer-lane
grok-peer-lane
qwen-peer-lane
peer-federator
```

The Codex, Claude, Grok, and Qwen plugin payloads were all present. The Qwen
payload contained its Agent Plugins v1 plugin/MCP manifests, executable native
entry, and five direct-child skills. Source, extracted-release, and installed
inventory assertions passed in both normal and race suites.

## Version, schema, and collision gates

Focused `internal/releaseevidence` and `internal/releasepkg` tests passed:

- schema validation plus RFC-8785-and-one-LF canonicalization;
- Unicode and adversarial schema rejection;
- commit/tree/archive cross-field rejection;
- authoritative inventory and gate-artifact generation;
- byte-stable archive creation and normalized metadata;
- unsafe package-root refusal;
- independent local/remote tag collision refusal; and
- independent GitHub release and asset collision refusal.

The candidate/tag fixtures both derive `0.2.4` from the authoritative version
file while independently requiring tag `v0.2.4`. The real non-mutating
preflight reported `release tag v0.2.4 is absent locally and on origin`.
Neither a tag nor a release was created.

## Cross-host archive observation

The focused macOS no-Go rehearsal used a Darwin-native archive built by the
same target at the same commit with Go 1.26.5. Its SHA-256 was
`bbc8ceee9fe9dc1fb143d1f32f94104b54faa35bad4defd01f20f5546d7fa12c`
and its size was 24,037,066 bytes. The Linux-built Darwin archive above was
296 bytes smaller and had a different digest. Both packagers normalized tar
ownership/modes/timestamps and gzip metadata, so this is a content difference;
its exact cause was not demonstrated. The attempted attribution to the CI
workflow's future Go 1.25.0 pin is withdrawn because both files in this
comparison were local Go 1.26.5 builds.

This cross-host comparison receives no reproducibility credit and no defect
classification. The credited byte-stability assertion is the two normal Linux
checkouts that match the Ubuntu build host used by the workflow. The macOS
archive independently passed the packaged no-Go peer/lane smoke recorded in
`us5-macos.md`. Final release hashes must come only from the exact workflow run
and toolchain required by T088, not from either rehearsal.

## No-Go installation

The linux-x64 archive was installed with Go absent from `PATH`. Its packaged
Qwen extension registered `agent_sessions` from a clean non-repository
workspace, an interactive packaged peer called `identity`, and a persistent
packaged lane completed and archived. Exact identifiers, tokens, profile
metadata, and cleanup are recorded in `us5-linux.md`.

## Discarded linked-worktree comparison

One detached linked worktree build emitted different hashes. `go version -m`
showed why: Go omitted VCS/module version metadata in that linked-worktree
shape (`(devel)`, no VCS revision), while both credited normal checkouts
embedded the exact clean commit. This was a harness comparison across unequal
build identities, not archive nondeterminism, and receives no reproducibility
credit.
