# Phase C DSH product-local evidence

**Verdict: GREEN for the committed DSH product-local slice.** This is not a
claim that the complete release acceptance matrix is green.

## Audited revision and scope

- Branch: `feature/six-product-support`
- Audited HEAD: `c29466af8d77f975214ed0c8bd813240614d3466`
- Audited HEAD tree: `f347198b315fd93f126874479fb0f19fae023d69`
- DSH implementation commit: `a7506127e7bbc5728ecbba0df011add1d22fcab2`
  (`Add DSH runtime integration`)
- Host/toolchain: Linux `6.17.4-2-pve` x86_64, Go `1.26.5`, Node
  `v25.9.0`, pnpm `10.28.1`.
- Scope: T041, T045, T049, T053, the DSH portion of T054, T058, and
  T062. No task status and no production code were changed by this audit.

The audit began with a clean worktree at the audited revision:

```text
$ git status --short --branch
## feature/six-product-support...origin/main [ahead 25]
```

Other agents subsequently changed specification artifacts in the shared
worktree. The audited production paths stayed clean throughout:

```text
$ git status --short -- internal/products/dsh integrations/dsh
<no output>
$ git diff --check -- internal/products/dsh integrations/dsh
<no output>
```

## Task audit

| Task | Product-local result | Evidence and boundary |
|---|---|---|
| T041 | GREEN | Go tests cover exact binding, delivery acceptance, late-result/reconnect reconciliation, exact tuple rejection, physical-cwd checks, and HOME/XDG socket rejection of `/tmp`, `/private/tmp`, and symlink escapes. Node tests cover native `followup`, `steer`, exact single-session selection, title observation, and reconnect re-announcement. |
| T045 | GREEN | `PeerDriver`, `CordisGateway`, and the shipped Cordis plugin bind one exact session to one admitted component binding, replace inherited `DSH_HOME` with the verified managed home, and fail closed on tuple/profile/cwd/socket drift. The real spike booted two isolated exact profiles and exposed zero siblings. |
| T049 | GREEN at the DSH boundary | Tests cover ACP new, exact resume/recovery, busy `-32602` mapping to `ErrUnsupportedSteer` before native acceptance, cancel as an ID-less notification, stop-reason settlement, lease exclusion/recovery, and archive/cleanup convergence. The durable queue itself is shared-ledger behavior and is not reimplemented or credited as a DSH-local queue. |
| T053 | GREEN at the DSH boundary | The typed ACP driver owns the process, validates exact session/cwd/profile, uses the exclusive lease, bounds prompt/terminal state, poisons ambiguous writes, and makes archive retries step-idempotent. End-to-end host ledger/federation composition is outside this product-local audit. |
| T054 (DSH) | GREEN | `default` maps exactly to `workspace-write:ask`; `bypassPermissions` maps exactly to `danger-full-access:never`; every unknown mode returns `ErrUnsupportedPolicy`. Node tests and the real spike also prove a persisted wider policy is overwritten and live-verified before resume returns. |
| T058 | GREEN at the product-local boundary | Node tests cover the registered native tool's exact `exec.agent` witness, cancellation, MCP explicit-environment scrubbing, sandbox-visible socket rules, and terminal notice delivery through native followup. Go tests reject false native IDs, foreign bindings, wrong peer/process evidence, and unproved sandbox children. The current real-scope rerun was keyless and did not independently re-observe the native `DSH_SESSION_ID`; the committed Phase-0 S2 evidence records that real native witness. |
| T062 | GREEN at the product-local boundary | The selected facade is the native Cordis-registered tool. `ParentAttester` requires the exact binding, native ID, peer UID/PID, process identity, or a live bounded ancestry proof for an MCP sandbox child. Production component-authorizer composition is deliberately uncredited below. |

## Focused commands and exact observed results

### Go normal

```text
$ GOTOOLCHAIN=local /usr/local/go/bin/go test -count=1 ./internal/products/dsh
ok  	github.com/antst/agent-sessions/internal/products/dsh	0.187s
```

### Go race

```text
$ GOTOOLCHAIN=local /usr/local/go/bin/go test -race -count=1 ./internal/products/dsh
ok  	github.com/antst/agent-sessions/internal/products/dsh	2.312s
```

### Node DSH and shared component contract

```text
$ node --test integrations/dsh/plugin.test.cjs integrations/shared/component/client.test.js
✔ Cordis protocol driver uses native followup for wake/queue and steer while busy (3.097796ms)
✔ Cordis ready/reconnect re-announces identity and durable turn/end emits completion (0.331187ms)
✔ native registered tool carries exact exec.agent witness (0.689883ms)
✔ managed profile selects one exact session and rejects invisible siblings (0.279545ms)
✔ managed plugin refuses a profile that already contains any session (0.339916ms)
✔ missing native title stays unannounced and product title events use observeRename (0.258984ms)
✔ missing or non-canonical native cwd is rejected instead of synthesized (0.286865ms)
✔ ambient profile stays inert before native helpers or Cordis services load (0.443648ms)
✔ ACP lane policy overwrites and live-verifies persisted wider session policy (0.456108ms)
✔ ACP lane policy rejects unknown presets and peer/lane role conflict (0.238394ms)
✔ MCP env is explicit, non-secret, and sandbox-visible (4.745867ms)
✔ shipped CLI, ACP app, plugin, profile, and pnpm tuple is exact (0.241905ms)
✔ ambient component is inert without the complete managed bootstrap (32.567834ms)
✔ optional process corroboration environment is all-or-none (0.359047ms)
✔ managed client reconnects in the same generation and preserves delivery/tool correlation (132.72069ms)
✔ protocol framing is bounded and diagnostics redact bootstrap material (0.407847ms)
✔ missing heartbeat acknowledgments force bounded reconnect (35.245742ms)
✔ rename contract revision and operation namespaces are exact (0.769364ms)
✔ native rename callback is correlated, bounded, replay-safe, and distinct from observations (35.291274ms)
✔ native rename callbacks have bounded deadline, disconnect, stop, and late-result cleanup (41.497277ms)
ℹ tests 20
ℹ suites 0
ℹ pass 20
ℹ fail 0
ℹ cancelled 0
ℹ skipped 0
ℹ todo 0
ℹ duration_ms 335.800593
```

### Keyless real exact-tuple spike

The exact DSH CLI and pnpm were already present in a disposable prior
verification area. The opt-in spike installs only offline into newly created
Agent Sessions-owned profiles under HOME/XDG state, kills its children, and
removes its spike root in `finally`; no credential was read or required.

```text
$ DSH_REAL_CLI=/tmp/dsh-verify.QF6Kif/node_modules/.bin/dsh DSH_REAL_PNPM=/usr/bin/pnpm DSH_REAL_PNPM_STORE=/tmp/dsh-verify.QF6Kif/pnpm-store node integrations/dsh/real-scope-spike.cjs
{"status":"PASS","tuple":"0.1.2-alpha.3","pnpm":"10.28.1","profiles":2,"exact_sessions":2,"sibling_sessions_visible":0,"disposable_doctor_store_growth":0,"persisted_policy_override":"danger-full-access:never -> workspace-write:ask","socket_root":"HOME/XDG state"}
```

This rerun earns real-product, keyless exact-tuple/profile/session/policy credit.
It does not earn credentialed model-turn credit.

## Accepted gap and no-credit cells

- **Accepted DSH rename gap:** Agent Sessions-initiated rename is intentionally
  unwired. `PeerDriver.Rename` validates the request and returns
  `productruntime.ErrUnsupportedRename` without changing a native title or
  creating a daemon-side alias. Native Cordis title events remain the sole
  writer and refresh the external projection. This is accepted for this slice,
  not silently counted as mutable rename support.
- **Production component authorizer:** no credit. Unit/product tests exercise
  exact bindings and attachment authorization, but this audit did not prove the
  production component Authorizer plus startup-reconciliation gate end to end.
  Therefore full peer acceptance is still pending.
- **Physical macOS:** no credit. Darwin path aliases are covered by portable
  validation, but no physical macOS execution occurred.
- **Credentialed real model turn:** no credit. The real rerun was keyless and
  did not execute a credentialed model turn on the exact tuple.
- **Native `DSH_SESSION_ID` rerun:** no fresh credit. The current unit boundary
  proves exact native-agent witness propagation/rejection, not observation of
  that environment variable. The committed Phase-0 S2 spike records the real
  `DSH_SESSION_ID` witness, but the keyless real-scope command above does not
  itself re-observe it.
- **Wider gates:** no credit from this document for the full repository normal,
  race, vet/lint, build, install/removal, federation, or end-to-end terminal
  notice matrices; only the focused commands above were requested and run.

No red product-local cell was observed, so the committed DSH slice is **GREEN**
within these explicit boundaries.
