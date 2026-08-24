# US3 Linux: four-product composition

- Date: 2026-08-23
- Platform: Linux amd64
- Qwen Code: `0.22.0`
- Claude Code: `2.1.240`
- Agent Sessions version: `0.2.4`
- Credited command: `QWEN_TEST_HOME=/home/antst/.qwen QWEN_TEST_RUNTIME_DIR=<private-runtime> CODEX_TEST_HOME=<isolated-authenticated-home> CLAUDE_TEST_CONFIG_DIR=<isolated-config> CLAUDE_TEST_SECURE_CONFIG_DIR=/home/antst/.claude ./scripts/test-qwen-composition`
- Result: `qwen.composition.passed` (exit 0)

## Credited edges

The deterministic parent fixture publishes the same process-attested local
registration, groups, lifecycle identity, and product metadata consumed by the
production parent contract. Every child below was a real product lane and
returned its unique terminal token:

| Parent | Target | Child identity | Exact terminal token |
| --- | --- | --- | --- |
| Codex | Codex | `01a03190-ddc5-7723-b709-b1224f34bdc6` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `c02ca920-d025-47e7-b905-09344b25146f` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `b50c59a2-b127-4b8e-b21a-11c1345d9ee9` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `9f99c696-a973-42f2-9c47-953a1b34570d` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a03191-962b-7231-95c5-40c23a4b0d5f` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `317adfee-73ef-4397-a438-c19c5c47dd34` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `8dfdd418-5bee-4e36-8922-a188e11d5ad1` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `af6faf6e-894c-43ec-a889-1ccda67a3595` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a03192-5484-7412-a660-7a30a0893a49` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `f0781d6f-59db-4d88-ab20-93d4768b428f` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `e5b3eb4e-7a52-47c6-b655-4dfb99795589` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `1553ca82-07d2-47e8-adf9-4472585703e4` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a03193-2737-7f60-8540-781da7f7accb` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `7f2dd98b-a3c3-4768-a1dc-101f02d40958` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `68fdde66-728b-44cd-9bb9-eedd15518e48` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `739caa4f-e7ad-4a29-8d6f-29bb373b8600` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

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

Claude authentication also expired between attempts. A direct owner-profile
inference reproduced the native failure. The operator refreshed Claude through
its native `/login`; no test or Agent Sessions code read, copied, selected, or
modified a credential. The credited run used an isolated Claude state/config
root plus Claude's supported secure-storage namespace selection.

After the credited run all fixture processes, the isolated host agent,
supervisors, lane managers/workers, delivery sockets, and owned active lane
records were absent. The supplied isolated profile roots had zero open files
and zero matching processes. The harness stopped the isolated Codex App Server
and did not stop or alter the owner's normal Codex daemon. Baseline and final
SHA-256 values were identical for Codex auth/config, Claude settings and local
settings, and Qwen settings/extension-store state.
