# Component Contract Narrow Re-freeze Evidence

Status: **PASS — adversarial review GREEN and fable-architect re-freeze granted**

The reviewed candidate was the shared worktree at HEAD
`1dd16eb2c4da88ca766470320c5bfbcbc7129d7d`; the exact reviewed delta was
materialized immediately afterward as
`039d25027afe9224ea32a91d4bf839aa550395b3`
(`Refreeze component authority and native rename contract`), whose parent is
`1dd16eb...`. Fable's round-1 ruling froze component contract revision
`agent-sessions.component.v1-r1` with 21 frames while retaining protocol
version 1. No product or physical-platform acceptance credit was granted by
that contract review.

Scope: one necessary daemon-to-component native rename operation. The frozen
daemon/productruntime/state records, component envelope, and wire version are
unchanged.

## Authorized delta

- component wire remains protocol version `1`;
- pinned vocabulary revision is `agent-sessions.component.v1-r1`;
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
- [x] no wire-version bump or new envelope field;
- [x] adversarial review GREEN;
- [x] fable-architect re-freeze GREEN.

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

Both review cells are green. This evidence freezes the shared component rename
contract only; product peer rename still requires each product's focused tests
and later central/physical acceptance gates.
