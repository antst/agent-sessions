# US4 macOS to Linux: Qwen federation

- Date: 2026-08-24
- Verdict: GREEN
- Commit: `ef4fd414aff2e214746e21902ca4daaf6e56f536`
- Tree: `28a7b1386c339083a809973c3db204fe28261d87`
- Parent: `642829eca0e18bd6949689563998fce78e30e02d`
- Subject: `Separate Qwen readiness from lane dispatch`
- Signature: good ED25519 signature for Anton Starikov with key
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Agent Sessions version/protocol: `0.2.4` / `3`
- Source: macOS arm64 host `mac-qwen-ef4`
- Destination: Linux amd64 host `linux-qwen-ef4`
- Hub: `10.2.20.132:7520`, owned by the isolated Linux test root

Both exact-current agents advertised all four lane capabilities. The source parent used a dedicated
macOS runtime/profile root and the destination used only `/tmp/qwen-fed-642-linux`. The remote Qwen
doctor was required to report ready before the lane could start.

## Exact source parent and call sequence

The managed macOS Qwen parent was:

| Field | Exact value |
| --- | --- |
| Native parent session | `6a7c3cf4-d6ef-4714-9e60-3f9ffac41250` |
| Explicit group | `qwen-fed-ef4` |
| Source runtime | `/tmp/qwen-fed-ef4-mac/agent-runtime` |

The 28-record parent transcript proves exactly one call each for doctor, start, wait, and archive:
doctor at record 9, start at 13, wait at 20, and archive at 24, with responses at records 11, 15, 22,
and 26. Each prompted operation was approved once; no blanket approval was accepted. The parent used
the structured `agent_sessions.lane` tool rather than a shell command.

Doctor returned `READY`: Qwen `0.22.0`, Agent Sessions `0.2.4`, contract version `1`, and package,
authentication, ACP, archive, interactive-mode, workspace-trust, integration, and all parser
contracts ready. The parent then issued one start to host `linux-qwen-ef4`, product `qwen`, with
input:

```text
Return only the exact token QWEN_FED_MAC_TO_LINUX_EF4_OK and nothing else.
```

and native arguments equivalent to:

```text
--name qwen-fed-m2l-ef4 --inherit-groups --no-yolo -C /home/antst/agent-sessions
```

No lifecycle flag was supplied; the federator owned the remote persistent lifecycle.

## Lane, anchors, notice, collection, and archive

| Field | Exact value |
| --- | --- |
| Lane/session/thread | `a19a8203-3e27-4754-81e3-8031a08209ef` |
| Native Qwen session | `77c778bf-c87f-4085-95a5-10e0b8fe7a2f` |
| Turn/latest/collected turn | `83d2a00e-5c0e-4e8d-b7ed-27f886b82073` |
| Name | `qwen-fed-m2l-ef4` |
| Cwd | `/home/antst/agent-sessions` |
| Launch preference | `non_yolo` |
| Requested/initial/current native mode | `default` / `default` / `default` |
| Parent and notify target | `mac-qwen-ef4/6a7c3cf4-d6ef-4714-9e60-3f9ffac41250` |
| Status/outcome/exit | `completed` / `completed` / `0` |
| Stop reason | `end_turn` |

The groups were exactly:

```text
qwen-fed-ef4
session:linux-qwen-ef4/a19a8203-3e27-4754-81e3-8031a08209ef
session:mac-qwen-ef4/6a7c3cf4-d6ef-4714-9e60-3f9ffac41250
```

Thus the explicit group, destination-child anchor, source-parent anchor, and exact parent notification
binding all propagated, with no unrelated or global group. `owner_session_id` was null and the remote
lifecycle was persistent with the documented 60-second auto-archive policy.

The destination emitted one terminal notice, ID `214892f64218db050754`, delivered on its first
attempt. Parent record 17 shows the decision to collect, record 18 is the notice, and record 20 is the
first and only wait call; there is no wait before the notice. The exact pointer was:

```text
peer-federator lane -runtime-dir /tmp/qwen-fed-ef4-mac/agent-runtime --host linux-qwen-ef4 --product qwen -- wait a19a8203-3e27-4754-81e3-8031a08209ef
```

It returned the exact token `QWEN_FED_MAC_TO_LINUX_EF4_OK`. The one archive call returned
`lane.archived`; auto-archive had already completed without changing the collected result.

## Independent Linux destination audit

The Linux lane record was retained at:

```text
/tmp/qwen-fed-642-linux/agent-peer-data/profiles/38d3708849c5c0514b78/qwen-lanes/f31ae707de10b4d20e9f.json
```

It independently corroborated every identity, group, parent/notify binding, mode, terminal outcome,
and collected turn above. The record was archived, `collectedTurnId` equalled `latestTurnId`,
`terminalOutcome` was `completed`, and `nativeArchiveState` was `archived`. Manager PID, worker PID,
endpoint, lane socket, and cleanup-error fields were absent or empty after retirement.

The only native transcript was archived at:

```text
/tmp/qwen-fed-642-linux/target-qwen-runtime/projects/-home-antst-agent-sessions/chats/archive/77c778bf-c87f-4085-95a5-10e0b8fe7a2f.jsonl
```

Its permitted test-owned SHA-256 was
`fc7ba30d75120703d8103130df44efa93c8b17db65a2492783768bb03b389498`. No active chat remained. The
mode-0600 manager log was empty; the lane delivery socket was absent; active lane records and exact
Qwen lane manager/worker processes were zero. Linux retained exactly one service row and only the
expected isolated supervisor/App Server infrastructure.

## Source cleanup and boundary

The source audit found no destination record for this lane on macOS, as required: the record lives
only on the Linux destination. Its only local lane record was the earlier archived Linux-to-macOS
lane, with manager/worker identities and socket cleared. Exact executable-path counts for every peer
and lane launcher were zero after the remote lane archived.

The Qwen CLI displayed `run the next acceptance cell` in dim placeholder styling after the run. ANSI
capture, complete keystroke history, and the unchanged 28-record transcript proved this was a native
placeholder with an already-empty input buffer, not submitted or buffered input. The verifier used
only the separately authorized exit sequence and did not begin another cell. Typing `/quit` without
Enter displaced the grey placeholder and rendered only `> /quit` in normal foreground, directly
confirming that the buffer had been empty. Enter then produced a native clean exit; no signal or tmux
kill was used, and the transcript stayed at 28 records with its original mtime.

After exit, macOS `local_peers` changed from `1` to `0`; the parent session socket retired; there were
zero non-supervisor sockets and no lane socket. Exact executable-path counts were zero for
`qwen-peer`, `qwen-peer-lane`, `codex-peer-lane`, `claude-peer-lane`, and `grok-peer-lane`. Native
Qwen processes under the parent runtime fell from two to zero, and `agent-session-runtime` fell from
two to one, leaving only the isolated supervisor. The one service row and exact-current connected
agent remained, with four capabilities, `remote_peers=0`, and no M2L lane record on the source.

Owner credential files were inspected by metadata only. No credential value or credential-bearing
file was read, copied, printed, logged, diffed, or hashed. The owner agent and App Server were not
selected or restarted, the installed Qwen extension remained at `0.2.4`, and no owner permission or
authentication setting was broadened.

## Discarded attempt and harness corrections

The first reverse attempt is void: its parent resolved an older owner-installed runtime through the
Qwen plugin's bare `PATH` command. The accepted attempt explicitly exercised the exact current
runtime, confirmed by its four-product MCP description. That finding produced a separate
extension-rooted exact-runtime correction and receives no acceptance credit by itself.

The verifier also retracted three harness readings: a false response count, a completion watcher that
looked for a field lane responses do not carry, and an emptiness substring check defeated by ANSI
style boundaries. None affected the authoritative transcript, destination record, token, or cleanup
assertions recorded above.
