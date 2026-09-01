# Phase-0 Truth-Gate Review

**Base**: `679fe9d3068b6362df867f8d78ce6708c4ce1342`  
**Tree**: `90dbec40e4114aa164c16d8d3fd958467f3051aa`  
**Scope**: OpenCode 1.18.25, Kilo 7.5.6, Pi 0.84.4, OMP 18.0.11,
CodeBuddy 2.143.0, and the exact DSH 0.1.2-alpha.3 tuple.

## Results

| Gate | Result | Frozen consequence |
|---|---|---|
| S0 base | PASS | Build on released `origin/main`; the old feature history was squash-merged. |
| S0 federation | PASS | Keep protocol 3. Capabilities become bounded opaque tokens; destination registry remains authoritative. Trusted-network scope is retained. |
| S1 Kilo | PASS | One authenticated server plus one full attach TUI per peer. `attach --mini` is not `/tui/*`-messageable and is unsupported. |
| S2 DSH | PASS | Exact pnpm-managed 0.1.2-alpha.3 tuple; native Cordis registered tool is the parent facade; HOME/XDG socket; busy ACP rejection queues; cancel is a notification; projcache is not liveness. |
| S3 CodeBuddy | RED, reconciled | No peer password/component/sidecar. Wrapper Adopt/Refresh re-attests registry claim -> socket-owner PID -> executable/start/ancestry. The separate Agent Sessions-owned lane server keeps memory-only password auth. |
| S4 component identity | PASS | One component-v1 frame vocabulary fits OpenCode, Kilo, Pi, and OMP while retaining authoritative native-session evidence and ephemeral bootstrap gating. |
| S5 legacy | PASS: extract-and-freeze | Three legacy entrypoints are unreachable, but no production file is independently deletable. Extract 32 live bridge exports, collapse duplicate catalog, and enforce a shrinking no-new-import baseline. |
| S6 catalog | PASS | One ten-product data source derives aliases, payloads, install strategies, opaque federation capabilities, and 110 OS/capability cells. Prototype digest `9a0b001b5b92d1c0123d7ea1c62dee3ead70f545c07117cc95f1096a7fea1702`. |

## Reconciliation

The sole material RED was S3. It removed, rather than added, a special case:
CodeBuddy is entirely in the wrapper-adopt/product-server family. The component
broker now has five real in-process consumers: OpenCode, Kilo, Pi, OMP, and
DSH. Peer and lane CodeBuddy endpoints are distinct typed surfaces and MUST NOT
share an authentication assumption.

S1 selected an isolated Kilo peer topology. S2 selected the DSH native Cordis
tool facade. S5 selected extract-and-freeze. Those decisions are reflected in
`spec.md`, `research.md`, `plan.md`, `tasks.md`, and the contracts before source
interfaces freeze.

The Linux CodeBuddy socket-owner proof is green. Its physical macOS
`proc_pidinfo`/`proc_pidfdinfo` cell remains a required implementation/Phase-E
acceptance cell, not release credit. Tencent-authenticated CodeBuddy model
completion likewise remains explicitly pending-never-pass until an account is
available.

## Reproduction

The following repository-owned runners were executed successfully from the
isolated worktree:

```text
scripts/spikes/six-product/base/run.sh
scripts/spikes/six-product/kilo/run.sh
scripts/spikes/six-product/dsh/run.sh
scripts/spikes/six-product/codebuddy/run.sh
scripts/spikes/six-product/component-context/run.sh
scripts/spikes/six-product/legacy/run.sh
scripts/spikes/six-product/catalog/run.sh
```

Each runner uses isolated temporary homes/prefixes, writes bounded JSON
evidence, and removes owned processes/resources. `git diff --check`, JSON
parsing, and each runner's syntax/secret/residue checks pass.

## Visible architecture review

The real Agent Sessions peer `fable-architect` reviewed the fresh twelve-file
planning set and granted conditional GO in
`delivery-e84cde49d5b5fadef4c60f56ba87873c`. Its N1/N2/N3 notes were applied:
explicit `CapabilityParent`, one shared token validator, and normative DSH
HOME/XDG socket placement.

Fable reviewed the S3 RED and explicitly accepted the no-sidecar correction in
`delivery-ee507a0f7053fcc23c5893055fb68812`, including socket-to-PID ownership
and the permanent peer/lane surface distinction.

Fable granted final T010 Phase-0 sign-off in
`delivery-c9de737f6666ee1f1f513343f33303dd` after reading the evidence JSON
directly. It found no blocking freeze delta and specifically confirmed the S1
feature-parity bar, every DSH invariant, the S3 peer/lane split and negative
authority cells, and the S5 cross-platform extract-and-freeze proof.

Phase A may now begin. Frozen runtime interfaces return to Fable again at T020
before shared-engine or product fan-out.
