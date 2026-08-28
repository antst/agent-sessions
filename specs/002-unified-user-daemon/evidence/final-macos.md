# macOS Acceptance Status — Greenfield Runtime Candidate 08e915d

## Candidate requiring validation

- Commit: `08e915da6b91ddb54eb4790054d1fc818d4b4dec`
- Tree: `dd448d5e9c3d0a412fee88f001cf15ed03cfe755`
- Subject: `Establish greenfield unified daemon install`
- Signature key: `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`

## Status

**PENDING.** The prior physical-Darwin and hosted-launchd evidence applies to runtime candidate
`8afd94f35d46b65f8c09f7662976cea53671303c`. It established the Darwin fixtures, launchd service
contract, BSD userland behavior, AF_UNIX budgets, lint coverage, and two-binary packaging at that
older tree. It is retained in Git history but is not credited to `08e915d`.

The new candidate deliberately deletes the pre-unification compatibility subsystem and changes
greenfield host installation, connector-before-restart ordering, configuration rollback, native
executable persistence, and Grok installed-plugin inventory. Those changes require an exact-candidate
macOS rerun rather than inference from the ancestor.

Required closure:

1. verify the signed commit and clean worktree on real Darwin arm64;
2. run `make test`, `make test-race`, `go vet ./...`, and `make lint`;
3. run the hosted clean-user launchd service fixture for install, restart, crash, explicit stop,
   connector rollback, removal, and purge;
4. build both Darwin and both Linux release archives and prove the exact two-binary inventory; and
5. install on the controlled Mac only after old Agent Sessions peers/processes and Agent
   Sessions-owned prototype roots are clean, without reading or mutating vendor credential/history
   content.

No macOS pass is asserted for `08e915d` until those cells complete.
