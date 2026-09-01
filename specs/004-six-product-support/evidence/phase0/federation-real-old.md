# Real pre-feature federation compatibility evidence

Date: 2026-09-01

Current candidate: `f7ddd63927beac4466cd2ba9f4eb0618c38d909d`

Old source: `679fe9d3068b6362df867f8d78ce6708c4ce1342`, tagged `v0.3.0` and the
feature's frozen `origin/main` base. Repository history contains no intervening
federation commit between that released base and the feature implementation;
the first federation change is `1779e53`.

## Verdict

The Phase-B foundation gate's first deferred Fable item is **PASS** against a
real pre-feature build. No production federation change was needed.

The focused probe exports the exact old tree with `git archive`, injects only a
small executable entrypoint, and compiles that entrypoint against the old
tree's real `internal/federation`. The current test then runs the old process
against the current in-process hub with these simultaneous clients:

- one current marked host advertising `codex-lane` and the additive
  `federation-peer-products` marker, with one peer for each original product:
  Codex, Claude, Grok, and Qwen;
- one current marked raw host with a live `opencode` peer and a sentinel group;
- one unmarked v0.3.0 embedded host with an original-four Codex peer.

The real old host:

- kept the recognized `codex-lane` capability and silently ignored the unknown
  marker on both initial and updated host rosters;
- received all four original-product peers initially and after a Codex name
  update, while never observing the live or updated OpenCode peer or its
  sentinel routing state;
- remained continuously connected across a 350 ms quiet interval with a 25 ms
  ping interval and 150 ms heartbeat timeout, exercising baseline ping/pong;
- routed an old-to-new grouped delivery to the Codex peer; and
- returned success when the current destination's protocol-3 `delivery_ack`
  carried `delivery_id`, `receipt_id`, `receipt_sequence`, and an additional
  future receipt key in `Message.Data`. The real v0.3.0 completion path decoded
  the frame and ignored the additive data.

## Commands

Run from the current candidate worktree on Linux amd64:

```text
./scripts/spikes/six-product/federation-old/run.sh
  PASS old=679fe9d3068b6362df867f8d78ce6708c4ce1342
       current=f7ddd63927beac4466cd2ba9f4eb0618c38d909d race=0

RACE=1 ./scripts/spikes/six-product/federation-old/run.sh
  PASS old=679fe9d3068b6362df867f8d78ce6708c4ce1342
       current=f7ddd63927beac4466cd2ba9f4eb0618c38d909d race=1

while IFS= read -r -d '' path; do
  test -z "$(git diff --no-index --check /dev/null "$path" 2>&1 || true)"
done < <(find internal/federation/old_version_integration_test.go \
  scripts/spikes/six-product/federation-old \
  specs/004-six-product-support/evidence/phase0/federation-real-old.md \
  -type f -print0)
  PASS
```

Both normal and race runs compile the old helper and current test with the same
mode. The runner isolates `GOCACHE`, `GOMODCACHE`, and `GOTMPDIR` and removes
the exported source, binaries, and caches through its exit trap.

## Exact topology limit

Compatibility requires the **new hub to distribute rosters**. Only the new hub
uses the additive marker to construct a filtered roster per client. The old
hub's strict pre-feature snapshot validator rejects a new-product peer at
ingress, so an old hub with a new peer plus a strict old host is unsupported
and rejected; it is not a mixed-version rollout topology. This result gives no
credit to old-hub distribution of six-product rosters.
