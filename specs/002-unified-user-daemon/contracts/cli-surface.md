# Contract: Canonical CLI, Help, Environment, and Exit Surface

## Purpose

The host command surface comes from one `agent-sessions` executable image. The one central network hub
comes from the separate `agent-sessions-hub` executable. This contract prevents either binary or the
host image's multi-call aliases from recreating divergent parsers, help text, environment handling, or
exit behavior.

## One authoritative descriptor inventory

Every Agent Sessions-owned command mode across both binaries and every installed host `argv[0]` alias
is declared in one typed descriptor inventory. A descriptor contains:

- canonical mode and any installed aliases;
- whether the mode is public, service-internal, connector-internal, or hub-only;
- subcommands and every Agent Sessions-parsed option, including short and compatibility aliases;
- option arity, repetition, default, environment binding, and command applicability;
- whether remaining arguments are passed unchanged to a vendor executable;
- stable human and JSON output schemas;
- exit-status class and cause-specific remediation;
- required daemon role and whether the operation is online or offline.

Parsers, generated `--help`, shell completion if shipped, package inventory, `docs/CLI.md`, and the
machine-readable command inventory all consume this descriptor. Tests fail if a parser accepts an
Agent Sessions option absent from its help/descriptor, help advertises an unparsed option, an installed
alias has no descriptor, or documentation and the descriptor differ.

Vendor-native options forwarded without Agent Sessions interpretation are outside this inventory. The
help must mark the native boundary and must never claim that forwarded native options are Agent
Sessions options.

## Required modes and aliases

| Surface | Canonical invocation or aliases | Contract |
|---|---|---|
| User service | `agent-sessions daemon` | Foreground service-manager process; no self-daemonization |
| Host CLI inventory | `agent-sessions help [MODE] [--json]` and `agent-sessions MODE --help` | Generated from the descriptor; JSON is the machine-readable host command contract |
| Runtime inspection | `agent-sessions status [--json]` | Stable metadata-only status; online |
| Diagnostics | `agent-sessions doctor [--json]` | Read-only cause-specific diagnosis; never starts service |
| Removal inspection | `agent-sessions remove inspect [--json]` | Exact online blocker/target inventory; no mutation |
| Offline purge | `agent-sessions purge inspect --plan PATH` and `agent-sessions purge apply --plan PATH` | Revision-bound offline plan/apply; never starts service |
| Central hub | `agent-sessions-hub [--listen ADDRESS]` and `agent-sessions-hub --help` | Separate central deployment binary, not a host authority |
| Hub inspection | `agent-sessions-hub status [--json]` | Stable metadata-only central-hub process/service/build/protocol/listener and routing health |
| Hub diagnostics | `agent-sessions-hub doctor [--json]` | Read-only cause-specific hub diagnosis; never starts either service |
| Hub removal inspection | `agent-sessions-hub remove inspect [--json]` | Exact hub blocker/target inventory; no host or remote-host mutation |
| Offline hub purge | `agent-sessions-hub purge inspect --plan PATH` and `agent-sessions-hub purge apply --plan PATH` | Revision-bound offline hub-only plan/apply; never starts either service |
| Interactive peers | `codex-peer`, `claude-peer`, `grok-peer`, `qwen-peer`, and `peer` | Short-lived prepare/adopt clients; vendor process remains external |
| Durable lanes | `codex-peer-lane`, `claude-peer-lane`, `grok-peer-lane`, `qwen-peer-lane` | Short-lived clients for `run`, `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list`, and `doctor` |
| Vendor connectors | connector aliases used by `.mcp.json` payloads and `scripts/native-entry` | Internal stateless stdio relay; not an administrative surface |

Host filesystem aliases resolve to the exact `agent-sessions` image. `agent-sessions-hub` is a distinct
binary and is never an alias of the host image. An internal host mode remains in the same inventory even
when omitted from top-level public help; its mode-specific `--help` and diagnostic must still describe
every parsed option without exposing credentials or raw capabilities.

Packaged lifecycle targets are descriptor-backed mappings, not additional command contracts:
`install-all`, host removal, and host purge select the host role; `install-hub`, `remove-hub`,
`purge-hub-inspect`, and `purge-hub` select the hub role. Hub purge inspection maps to
`agent-sessions-hub purge inspect`, and apply maps to `agent-sessions-hub purge apply`. Neither target
may infer or mutate the other role's selection.

## Shared wrapper options

The peer descriptor inventory includes every wrapper-owned spelling before vendor passthrough,
including where applicable `-n/--name`, `-g/--group`, `--inherit-groups`, `--no-inherit-groups`,
`--yolo`, `--no-yolo`, managed resume selection, explicit product profile selection, and `-h/--help`.
Arguments after `--` are never parsed by Agent Sessions. Internal parent/capability options are rejected
on public peer surfaces rather than silently accepted.

The lane descriptor inventory includes the shared commands listed above and every shared spelling:
`-n/--name`, compatibility `--peer-name`, `-C/--cd`, compatibility `--cwd`, `--timeout`,
`--prompt-file`, `--notify`, `--no-notify`, `--persistent`, `--no-auto-archive`,
`--auto-archive-after`, `-g/--group`, `--inherit-groups`, `--no-inherit-groups`,
`--allow-duplicate-name`, `--all`, `--mine`, `--json`, and `-h/--help`. Product-specific lane options
remain explicit descriptor entries rather than handwritten help fragments.

## Environment contract

The complete supported environment inventory is:

| Variable | Consumer | Contract |
|---|---|---|
| `PATH` | short-lived host clients | Native executable discovery only; never identity or authority |
| `HOME` | both binaries and native clients | Standard user configuration root; the hub service uses its configured service account and never inspects host vendor profiles |
| `XDG_CONFIG_HOME` | host daemon and hub | Standard non-secret configuration root; default is `$HOME/.config` |
| `XDG_STATE_HOME` | host daemon and hub | Standard owned state root; default is `$HOME/.local/state`; host and hub use distinct fixed subdirectories |
| `XDG_RUNTIME_DIR` | Linux host daemon | Standard same-user runtime parent for the fixed host endpoint; it does not select another daemon namespace |
| `CODEX_HOME` | Codex adapter/client | Exact native profile selection; credentials and transcript content remain opaque |
| `CLAUDE_CONFIG_DIR` | Claude adapter/client | Exact native profile selection; credential values remain opaque |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` | Claude native process | Passed through as vendor-owned secure-storage namespace, including the valid explicit-empty Darwin value; Agent Sessions does not inspect it |
| `QWEN_HOME` | Qwen adapter/client | Exact native profile selection; credential values remain opaque |
| `QWEN_RUNTIME_DIR` | Qwen adapter/client | Exact native runtime/transcript selection, not Agent Sessions authority |

Grok's user profile follows its native `HOME` contract. Credential variables such as vendor API keys
are opaque vendor inputs: Agent Sessions does not parse, copy, print, hash, persist, or describe their
values. No public `AGENT_SESSIONS_*` environment variable selects daemon identity, endpoint, state root,
group space, host identity, hub identity, attachment, parent, product, session, permission, or
capability. Those values come from fixed owned configuration, CLI descriptors, daemon-issued protocol
state, and exact attestation. Tests inject paths/configuration through in-process fixtures rather than a
second production environment namespace.

Any future parsed environment variable is a protocol change: it must be added here and to the typed
descriptor, generated help, `docs/CLI.md`, and completeness tests before use.

## Stable exit classes

Public Agent Sessions processing uses this exact mapping before a launcher successfully execs a native
vendor process:

| Code | Class | Meaning |
|---:|---|---|
| 0 | success | Requested read or committed operation completed |
| 1 | internal | Attributable implementation failure with bounded non-secret diagnostic |
| 2 | usage | Unknown mode/option, missing value, contradictory options, or invalid positional shape |
| 3 | unavailable | Required daemon, service manager, hub, or native dependency is unavailable before acceptance |
| 4 | refused | Exact active blocker, identity conflict, permission/group denial, or unsafe mutation precondition |
| 5 | incompatible | Local protocol, state schema, release contract, or federation protocol is unsupported |
| 6 | retryable | No false success; operation was not accepted or remains explicit durable debt |

After a peer launcher successfully replaces itself with a vendor executable, the process exposes that
vendor's exit behavior. Termination by signal follows the platform shell convention `128 + signal` for
Agent Sessions scripts that translate it. No command remaps a vendor exit into false Agent Sessions
success.

## Stable JSON envelope and result fields

Every `--json` command emits exactly one object. Success uses:

```json
{"schema_version":1,"ok":true,"command":"status","result":{}}
```

Failure uses:

```json
{"schema_version":1,"ok":false,"command":"status","error":{"class":"unavailable","code":"daemon_unavailable","retryable":true,"message":"agent-sessions daemon is unavailable","next_action":"systemctl --user status agent-sessions.service"}}
```

The top-level fields are exactly `schema_version`, `ok`, `command`, and either `result` or `error`.
Error fields are exactly `class`, `code`, `retryable`, `message`, and `next_action`. Content-bearing
values are forbidden. Command result objects use these stable top-level fields:

| Command | Result fields |
|---|---|
| `help` | `binaries`, `modes` |
| `status` | `runtime_version`, `runtime_identity`, `generation`, `pid`, `proc_start`, `endpoint`, `service`, `products`, `attachments`, `lanes`, `federation`, `debt` |
| `doctor` | `healthy`, `checks` |
| hub `status` | `runtime_version`, `runtime_identity`, `pid`, `proc_start`, `listener`, `service`, `protocol_version`, `connected_hosts`, `routing`, `debt` |
| hub `doctor` | `healthy`, `checks` |
| `remove inspect` | `role`, `revision`, `blockers`, `targets`, `preserved` |
| `purge inspect` | `role`, `plan_revision`, `targets`, `exclusions` |
| `purge apply` | `role`, `plan_revision`, `deleted`, `debt` |

Nested schemas come from the corresponding local-control and durable-record contracts and are included
verbatim in the machine-readable `help --json` descriptor. Adding, removing, or changing a stable field
requires an explicit contract change and regression update.

Human output states the same failed precondition without requiring inspection or manual editing of
internal state.

## Completeness gate

The release gate enumerates every descriptor, instantiates every parser, compares accepted options to
generated help and checked documentation, verifies alias-to-image identity, proves model-facing MCP
inventories contain no administration modes, and fails on every undocumented parser, environment
binding, JSON field, or exit class.
