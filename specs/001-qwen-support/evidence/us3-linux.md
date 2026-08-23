# US3 Linux: four-product composition with Qwen

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
| Qwen | Codex | `01a030ad-3cd0-76a3-86d3-b666d4dfb69d` | `QWEN_COMPOSE_QWEN_TO_CODEX_1_OK` |
| Qwen | Claude | `430ddffd-df1b-4b03-b81f-bc43d3dc194c` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_2_OK` |
| Qwen | Grok | `e2394857-7810-42a3-a308-23c73b926a68` | `QWEN_COMPOSE_QWEN_TO_GROK_3_OK` |
| Qwen | Qwen | `60e51567-9b10-4de2-a6a2-194deb693d2b` | `QWEN_COMPOSE_QWEN_TO_QWEN_4_OK` |
| Codex | Qwen | `54d0a184-1b4e-4b3d-b247-ad7414e5db54` | `QWEN_COMPOSE_CODEX_TO_QWEN_5_OK` |
| Claude | Qwen | `e83ea488-bdc8-4672-98ef-50ba2a2d9ea3` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_6_OK` |
| Grok | Qwen | `93884eae-5f55-484b-a8bd-4a9825b1340f` | `QWEN_COMPOSE_GROK_TO_QWEN_7_OK` |

For every edge the runner asserted the exact immediate parent session, owner,
and notification target; the explicit `qwen-composition` group; both mandatory
source-parent and destination-child private anchors; `persistent=false`; one
collection; terminal completion; idempotent archive; cleared manager/worker
identities; and absence of the recorded delivery socket after cleanup.

The complete 4x4 product matrix is independently table-tested in
`internal/bridge/group_agent_test.go`; this live run covers its seven Qwen row
and column edges.

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

After the credited run the composition root, all fixture processes, the
isolated host agent, supervisors, lane managers/workers, delivery sockets, and
owned active lane records were absent. The harness stopped the isolated Codex
App Server and did not stop or alter the owner's normal Codex daemon.
