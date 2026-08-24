# Cross-product DRY audit

Date: 2026-08-24  
Branch: `feature/qwen-support`  
Status: implementation complete; Linux and macOS release gates pending

## Method

- Enabled the repository-managed `dupl` linter and reduced its threshold from
  the default 150 tokens to 100.
- Audited repeated product inventories, CLI parsing, permission inference,
  environment handling, lane lifecycle helpers, socket/path/process handling,
  installers, and tests.
- Required shared implementations to preserve native behavior through focused
  conformance and boundary tests; similarity alone was not treated as proof
  that two native state machines have the same authority model.

## Drift found and remediated

- Four hand-written lane parsers had accumulated aliases and validation
  differences. All owned lane CLIs now use `github.com/spf13/pflag` through one
  `laneCommonOptions` parser; product flags remain declarative. Transparent peer
  wrappers deliberately retain narrow extraction so unknown vendor flags pass
  through.
- Permission-mode argv classifiers disagreed about later overrides and explicit
  false values. `internal/permissionmode` is now authoritative.
- Parent projection could drop an explicit resume notification target in some
  products. One common projection now preserves it.
- Environment lookup/replacement existed in bridge, readiness, federator, and
  launcher with different ordering. `internal/envutil` now defines last-value
  lookup and deterministic replacement.
- Lane dispatch, state-directory reads, name selection, list filtering,
  manager readiness, advisory locking, terminal notices, and control accept
  loops were copied between products. These mechanics now live in the common
  lane contract; product code supplies typed state accessors and native actions.
- Grok and Qwen independently rewrote the same MCP schema adaptation. One
  product-parameterized adapter now owns it.
- Native local carrier encoding existed separately for delivery and result
  frames. One encoder/sender now owns both.
- Make install/dev-install and install tests projected partial hand-written
  binary sets. They now consume `scripts/release-inventory` through
  `BINARY_NAMES`.
- Shared path identity, socket budgets/test roots, and process observability are
  covered separately by task T079A and the adapter platform contract.

At the 100-token threshold, the remaining production clone is a pair of
declarative product-command bindings. It carries a local documented lint
exception because dispatch mechanics are already shared and hiding the native
function table behind another constructor would reduce reviewability without
removing behavior.

## Deliberate native boundaries

- Codex owns App Server thread/turn rollout and native archive/delete recovery.
- Claude owns stream-JSON worker and native roster/socket behavior.
- Grok owns ACP session lifecycle and its headless permission contract.
- Qwen owns ACP lifecycle, current native permission observation, profile
  identity, and native archive/unarchive reconciliation.

Their state schemas, terminal evidence, and rollback transitions are therefore
not unified. They consume shared locks, atomic storage, identity, grouping,
notification, and CLI mechanics. This is the intended DRY boundary: centralize
host/product-neutral invariants, preserve vendor authority semantics.

## Gate evidence still required

- Full Linux test, race, vet, repository lint, and four-platform release build.
- Full stock-TMPDIR macOS test, race, vet, repository lint, and four-platform
  release build at the exact signed successor.
- Isolated Qwen profile install/readiness and live peer/lane/federation cells
  required by the feature tasks, without changing the owner's Qwen profile.
