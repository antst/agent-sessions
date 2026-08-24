# US3 macOS: four-product composition

- Date: 2026-08-24
- Platform: macOS arm64
- Commit: `6dd02d395240a63297856157c07c460e42a8c6f7`
- Tree: `1f6ef1eed1e9aac155e888b0e8dbd3af0d1c95a4`
- Qwen Code: `0.22.0`
- Claude Code: `2.1.241`
- Grok: `1.0.5 (5115b46bc9)`
- Agent Sessions version: `0.2.4`
- Result: `qwen.composition.passed` (exit 0)
- S-tier: `make test`, `make test-race`, `go vet ./...`, repository `make lint`, and all four release-platform builds passed under the stock Darwin temporary directory; zero data races.
- Isolation discriminator: every target used a fresh trusted workspace with no `.mcp.json`; the isolated Grok profile contained neither an Agent Sessions plugin nor an `agent_sessions` MCP. The current runtime's session-scoped MCP injection was therefore the only Grok readiness path.

## Credited edges

| Parent | Target | Child identity | Exact terminal token |
| --- | --- | --- | --- |
| Codex | Codex | `01a033a5-c556-7c10-8d6f-f17239b41d20` | `QWEN_COMPOSE_CODEX_TO_CODEX_1_OK` |
| Codex | Claude | `1a5046da-f4f8-40b4-be11-0e92274a00fd` | `QWEN_COMPOSE_CODEX_TO_CLAUDE_2_OK` |
| Codex | Grok | `8c769ef5-15c5-4c82-8ce6-d7b6d4080c9b` | `QWEN_COMPOSE_CODEX_TO_GROK_3_OK` |
| Codex | Qwen | `236608dd-f903-4150-b640-953fcb99e749` | `QWEN_COMPOSE_CODEX_TO_QWEN_4_OK` |
| Claude | Codex | `01a033a6-7fe2-7780-9844-54b0f583ec26` | `QWEN_COMPOSE_CLAUDE_TO_CODEX_5_OK` |
| Claude | Claude | `fe5f1ac3-32d3-4b00-9984-3aa2ddb61485` | `QWEN_COMPOSE_CLAUDE_TO_CLAUDE_6_OK` |
| Claude | Grok | `549cf2e9-57ff-4002-a7dd-4fef4546906e` | `QWEN_COMPOSE_CLAUDE_TO_GROK_7_OK` |
| Claude | Qwen | `c2524b0d-12ee-4adb-9261-aa3925cf273b` | `QWEN_COMPOSE_CLAUDE_TO_QWEN_8_OK` |
| Grok | Codex | `01a033a7-3527-7853-b637-790483c7cea3` | `QWEN_COMPOSE_GROK_TO_CODEX_9_OK` |
| Grok | Claude | `fb46c085-b83b-4194-ad23-a14795207688` | `QWEN_COMPOSE_GROK_TO_CLAUDE_10_OK` |
| Grok | Grok | `e81c6af9-063c-45b6-b195-7078d1873b89` | `QWEN_COMPOSE_GROK_TO_GROK_11_OK` |
| Grok | Qwen | `4c495593-8411-45a1-9867-519d057c2e8d` | `QWEN_COMPOSE_GROK_TO_QWEN_12_OK` |
| Qwen | Codex | `01a033a7-eb52-75f2-a893-13bb04d819ba` | `QWEN_COMPOSE_QWEN_TO_CODEX_13_OK` |
| Qwen | Claude | `450d2f2d-f270-4824-a563-a2a2c5b90239` | `QWEN_COMPOSE_QWEN_TO_CLAUDE_14_OK` |
| Qwen | Grok | `9da88fd9-a371-4b9e-81b2-c15e66004997` | `QWEN_COMPOSE_QWEN_TO_GROK_15_OK` |
| Qwen | Qwen | `01ac3c06-6dd5-4013-8368-9369697a72e8` | `QWEN_COMPOSE_QWEN_TO_QWEN_16_OK` |

The verifier independently found all sixteen tokens in the corresponding native target stores—four each under the isolated Codex, Claude, and Grok profiles and four archived Qwen chats—rather than crediting only the runner's summary. For every edge the runner asserted exact parent, owner, notification target, explicit group, both private anchors, non-persistence, terminal answer, archive, cleared worker/manager identities, and removed delivery socket.

## Profile-scoped integration boundary

The authenticated default Qwen profile initially contained no Agent Sessions extension. With explicit owner authorization, `make dev-install-qwen QWEN="$HOME/.local/bin/qwen"` installed the exact source payload at version `0.2.4`; all seven shipped payload files matched the tree byte-for-byte. After the matrix, `make remove-qwen QWEN="$HOME/.local/bin/qwen"` removed the extension payload and active policy successfully.

A recursive pre/post manifest found no removed baseline path and no credential or unrelated setting mutation. Qwen's native extension manager retained its normal bookkeeping: an empty artifact-keyed plugin-data directory, `state.previous.json`, an empty `extension-preferences.json`, and a monotonic `state.json` generation change. The authenticated run also appended native usage records and created the client's log-cleanup marker. These are native journal/usage artifacts, not active Agent Sessions installation state; none was manually restored or deleted.

## RCA and cleanup

Earlier macOS attempts established that the repository workspace's checked-in `.mcp.json` had masked whether Grok used session injection. Once every lane used the clean workspace and both fallback sources were rejected, Codex-to-Grok and the remaining Grok edges passed on the same Darwin Grok build. The earlier conclusion that Darwin discarded session MCP configuration is therefore withdrawn.

An externally terminated diagnostic run had left an isolated bridge supervisor alive after its temporary root was removed. The composition runner now converts SIGINT/SIGTERM into an orderly cleanup unwind, continues independent cleanup after an individual failure, and the supervisor `stop` command waits for both its control socket and exact PID/process-start identity to retire before the runner removes the root. Regression coverage exercises the signal handler and proves stop does not return merely because the socket disappeared.

The credited matrix left zero composition roots and zero processes whose executable belonged to the test runtime. Owner Grok configuration/plugin state was unchanged. No evidence writer read or copied any credential value, and no merge, tag, or release occurred during acceptance.
