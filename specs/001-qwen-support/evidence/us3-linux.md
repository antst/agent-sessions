# US3 Linux: four-product composition

- Date: 2026-08-24
- Platform: Linux amd64
- Qwen Code: `0.22.0`
- Claude Code: `2.1.240`
- Grok: `1.0.5 (5115b46bc9)`
- Agent Sessions version: `0.2.4`
- Credited command: `QWEN_TEST_HOME=/home/antst/.qwen QWEN_TEST_RUNTIME_DIR=<private-runtime> CODEX_TEST_HOME=<isolated-authenticated-home> CLAUDE_TEST_CONFIG_DIR=<isolated-config> CLAUDE_TEST_SECURE_CONFIG_DIR=/home/antst/.claude GROK_TEST_HOME=<isolated-authenticated-home> GROK_TEST_BIN=/home/antst/.grok/bin/grok ./scripts/test-qwen-composition`
- Result: `qwen.composition.passed` (exit 0)
- Isolation discriminator: the target cwd was a fresh trusted workspace with no `.mcp.json`; the
  isolated Grok profile was inspected before launch and contained neither an `agent-sessions`
  plugin nor an `agent_sessions` MCP. The injected session MCP was therefore the only possible
  Grok readiness path.

## Credited edges

The deterministic parent fixture publishes the same process-attested local
registration, groups, lifecycle identity, and product metadata consumed by the
production parent contract. Every child below was a real product lane and
returned its unique terminal token:

| Parent | Target | Child identity | Exact terminal token |
| --- | --- | --- | --- |
| Codex | Codex | `01a03321-da83-77c2-a8ba-ea4e9ff19560` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `f7349391-4757-45e4-8aa2-7e62ecdbf3bc` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `181847a2-c1d7-4d9b-a073-72c3a1525675` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `f4d2710b-91f3-46db-8672-2784e3112d99` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a03322-a1c1-7a81-af8d-3692124c30ca` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `f70d9869-eba3-4459-9ad0-31c3ec36c5dc` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `51be7bdc-371f-4444-ae6b-5d1cb1f8854a` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `f19eac29-21b5-4442-894e-cb0920fdcaf4` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a03323-52dc-7ab0-9bdb-9038ba4d2da9` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `6ac75b93-4045-4e5d-8b79-93c2ad4f99ae` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `e5a1b397-96fd-43e6-a263-5ff3fac6e61b` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `df13169a-591a-4bd5-80f8-d0ab35e33218` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a03324-0ac5-73d0-9904-81f55411a0d6` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `09f02bdf-1bcb-43cf-9dc9-a0ed2dbcdbdd` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `df1cbbd1-8261-4696-8e1a-a906113b5992` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `97271a78-a3af-4f06-82a1-18e0e5f67bb3` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

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
   plugin already installed in the owner's Grok profile. An intermediate
   harness then staged the source plugin into `GROK_TEST_HOME`, which isolated
   owner state but could still mask whether session-scoped MCP injection worked.
   The credited runner instead refuses any preinstalled Agent Sessions plugin,
   inspects zero `agent_sessions` MCPs from the clean target workspace, pins the
   native Grok executable through `GROK_TEST_BIN`, and never reads or mutates the
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
8. The earlier Linux matrix also launched targets from the repository root,
   whose checked-in `.mcp.json` defines `agent_sessions`. That made its Grok
   result non-discriminating even after session injection landed. The credited
   rerun uses a fresh trusted target workspace for every edge and asserts both
   fallback sources absent before launching the first parent.

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
