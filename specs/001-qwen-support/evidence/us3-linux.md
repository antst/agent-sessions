# US3 Linux: four-product composition

- Date: 2026-08-24
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
| Codex | Codex | `01a032ea-68a2-7b70-b741-69a3014d5b21` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `065551c3-c9c1-4620-833d-3cf048ccc6ac` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `66cff8bd-c2d5-4563-ba0e-48630e8ed2ac` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `9c888d17-563f-48b4-a0bb-65108d396ac8` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a032eb-37b6-7483-a38d-0f84e43f3eaf` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `3a47b28c-3b8c-4852-b412-21342faa2f89` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `d3a01ab3-7a90-4b18-8dc0-e15f2a9e6830` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `5a9ad75b-50c1-4d05-98c7-6d2052a85652` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a032eb-f73e-7b21-9665-d17c6520c05a` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `bc34ae5d-7636-47a8-adfc-b69834eb3580` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `fdd07bfd-5cbe-4bf0-8179-918a9ab45cb2` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `21829a51-022f-40b6-aa1e-1a670e5f7c88` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a032ec-aa6a-7dd3-bd15-a359bfe3f1da` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `62c7523e-0e1c-4fe9-be32-41aceb7bcca1` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `edf5e468-93be-464f-92be-52634fc2bfe4` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `cb914ba2-c105-469d-93d0-88355598654f` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

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
6. Static Grok plugin inspection passed while the live headless ACP session
   admitted zero MCP servers. Grok and Qwen now share one structured ACP-server
   constructor and explicitly inject the current Agent Sessions runtime at
   session create/resume, so the live lane does not depend on an owner-profile
   plugin. The first injection attempt omitted Grok's required empty `env`
   array and failed closed with `-32602`; the credited run emits `env: []`,
   serializes no launch capability, and inherits the attested worker environment.
7. Grok's JSON-RPC decoder and readiness retry initially reduced deterministic
   vendor causes to `Internal error` and then to a deadline. The bridge now
   retains bounded `error.data`, repetition count, and the last substantive
   failure. Lane initialize/auth/create errors are written directly to the
   private manager log rather than being misclassified as an exited process
   and masked by a spurious process-join error.

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
