# Codex product facts

- `codex app-server --stdio` accepts newline-delimited JSON-RPC requests and emits response objects without a `jsonrpc` member. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- An App Server client initializes with `initialize`, then sends the `initialized` notification. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `thread/start` with `ephemeral:false`, `serviceName:"codex-peer"`, and `historyMode:"legacy"` returns the product-owned thread UUID. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `thread/start` reports the effective model, cwd, approval policy, sandbox, reasoning effort, and thread record at the top level. — verified: 0.153.2 — source: `/home/antst/agentbus-evidence/codex-20260906T071535Z/P0-appserver.jsonl`
- A newly created and named zero-turn legacy thread is materialized by `thread/resume` with `excludeTurns:true`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `thread/resume` reports the effective model, cwd, approval policy, sandbox, reasoning effort, and thread record at the top level. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `turn/start` returns a turn ID before the matching `turn/started` notification. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `turn/started` carries `threadId` and a turn object whose running status is `inProgress`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- `turn/steer` accepts `threadId`, `expectedTurnId`, and input, then returns the same `turnId`; the steered text affects the final answer. — verified: 0.153.2 — source: `/home/antst/agentbus-evidence/codex-p1-20260906T071721Z/P1-appserver.jsonl`
- `turn/interrupt` before `turn/started` fails with code `-32600` and message `no active turn to interrupt`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074006Z/P3-appserver.jsonl`
- `turn/interrupt` after `turn/started` returns `{}`, then `turn/completed` reports status `interrupted`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`
- A completed `turn/completed` notification carries summary items, timestamps, nullable error, and an `agentMessage` item with phase `final_answer`. — verified: 0.153.2 — source: `/home/antst/agentbus-evidence/codex-20260906T071535Z/P0-appserver.jsonl`
- Per-thread `config.mcp_servers.<name>.url` starts a streamable HTTP MCP client that sends initialize, initialized, tools/list, and tools/call without a session header. — verified: 0.153.2 — source: `/home/antst/agentbus-evidence/codex-20260906T071535Z/P0-http.jsonl`
- MCP `tools/call` metadata includes `_meta.threadId` and `x-codex-turn-metadata.thread_id`, both equal to the native thread UUID. — verified: 0.153.2 — source: `/home/antst/agentbus-evidence/codex-20260906T071535Z/P0-http.jsonl`
- The 0.4.0 connector takes Codex identity from MCP `_meta.threadId`, never from tool arguments. — verified: 0.4.0 — source: `cmd/agent-sessions/connector.go:247`
- The 0.4.0 connector delivers to an active interactive thread with `turn/steer` and wakes an idle thread with `turn/start`. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:479`
- The 0.4.0 launcher starts `codex app-server daemon` and runs the TUI through `--remote unix://<control-socket>`. — verified: 0.4.0 — source: `internal/launcher/codex_peer.go:129`
- Informational and non-TUI subcommand invocations pass through with their original argv and environment; `resume` and `fork` are coordinated TUI selectors and receive the wrapper-owned `--remote` address. — verified: 0.4.0 — source: `internal/launcher/codex_peer.go:122`
- The Codex interactive option table records value-taking `--remote`, `--remote-auth-token-env`, `-i`/`--image`, `--local-provider`, and `--add-dir`; a wrapper must preserve their value arity when projecting its own flags. — verified: 0.4.0 — source: `internal/launcher/options.go:18`
- The 0.4.0 lane validates effective approval after `thread/start`, validates effective cwd and approval after `thread/resume`, and applies approval and sandbox through `thread/settings/update`. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:224`
- The 0.4.0 lane disables `features.code_mode_host` so headless tool calls stay in the App Server. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:713`
- The 0.4.0 App Server bridge handles `mcpServer/elicitation/request` and `item/tool/call`; it rejects other headless server requests with `-32601`. — verified: 0.4.0 — source: `internal/bridge/dynamic_tools.go:26`
- The 0.4.0 observer recognizes `thread/started`, `turn/started`, `turn/completed`, `thread/status/changed`, and `thread/name/updated`. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:904`
- The 0.4.0 resume path avoids a second `thread/resume` while its exact App Server client already owns the loaded thread. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:355`
- The 0.4.0 recovery path can unarchive a thread, restore loaded subscriptions after reconnect, and resolve a durable active turn with `thread/turns/list`. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:409`
- The 0.4.0 close path archives the exact thread and unsubscribes so its stdio MCP child is released. — verified: 0.4.0 — source: `internal/bridge/codex_native.go:759`
- A turn started with unsupported model `agentbus-invalid-model` closes with status `failed`; its error object carries `message`, `codexErrorInfo`, `additionalDetails`, and `misalignment`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-failed-turn-20260906T085803Z/appserver.jsonl`
- For an interactive TUI connected through `--remote`, command-line `mcp_servers.agent_sessions.command` and `.args` do not override an installed same-name plugin MCP entry: Codex spawned the installed `agent-sessions connector auto` and did not spawn the configured `codex-peer mcp`. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-cells-20260906T094434Z/peer/C1-failure-processes.txt`
- In remote mode the App Server daemon spawns MCP servers from the configuration it loaded; changing the installed `mcp_servers.agent_sessions` entry requires restarting that daemon before an interactive TUI can use the replacement. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-cells-20260906T094434Z/peer/C1-installed-mcp-configs.txt`
- With the global entry set to `command = "codex-peer"`, `args = ["mcp"]`, and the App Server daemon restarted, Codex started `agent_sessions` but its MCP client timed out after 30 seconds and published no peer presence. — verified: 0.153.4 — source: `/home/antst/agentbus-evidence/codex-cells-20260906T094434Z/peer-rerun/C1-pane-final.txt`
- `UNVERIFIED:` An active `thread/read` response and the one-row `thread/turns/list` response used by interactive peer delivery have not yet been captured on 0.153.x.

## Exact captured frames typed by the wrapper

Each line below is copied byte-for-byte from the `raw` field in the named evidence file.

- Initialize response — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":1,"result":{"userAgent":"agentbus-probe/0.153.4 (Ubuntu 24.4.0; x86_64) unknown (agentbus-probe; 1)","codexHome":"/home/antst/.codex","platformFamily":"unix","platformOs":"linux"}}
```

- Native error response — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074006Z/P3-appserver.jsonl`

```json
{"error":{"code":-32600,"message":"no active turn to interrupt"},"id":6}
```

- `thread/start` result, including the effective cwd, approval, and sandbox — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":2,"result":{"thread":{"id":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","extra":null,"sessionId":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","forkedFromId":null,"parentThreadId":null,"preview":"","ephemeral":false,"section":null,"sectionEnteredAt":null,"projectId":null,"historyMode":"legacy","modelProvider":"openai","model":"gpt-6-astra","reasoningEffort":null,"createdAt":1788680429,"updatedAt":1788680429,"recencyAt":1788680429,"status":{"type":"idle"},"path":"/home/antst/.codex/sessions/2026/09/06/rollout-2026-09-06T07-40-29-01a075a9-5eb8-7540-95ef-c8d3ffb8a752.jsonl","cwd":"/home/antst/agentbus-evidence/codex-p3-20260906T074029Z","cliVersion":"0.153.4","source":"vscode","canAcceptDirectInput":true,"threadSource":null,"agentNickname":null,"agentRole":null,"gitInfo":null,"name":null,"turns":[]},"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"cwd":"/home/antst/agentbus-evidence/codex-p3-20260906T074029Z","runtimeWorkspaceRoots":["/home/antst/agentbus-evidence/codex-p3-20260906T074029Z"],"instructionSources":[],"approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"activePermissionProfile":{"id":":read-only","extends":null},"reasoningEffort":null,"multiAgentMode":"explicitRequestOnly"}}
```

- `thread/resume` result — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":4,"result":{"thread":{"id":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","extra":null,"sessionId":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","forkedFromId":null,"parentThreadId":null,"preview":"","ephemeral":false,"section":null,"sectionEnteredAt":null,"projectId":null,"historyMode":"legacy","modelProvider":"openai","model":"gpt-6-astra","reasoningEffort":null,"createdAt":1788680429,"updatedAt":1788680430,"recencyAt":1788680429,"status":{"type":"idle"},"path":"/home/antst/.codex/sessions/2026/09/06/rollout-2026-09-06T07-40-29-01a075a9-5eb8-7540-95ef-c8d3ffb8a752.jsonl","cwd":"/home/antst/agentbus-evidence/codex-p3-20260906T074029Z","cliVersion":"0.153.4","source":"vscode","canAcceptDirectInput":true,"threadSource":null,"agentNickname":null,"agentRole":null,"gitInfo":null,"name":"codex-p3","turns":[]},"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"cwd":"/home/antst/agentbus-evidence/codex-p3-20260906T074029Z","runtimeWorkspaceRoots":["/home/antst/agentbus-evidence/codex-p3-20260906T074029Z"],"instructionSources":[],"approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"activePermissionProfile":{"id":":read-only","extends":null},"reasoningEffort":null,"multiAgentMode":"explicitRequestOnly","initialTurnsPage":null,"turnsBackwardsCursor":null,"itemsBackwardsCursor":null}}
```

- `turn/start` result — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":5,"result":{"turn":{"id":"01a075a9-638a-77f2-9722-3bc09dce6494","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}
```

- `turn/started` notification — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"method":"turn/started","params":{"threadId":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","turn":{"id":"01a075a9-638a-77f2-9722-3bc09dce6494","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":1788680430,"completedAt":null,"durationMs":null}},"emittedAtMs":1788680430493}
```

- `turn/steer` result — source: `/home/antst/agentbus-evidence/codex-p1-20260906T071721Z/P1-appserver.jsonl`

```json
{"id":4,"result":{"turnId":"01a07594-3310-78a0-a9a5-8afe4689eea5"}}
```

- `turn/interrupt` result — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":6,"result":{}}
```

- Completed `turn/completed` notification with a final-answer item — source: `/home/antst/agentbus-evidence/codex-p1-20260906T071721Z/P1-appserver.jsonl`

```json
{"method":"turn/completed","params":{"threadId":"01a07594-327d-7be1-ba09-0047b18e8770","turn":{"id":"01a07594-3310-78a0-a9a5-8afe4689eea5","items":[{"type":"agentMessage","id":"msg_09664ff8842ada9c016a9d13a9630c87d281743b358bd23e90","text":"STEER_PROBE_OK","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}],"itemsView":"summary","status":"completed","error":null,"startedAt":1788679041,"completedAt":1788679081,"durationMs":39942}},"emittedAtMs":1788679081756}
```

- Interrupted `turn/completed` notification — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"method":"turn/completed","params":{"threadId":"01a075a9-5eb8-7540-95ef-c8d3ffb8a752","turn":{"id":"01a075a9-638a-77f2-9722-3bc09dce6494","items":[],"itemsView":"notLoaded","status":"interrupted","error":null,"startedAt":1788680430,"completedAt":1788680430,"durationMs":22}},"emittedAtMs":1788680430509}
```

- Failed `turn/completed` notification — source: `/home/antst/agentbus-evidence/codex-failed-turn-20260906T085803Z/appserver.jsonl`

```json
{"method":"turn/completed","params":{"threadId":"01a075f0-66ba-7b52-87a7-5dca3de53066","turn":{"id":"01a075f0-6737-7b02-ae86-c9be43e17c19","items":[],"itemsView":"notLoaded","status":"failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'agentbus-invalid-model' model is not supported when using Codex with a ChatGPT account.\"}}","codexErrorInfo":"other","additionalDetails":null,"misalignment":null},"startedAt":1788685084,"completedAt":1788685085,"durationMs":1261}},"emittedAtMs":1788685085742}
```

- `thread/read` result with the captured `notLoaded` status — source: `/home/antst/agentbus-evidence/codex-failed-turn-20260906T085803Z/read-frames.jsonl`

```json
{"id":2,"result":{"thread":{"id":"01a075f0-66ba-7b52-87a7-5dca3de53066","extra":null,"sessionId":"01a075f0-66ba-7b52-87a7-5dca3de53066","forkedFromId":null,"parentThreadId":null,"preview":"Reply exactly NEVER.","ephemeral":false,"section":null,"sectionEnteredAt":null,"projectId":null,"historyMode":"legacy","modelProvider":"openai","model":"agentbus-invalid-model","reasoningEffort":null,"createdAt":1788685084,"updatedAt":1788685084,"recencyAt":1788685084,"status":{"type":"notLoaded"},"path":"/home/antst/.codex/archived_sessions/rollout-2026-09-06T08-58-04-01a075f0-66ba-7b52-87a7-5dca3de53066.jsonl","cwd":"/home/antst/agentbus-evidence/codex-failed-turn-20260906T085803Z","cliVersion":"0.153.4","source":"vscode","canAcceptDirectInput":null,"threadSource":null,"agentNickname":null,"agentRole":null,"gitInfo":null,"name":null,"turns":[]}}}
```

- Authoritative full `thread/turns/list` result — source: `/home/antst/agentbus-evidence/codex-failed-turn-20260906T085803Z/read-frames.jsonl`

```json
{"id":3,"result":{"data":[{"id":"01a075f0-6737-7b02-ae86-c9be43e17c19","items":[{"type":"userMessage","id":"item-1","clientId":null,"content":[{"type":"text","text":"Reply exactly NEVER.","text_elements":[]}]}],"itemsView":"full","status":"failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'agentbus-invalid-model' model is not supported when using Codex with a ChatGPT account.\"}}","codexErrorInfo":"other","additionalDetails":null,"misalignment":null},"startedAt":1788685084,"completedAt":1788685085,"durationMs":1261}],"nextCursor":null,"backwardsCursor":"{\"turnId\":\"01a075f0-6737-7b02-ae86-c9be43e17c19\",\"includeAnchor\":true}"}}
```

- `thread/unsubscribe` result; empty mutation results use the captured `turn/interrupt` shape above — source: `/home/antst/agentbus-evidence/codex-p3-20260906T074029Z/P3-appserver.jsonl`

```json
{"id":8,"result":{"status":"notLoaded"}}
```

## Wrapper differences from the 0.4.0 source

- `CONTRADICTION:` The failed peer probe assumed TUI `-c` values would override the remote daemon's MCP configuration. Codex 0.153.4 instead used the entry already loaded by that daemon; the installer now owns the global `agent_sessions` entry through `codex mcp` and restarts the daemon only when it rewrites the entry. — source: `/home/antst/agentbus-evidence/codex-cells-20260906T094434Z/peer/C1-installed-mcp-configs.txt`
- `CONTRADICTION:` The current peer launcher starts the App Server daemon and injects `--remote` only for interactive TUI launches; informational and native subcommand invocations pass through unchanged. The 0.4.0 launcher used the same branch split but also carried pending-launch coordination that the universal peer removes. — source: `internal/launcher/codex_peer.go:122`
- `CONTRADICTION:` The current lane passes its approval, sandbox, model, and private MCP config through thread requests; the 0.4.0 bridge also issued `thread/settings/update` after resume. — source: `internal/bridge/native_support.go:84`
- `CONTRADICTION:` The current App Server client rejects every server request with `-32601`; the 0.4.0 bridge handled Agent Sessions dynamic tool calls and elicitation inside the App Server connection. The universal wrapper instead supplies tools through the private stdio MCP child. — source: `internal/bridge/dynamic_tools.go:26`
- `CONTRADICTION:` The current notification reader consumes only exact `turn/started` and `turn/completed` frames. The 0.4.0 observer also projected thread start, status, and name events for host-global coordination. — source: `internal/bridge/codex_native.go:904`
- `CONTRADICTION:` The current wrapper owns one App Server child and exits on its death. The 0.4.0 host-global bridge reconnected, restored loaded subscriptions, and could unarchive recovery targets. — source: `internal/bridge/codex_native.go:409`
