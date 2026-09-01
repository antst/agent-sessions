# LaneDriver Deferred Native-Session Binding Re-freeze Evidence

Status: **PASS — Fable Amendment Round 2 re-stamped on commit
`89fce56b50727600f6ca69da334e37cf8278fcc8`**

Scope: product-neutral support for a native product whose session is created
only by its first accepted turn. Candidate base: `039d25027afe9224ea32a91d4bf839aa550395b3`.
No commit is made by this implementation worker.

## Authority ruling

The visible `fable-architect` ruling (`delivery-aa921255745ae5f32fce54f84f063a87`)
approved deferred binding with seven conditions: explicit per-product
capability; fresh Open returns explicitly unbound; no session/job creation or
model input at Open; first StartTurn binds atomically with exact native
acceptance; exact identity thereafter; unbound restart means no native session;
possible first-turn write is ambiguous and requires authoritative native
reconciliation; every generic lane path tolerates the unbound window by
`LaneID`.

## Implemented contract seam

- `LaneCapabilitySet.DeferredSessionBinding` is an explicit ephemeral driver
  capability. No product name or durable capability field was introduced.
- `ValidateLaneOpenResult` requires an empty native ID for a flagged fresh
  Open. A flagged fresh bound result, nonflagged unbound Open, inexact resume,
  wrong lane, blank ID, and absent generation fail closed.
- `NativeSessionBindingFromOpen` is the central commit guard. Only
  `bindAtOpen=true` authorizes `LaneEngine.SetNativeSessionID`; a valid deferred
  result returns false. Deferred drivers MUST skip that exact-at-Open path.
- `ValidateLaneStartTurnResult` admits an unbound input only for a flagged
  driver and requires the same `LaneID`/generation plus product-created session
  and turn identities. A previously bound native session must match exactly.
- `MarkInjectedAndSetNativeDispatch` now performs one catalog CAS that sets an
  empty `Lane.NativeSessionID`, receipt `Injected` plus exact
  `NativeAcceptanceRef`, and Turn `dispatched` plus native dispatch ID. A
  nonempty lane ID must match. Exact replay performs no commit.
- The same atomic boundary permits `Ambiguous -> Injected` only when a caller
  supplies exact authoritative acceptance after product-native reconciliation.
  The generic engine never performs that query or auto-adopts a session.
- Present durable records and schema constants are unchanged. In particular,
  there is no unbound marker field, catalog version, lane record version, lease
  version, or receipt version change.
- Existing-lane Accept, Dispatch, and lane-input mutations preserve the durable
  ID when a caller omits it, reject a nonempty mismatch, and cannot bind an
  unbound lane. Complete preserves durable authority and terminalizes
  conflicting native evidence as a failed diagnostic rather than stranding
  accepted work.
- An architecture test inventories every production `.NativeSessionID =`
  assignment in `internal/daemon`: the only lane binders are the exact-at-Open
  setter and atomic first-acceptance method; the remaining lane assignments are
  preservation-only.
- Command routing resolves a cached lane actor only when its durable Lane row
  exists, is not staged, and has the exact same native session ID. The command
  architecture tripwire also inventories `laneActor{nativeID: ...}` composite
  initialization and whole-struct `*actor = ...` writes. It permits only
  hydration from durable `lane.NativeSessionID` and the three exact
  `priorActor` pre-native rollback restores.
- Resolver equality is not a nonempty requirement: a durable, non-staged
  deferred lane with empty durable and cached native IDs remains routeable by
  `LaneID`. Static call-site audit confirms both binding writers bypass actor
  resolution: the create/admit CAS writes its Lane directly, and
  `MarkInjectedAndSetNativeDispatch` binds at the first-acceptance CAS.
- The command-layer actor is a cache, never another authority. The exact native
  setter commits durable state before updating `actor.nativeID`; a rejected
  setter leaves the cache unchanged. Completion only corroborates an already
  bound ID and terminalizes a mismatch as failed. A second AST tripwire
  inventories the reviewed command-layer cache/projection writes and forbids a
  completion writer.

## Hostile closure cells

- [x] flagged fresh Open must be unbound and a bound result is rejected;
- [x] nonflagged fresh Open cannot be unbound;
- [x] flagged resume cannot be unbound or substitute another native ID;
- [x] wrong LaneID, generation, blank ID, and unflagged first StartTurn fail;
- [x] deferred Open produces `bindAtOpen=false`, so it does not authorize
  `SetNativeSessionID`;
- [x] exact-at-Open binding remains one-time and rejects empty/replacement IDs;
- [x] unbound LaneID admits and orders the queued first input;
- [x] unbound lane cannot own a native-session lease;
- [x] first-turn acceptance binds Lane, receipt, and Turn in one store revision
  and one `Host.LaneRevision` increment;
- [x] exact replay is idempotent; conflicting bound-session replay is rejected
  without mutation;
- [x] Accept, Dispatch, and lane-input mutations cannot bind an unbound lane or
  replace a bound lane; omitted projections preserve durable authority;
- [x] terminal mismatch preserves the durable ID and converges to stable failed
  evidence instead of leaving a hidden accepted Turn;
- [x] a source architecture tripwire rejects any unreviewed daemon assignment
  that could create a third lane native-session binding path;
- [x] a process-local actor installed before the create CAS is not routeable
  without a durable Lane row;
- [x] an exact empty-cache/empty-durable non-staged deferred lane is routeable
  by `LaneID` before its first native acceptance;
- [x] a bound durable Lane is not routeable while its actor cache is still
  empty or otherwise differs from durable native authority;
- [x] command source architecture rejects unreviewed `laneActor.nativeID`
  assignment, composite-literal hydration, and whole-struct replacement paths;
- [x] setter rejection leaves `actor.nativeID` unchanged and exact durable
  authority preserved;
- [x] matching completion corroborates without writing; mismatching completion
  terminalizes failed without changing the actor cache or durable lane;
- [x] a command-layer architecture tripwire permits only durable-create,
  durable-first cache-follow, and guarded projection writes; completion is not
  an allowed writer;
- [x] incomplete acceptance and uncoupled `MarkInjected` cannot fake-adopt an
  unbound lane;
- [x] possible write remains Ambiguous and unbound;
- [x] authoritative ambiguity reconciliation uses the same atomic CAS;
- [x] proven-pre-I/O restart leaves no native session, requeues the exact
  receipt by LaneID, and a later first turn may bind it;
- [x] existing non-Codex terminal CAS failure still converges to a durable
  ambiguous receipt and failed terminal Turn;
- [ ] production central composition calls both shared validation helpers and
  `NativeSessionBindingFromOpen` before any state/native follow-up;
- [ ] create-on-first-turn product tests prove Open performs no native
  session/job creation and possible-write reconciliation uses authoritative
  product APIs rather than cwd-only adoption;
- [x] adversarial review GREEN;
- [x] fable-architect Amendment Round 2 re-freeze GREEN, independently
  verified in an isolated detached worktree at `89fce56`.

The two unchecked production/product cells are deliberately outside this
product-neutral fenced task. They block product credit, not this shared seam.

## Verification

```text
/usr/local/go/bin/go test ./internal/productruntime ./internal/daemon -count=10
  PASS
/usr/local/go/bin/go test -race ./internal/productruntime ./internal/daemon -count=3
  PASS
/usr/local/go/bin/go test ./cmd/agent-sessions \
  -run '^(TestNonCodexTerminalNativeAcceptanceCASFailureIsDurableDiagnostic|TestCodexRestartAfterExactStartAckRecoversAndRetiresInjectedReceipt)$' \
  -count=10
  PASS
/usr/local/go/bin/go test -race ./cmd/agent-sessions \
  -run '^(TestNonCodexTerminalNativeAcceptanceCASFailureIsDurableDiagnostic|TestCodexRestartAfterExactStartAckRecoversAndRetiresInjectedReceipt)$' \
  -count=3
  PASS
/usr/local/go/bin/go test ./cmd/agent-sessions \
  -run 'Lane.*(Input|Restart|Receipt|Recovery|Acceptance|Dispatch)|.*(LaneInput|Receipt|NativeAcceptance|RestartAfterExact)' \
  -count=3
  PASS
/usr/local/go/bin/go test -race ./cmd/agent-sessions \
  -run 'Lane.*(Input|Restart|Receipt|Recovery|Acceptance|Dispatch)|.*(LaneInput|Receipt|NativeAcceptance|RestartAfterExact)' \
  -count=1
  PASS
/usr/local/go/bin/go vet ./internal/productruntime ./internal/daemon ./cmd/agent-sessions
  PASS
/usr/local/go/bin/go test ./cmd/agent-sessions \
  -run '^(TestACPWorkerPublishesNativeIdentityBeforeTurnTerminal|TestRecordLaneNativeIDUpdatesActorOnlyAfterDurableAcceptance|TestLaneCompletionOnlyCorroboratesNativeIdentity|TestResolveLaneActorRequiresDurableRowAndExactNativeCache|TestLaneActorNativeSessionWritesStayAtReviewedBoundaries|TestLaneActorNativeSessionGuardDetectsWholeStructForgery|TestNonCodexTerminalNativeAcceptanceCASFailureIsDurableDiagnostic)$' \
  -count=10
  PASS
/usr/local/go/bin/go test -race ./cmd/agent-sessions \
  -run '^(TestRecordLaneNativeIDUpdatesActorOnlyAfterDurableAcceptance|TestLaneCompletionOnlyCorroboratesNativeIdentity|TestResolveLaneActorRequiresDurableRowAndExactNativeCache|TestLaneActorNativeSessionWritesStayAtReviewedBoundaries|TestLaneActorNativeSessionGuardDetectsWholeStructForgery|TestNonCodexTerminalNativeAcceptanceCASFailureIsDurableDiagnostic)$' \
  -count=3
  PASS
/usr/local/go/bin/go test ./internal/productcatalog \
  -run '^TestNoNewProductDispatchSwitches$' -count=10
  PASS (cmd/agent-sessions/lane.go remains at the frozen 32 product dispatch literals)
git diff --check -- internal/productruntime internal/daemon \
  specs/004-six-product-support/contracts \
  specs/004-six-product-support/evidence \
  specs/004-six-product-support/tasks.md
  PASS
/usr/local/go/bin/go test ./... -count=1
  PASS
```

An independent hostile review reran the stable candidate's normal, repeated,
race, vet, and cmd durability cells and returned GREEN after the flagged-fresh
Open correction and binding-path tripwire landed.

## Data-model reconciliation

`data-model.md` now records the same narrow exception: only a flagged fresh
Open may carry an empty native ID, and the first exact native acceptance binds
it atomically. Nonflagged Open, resume, binding onward, leases, and every
native-session-addressed operation still require the exact non-empty ID. The
durable `Lane.NativeSessionID` field was already `omitempty`; no record-format
contradiction or migration is introduced.
