# Uniform Federation Protocol 4 Refreeze

Status: **PASS / FROZEN**

Commit: `35e07e5938f4f7fba3841db577e22003dfdc72e3`

Reviewed 32-file manifest SHA-256:
`b4e1ac2cafa2f4850f1c7c8ed26e4de600efd3a69f7ba14e6409d69b360e6551`

## Frozen result

- Protocol version 4 is one exact uniform host/hub contract. Every mismatch,
  including N+1 against N, is rejected before registration.
- Every admitted client receives the same complete roster.
- Every lane request carries exactly one explicit opaque capability.
- The protocol contains no legacy feature marker, product-to-capability
  inference, differential roster, old-host asymmetry, or old-binary scaffold.
- Prospective roster bounds are evaluated before state replacement. A rejected
  update preserves the last-good roster and unrelated incumbents.

## Review RED and closure

Independent review found that the first implementation could replace a live
same-host generation at `hello` before the reconnect candidate's initial
snapshot passed the prospective roster bound. The final implementation keeps
the candidate provisional, validates its complete projected roster, then
promotes atomically. A live regression proves that an amplifying generation
N+1 candidate cannot evict generation N or damage its last-good roster.

## Verification

- dependent federation/daemon/CLI/hub suites, normal and race;
- hostile admission cells under repeated race execution;
- `go vet` and all five federation fuzz targets;
- federation scripts in normal and race modes;
- production binary-pair smoke with clean N+1 rejection;
- whole-tree marker/protocol-3 active-surface scan;
- full build and test in an isolated detached worktree.

Fable independently verified the commit in an isolated detached worktree and
froze it. The change adds no durable state and does not reopen component,
runtime, ledger, or native-session contracts.
