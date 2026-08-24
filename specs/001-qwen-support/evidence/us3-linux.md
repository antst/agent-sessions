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
| Codex | Codex | `01a03218-71f3-7a50-9364-4c3cb88b26a8` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `9e6cf13c-1c72-4c35-bb60-f1a07f4b5b7d` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `6f57f1e9-2087-47b7-9812-1418fbab218b` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `bd9f7279-1622-4cec-be57-572c9d4fb50d` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a03219-2442-74c1-8b3d-a128b4b742e7` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `b197bacc-70bc-4652-b21e-4d2f2dcce327` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `4f6707bb-04df-4b0a-8973-20fff1415d44` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `8eb88aea-ebcf-4a7f-a8fb-d3f8815d7d9a` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a03219-d703-7760-b68f-5d70c9beb433` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `69fa2a50-ecbf-46ea-a7d0-af0b25c7bab5` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `961bcc6b-6857-46a1-85c8-62a91577ba6a` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `5b597767-f00f-4a29-bf97-8d06297ad045` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a0321a-9a95-7e73-9ace-a15110aedff0` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `6ad4748f-efe7-48a4-bb6d-642e0eeb058d` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `9d2fce6e-511a-498a-97b7-1270028e1e87` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `2cdb7a78-75af-433c-aa12-0d3fd6344a9b` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

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
SHA-256 values were identical for Codex auth/config, Claude settings and local
settings, Grok auth/config, and the complete Qwen profile file manifest.
