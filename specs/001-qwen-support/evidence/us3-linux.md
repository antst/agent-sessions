# US3 Linux: four-product composition

- Date: 2026-08-23
- Platform: Linux amd64
- Qwen Code: `0.22.0`
- Claude Code: `2.1.240`
- Agent Sessions version: `0.2.4`
- Credited command: `QWEN_TEST_HOME=/home/antst/.qwen QWEN_TEST_RUNTIME_DIR=<private-runtime> CODEX_TEST_HOME=<isolated-authenticated-home> CLAUDE_TEST_CONFIG_DIR=<isolated-config> CLAUDE_TEST_SECURE_CONFIG_DIR=/home/antst/.claude GROK_TEST_HOME=<isolated-authenticated-home> GROK_TEST_BIN=/home/antst/.grok/bin/grok ./scripts/test-qwen-composition`
- Result: `qwen.composition.passed` (exit 0)

## Credited edges

The deterministic parent fixture publishes the same process-attested local
registration, groups, lifecycle identity, and product metadata consumed by the
production parent contract. Every child below was a real product lane and
returned its unique terminal token:

| Parent | Target | Child identity | Exact terminal token |
| --- | --- | --- | --- |
| Codex | Codex | `01a032ac-39b2-7e72-b642-bea7cebfe0ac` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `cda3780a-b177-4ac3-81f0-049f3d514cfb` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `bf2db783-258a-453d-b4d4-92a192f06158` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `58cba8db-7b9e-40e8-86b5-bba42dca0d66` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a032ad-0ca3-7a71-aa62-d3fa29019143` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `1ec56f11-ff83-4a39-9342-c810b81485a8` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `d428bd1e-0ec1-4fc2-aa03-8f2fc4765b3a` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `ac48bc80-c328-42f5-b7f5-9e6db9c08221` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a032ad-c333-79c2-ae12-2e0b6871d6aa` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `e1975083-07d4-4a60-b41a-5314f703e7fe` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `482fd5cb-ad9c-4448-bf10-376e0569a1ea` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `18ad334b-5731-4b5a-8429-13c54c6e4fbc` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a032ae-82a8-7893-8a82-4ccb2460d299` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `cc22a400-6d35-4b77-810b-afb0098d95db` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `40c30ca2-d3f1-415c-865b-2499e8551f38` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `6f3217d6-2773-4960-9e47-9f694a0500a7` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

For every edge the runner asserted the exact immediate parent session, owner,
and notification target; the explicit `qwen-composition` group; both mandatory
source-parent and destination-child private anchors; `persistent=false`; one
collection; terminal completion; idempotent archive; cleared manager/worker
identities; and absence of the recorded delivery socket after cleanup.

The complete 4x4 product matrix is independently table-tested in
`internal/bridge/group_agent_test.go` and was repeated ten times before this
live run. The live runner now executes every one of those 16 combinations by
default with an attested product parent fixture and a real target lane.

## First-RED record and RCA

Three discarded runs identified harness and shared-contract defects before the
credited run:

1. An isolated Codex App Server survived a failed attempt. The runner now stops
   only the App Server rooted in its caller-supplied test `CODEX_HOME` before
   admission and during cleanup.
2. The lifecycle fixture inherited an already-closed stdin, exited immediately
   after `lane start`, and correctly triggered owner-exit interruption. The
   runner now holds a private stdin pipe open and asserts parent liveness before
   collection.
3. Reverse Codex/Claude/Grok-to-Qwen edges depended on product-specific native
   ancestry while Qwen already used a strongly attested registration. A shared
   fallback now requires matching product/session metadata, exact adapter and
   lifecycle strong-start identities, exact lifecycle ancestry, and a real
   non-symlink socket. Native-specific inference remains first, and Qwen still
   requires its capability digest.
4. The first live matrix unknowingly depended on an exact-tip Agent Sessions
   plugin already installed in the owner's Grok profile. A clean isolated Grok
   home therefore exposed two missing harness preconditions: the source-tree
   plugin must be installed into that test-owned profile, and the workspace
   must be explicitly trusted before Grok will load plugin MCP servers. The
   runner now performs both operations inside `GROK_TEST_HOME`, pins the native
   Grok executable through `GROK_TEST_BIN`, and never reads or mutates the
   owner's plugin/configuration.
5. Applying that isolated Grok home as the shared `HOME` passed on Linux but
   moved Darwin Claude into a different Keychain namespace. The runner now
   retains the caller's `HOME` for its common environment and applies
   `GROK_TEST_HOME` only to Grok target setup, launch, collection, archive, and
   failure cleanup. A pure contract test asserts that Codex, Claude, and Qwen
   preserve the original home and that product scoping never mutates the input
   environment.

One manual authentication probe outside the credited matrix omitted the
isolated `CODEX_HOME` from an App Server stop command and stopped the owner's
App Server, terminating the validating Codex session. The operator resumed the
session; the probe is discarded and receives no acceptance credit. The matrix
runner's own App Server stop path remained correctly bound to its supplied
isolated `CODEX_TEST_HOME`.

Claude authentication also expired between attempts. A direct owner-profile
inference reproduced the native failure. The operator refreshed Claude through
its native `/login`; no test or Agent Sessions code read, copied, selected, or
modified a credential. The credited run used an isolated Claude state/config
root plus Claude's supported secure-storage namespace selection.

After the credited rerun all fixture processes, the isolated host agent,
supervisors, lane managers/workers, delivery sockets, and owned active lane
records were absent. The supplied isolated profile roots had zero open files
and zero matching processes. The harness stopped the isolated Codex App Server
and did not stop or alter the owner's normal Codex daemon. Baseline and final
metadata showed only Qwen's native usage ledger and Claude's native credential
file updated during the authenticated run. No harness code read or wrote a
credential value; Codex auth, Grok auth/config, Qwen settings/plugin state, and
every other Qwen profile file retained pre-run timestamps.
