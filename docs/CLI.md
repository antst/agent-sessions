# Agent Sessions command reference

Generated from `internal/clihelp`; edit the descriptor, not this table.

## Binaries and modes

| Key | Invocation | Visibility | Online | Summary |
|---|---|---|---:|---|
| `connector.claude.mcp` | `agent-sessions connector claude mcp` | connector-internal | true | run the stateless Claude Code connector relay |
| `connector.codex.mcp` | `agent-sessions connector codex mcp` | connector-internal | true | run the stateless Codex connector relay |
| `connector.grok.mcp` | `agent-sessions connector grok mcp` | connector-internal | true | run the stateless Grok connector relay |
| `connector.qwen.mcp` | `agent-sessions connector qwen mcp` | connector-internal | true | run the stateless Qwen Code connector relay |
| `host.connector.install` | `agent-sessions connector install --product PRODUCT --source-root ROOT` | public | false | install one explicit native product connector transaction |
| `host.connector.remove` | `agent-sessions connector remove --product PRODUCT` | public | false | remove one explicit native product connector through its supported installer |
| `host.daemon` | `agent-sessions daemon` | service-internal | false | run the foreground user service |
| `host.doctor` | `agent-sessions doctor` | public | true | diagnose host readiness without starting it |
| `host.help` | `agent-sessions help [MODE]` | public | false | render the canonical command contract |
| `host.install` | `agent-sessions lifecycle install --role host --source-root ROOT --prefix PREFIX --version VERSION` | service-internal | false | install or upgrade only the unified host role transaction |
| `host.lane` | `agent-sessions lane --host HOST --product PRODUCT -- COMMAND [ARGS...]` | public | true | run one lane operation through the connected host daemon |
| `host.migrate.inspect` | `agent-sessions migrate inspect` | public | true | inspect exact legacy candidates and blockers |
| `host.migrate.status` | `agent-sessions migrate status` | public | true | show the migration journal and retry action |
| `host.purge.apply` | `agent-sessions purge apply` | public | false | apply an exact host purge plan |
| `host.purge.inspect` | `agent-sessions purge inspect` | public | false | create or inspect a revision-bound host purge plan |
| `host.remove.apply` | `agent-sessions lifecycle remove --role host --prefix PREFIX` | service-internal | false | remove only the quiescent unified host role transaction |
| `host.remove.inspect` | `agent-sessions remove inspect` | public | true | inspect host removal blockers and targets |
| `host.status` | `agent-sessions status` | public | true | show metadata-only host status |
| `hub.doctor` | `agent-sessions-hub doctor` | hub-only | true | diagnose hub readiness without starting it |
| `hub.install` | `agent-sessions-hub lifecycle install --role hub --source-root ROOT --prefix PREFIX --version VERSION` | service-internal | false | install or upgrade only the central hub role transaction |
| `hub.purge.apply` | `agent-sessions-hub purge apply` | hub-only | false | apply an exact hub purge plan |
| `hub.purge.inspect` | `agent-sessions-hub purge inspect` | hub-only | false | create or inspect a revision-bound hub purge plan |
| `hub.remove.apply` | `agent-sessions-hub lifecycle remove --role hub --prefix PREFIX` | service-internal | false | remove only the central hub role transaction |
| `hub.remove.inspect` | `agent-sessions-hub remove inspect` | hub-only | true | inspect hub removal blockers and targets |
| `hub.serve` | `agent-sessions-hub` | hub-only | false | run the central federation hub |
| `hub.status` | `agent-sessions-hub status` | hub-only | true | show metadata-only hub status |
| `lane.claude.archive` | `claude-peer-lane archive` | public | true | archive a Claude Code lane |
| `lane.claude.doctor` | `claude-peer-lane doctor` | public | true | doctor a Claude Code lane |
| `lane.claude.interrupt` | `claude-peer-lane interrupt` | public | true | interrupt a Claude Code lane |
| `lane.claude.list` | `claude-peer-lane list` | public | true | list a Claude Code lane |
| `lane.claude.resume` | `claude-peer-lane resume` | public | true | resume a Claude Code lane |
| `lane.claude.run` | `claude-peer-lane run` | public | true | run a Claude Code lane |
| `lane.claude.start` | `claude-peer-lane start` | public | true | start a Claude Code lane |
| `lane.claude.status` | `claude-peer-lane status` | public | true | status a Claude Code lane |
| `lane.claude.wait` | `claude-peer-lane wait` | public | true | wait a Claude Code lane |
| `lane.codex.archive` | `codex-peer-lane archive` | public | true | archive a Codex lane |
| `lane.codex.doctor` | `codex-peer-lane doctor` | public | true | doctor a Codex lane |
| `lane.codex.interrupt` | `codex-peer-lane interrupt` | public | true | interrupt a Codex lane |
| `lane.codex.list` | `codex-peer-lane list` | public | true | list a Codex lane |
| `lane.codex.resume` | `codex-peer-lane resume` | public | true | resume a Codex lane |
| `lane.codex.run` | `codex-peer-lane run` | public | true | run a Codex lane |
| `lane.codex.start` | `codex-peer-lane start` | public | true | start a Codex lane |
| `lane.codex.status` | `codex-peer-lane status` | public | true | status a Codex lane |
| `lane.codex.wait` | `codex-peer-lane wait` | public | true | wait a Codex lane |
| `lane.grok.archive` | `grok-peer-lane archive` | public | true | archive a Grok lane |
| `lane.grok.doctor` | `grok-peer-lane doctor` | public | true | doctor a Grok lane |
| `lane.grok.interrupt` | `grok-peer-lane interrupt` | public | true | interrupt a Grok lane |
| `lane.grok.list` | `grok-peer-lane list` | public | true | list a Grok lane |
| `lane.grok.resume` | `grok-peer-lane resume` | public | true | resume a Grok lane |
| `lane.grok.run` | `grok-peer-lane run` | public | true | run a Grok lane |
| `lane.grok.start` | `grok-peer-lane start` | public | true | start a Grok lane |
| `lane.grok.status` | `grok-peer-lane status` | public | true | status a Grok lane |
| `lane.grok.wait` | `grok-peer-lane wait` | public | true | wait a Grok lane |
| `lane.qwen.archive` | `qwen-peer-lane archive` | public | true | archive a Qwen Code lane |
| `lane.qwen.doctor` | `qwen-peer-lane doctor` | public | true | doctor a Qwen Code lane |
| `lane.qwen.interrupt` | `qwen-peer-lane interrupt` | public | true | interrupt a Qwen Code lane |
| `lane.qwen.list` | `qwen-peer-lane list` | public | true | list a Qwen Code lane |
| `lane.qwen.resume` | `qwen-peer-lane resume` | public | true | resume a Qwen Code lane |
| `lane.qwen.run` | `qwen-peer-lane run` | public | true | run a Qwen Code lane |
| `lane.qwen.start` | `qwen-peer-lane start` | public | true | start a Qwen Code lane |
| `lane.qwen.status` | `qwen-peer-lane status` | public | true | status a Qwen Code lane |
| `lane.qwen.wait` | `qwen-peer-lane wait` | public | true | wait a Qwen Code lane |
| `peer` | `peer PRODUCT` | public | true | launch or resume an interactive peer |
| `peer.claude` | `claude-peer` | public | true | launch or resume a Claude Code peer |
| `peer.codex` | `codex-peer` | public | true | launch or resume a Codex peer |
| `peer.grok` | `grok-peer` | public | true | launch or resume a Grok peer |
| `peer.qwen` | `qwen-peer` | public | true | launch or resume a Qwen Code peer |

## Environment

- `PATH`
- `HOME`
- `XDG_CONFIG_HOME`
- `XDG_STATE_HOME`
- `XDG_RUNTIME_DIR`
- `CODEX_HOME`
- `CLAUDE_CONFIG_DIR`
- `CLAUDE_SECURESTORAGE_CONFIG_DIR`
- `QWEN_HOME`
- `QWEN_RUNTIME_DIR`

## Exit classes

| Code | Class | Meaning |
|---:|---|---|
| 0 | `success` | requested read or committed operation completed |
| 1 | `internal` | bounded attributable implementation failure |
| 2 | `usage` | invalid command, option, or positional shape |
| 3 | `unavailable` | required daemon, service manager, hub, or native dependency is unavailable |
| 4 | `refused` | exact blocker, conflict, denial, or unsafe precondition |
| 5 | `incompatible` | unsupported protocol, state, or release contract |
| 6 | `retryable` | operation was not accepted or remains durable debt |
