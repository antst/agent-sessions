# Grok product facts

- verified: Grok Build 1.0.13 (`5e9a58528b76`, stable) was installed at `/home/antst/.local/bin/grok` on `umka-dev1`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P0-version-help.txt`
- verified: Grok Build 1.0.13 — Grok's product interface is ACP over stdio; `grok agent stdio` and `grok agent leader` are native commands, and Grok is an MCP client rather than an MCP server. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P0-version-help.txt`
- verified: Grok Build 1.0.13 — ACP initialization uses protocol version 1 and advertises `cached_token`; unattended authentication is `authenticate {methodId:"cached_token",_meta:{headless:true}}`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`
- verified: Grok Build 1.0.13 — a fresh ACP session is created with `session/new`; the returned product-minted `sessionId` is the Agentbus session id. Passing `--session-id` to the ACP process did not determine the id returned by `session/new`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`
- verified: Grok Build 1.0.13 — the P7 capture loaded an already-live session with `session/load {sessionId:<product-id>}`; it did not prove an offline resume because `session/close` followed the load. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt:547-705`
- verified: Grok Build 1.0.13 — an offline lane launches the ACP primary without `--resume`; `session/load {sessionId:<product-id>}` is the sole resume selector. source: `c5b280d:internal/bridge/grok_native_session.go:191-227`; `/home/antst/agentbus-evidence/grok-w2-rerun-20260906T125544Z/retry-strace.1090844`
- verified: Grok Build 1.0.13 — an offline `session/load` result may omit top-level `sessionId`; `_meta.sessionId` and `_meta.x.ai/sessionDetail.sessionId` carry the requested identity. Every returned identity is checked and the requested id is the fallback, matching the 0.4.0 closed parser. source: `/home/antst/agentbus-evidence/grok-w2-rerun-20260906T125544Z/retry-strace.1090823`; `c5b280d:internal/bridge/grok_native_session.go:227-237`
- verified: Grok Build 1.0.13 — an offline row resumed with its original product id, completed one `Reply exactly W2_OK` turn with native stop reason `end_turn`, and closed with no wrapper, Grok, MCP, or zombie process left by the run. source: `/home/antst/agentbus-evidence/grok-w2-20260906T130703Z`
- verified: Grok Build 1.0.13 — killing an isolated daemon during one active turn made the caller lose its result, the wrapper reaped its Grok and MCP processes, and a restarted daemon loaded the durable row offline; the same product id resumed and then closed with no wrapper, Grok, MCP, or zombie process left. source: `/home/antst/agentbus-evidence/grok-w8-rerun-20260906T133939Z`
- verified: Grok Build 1.0.13 — one active turn exposed `running:true`, rejected a second run as `busy`, accepted `turn.interrupt`, and terminated as `interrupted` with native stop reason `cancelled`. source: `/home/antst/agentbus-evidence/grok-w4-w7-20260906T132250Z`
- verified: Grok Build 1.0.13 — closing during one active turn issued the native cancellation, rejected a concurrent public interrupt as `busy`, wrote the interrupted terminal before close `{}`, and left no wrapper, Grok, MCP, or zombie process. source: `/home/antst/agentbus-evidence/grok-w4-w7-20260906T132250Z`
- verified: the Agentbus schema, closed-type decoder, malformed/trailing/unmatched response handling, 1 MiB frame bound, separate name/session-id grammar, and invalid-product no-exec rows passed under race on `umka-dev1` without a Grok turn or a new process/zombie. source: `/home/antst/agentbus-evidence/grok-w9-20260906T135014Z`
- verified: Grok Build 1.0.13 runtime cells use one isolated checkout, binary prefix, daemon, socket, and evidence directory per product run; unrelated installed binaries, daemons, and sessions stay untouched. source: `/home/antst/agentbus-evidence/grok-w2-20260906T130703Z/RUN.txt`; `/home/antst/agentbus-evidence/grok-w2-20260906T130703Z/processes-final.txt`
- verified: Grok Build 1.0.13 — `cwd`, `permission_mode`, `reasoning_effort`, `model`, and supported extra arguments are process flags on the primary ACP command. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`
- verified: Grok Build 1.0.13 — the pinned CLI accepts permission modes `default`, `auto`, `plan`, `acceptEdits`, `dontAsk`, and `bypassPermissions`, and accepts reasoning efforts `low`, `medium`, `high`, and `xhigh`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P0-version-help.txt`
- verified: Grok Build 1.0.13 — the session open object carries `cwd`, `mcpServers`, `_meta.yoloMode`, and `_meta.autoMode`; bypass permission maps to `yoloMode:true`, while default maps to `false`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_native_session.go:211-240`
- verified: Grok Build 1.0.13 — the lane's private MCP entry is described in `session/new` or `session/load`; Grok invokes it as an MCP client during a turn. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P8-product-mcp-raw-v5.txt`; `/home/antst/agentbus-evidence/grok-20260906T072333Z/P8-mcp-raw-v5.txt`
- verified: Grok Build 1.0.13 — the wrapper starts a private leader with `agent leader`, `--leader-socket`, `--relay-on-demand`, and `--no-auto-update`; default permission also grants `MCPTool(agent_sessions__*)`. source: `c5b280d:internal/bridge/grok_native_session.go:31-72`
- verified: Grok Build 1.0.13 — the resident observer authenticates after leader socket readiness and before the TUI starts, then stays quiet until the leader's first `_x.ai/sessions/changed`; that same connection serves readiness, later roster changes, and delivery. The relay serializes a roster request issued before the TUI session exists and the TUI never becomes ready. source: `c5b280d:internal/bridge/grok_native_session.go:83-135`; `c5b280d:internal/bridge/grok_native_observer.go:22-45`; `/home/antst/agentbus-evidence/grok-new-20260906T164113Z`; `/home/antst/agentbus-evidence/grok-new-retry-20260906T170014Z`
- verified: Grok Build 1.0.13 — native title changes use `_x.ai/session/rename {sessionId,title}` and are confirmed by a later exact roster read. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_native_observer.go:226-245`
- verified: Grok Build 1.0.13 — `_x.ai/sessions/list` is the authority for an interactive peer's current title, cwd, activity, resident state, and yolo state. Exactly one matching live row is required. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_roster.go:9-25`; `c5b280d:internal/bridge/grok_roster.go:58-116`
- verified: Grok Build 1.0.13 — the retired 0.4.0 status projection mapped `working` to busy, `needs_input` to waiting, and `idle` to idle; Agentbus has no corresponding status surface. The wrapper validates all three native states and uses only working versus non-working to choose active interjection or idle rejection/FIFO. Missing `resident` or `yolo`, duplicate exact rows, and unknown live activity are authority errors. source: `c5b280d:internal/bridge/grok_roster.go:58-116`; `wrappers/grok/peer.go`
- verified: Grok Build 1.0.13 — a run is `session/prompt` with the exact session id and a text prompt; `agent_message_chunk` notifications are accumulated, while `agent_thought_chunk` is not part of the returned text. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_native_session.go:270-309`; `c5b280d:internal/bridge/grok_native_session.go:331-360`
- verified: Grok Build 1.0.13 — `session/prompt` returns `stopReason:end_turn` for completion and `stopReason:cancelled` after cancellation; the native stop reason is preserved. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`
- verified: Grok Build 1.0.13 — interrupt is the ACP notification `session/cancel {sessionId}`; there is no response, and the run terminal supplies the result. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_native_session.go:311-322`
- verified: Grok Build 1.0.13 — delivery uses `_x.ai/interject {sessionId,text,interjectionId}` and is acknowledged by the actor notification `_x.ai/session/interjection` carrying the same identifiers. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`
- verified: Grok Build 1.0.13 — an interjection while the actor is working is injected into that run. An idle interjection instead starts Grok's own `interject-fallback` prompt and queues a later `session/prompt` behind it, so lane-idle delivery uses the shared wrapper FIFO and native interject is active-only. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt:286-302`; `/home/antst/agentbus-evidence/grok-runtime-20260906T120424Z/cells/W5-idle-delivery.txt`
- verified: Grok Build 1.0.13 — streamed chunks and the terminal response carry a product prompt ID; the wrapper selects only chunks matching the ID returned for its own `session/prompt`. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt:303-493`
- verified: Grok Build 1.0.13 — only exact absence of the asserted actor maps to `no_leader`; malformed, duplicate, nonresident, or otherwise invalid roster state remains its own diagnostic. source: `c5b280d:internal/bridge/grok_roster.go:9-25`; `c5b280d:internal/bridge/grok_roster.go:58-116`
- verified: Grok Build 1.0.13 — native orderly close is `session/close {sessionId}` on the primary. Agentbus then stops the leader and releases the wrapper-owned endpoint and session lock; the daemon's close bound is the only close clock. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt`; `c5b280d:internal/bridge/grok_native_session.go:324-329`
- verified: Grok Build 1.0.13 — the early direct MCP probe could observe and interject through the user's default Grok leader, but the product killed that helper after each call; this proves the native delivery path, not the accepted resident ownership topology. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P8-product-mcp-raw-v5.txt`; `/home/antst/agentbus-evidence/grok-20260906T072333Z/P8-no-leader-raw-v5.txt`
- verified: Grok Build 1.0.13 — an MCP server named `agent_sessions` exposing the `agent_sessions` tool is presented to the model as `agent_sessions__agent_sessions`; the model invoked that exact namespaced name successfully. source: `/home/antst/agentbus-evidence/grok-peer-20260906T135533Z/C1-rerun-export.md`; `/home/antst/agentbus-evidence/grok-peer-20260906T135533Z/work/.grok/config.toml`
- verified: Grok Build 1.0.13 — its configured stdio MCP helper is not resident between tool calls: one action completed during a 45-second native terminal hold, while 30 one-second Agentbus snapshots and the mid-hold process tree found no Grok peer attachment or `grok-peer mcp` process. source: `/home/antst/agentbus-evidence/grok-peer-20260906T135533Z/C1-rerun-export.md`; `/home/antst/agentbus-evidence/grok-peer-20260906T135533Z/C1-rerun-list-*.json`
- verified: Grok Build 1.0.13 — interactive Agentbus mode therefore keeps `grok-peer` resident: the launcher owns one private leader, the TUI child, observer, peer connection, caller state, and private action endpoint; each short-lived `grok-peer mcp` invocation carries one action to that endpoint and owns no session state. source: `cmd/grok-peer/main.go`; `wrappers/grok/peer.go`; `wrappers/mcp/lane.go`
- verified: Grok Build 1.0.13 — the managed interactive surface preserves native informational commands, requires an exact `--session-id` or valued `--resume`/`--load` identity, and rejects bare resume, `--continue`, `--fork-session`, caller-owned leader selection, and headless-only flags; headless work uses an Agentbus Grok lane. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P0-version-help.txt`; `wrappers/grok/peer.go`; `c5b280d:internal/launcher/grok_peer.go:1-240`
- verified: Grok Build 1.0.13 — the 0.4.0 launcher searched configured and product-specific fallback locations for Grok. That behavior is retired: the split-ready wrapper follows the product-independent Agentbus rule and resolves the real `grok` executable from `PATH`. source: `c5b280d:internal/launcher/grok_peer.go:478-547`
- verified: Grok Build 1.0.13 — inherited session locks, the token-digest provisional lock/socket name, MCP message rendering, and peer reconnect/identity cancellation are shared host or SDK mechanisms rather than Grok-specific lifecycle state. The interactive wrapper owns its ACP observer's process group directly: it starts the observer with `Setpgid`, then kills the group and joins the child at shutdown; the lane-oriented host child runner cannot own this process because it requires and releases a session lock and private endpoint. source: `wrappers/grok/peer.go`; `c5b280d:internal/bridge/grok_process_unix.go:1-49`; `c5b280d:internal/sessiontools/envelope.go:1-78`
- verified: Grok Build 1.0.13 — the pre-correction resident cell used the product-presented `agent_sessions__agent_sessions` tool, accepted an active default-leader interjection, and stayed resident after the native turn; the exported transcript proves tool naming, interjection, and residency, while the later topology correction replaces only leader ownership. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C1-export.md`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C5-active-send.json`
- verified: Grok Build 1.0.13 — the resident launcher kept one caller registry across separate short action connections: start returned `t-1`, status and a bounded wait remained `running`, interrupt returned `{}`, and the collected terminal was `interrupted`. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C3-start.json`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C3-status.json`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C3-wait-timeout.json`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C4-wait-terminal.json`
- verified: Grok Build 1.0.13 — `/rename Grok Renamed Title` changed the native title; the next resident action serialized roster observation, same-ID re-hello, and publication, and Agentbus preserved the spaced title exactly. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C8-rename-pane.txt`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C8-title-rehello.json`
- verified: Grok Build 1.0.13 — killing only the isolated Agentbus daemon while an example lane turn was outstanding settled the resident local turn as `unavailable` with `result unavailable, lane resumable`; a call while disconnected returned `not_connected`, and the same Grok identity reconnected after the daemon restarted. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C4-C8-status-after-eof.json`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C8-call-disconnected.json`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C8-list-after-reconnect.json`
- verified: Grok Build 1.0.13 — a `--single` headless invocation completed its prompt but did not remain a live roster row for peer attachment; the installed peer cell therefore used the interactive TUI, which stayed resident between turns. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C-product.stdout`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C-list-now.json`
- verified: Grok Build 1.0.13 — that discarded `--single` attempt auto-started a detached default leader which outlived the command; the stale leader then held later interactive startup at `Starting session`. Agentbus interactive mode therefore rejects headless flags and starts, routes to, joins, and removes one private leader of its own. source: `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C-headless-leader-before-cleanup.txt`; `/home/antst/agentbus-evidence/grok-peer-resident-20260906T153610Z/C-headless-leader-after-cleanup.txt`; `/home/antst/agentbus-evidence/grok-clear-20260906T162206Z/pane-not-ready.txt`; `wrappers/grok/peer.go`
- UNVERIFIED: Grok Build versions after 1.0.13 may add or change ACP methods, roster fields, activity values, permission values, or interactive flags; a version update requires fresh captures before changing these closed surfaces. source: `/home/antst/agentbus-evidence/grok-20260906T072333Z/P0-version-help.txt`

## Exact captured frames typed by the wrapper

Each line below is copied byte-for-byte, excluding its terminating newline, from a `STDIN` frame in `/home/antst/agentbus-evidence/grok-20260906T072333Z/P1-P7-acp-raw-v3.txt` (Grok Build 1.0.13).

- `initialize`

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}
```

- `authenticate`

```json
{"jsonrpc":"2.0","id":2,"method":"authenticate","params":{"methodId":"cached_token","_meta":{"headless":true}}}
```

- `session/new`

```json
{"jsonrpc":"2.0","id":30,"method":"session/new","params":{"cwd":"/home/antst/agentbus-evidence/grok-20260906T072333Z/work","mcpServers":[],"_meta":{"yoloMode":false,"autoMode":false}}}
```

- `session/load`

```json
{"jsonrpc":"2.0","id":303,"method":"session/load","params":{"sessionId":"01a075a0-43ac-7d70-919f-f5e0f6f5e4e2","cwd":"/home/antst/agentbus-evidence/grok-20260906T072333Z/work","mcpServers":[],"_meta":{"yoloMode":false,"autoMode":false}}}
```

- `session/prompt`

```json
{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"01a075a0-43ac-7d70-919f-f5e0f6f5e4e2","prompt":[{"type":"text","text":"Reply with exactly GROK_PROBE_OK after waiting five seconds."}]}}
```

- `session/cancel`

```json
{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"01a075a0-43ac-7d70-919f-f5e0f6f5e4e2"}}
```

- `_x.ai/interject`

```json
{"jsonrpc":"2.0","id":106,"method":"_x.ai/interject","params":{"sessionId":"01a075a0-43ac-7d70-919f-f5e0f6f5e4e2","text":"Also include ACTIVE_INTERJECT_OK in the same answer.","interjectionId":"active-message-1"}}
```

- `_x.ai/sessions/list`

```json
{"jsonrpc":"2.0","id":103,"method":"_x.ai/sessions/list","params":{}}
```

- `session/close`

```json
{"jsonrpc":"2.0","id":305,"method":"session/close","params":{"sessionId":"01a075a0-43ac-7d70-919f-f5e0f6f5e4e2"}}
```
