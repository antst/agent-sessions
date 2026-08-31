# US4 Linux to macOS: Qwen federation

- Date: 2026-08-24
- Credited interval: `2026-08-24T20:08:57Z` through `2026-08-24T20:15:38Z`
- Verdict: GREEN
- Commit: `ef4fd414aff2e214746e21902ca4daaf6e56f536`
- Tree: `28a7b1386c339083a809973c3db204fe28261d87`
- Parent: `642829eca0e18bd6949689563998fce78e30e02d`
- Subject: `Separate Qwen readiness from lane dispatch`
- Signature: good ED25519 signature for Anton Starikov with key
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Agent Sessions version/protocol: `0.2.4` / `3`
- Qwen Code: `0.22.0` on both hosts
- Source: Linux amd64 host `linux-qwen-ef4`
- Destination: macOS arm64 host `mac-qwen-ef4`
- Hub: `10.2.20.132:7520`, owned by Linux PID `1137472`, with the only credited
  federation listener bound at that address

Both exact-current host agents advertised `codex-lane`, `claude-lane`, `grok-lane`, and
`qwen-lane`. The source had no local peer before the cell, the destination had no active Qwen lane,
and neither host ran a Qwen lane manager or worker. The Linux agent used only
`/tmp/qwen-fed-642-linux`; the Mac agent used only `/tmp/qwen-fed-ef4-mac`. Each agent had one
service row and one outbound hub connection; neither agent opened a host listener.

## Exact source and parent

The Linux agent was PID `1171481`, process start `1798412231`, executing the exact source
`bin/linux-x64/peer-federator` with SHA-256
`e18f88cdf2ac791133163007a25c4402a8bd4068721ead30927d7269157a7c7e`. Its exercised runtime and
Qwen launchers were:

| Executable | SHA-256 |
| --- | --- |
| `bin/linux-x64/agent-session-runtime` | `fddcb08415030b77972ea65ea89dfc3f6669d1e7d4abf01079e09e1eb5fbcdef` |
| `bin/linux-x64/qwen-peer` | `8f5df9c6b937f27d4c693a6b3c4eeca63aa52150d6b03c09fe35c874ea5fdd0a` |
| `bin/linux-x64/qwen-peer-lane` | `c6521135ffeea09407a5761721ac93fa2d39f1cd27e13d02786f41e1abe7eed8` |

The credited managed Qwen parent was:

| Field | Exact value |
| --- | --- |
| Name | `linux-qwen-l2m-parent` |
| Session ID | `504c40d2-7011-488e-b5e3-c154dcccc10f` |
| Explicit group | `qwen-fed-ef4` |
| Cwd | `/home/antst/agent-sessions` |
| Native transcript | `/tmp/qwen-fed-642-linux/qwen-runtime/projects/-home-antst-agent-sessions/chats/504c40d2-7011-488e-b5e3-c154dcccc10f.jsonl` |
| Transcript SHA-256 | `82aeb5575d612eeed3afb85cee2c612b0741ecc29045434095b341eb809b745f` |

The parent first ran the scoped remote Qwen doctor against `mac-qwen-ef4`; it reported Qwen
`0.22.0`, package, trust, parser, ACP, archive, integration, and provider configuration ready. Only
then did it make the single credited start call.

## Lane, notice, collection, and archive

The parent called `agent_sessions.lane` exactly once with product `qwen`, command `start`, host
`mac-qwen-ef4`, input `Return only the exact token QWEN_FED_LINUX_TO_MAC_EF4_OK and nothing else.`,
and native arguments:

```text
--name qwen-fed-l2m-ef4 --inherit-groups --no-yolo -C /Users/antst/work/ai/agent-sessions
```

No lifecycle flag was supplied. The destination-owned remote lifecycle reported
`persistent=true`, `owner_session_id=null`, auto-archive enabled, and these exact identities:

| Field | Exact value |
| --- | --- |
| Lane/session/thread | `d6517e01-5e32-4cee-a146-31aa0bb206c7` |
| Native Qwen session | `fe11f460-1227-453a-bbc5-22aad0dab6a9` |
| Turn | `2f506488-74a1-4f39-812d-05baa2102fe5` |
| Name | `qwen-fed-l2m-ef4` |
| Cwd | `/Users/antst/work/ai/agent-sessions` |
| Launch preference | `non_yolo` |
| Initial/current native mode | `default` / `default` |
| Notify target | `linux-qwen-ef4/504c40d2-7011-488e-b5e3-c154dcccc10f` |

The groups were exactly:

```text
qwen-fed-ef4
session:linux-qwen-ef4/504c40d2-7011-488e-b5e3-c154dcccc10f
session:mac-qwen-ef4/d6517e01-5e32-4cee-a146-31aa0bb206c7
```

There was no unrelated or global group. The source-parent and destination-child private anchors and
the exact parent notification binding were therefore present.

The destination emitted exactly one terminal notice, ID `8a19c624497bb24091ac`, with one delivery
attempt and no retry. It carried status/outcome `completed`/`completed`, exit `0`, collection
`required`, and this exact pointer:

```text
peer-federator lane -runtime-dir /tmp/qwen-fed-642-linux/agent-runtime --host mac-qwen-ef4 --product qwen -- wait d6517e01-5e32-4cee-a146-31aa0bb206c7
```

The literal line's SHA-256 is
`6c580810a1f442223fcc729920bcc08d2dc22c52483ed23ba0bc838f203d6b4c`. The structured lane tool
mapped that pointer once to product `qwen`, host `mac-qwen-ef4`, command `wait`, and argument
`d6517e01-5e32-4cee-a146-31aa0bb206c7`; the parent transcript contains one and only one such call.
It returned the exact token `QWEN_FED_LINUX_TO_MAC_EF4_OK`, status/outcome
`completed`/`completed`, exit `0`, and stop reason `end_turn`. The transcript then contains exactly
one archive call for that same host, product, and session, which returned `lane.archived`.

## Independent destination audit and cleanup

The Mac verifier inspected the destination record
`peer-data/profiles/524792247b73d1bbe85e/qwen-lanes/a14a347bd9c0b5dd4328.json` independently.
It corroborated the thread, native session, turn, token, groups, parent/notify binding, cwd, launch
preference, native modes, terminal state, and exit code. `collectedTurnId` equalled `latestTurnId`;
status was `archived`; `terminalOutcome` was `completed`; and `nativeArchiveState` was `archived`.
The record retained no manager PID, worker PID, endpoint, lane socket, or cleanup error.

The native destination transcript was present only at:

```text
/tmp/qwen-fed-ef4-mac/qwen-runtime/projects/-Users-antst-work-ai-agent-sessions/chats/archive/fe11f460-1227-453a-bbc5-22aad0dab6a9.jsonl
```

No active native chat remained. The manager log was empty; the delivery socket was absent; active
Qwen lanes were zero; exact executable-path counts for every lane and peer launcher were zero; and
the Mac service-row count remained one. The destination retained only its expected isolated
supervisor and isolated Codex App Server infrastructure. The source parent then exited cleanly:
Linux returned to `local_peers=0`, one service row, one agent socket, zero preparations, zero Qwen
lane records, and zero lane/parent processes.

Owner credential files and secure storage were checked by metadata only. Credential values were not
read, printed, logged, copied, or hashed. Neither host broadened owner permission/authentication
settings, selected an owner daemon for isolated work, opened a fallback listener, or routed the lane
through any host other than the explicitly named destination.

## Discarded attempts

One earlier disposable Linux Qwen parent completed doctor but hit Qwen's native default 2 GiB Node
heap limit before it could create a lane. It created no destination lane and receives no acceptance
credit. The credited parent used an explicit larger native heap. An earlier Mac setup also lacked an
isolated Codex package tree required by the bridge supervisor; it stopped before the credited cell.
The accepted destination used a fully isolated package tree and App Server and did not select the
owner daemon.
