# Component Contract Amendment Evidence

Status: **R2 IMPLEMENTED — RE-FREEZE REVIEW PENDING**

The reviewed candidate was the shared worktree at HEAD
`1dd16eb2c4da88ca766470320c5bfbcbc7129d7d`; the exact reviewed delta was
materialized immediately afterward as
`039d25027afe9224ea32a91d4bf839aa550395b3`
(`Refreeze component authority and native rename contract`), whose parent is
`1dd16eb...`. That prior round established the 21-frame rename vocabulary.
The current narrow amendment advances the pinned contract to
`agent-sessions.component.v1-r2` while retaining protocol version 1. It changes
only title-observation validation: empty and product whitespace are genuine
observations; invalid UTF-8, oversized text, and Unicode controls fail closed.
Daemon rename requests and their correlated responses remain nonempty and
exact. The current amendment does not claim re-freeze or product/platform
credit until its candidate is reviewed.

Scope: one necessary daemon-to-component native rename operation. The frozen
daemon/productruntime/state records, component envelope, and wire version are
unchanged.

## Authorized delta

- component wire remains protocol version `1`;
- pinned vocabulary revision is `agent-sessions.component.v1-r2`;
- frame vocabulary grows from 20 to 21 with
  `session.rename.request {native_session_id, requested_name}`;
- success reuses `session.rename` with the same stable frame ID;
- `daemon.rename.*` request/response IDs and `component.rename.*` unsolicited
  observation IDs are disjoint and validated;
- native title is the single writer; daemon name is a derived projection;
- Go broker and shared JavaScript client retain only bounded in-memory
  outstanding/completed rename correlation state.

## Required closure cells

- [x] exact request and correlated exact native-name response;
- [x] unsolicited observation remains a durable-handler event;
- [x] unknown daemon response cannot collide with observation routing;
- [x] same ID/same body replays exact result without a second native call;
- [x] same ID/different body fails closed;
- [x] mismatched native session/name reply fails closed;
- [x] unsupported callback and native failure return typed rejection;
- [x] timeout and disconnect return component-local typed results;
- [x] callback deadline, disconnect, and stop cancel callbacks; late results are ignored;
- [x] exact correlated response is durably handled before caller acceptance;
- [x] transient durable-handler failure permits same-ID convergence retry;
- [x] public client cannot forge either rename success or correlated reject;
- [x] outstanding and completed caches remain bounded;
- [x] diagnostics redact sensitive native failure details;
- [x] Go and JavaScript contract revisions match exactly;
- [x] announce and unsolicited rename share the empty-capable title rule;
- [x] unsafe observations fail before follower mutation or wire emission;
- [ ] r2 adversarial review GREEN;
- [ ] r2 fable-architect re-freeze GREEN;
- [x] no wire-version bump or new envelope field;
- [x] preceding rename-frame amendment adversarial review GREEN;
- [x] preceding rename-frame amendment fable-architect re-freeze GREEN.

## Verification commands

The verification below was run against the reviewed shared-worktree candidate
before it was committed as `039d250...`.

```text
go test ./internal/component ./internal/componentruntime ./internal/daemon -count=10
  PASS (component 2.277s; componentruntime 0.927s; daemon 2.980s)
go test -race ./internal/component ./internal/componentruntime ./internal/daemon -count=5
  PASS (component 2.332s; componentruntime 2.124s; daemon 5.036s)
go vet ./internal/component ./internal/componentruntime ./internal/daemon
  PASS
node --test integrations/shared/component/client.test.js   # repeated 10x
  PASS 10/10; final run 7 tests, 0 failures (307ms)
git diff --check -- internal/component integrations/shared/component \
  specs/004-six-product-support/contracts/component-protocol.md
  PASS
```

Go coverage includes exact correlation, native-name/session mismatch,
daemon-vs-component namespace routing, unknown-client typed unsupported,
timeout, disconnect, direct-send bypass rejection, durable-handler-before-
acceptance ordering, retry after transient durable-handler failure, redaction,
maximum payload bounds, and bounded outstanding/completed replay. Node coverage
includes exact callback acceptance, duplicate replay without a second callback,
conflicting body rejection, typed failure/unsupported, redaction, observation
namespace, forged-success/reject guards, callback deadline/disconnect/stop
cancellation with late-result suppression, bounded replay, exact 21-frame
vocabulary, and contract revision mismatch.

The listed commands and hashes above are historical evidence for the preceding
rename-frame amendment. The r2 implementation must attach its new candidate
hash and fresh Go/JavaScript verification before the two pending review cells
can be checked. Product peer rename still requires focused product tests and
later central/physical acceptance gates.
