# Qwen Code — verified product facts

Flat fact list for Qwen Code as a Sessionbus product. No prose beyond facts.

Provenance and tag legend:

- Repo facts cite `path:lines` in the 0.4.0 tree at ff81565; every cited qwen file is
  byte-identical at 2e0e498 (this commit) and c5b280d. Design facts cite
  `docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:lines` at 2e0e498 and are tagged
  `source: picture`.
- Product facts were verified directly against the installed qwen-code 0.23.0 bundle at
  `/home/antst/.local/lib/qwen-code` (cited `bundle:lib/chunks/<file>:<lines>`). CLI
  surface facts cite the committed fixture `docs/products/qwen-0.23.0-help.txt`: line 1
  is the exact `qwen --version` output (`0.23.0`), lines 2-81 are the exact `qwen --help`
  output, both captured 2026-09-06 on this host (`/home/antst/.local/bin/qwen` →
  `/home/antst/.local/lib/qwen-code`), non-TTY, unedited.
- Fixture frames were captured by the repo against qwen-code 0.21.15 and are tagged
  `verified: 0.21.15 (fixture)`.
- `verified: pin` = fact about Sessionbus code, which holds against the catalog pin
  "minimum 0.22.0; validated on 0.22.3 and 0.23.0" (docs/PRODUCTS.md:12).
- `verified: n/a (design)` = design decision; not a product behavior.
- Catalog pin: minimum 0.22.0; validated on 0.22.3 and 0.23.0. (verified: pin;
  source: docs/PRODUCTS.md:12)

## Layout

- ff81565/2e0e498 have no `integrations/qwen/`; qwen code lives in
  `internal/products/qwen/`, `internal/launcher/qwen_peer*.go`,
  `ff81565:cmd/agent-sessions/qwen_peer.go`, `ff81565:cmd/agent-sessions/connector.go`, `internal/bridge/qwen*.go`,
  `internal/qwenprofile/`, `internal/daemon/adapter_qwen.go`, and `qwen/`. (verified: pin;
  source: path inventory — `git ls-tree -r --name-only ff81565 | grep -i qwen`, code and
  plugin paths: `ff81565:cmd/agent-sessions/qwen_peer.go`, `ff81565:cmd/agent-sessions/qwen_peer_test.go`,
  internal/bridge/qwen.go, internal/bridge/qwen_test.go, internal/bridge/qwen_title.go,
  internal/bridge/qwen_title_test.go, internal/bridge/testdata/qwen/{acp.jsonl,
  dual-output.jsonl,serve.json,version.json}, internal/daemon/adapter_qwen.go,
  internal/daemon/adapter_qwen_test.go, internal/launcher/qwen_peer.go,
  internal/launcher/qwen_peer_test.go, internal/launcher/qwen_test_helpers_test.go,
  internal/products/qwen/{client.go,lane.go,lane_test.go,process.go},
  internal/qwenprofile/{profile.go,profile_test.go}, qwen/mcp.json, qwen/plugin.json,
  qwen/scripts/native-entry; the same grep also lists qwen-named skill and spec docs
  (claude/skills/qwen-lane/, qwen/skills/, skills/qwen-lane/, specs/001-qwen-support/),
  which carry no runtime code)
- `bus/` contains zero qwen mentions at ff81565 and 2e0e498: the universal `qwen-peer`
  wrapper is designed but not yet implemented; current facts describe the 0.4.0
  launcher/driver paths, picture facts describe the accepted design. (verified: pin;
  source: `git grep -i qwen -- bus/` empty at both commits)

## Process contract and mode select

- One process contract: daemon execs the product binary with empty argv;
  `SESSIONBUS_LAUNCH_TOKEN` present → resident lane wrapper, absent → interactive launcher.
  (verified: n/a (design); source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:613-618)
- 0.4.0 catalog aliases: peer `qwen-peer`, lane `qwen-peer-lane`. (verified: pin;
  source: docs/PRODUCTS.md:12)
- Lane wrapper (`qwen-peer` + token) owns one `qwen --acp` child and one ACP client;
  without a token `qwen-peer` mints a Qwen-compatible v4 UUID, passes it as both
  `--session-id` and `SESSIONBUS_SESSION_ID`, then execs interactive Qwen; the spawned
  stdio MCP server owns the peer connection. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1566)

## Lane process flags

- Current lane child argv: `qwen --acp`; `--yolo` appended only when bypass permission is
  requested. (verified: pin; source: internal/products/qwen/lane.go:102-104)
- Picture lane argv: `qwen --acp`; supported open fields `cwd`, `permission_mode`,
  `model`, `arguments`; model maps to `-m`; default permission uses Qwen's ordinary mode;
  bypass adds `--yolo` and verifies the returned mode. (verified: n/a (design);
  source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1567)
- Product flags (installed 0.23.0): `--acp` (qwen-0.23.0-help.txt:45), `--session-id`
  (:69), `-m/--model` (:34), `--approval-mode` choices plan|default|auto-edit|auto|yolo
  (:44), `-y/--yolo` (:43), `-r/--resume` (:68), `--chat-recording` (:33), `--json-file`
  (:64), `--input-file` (:66). (verified: 0.23.0;
  source: docs/products/qwen-0.23.0-help.txt:33-69)
- Reserved lane arguments (rejected): `--acp`, `--approval-mode`, `--yolo`, `-r`,
  `--resume`, `-c`, `--continue`, `--session-id`, `-p`, `--prompt`, `-i`,
  `--prompt-interactive`, `-o`, `--output-format`, `-n`, `--name`, `--`, including
  `name=value` forms. (verified: pin; source: internal/products/qwen/lane.go:394-405)
- Picture reserved controls: arguments may not claim `--acp`, approval/yolo,
  resume/continue/session-id, prompt/input/output, or name controls. (verified: n/a
  (design); source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1567)
- Lane driver capabilities: `{DurableResume: true, CallerSuppliedSessionID: true}`.
  (verified: pin; source: internal/products/qwen/lane.go:62-64)

## Peer process flags (current 0.4.0 launcher)

- Managed args inserted for every peer launch: `--chat-recording=true`,
  `--input-file <tmp>/input.jsonl`, `--json-file <tmp>/events.jsonl` in a fresh
  `sessionbus-qwen-` temp dir; fresh launches add `--session-id <uuid>`.
  (verified: pin; source: internal/launcher/qwen_peer.go:141-146,159-165)
- Managed-arg inspection rejects `--fork-session` ("not owner-attested"), `--session-id`,
  and `--continue`; `--resume` forwards to the native flag. (verified: pin;
  source: internal/launcher/qwen_peer.go:510-526)
- Fresh peer session id: launcher mints UUIDv4 locally (crypto/rand). (verified: pin;
  source: internal/launcher/qwen_peer.go:336 → internal/launcher/grok_peer.go:653-664)
- Startup name: launcher waits for the session to register under QWEN_HOME, then submits
  `/rename <name>` through the input file. (verified: pin;
  source: internal/launcher/qwen_peer.go:179-227,195-196)

## Environment

- Current peer env: `SESSIONBUS_SESSION_ID`, `SESSIONBUS_PRODUCT=qwen`,
  `SESSIONBUS_SESSION_NAME`, `SESSIONBUS_GROUPS` (JSON array string),
  `SESSIONBUS_QWEN_INPUT_FILE`, `SESSIONBUS_QWEN_EVENTS_FILE`. (verified: pin;
  source: internal/launcher/peer_context.go:14-19,63-82;
  internal/launcher/qwen_peer.go:20-21,148-151)
- Current lane MCP child env (inside the stdio mcpServers entry):
  `SESSIONBUS_HOST_BINARY`, `SESSIONBUS_PRODUCT`, `SESSIONBUS_SESSION_ID`.
  (verified: pin; source: internal/products/qwen/lane.go:373-392)
- `QWEN_HOME` resolution: `$QWEN_HOME` when set, else `$HOME/.qwen`; `QWEN_RUNTIME_DIR`
  is part of the profile identity and both are rewritten in the child env. (verified: pin;
  source: internal/qwenprofile/profile.go:30-58,74)
- Picture env contract (exact table): `SESSIONBUS_SESSION_ID` bare ID part (peer launcher),
  `SESSIONBUS_SESSION_NAME` bare name part (peer launcher), `SESSIONBUS_GROUPS` JSON array
  string (peer launcher), `SESSIONBUS_SOCKET` (both), `SESSIONBUS_LOCAL_KEY` optional (both),
  `SESSIONBUS_LAUNCH_TOKEN` (lane spawn only), `SESSIONBUS_LANE_SOCKET` (lane wrapper only).
  (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1502-1512)
- Picture exclusivity lock: `dirname(SESSIONBUS_SOCKET)/locks/<product>/<session_id>`
  opened `O_CREAT` + exclusive flock before resume (and before fresh creation when the
  wrapper mints the ID); fd inherited by the native child; stale files harmless;
  contention → `spawn_failed` "session busy". (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1492)

## Session id minting and resume

- Product rule: caller-supplied session ids (CLI `--session-id` and ACP
  `_meta["qwen-code/sessionId"]`) must match RFC UUID v1–v5, case-insensitive
  (`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`); v6/v7
  are rejected. (verified: 0.23.0; source: bundle:lib/chunks/chunk-ZEZKIS2K.js:14-25)
- ACP `session/new` rejects a non-conforming `_meta` id with invalidParams
  `_meta["qwen-code/sessionId"] must be an RFC UUID v1-v5`. (verified: 0.23.0;
  source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:17575-17580)
- Current wrapper passes its minted ID via `_meta["qwen-code/sessionId"]` on fresh and
  the requested exact UUID as `sessionId` on resume. Qwen 0.23.0 echoes `sessionId` on
  fresh open but omits it from a successful `session/resume` result; resume therefore
  keeps the requested ID when the field is absent and rejects a different ID when one
  is present. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-20260906T164729Z/cells/01b-open-effort-busy-resume/resume-load-frames.raw)
- Picture: wrapper mints one v4 UUID at fresh open, passes it via
  `_meta["qwen-code/sessionId"]`, verifies equality, and returns it as the lane's
  session id (no separate native id); resume = capability-checked `session/resume` with
  `resume_session_id`. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1567; UUIDv4 satisfies the v1–v5 rule above)
- Resume gate: `agentCapabilities.loadSession` must be true in the initialize result.
  (verified: pin + 0.23.0; source: internal/products/qwen/lane.go:136-141;
  bundle:lib/chunks/acpAgent-2GTFCIEP.js:17422; fixture acp.jsonl:1)
- Durability: chat recording defaults on (`chatRecording ?? true`); the ACP resume path
  leaves it undefined → recorded → resumable after process exit; `--chat-recording=false`
  disables persistence and breaks `--continue`/`--resume`. (verified: 0.23.0;
  source: bundle:lib/chunks/chunk-4F7GQGXB.js:160402;
  bundle:lib/chunks/acpAgent-2GTFCIEP.js:17827-17841;
  docs/products/qwen-0.23.0-help.txt:33)
- UNVERIFIED: the old catalog says native name/title lookup is cwd/project-scoped, so
  resume must use the original cwd. (unverified: old catalog; source:
  docs/PRODUCTS.md:34)
- Current peer identity observer accepts any UUID shape (no version check) — looser than
  the product's v1–v5 rule. (verified: pin; source: `ff81565:cmd/agent-sessions/qwen_peer.go:23`)

## Frame shapes (verbatim)

- ACP transport: newline-delimited JSON-RPC 2.0 over the child's stdio. (verified: pin;
  source: internal/products/qwen/client.go:84-135,193-198)
- initialize request (current code): (verified: pin; source: internal/products/qwen/lane.go:119-124)
```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}
```
- initialize result (fixture, captured 0.21.15): (verified: 0.21.15 (fixture);
  source: internal/bridge/testdata/qwen/acp.jsonl:1)
```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"qwen-code","title":"Qwen Code","version":"0.21.15"},"authMethods":[{"id":"openai","name":"Use OpenAI API key","description":"Requires setting the `OPENAI_API_KEY` environment variable","_meta":{"type":"terminal","args":["--auth-type=openai"]}}],"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true,"audio":true,"embeddedContext":true},"sessionCapabilities":{"list":{},"resume":{}},"mcpCapabilities":{"sse":true,"http":true},"_meta":{"imageCapability":{"autoHandlesWrongModel":true,"maxBytes":10380902,"maxImagesPerTurn":4}}}}}
```
- Driver requires `protocolVersion == 1` and `agentInfo.name == "qwen-code"`, else
  ErrProtocol. (verified: pin; source: internal/products/qwen/lane.go:128-132)
- session/new params (fresh): `{"cwd":<cwd>,"mcpServers":[<entry>],"_meta":{"qwen-code/sessionId":"<uuid>"}}`;
  resume: method `session/resume`, params `{"cwd":…,"mcpServers":[…],"sessionId":"<uuid>"}`
  (no `_meta`). (verified: pin; source: internal/products/qwen/lane.go:134-145)
- mcpServers stdio entry (current code) — `env` is a name-sorted list of `{name,value}`
  objects, not a map: (verified: pin; source: internal/products/qwen/lane.go:373-392)
```json
{"name":"sessionbus","command":"<host binary>","args":["connector","qwen"],"env":[{"name":"SESSIONBUS_HOST_BINARY","value":"<host binary>"},{"name":"SESSIONBUS_PRODUCT","value":"qwen"},{"name":"SESSIONBUS_SESSION_ID","value":"<lane id>"}]}
```
- Open result: `sessionId` required; fresh opens also read `modes.currentModeId` and
  require `"yolo"` when bypass was requested. (verified: pin;
  source: internal/products/qwen/lane.go:150-165)
- renameSession (fresh only): request `{"sessionId":"<id>","title":"<name>"}`; result must
  carry `"success":true`. (verified: pin; source: internal/products/qwen/lane.go:167-176;
  ext-method dispatch verified 0.23.0 at bundle:lib/chunks/acpAgent-2GTFCIEP.js:20211)
- session/prompt request: `{"sessionId":"<id>","prompt":[{"type":"text","text":"<input>"}]}`;
  empty prompt rejected client-side. (verified: pin;
  source: internal/products/qwen/lane.go:209-221)
- session/update notification (fixture): (verified: 0.21.15 (fixture);
  source: internal/bridge/testdata/qwen/acp.jsonl:2; driver accumulates only
  `agent_message_chunk` text, internal/products/qwen/lane.go:343-371)
```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"11111111-2222-4333-8444-555555555555","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"deterministic fixture response"}}}}
```
- current_mode_update notification (fixture): (verified: 0.21.15 (fixture);
  source: internal/bridge/testdata/qwen/acp.jsonl:3)
```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"11111111-2222-4333-8444-555555555555","update":{"sessionUpdate":"current_mode_update","currentModeId":"yolo"}}}
```
- Prompt result carries `stopReason`; driver maps `cancelled` (or an interrupted flag) →
  interrupted, every other value → completed, and records sha256 of accumulated output.
  (verified: pin; source: internal/products/qwen/lane.go:233-261,253-259)
- craft/cancelPendingPrompt request `{"sessionId":"<id>"}` → result `{"cancelled":<bool>}`;
  current driver treats `cancelled == false` as ErrNativeRejected. (verified: pin;
  source: internal/products/qwen/lane.go:267-286; product side verified 0.23.0:
  bundle:lib/chunks/chunk-ZGCKTAL5.js:81, bundle:lib/chunks/acpAgent-2GTFCIEP.js:20211,20259,5922)
- session/request_permission from the agent is auto-answered
  `{"outcome":{"outcome":"cancelled"}}` without consulting the caller (headless lane).
  (verified: pin; source: internal/products/qwen/client.go:153,173-174)
- ACP stdio wire table (0.23.0): initialize, authenticate, session/new, session/load,
  session/resume, session/prompt, session/cancel, session/fork, session/list,
  session/set_config_option, session/set_mode, session/set_model, session/update,
  session/request_permission. (verified: 0.23.0;
  source: bundle:lib/chunks/chunk-IW6RQPQB.js:19-33)
- Agent ext methods (0.23.0): renameSession, deleteSession, rewindSession,
  restoreSessionHistory, craft/cancelPendingPrompt, craft/drainMidTurnQueue,
  craft/claimTodoStopGuardContinuation, craft/todoStopGuardQueueReleased, plus qwen/*
  namespaces. (verified: 0.23.0; source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:20211;
  bundle:lib/chunks/chunk-ZGCKTAL5.js:77-81)
- ACP method probe: `session/list` returned `{"sessions":[]}`, `session/set_mode`
  returned `{}`, `session/set_model` returned `_meta.qwenModelSwitch`, and
  `session/set_config_option` returned the config-option inventory; `session/fork`
  returned JSON-RPC `-32601` with message `"Method not found": session/fork`. (verified:
  0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P1-methods/frames.jsonl)
- ACP `session/new` advertises config option `reasoning_effort`, with values `none`,
  `low`, `medium`, and `xhigh`; the captured current value and
  `_meta.qwenCode/reasoning.defaultEffort` were both `xhigh`. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P1-methods/frames.jsonl)
- Dual-output (`--json-file`) session_start event (fixture): (verified: 0.21.15 (fixture);
  source: internal/bridge/testdata/qwen/dual-output.jsonl:1)
```json
{"type":"system","subtype":"session_start","uuid":"00000000-0000-4000-8000-000000000001","session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null,"data":{"session_id":"11111111-2222-4333-8444-555555555555","cwd":"/workspace/qwen-fixture","protocol_version":2,"version":"0.21.15","supported_events":["system","user","assistant","stream_event","result","control_request","control_response"]}}
```
- Bridge admission: first event must be `system/session_start` and match expected
  session_id, cwd, version, protocol_version 2, and the full supported_events inventory;
  event-file reads are capped at 16 MiB. (verified: pin;
  source: internal/bridge/qwen.go:19,60,79-81,84-149)
- Input-file submit record: `{"type":"submit","text":"<body>"}` appended as one JSONL
  line; rename variant uses `"text":"/rename <name>"`. (verified: pin;
  source: internal/bridge/qwen.go:282-305; internal/launcher/qwen_peer.go:195-196)

## Delivery — lane, idle vs mid-turn

- Picture: idle delivery enters the shared bounded FIFO (64 deliveries / 1 MiB rendered)
  and is prepended to the next run; the host renderer emits the
  `[sessionbus-metadata: ...]` carrier line, preserves arrival order, joins with newlines;
  at run start the host atomically swaps the FIFO; overflow → `queue_full`. (verified:
  n/a (design); source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1496,1570)
- Picture: during a run, `craft/drainMidTurnQueue` hands queued entries to Qwen and an
  acknowledged drain reports `injected`; `injected` only when the product callback
  confirms an actually active native turn. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1570,1496)
- Picture: at terminal, every undrained entry — including aborts and runs with no tool
  round — moves back to the shared FIFO for the next run; no delivery is dropped between
  the native queue and the wrapper FIFO. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1570)
- A restored pending delivery survives only within the resident wrapper's lifetime;
  wrapper exit may lose its in-memory FIFO by the accepted loss boundary. (verified:
  n/a (design); source: docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1504;
  /home/antst/agentbus-evidence/qwen-20260906T164729Z/RUN-SUMMARY.md)
- Product drain mechanics (0.23.0): agent→client ext method `craft/drainMidTurnQueue`
  with `{sessionId}`; pulled at tool-run boundaries, stop-inspection, and stopped-run
  preservation; drained content becomes user parts inside the same running turn.
  (verified: 0.23.0; source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:8625,8364,7215,7300,8345)
- Drain response shapes: `{"messages":["<text>",…]}` or
  `{"items":[{"content":[<ContentBlock>],"displayText":…,"attachmentReferences":…}]}`;
  client-side answers may add `hasQueuedPrompt`. (verified: 0.23.0;
  source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:3697-3743;
  bundle:lib/chunks/chunk-U4DJ5XB3.js:1083-1085)
- Drain bounds (product-side): 2 s timeout per call; three consecutive failures disable
  draining for the turn; client JSON-RPC -32601 disables it permanently; a late response
  arriving within 30 s is recovered for a later batch. (verified: 0.23.0;
  source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:3500,3506,8668,3501,8697-8728)
- Run end after tool rounds: the product performs one final drain and appends drained
  user parts to the session chat history; aborted runs instead take only already-drained
  messages from the internal recovery buffer. (verified: 0.23.0;
  source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:8340-8358,8318-8338,8341-8344,8682-8688)
- Observed drain call sites (0.23.0): the agent pulls `craft/drainMidTurnQueue` at
  tool-run boundaries, stop-inspection, and stopped-run preservation paths. (verified:
  0.23.0; source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:7215,7300,8345,8364)
- A captured tool-less run emitted zero `craft/drainMidTurnQueue` requests before its
  `end_turn` terminal; the wrapper must recover every undrained active delivery to its
  FIFO at terminal. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P2-tool-less/frames.jsonl)
- When the client answered a captured drain with
  `{"messages":["QUEUE_MARK"],"hasQueuedPrompt":true}`, Qwen retained the message in
  the same running turn and later returned `end_turn`. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P5-has-queued-prompt/frames.jsonl)
- Current 0.4.0 lane driver has no mid-turn path: Steer returns ErrUnsupportedSteer.
  (verified: pin; source: internal/products/qwen/lane.go:263-264)

## Delivery — peer / interactive inbound

- Current: daemon-routed `message.deliver` for a qwen peer is served by the launcher's
  live call, which appends one submit record to the `--input-file`; success → `{}`.
  (verified: pin; source: `ff81565:cmd/agent-sessions/qwen_peer.go:126-146`)
- Product: `--input-file` = "File path for receiving remote input commands (bidirectional
  sync). An external process writes JSONL commands; the TUI watches and processes them."
  (verified: 0.23.0; source: docs/products/qwen-0.23.0-help.txt:66)
- An exact input-file record `{"type":"submit","text":"INPUT_MARK"}` appended after
  the active turn's initial user event was observed by Qwen inside that same turn, with no
  intervening prompt boundary; its monotonic append timestamp was 19089588312077108 after
  the initial user event at 19089588311695131. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P4-active-input-file-rerun/{harness.jsonl,events.jsonl})
- The captured TUI dual-output stream contained `system`, `user`, `stream_event`, and
  `assistant` events plus `system/session_end`, but no `type:"result"` event; `/exit`
  then returned process status 0. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P4-active-input-file-rerun/{harness.jsonl,events.jsonl})
- Picture: a peer integration may let the interactive product start a turn in response to
  delivery and report `injected`; the FIFO rule is lane-only. (verified: n/a (design);
  source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1497)
- Title observation (current): product title read from the transcript JSONL
  `system/custom_title` events; fallback title = session id; 1 Hz projection re-reports
  changes. (verified: pin; source: internal/bridge/qwen_title.go:41-84,87-128,125-126;
  `ff81565:cmd/agent-sessions/qwen_peer.go:99-124`)
- Transcript location: exactly one regular non-symlink
  `QWEN_HOME/projects/<project-dir>/chats/<session-id>.jsonl`; ambiguity or absence → no
  title fact. (verified: pin; source: internal/bridge/qwen_title.go:11-37)
- Picture rename: no wire rename method; a live peer re-hellos with the same
  `session_id`, updating name/info in place; `groups` must equal the original slice
  exactly or the daemon returns invalid_hello and closes. (verified: n/a (design);
  source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:120-127,1488)
- Hand-started qwen without launcher identity: the MCP entry never hellos; every Sessionbus tool call returns an error naming the required launcher. (verified: n/a
  (design); source: picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1488)
- In the sealed zero-turn peer cell, Qwen 0.23.0 spawned the exact `qwen-peer mcp`
  helper with session id, title, groups, bus socket, and input-file environment before
  any prompt. The pre-fix helper completed MCP initialize and tools/list but published
  no peer because it deferred `ConnectPeer` until tools/call. (verified: 0.23.0 + host
  finding; source:
  /home/antst/agentbus-evidence/qwen-peer-20260906T172159Z/{RUN-SUMMARY.md,cells/02-peer-tui/mcp-agentbus-env.txt,cells/02-peer-tui/roster.json})
- The corrected root helper creates one resident Peer and Caller before serving MCP;
  initialize and tools/list require no action, and Prepare gates later actions on that
  same connection. Missing launcher identity still serves MCP discovery and reports the
  identity error on tools/call. (verified: implementation + 0.23.0 runtime; source:
  wrappers/qwen/peer.go:32-118; cmd/qwen-peer/main.go:58-75;
  cmd/qwen-peer/main_test.go;
  /home/antst/agentbus-evidence/qwen-peer-rerun-20260906T173801Z/RUN-SUMMARY.md)
- The zero-turn rerun observed the idle Qwen peer before any prompt or tools/call with
  its minted UUID, exact title `Qwen Peer Runtime`, requested `qwen-peer-cells` group,
  `connected:true`, `running:false`, and cwd `/home/antst`; `/exit` then reaped the TUI
  and MCP helper, and final teardown left no owned process or socket. (verified: 0.23.0;
  source:
  /home/antst/agentbus-evidence/qwen-peer-rerun-20260906T173801Z/{RUN-SUMMARY.md,cells/01-peer/roster.json,cells/01-peer/processes-relevant-final.txt})

## Interrupt and close

- Interrupt (current and picture): `craft/cancelPendingPrompt`. Accepted ruling (S6,
  2026-09-05): the wrapper returns `{}` even when `cancelled == false` — the stop request
  was delivered and the terminal outcome is authoritative; this relaxes the current
  ErrNativeRejected. (verified: pin; source: internal/products/qwen/lane.go:267-286;
  picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1571)
- Picture close: cancel the ACP lifetime and reap the child; the chat-recorded session
  stays on disk, so the row remains resumable. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1571 + durability fact above)
- Current archive: context cancel plus bounded process cleanup (structuredprocess
  supervisor TERM/KILL). (verified: pin; source: internal/products/qwen/lane.go:288-305;
  internal/products/qwen/process.go:11-22)
- Standard `session/cancel` exists in the stdio wire table; the integration does not use
  it (uses craft/cancelPendingPrompt). (verified: 0.23.0;
  source: bundle:lib/chunks/chunk-IW6RQPQB.js:19-33; internal/products/qwen/lane.go:272)
- In plan mode, after the client answered `session/request_permission` with
  `{"outcome":{"outcome":"cancelled"}}`, Qwen ended the prompt with exact
  `stopReason:"end_turn"`; no `refused` terminal was emitted. (verified: 0.23.0;
  source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P6-refused/frames.jsonl)
- Picture handoff boundary: FIFO extraction, native-turn creation, and interrupt are
  serialized under one boundary — interrupt before native creation aborts creation and
  returns terminal `interrupted` without a native call; after creation it calls native
  cancel. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1496)

## Unsupported and exceptions

- `reasoning_effort` has no CLI flag, but ACP advertises and applies it through
  `session/set_config_option`; the wrapper supports it by verifying the returned
  `currentValue`. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P1-methods/frames.jsonl)
- Approval policy (0.23.0): `yolo` acts; `auto` and the omitted-mode default block tool
  actions triggered solely by a cross-session message; `auto-edit` and `default` prompt
  the human in the TUI; `plan` rejects the action. A lane must use
  `permission_mode=bypass` (`--yolo`) to act on a steer. (verified: 0.23.0 via cited
  product docs/code; source: docs/PRODUCTS.md:66-77, citing bundle
  lib/bundled/qc-helper/docs/features/approval-mode.md:3-31,
  lib/bundled/qc-helper/docs/configuration/settings.md:374 (default `auto`),
  lib/chunks/chunk-4F7GQGXB.js:50826-50840)
- Section 1 code exceptions: 0. Wrapper-only state: the bounded idle and recovery FIFO;
  native mid-turn drain state remains Qwen-owned. (verified: n/a (design); source:
  picture docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1572)
- Wrapper size cap: 740 production / 600 test logical lines including the lane and peer
  end state, excluding the shared host. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1573; accepted per-block review,
  2026-09-06)
- Deletion inventory at migration: internal/products/qwen (4 files / 1,031 lines),
  internal/launcher qwen_peer files (3 / 1,412), `ff81565:cmd/agent-sessions/qwen_peer.go`
  and `ff81565:cmd/agent-sessions/qwen_peer_test.go`
  (2 / 234), qwen/scripts/native-entry (11): 10 files / 2,688 lines. (verified: pin —
  counts re-verified in tree; source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1574)

## MCP transport support

- Product transports for ACP `mcpServers` (session/new and load/resume): stdio
  (command/args/env), http (requires `httpUrl`, supports `headers`), sse. (verified:
  0.23.0; source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:15416-15472,17573,17829-17833)
- initialize result advertises `mcpCapabilities {"sse":true,"http":true}`. (verified:
  0.21.15 (fixture); source: internal/bridge/testdata/qwen/acp.jsonl:1)
- Current lane ingress: stdio MCP entry injected at session/new (frame above); current
  peer ingress: `qwen/mcp.json` stdio server `sessionbus` → `./scripts/native-entry`
  → `${SESSIONBUS_HOST_BINARY:-$HOME/.local/bin/sessionbus} connector qwen
  --release-identity @SESSIONBUS_RELEASE_ID@`. (verified: pin;
  source: qwen/mcp.json:1-15; qwen/scripts/native-entry:6)
- Picture tool ingress: lane mode — ACP `mcpServers` starts `qwen-peer mcp` (stdio helper)
  against the wrapper's private Unix socket
  `dirname(SESSIONBUS_SOCKET)/lanes/<session_id>.sock` passed as `SESSIONBUS_LANE_SOCKET` and
  unlinked on exit; peer mode — the same stdio MCP entry owns the direct daemon peer
  connection. (verified: n/a (design); source: picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1493,1569)
- URL transports (http/sse) exist in the product but the design uses none: lane tool
  ingress is the stdio helper `qwen-peer mcp` against the wrapper's private per-session
  Unix socket. (verified: 0.23.0 for product support;
  source: bundle:lib/chunks/acpAgent-2GTFCIEP.js:15416-15472; picture
  docs/designs/UNIVERSAL-SESSION-PROTOCOL.md:1493,1569; ruling (fable-architect,
  2026-09-05): stdio helper over the per-session socket; loopback HTTP not used)
- Qwen 0.23.0 accepted and spawned both bare and absolute stdio MCP commands, and both
  exchanged initialize and discovery frames. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P0-http/{stdio-bare.jsonl,stdio-absolute.jsonl})
- Qwen 0.23.0 connected to an ACP HTTP MCP entry and sent initialize, initialized,
  prompts/list, resources/list, and tools/list; the probe server returned invalid empty
  list shapes for prompts/resources, Qwen emitted exact warning `Warning: MCP server(s)
  failed to start: http_probe. Continuing with built-in tools and any servers that did
  connect.`, and HTTP tools/call therefore remains unverified. (verified: 0.23.0;
  source: /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P0-http/{http.jsonl,http-server.err})
- Teardown of that failed HTTP probe ended its active ACP prompt with exact
  `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`. (verified:
  0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P0-http/frames.jsonl)

## Peer identity resolution (current connector)

- Identity: `SESSIONBUS_SESSION_ID` env, with qwen events-file first-line fallback;
  name from `SESSIONBUS_SESSION_NAME`; groups parsed from `SESSIONBUS_GROUPS`
  JSON array. (verified: pin; source: `ff81565:cmd/agent-sessions/connector.go:420`)
- Qwen is not a live-presence owner in the connector (claude/grok only): the launcher
  process reports qwen peer presence via livepresence. (verified: pin;
  source: `ff81565:cmd/agent-sessions/connector.go:304-306`; `ff81565:cmd/agent-sessions/qwen_peer.go:41-56`)
- Identity admission (current): first events-file line must be `system/session_start`
  with a UUID-shaped session_id; 45 s deadline. (verified: pin;
  source: `ff81565:cmd/agent-sessions/qwen_peer.go:21-23`)

## qwen serve surface (unused by Sessionbus)

- `qwen serve` HTTP bridge v1 (fixture): features include session_create,
  session_id_override, session_load, session_resume, unstable_session_resume,
  session_list, session_prompt; contract versions dual_output=2, acp=1, serve=1 for
  @qwen-code/qwen-code v0.21.15 commit 5dce2515. (verified: 0.21.15 (fixture);
  source: internal/bridge/testdata/qwen/serve.json; internal/bridge/testdata/qwen/version.json)
- `session/close`, `session/set_mode`, `session/set_model` handlers log "qwen serve:
  /acp …" — serve-surface; unused by the integration. (verified: 0.23.0;
  source: bundle:lib/chunks/server-AOYZVVOM.js:13344,13798,13839)

## Version-specific quirks

- Version spread: fixtures captured at 0.21.15; installed/verified bundle 0.23.0; catalog
  pin minimum 0.22.0, validated 0.22.3/0.23.0. (verified: pin;
  source: internal/bridge/testdata/qwen/version.json; docs/PRODUCTS.md:12)
- 0.23.0 surfaces the picture relies on: craft/* ext namespace (drainMidTurnQueue,
  cancelPendingPrompt, todo-stop-guard methods), `session/resume` in the stdio table,
  `_meta["qwen-code/sessionId"]`, the UUID v1–v5 caller-id rule, `loadSession`
  capability. (verified: 0.23.0; source: bundle citations above)
- 0.23.0 default approval mode is `auto` (per cited product settings doc). (verified:
  0.23.0 via citation; source: docs/PRODUCTS.md:73 → bundle
  lib/bundled/qc-helper/docs/configuration/settings.md:374)
- Installed provenance is `@qwen-code/qwen-code` 0.23.0; no 0.22.3 package was locally
  available under the existing npm/cache trees, so the installed binary was not changed
  for comparison. (verified: 0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P3-provenance/{installed.txt,qwen-0.22.3-local-packages.txt})
- Every probe-session orphan check and the final process-tree check found no Qwen,
  wrapper, or probe MCP process; the final relevant-process file is empty. (verified:
  0.23.0; source:
  /home/antst/agentbus-evidence/qwen-probes-20260906T150057Z/P7-final-processes-relevant.txt)

## UNVERIFIED

- UNVERIFIED: Qwen 0.23.0 HTTP MCP tool invocation; the transport handshake succeeded,
  but the probe server returned invalid list-result shapes before a tool could be called.
- UNVERIFIED: whether `--input-file`, `--json-file`, `--chat-recording` exist in upstream
  qwen-code releases (present in the committed 0.23.0 fixture at
  docs/products/qwen-0.23.0-help.txt:33,64,66; upstream parity not checked).
- UNVERIFIED: 0.22.x behavior (catalog pin claims validation on 0.22.3; this file
  directly verifies 0.23.0 only; fixtures are 0.21.15).
- UNVERIFIED: any Qwen version emitting terminal stopReason `refused`; 0.23.0 emitted
  `end_turn` after a cancelled permission response.
