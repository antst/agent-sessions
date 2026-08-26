# Qwen Code adapter (historical pre-design)

Status: advisory history only. This document predates the implemented Qwen
adapter and is not authoritative for current behavior. The normative feature
specification is under `specs/001-qwen-support/`; current operator contracts
are [../QWEN-ADAPTER.md](../QWEN-ADAPTER.md),
[../QWEN-LANES.md](../QWEN-LANES.md), and
[../QWEN-INSTALL.md](../QWEN-INSTALL.md). Where this pre-design conflicts with
those documents or the tested CLI, it is obsolete.

This design was prepared against the then-installed Qwen Code `0.21.12` and
the matching upstream tag at commit
`b965d5f8c24f48e65fb0b17c7d45f34ca4ce8f38`. Authoritative upstream surfaces:

- [Qwen Code v0.21.12](https://github.com/QwenLM/qwen-code/tree/v0.21.12)
- [ACP implementation](https://github.com/QwenLM/qwen-code/blob/v0.21.12/packages/cli/src/acp-integration/acpAgent.ts)
- [dual-output and remote-input protocol](https://github.com/QwenLM/qwen-code/blob/v0.21.12/docs/users/features/dual-output.md)
- [CLI session and MCP flags](https://github.com/QwenLM/qwen-code/blob/v0.21.12/packages/cli/src/config/config.ts)
- [session persistence and native archive support](https://github.com/QwenLM/qwen-code/blob/v0.21.12/packages/core/src/services/sessionService.ts)
- [Unix shell process behavior](https://github.com/QwenLM/qwen-code/blob/v0.21.12/packages/core/src/services/shellExecutionService.ts)

## Boundaries

Qwen support must reuse the existing grouped host agent, persistent session
catalog, and `AgentFrame` v1. It requires no new AgentFrame fields, message
types, global namespace, group semantics, or federation envelope.

Bare `qwen` remains outside Agent Sessions. Only `qwen-peer` or a managed Qwen
lane registers with the host agent and receives the Agent Sessions MCP and
group context.

The proposed product surface has two adapters:

- `qwen-peer`: an interactive Qwen supershim and parent session.
- `qwen-peer-lane`: a durable ACP-managed Qwen target lane.

`qwen serve` is not the initial lane transport. Its HTTP daemon, port and bearer
token add lifecycle and ownership that one stdio ACP worker per lane does not
need.

## Interactive `qwen-peer`

Qwen has no native peer registry or peer-to-peer socket. It does expose two
supported embedding surfaces:

- `--input-file PATH` accepts JSONL `submit` commands and queues them while the
  TUI is busy.
- `--json-fd N` or `--json-file PATH` emits structured protocol-v2 events. The
  first `system/session_start` event carries the exact native session UUID,
  cwd, Qwen version, and supported events.

For a new peer, `qwen-peer` allocates the catalog UUID and supplies the same
UUID through `qwen --session-id`. It creates a private `0700` runtime directory,
a `0600` input file, and a private delivery socket. With an inherited real
terminal it uses `--json-fd 3`; a future PTY-hosted implementation must use a
private `--json-file`, because PTY launchers do not preserve fd 3.

Registration occurs only after `session_start` proves the expected UUID.
Inbound host delivery decodes the existing AgentFrame, renders the existing
trusted cross-session presentation, and appends exactly one
`{"type":"submit","text":"..."}` record. Outbound discovery, direct messages,
multicast, and named-group broadcast use the existing `agent_sessions` MCP,
injected per invocation with `--mcp-config`; global Qwen configuration is not
modified.

The current registration shape is sufficient: the native Qwen child is the
adapter process root, while the `qwen-peer` wrapper is the lifecycle process
and owns the delivery socket. Wrapper death can therefore retire the exact
Qwen tree; native Qwen exit causes the wrapper to unregister and exit.

Qwen supports exact `--session-id UUID` and `--resume UUID`. Generic
`peer resume UUID` dispatches through catalog `product=qwen` and restores cwd,
groups, parent context and yolo unless explicitly overridden. Managed resume
must reject `--continue`, which cannot prove catalog identity, and initially
reject `--fork-session`, which represents a new session rather than a resume.
Agent Sessions archive stops the managed process while retaining the native
Qwen transcript; it does not silently invoke Qwen's separate transcript
archive or unarchive operations.

## Durable `qwen-peer-lane`

The target layer launches `qwen --acp` over stdio with
`QWEN_CODE_NO_RELAUNCH=1`. It initializes ACP, supplies the existing
`agent_sessions` MCP in `session/new`, and persists the returned native Qwen
UUID separately from the Agent Sessions lane UUID. Resume uses the ACP wire
method `session/resume` (currently exposed by the SDK as
`unstable_resumeSession`), which restores without `session/load` history
replay. ACP prompt, update, and cancel map onto the existing durable turn,
queue, interrupt, wait, collection, archive, autoarchive, and owner-exit state
machine.

The manager must not select or mutate Qwen authentication. `doctor` reports
the exact executable and version, ACP availability, authentication readiness,
workspace trust, and requested versus effective approval mode. Qwen downgrades
privileged approval modes in untrusted folders, so a requested yolo lane fails
startup clearly unless effective yolo is confirmed.

`QWEN_HOME` remains the user's normal Qwen home so existing authentication,
settings, skills, and transcripts remain visible. `QWEN_RUNTIME_DIR` is not
redirected unless its exact value is persisted and restored with the session.

## Parent and target composition

The parent layer and target layer remain independent. Adding Qwen produces the
following supported composition contract:

| Parent | Local or remote targets |
| --- | --- |
| Codex | Codex, Claude, Grok, Qwen |
| Claude | Codex, Claude, Grok, Qwen |
| Grok | Codex, Claude, Grok, Qwen |
| Qwen | Codex, Claude, Grok, Qwen |

A child always receives its own private group and the immediate parent's
anchor group. It receives the parent's other groups only when that parent
explicitly requests inheritance. Qwen adds the `qwen-lane` federation
capability and executable dispatch, but no routing or framing extension.

## Process ownership risk

This is the main implementation and macOS acceptance risk. On Unix, Qwen
resolves `bash` through `PATH` and launches shell tools in detached process
groups. Killing only the ACP worker or its process session cannot prove that
tool descendants are gone.

Qwen must reuse a narrowly generalized form of the existing Grok tool-root
ledger. A private PATH-prepended `bash` wrapper records PID, process start, and
strong start under a locked, private registry before executing the real shell.
Cleanup closes admission, reads all exact roots, captures live ancestry before
closing the worker, signals only corroborated identities, sweeps the worker
session and MCP descendants, and verifies quiescence before removing state.
The persisted worker and tool-root identities must also allow host-agent
reconciliation after wrapper or manager SIGKILL. This is a focused reuse of a
proven lifecycle primitive, not a new generic messaging framework.

## Packaging and skills

Future packages add `qwen-peer` and `qwen-peer-lane`, taking the current release
from nine to eleven binaries. The shared runtime gains `qwen-lane` and
`qwen-lane-manager` roles, and executable resolution gains
`QWEN_PEER_QWEN_BIN`. Federation advertises `qwen-lane` only when the exact
launcher is available.

Target skills must expose Qwen lanes to Codex, Claude, and Grok. A Qwen parent
skill installed under Qwen's user skills directory must expose all four lane
targets, local and remote execution, and the common group/inheritance rules.
Installation is Qwen-only: it must not change Qwen authentication or settings,
and must not replace shared Agent Sessions runtime while unrelated clients are
live.

## Acceptance matrix

The following is the minimum product matrix. Q-03 through Q-09 run separately
on Linux and macOS.

| ID | Required evidence |
| --- | --- |
| Q-01 | Pinned Qwen doctor reports executable/version, ACP, auth, trust, and effective yolo without launching a session. |
| Q-02 | Normal, race, vet, and lint gates pass; all four platform archives contain eleven binaries and Qwen skills. |
| Q-03 | Bare `qwen` creates no Agent Sessions catalog, registration, socket, MCP, or environment artifacts. |
| Q-04 | Interactive new peer proves exact UUID, product, groups and yolo; real AgentFrame input plus MCP reverse, multicast, and group broadcast succeed. |
| Q-05 | Exit and generic resume retain UUID, cwd, groups and yolo; explicit supported overrides are honored. |
| Q-06 | Interactive wrapper SIGKILL during real detached shell and MCP activity leaves no owned process or private artifact. |
| Q-07 | Lane start/wait, idle follow-up, reverse delivery, queued interrupt, archive/idempotent archive, and resume retain both lane and native Qwen UUID plus transcript. |
| Q-08 | Worker and manager SIGKILL during a real detached `sleep 300` and live MCP processes produce the expected terminal result and zero residue at return and +1/+5/+10/+30 seconds. |
| Q-09 | Owner exit, autoarchive, mandatory parent anchor, and explicit-only parent-group inheritance pass. |
| Q-10 | Unit coverage exercises all 16 parent-target compositions; live edges cover Qwen parent to every target and every parent to Qwen. |
| Q-11 | One Linux-to-macOS and reverse federated Qwen lane roundtrip returns terminal notice and collection through shared groups. |
| Q-12 | A separately native-archived Qwen transcript fails managed resume clearly; explicit native unarchive makes exact resume succeed. |

No Qwen support is complete until the real detached-process crash cells are
green on both operating systems.
