# Worker lanes

A lane is a product-native session driven by a managed parent. Any live peer may launch any of the
nine lane products. Inside a managed product, agents use the structured `agent_sessions.lane` MCP
tool. The `agent-sessions lane` command and `*-peer-lane` aliases expose the same engine for people
and scripts.

The aliases are `codex-peer-lane`, `claude-peer-lane`, `grok-peer-lane`, `qwen-peer-lane`,
`opencode-peer-lane`, `kilo-peer-lane`, `pi-peer-lane`, `omp-peer-lane`, and `dsh-peer-lane`.

## Lifecycle

The common operations are:

| Operation | Meaning |
| --- | --- |
| `run` | Open a fresh lane, start one turn, and return its terminal result. |
| `start` | Open a fresh lane and start a detached turn. |
| `resume` | Open or reuse the exact native session and start a turn. |
| `wait` | Wait for the current detached turn without changing it. |
| `status` | Report the live actor and current product turn projection. |
| `steer` | Submit native mid-turn input on products that genuinely support it. |
| `interrupt` | Ask the product to interrupt the active turn. |
| `archive` | Drop the live lane while preserving its product session. |
| `list` | List live lanes; `--all` also asks products to confirm eligible offline candidates. |

Prompts arrive through stdin or `--prompt-file`; they are not positional arguments. `run` and
`resume` return their own result synchronously. `start` can produce a terminal notice. The notice
uses `collection=required` and contains a structured MCP `wait` hint only when a detached terminal
turn still lacks a collector. Otherwise it uses `collection=none` and carries no hint.

The product's native session ID is the lane's only identity. Once open completes, every response
uses `session_id`; the provisional admission token is gone. A product-generated ID causes one
atomic re-key in the coordinator. Products that accept a caller-supplied ID use one identity from
birth.

## Launch facts and defaults

Each start or resume invocation supplies its whole launch context: cwd, explicit groups, model,
agent, effort, permission, persistence, and auto-archive choice. Omitted selections are not copied
from an earlier actor. The product receives no override and owns its default.

The default idle auto-archive interval is 60 seconds for start and resume. `--no-auto-archive` or
`--auto-archive-after` changes it for that invocation only. Persistence keeps an idle native driver
session open; it does not make turns or presence durable.

## Turns, messages, and native control

The engine serializes tracked lane turns, but it does not decide whether a product can accept an
ordinary message while busy. A message takes exactly one statically selected native inbound path:

- Codex starts or steers through the App Server.
- Claude writes a user frame to the stream. Replay acknowledgements correlate the tracked turn
  even when Claude interjects at a tool boundary or queues a separate inference.
- Grok uses `_x.ai/interject` for both messaging and steer.
- Pi and OMP use their proven native steer path.
- OpenCode, Kilo, and Qwen receive delivery through the held native integration connection and
  return the product's acceptance or rejection.
- DSH uses `Agent.steer` for delivery and steer; tracked lane turns use `Agent.followup` and DSH's
  own receipt and turn events.

There is no daemon mailbox, retry loop, or alternate fallback route. A product rejection is returned
verbatim. Steer is advertised only when a semantic test proved it changes the running turn:
Claude, Grok, Pi, OMP, and DSH support it; OpenCode, Kilo, Qwen, and Codex do not expose a lane
steer operation. Codex still accepts ordinary lane messages through its native inbound method.

Interrupt has one authority: the driver asks the product and waits for the product terminal. The
engine does not cancel its waiter as a second interrupt mechanism. A driver that cannot terminate
its own wait does not advertise interrupt.

## Archive, restart, and discovery

Archive is non-destructive. It closes the driver session, server, leader, ACP client, stream, or DSH
profile owned by the live lane, but it does not delete the product session. Codex performs native
unarchive only when Codex itself reports that exact thread archived.

Agent Sessions persists one immutable discovery-candidate row at fresh native identity. The row
contains the native ID, product, historical parent, parent primary group, assigned secondary
groups, and optional host. It is never served as an answer. `list --all` filters eligible rows by
group, asks the product to confirm each native ID, and returns only the product-confirmed title and
session facts. Resume never rewrites the row.

Daemon-owned workers are non-live after daemon replacement. Their product sessions remain in the
product store; `list --all` confirms them and resume opens the exact native session. A live process
that owns its own presence connection may re-report after reconnect, but no engine promise depends
on that topology.

## Groups and handover

A lane's effective groups are the invocation's explicit groups, the parent's private anchor, and a
derived lane anchor formed as:

```text
<parent-private-anchor>/<native-session-id>
```

The derived anchor is never stored as a secondary group; it is recomputed from the durable row.
Archive, daemon restart, and resume therefore restore the same historical groups without keeping a
second copy in the name cache.

A lane can be reparented only through a group the two parents share. The resuming live parent's
groups must intersect the recorded lane groups, and the product must confirm the native ID. The
new owner is in memory only; the immutable row keeps historical parentage. A lane genuinely live
under another connected parent cannot be taken over or opened twice.

See [Groups](GROUPS.md) for the full visibility rules, [Products](PRODUCTS.md) for native surfaces,
and the [native presence protocol](PROTOCOL.md) for the shared wire contract.
