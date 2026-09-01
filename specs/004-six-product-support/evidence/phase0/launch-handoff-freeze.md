# One-Shot Native Launch Handoff Freeze

Status: **MECHANISM PASS / FROZEN**

Commit: `c29466a`

Reviewed 16-file manifest SHA-256:
`2fa5a3a2112b24358b02f79cf5ed9b2ab0aa859e5941862bc357f0dd4d38f04f`

## Frozen result

- The generation-local broker holds one bounded memory-only `NativeCommand`
  for one exact prepared attachment and exact live wrapper identity.
- The ticket is a selector only; live UID/PID/start/strong-start evidence is
  the authority.
- Full, proven-zero, and partial/possible `go` writes resolve to consume,
  destructive rollback, and typed ambiguous finalization respectively.
- The ambiguous resolution variant structurally carries no rollback
  capability and is never replayable.
- Attachment reservation and aggregate capacity remain held through write
  classification and the selected finalizer. Close and expiry drain active
  work before returning.
- The binary transport is private, no-follow, bounded, and CLOEXEC. The public
  wrapper API closes the socket and calls `chdir` plus native image-replacement
  exec with the envelope-only environment. Truncated `go` never reaches exec.
- Production-source architecture tests forbid persistence/logging/JSON imports
  and `os.Setenv`; transient mutable buffers are zeroed best-effort.

## Verification

- focused normal tests and repeated race tests;
- full build, test, race, and vet suites;
- structural callback-separation and full/zero/partial hostile cells;
- capacity/reservation, Close/expiry, foreign identity, restart-stale, secret
  absence, and truncated-frame cells;
- Darwin amd64/arm64 compile-only checks and clean formatting/diff checks.

Fable independently verified the exact commit in an isolated detached
worktree and froze the mechanism. Production central ambiguous
adoption-versus-absence reconciliation and physical macOS execution remain
explicitly deferred and uncredited; no secret-bearing product launch receives
credit until those later gates close.
