# Grok Build worker lanes

`grok-peer-lane` is the headless worker companion to `grok-peer`. It gives an
orchestrator a durable, named Grok Build session that can receive peer
messages, execute turns without a TUI, emit normalized JSONL results, resume
by exact identity, and archive cleanly.

This document is the implementation and acceptance contract. Grok lanes use the same explicitly
enabled, hub-routed remote lifecycle transport as Codex and Claude lanes; SSH is never a fallback.

## Ownership boundary

One lane manager owns one Grok ACP process and one Grok conversation. The
manager is the sole ACP driver for that conversation from creation through
archive. It must never load, prompt, cancel, or otherwise attach to a session
owned by an interactive `grok` or `grok-peer` process.

The supported ACP lifecycle is:

```text
spawn: grok agent --no-leader stdio
initialize
authenticate
session/new                         # start/run; returns native Grok UUID
session/load                        # exact lane resume only
session/prompt                      # launcher and peer turns, serialized
session/cancel                      # interrupt
shutdown + verified process cleanup # archive
```

Agent Sessions selects an immutable lane UUID before process start. ACP `session/new` separately
returns Grok's native conversation UUID; both are persisted, and every prompt/load/cancel/roster
operation uses the native UUID. Names are local lifecycle aliases; resume resolves them only
through Agent Sessions lane state, then `session/load`s the persisted native UUID. It never scrapes
Grok's private store or uses Grok's title matching.

## Commands and output

The native command surface mirrors the established lane products:

```text
grok-peer-lane run      --name NAME [OPTIONS] < prompt.md
grok-peer-lane start    --name NAME [OPTIONS] < prompt.md
grok-peer-lane resume   SESSION_OR_NAME [OPTIONS] < prompt.md
grok-peer-lane wait     SESSION_OR_NAME [--timeout SECONDS]
grok-peer-lane status   SESSION_OR_NAME
grok-peer-lane interrupt SESSION_OR_NAME
grok-peer-lane archive  SESSION_OR_NAME
grok-peer-lane list     [--all] [--mine]
grok-peer-lane doctor   --json
```

`run` waits for the submitted turn. `start` returns after `lane.ready`.
`wait` observes but does not cancel on timeout. `resume` refuses while an
uncollected active or terminal result is owed. Stdout is JSONL and diagnostics
go to stderr. The normalized event sequence is:

```text
thread.started | thread.resumed
turn.started
lane.ready
item.completed (user_message)
item.completed (agent_message, phase=final_answer)
turn.completed (status/outcome/exit describe every terminal result)
```

Unknown ACP notifications are ignored and never interpreted as terminal
results. A turn completes only from the corresponding ACP prompt response or
a protocol-defined terminal error.

`lane.status` and each `lane.list` row expose the stable lane identity, native
Grok conversation UUID, cwd, lifecycle status, current collection-debt
`turn_id`, last collected turn, owner/persistence, and auto-archive policy.
Terminal result details are emitted by `wait` as `turn.completed`; the status
event is not a substitute for collection.

## Peer messaging and attestation

The lane loads the installed `agent_sessions` Grok plugin in its own session.
Its MCP process is authorized by a per-launch capability plus exact process
identity and ancestry. Model-supplied session IDs, lane names, socket paths,
and permission labels are corroboration only and never grant authority.
Lifecycle control is a local same-UID boundary: the Unix control socket is
owner-only and every request must carry the exact stable lane session ID.

The manager publishes one peer row only after all of these are true:

1. the ACP process is initialized and authenticated;
2. exactly one resident roster row matches the lane UUID;
3. the row's live permission and activity fields are valid;
4. a direct read-only `agent_sessions.list_peers` probe succeeds; and
5. manager, ACP process, MCP caller, and control socket match the persisted
   launch identities.

An inbound peer message becomes the next serialized lane turn. If a launcher
turn is active, the manager may use Grok's supported interjection contract only
when it can preserve exactly-once accounting; otherwise the message stays in a
durable queue for the next turn. It never starts a concurrent prompt RPC.

## Durability and cleanup

Lane state records the session UUID, name, canonical cwd, selected model and
reasoning policy, effective permission class, exact manager/ACP process
identities, active and terminal turn records, result collection state,
auto-archive deadline, and lifecycle owner identity. Raw launch capabilities
never reach argv, JSONL, logs, or disk; only their hashes are persisted.

The manager survives the initiating shell. A parent-owned lane archives after
its exact owner exits; `--persistent` has no lifecycle owner. Normal archive,
owner death, auto-archive, ACP failure, manager restart reconciliation, and
install preflight converge on one idempotent cleanup path. Cleanup withdraws
the peer before deleting sockets and state, stops only exact process identities,
and preserves uncollected terminal results until explicit archive.

Archive is bridge-owned. It retires the Agent Sessions manager, sole ACP driver, peer publication,
MCP children, and owned runtime artifacts while preserving Grok's native transcript. Resume starts
a fresh sole ACP driver and calls `session/load` for the exact stored native Grok UUID. Agent
Sessions does not call a native Grok archive or unarchive API.

Owned-process cleanup combines the durable worker session, exact live process ancestry, the
per-launch capability tag, and a private tool-shell registry. Before Grok executes bash or zsh, a
token-bound wrapper durably registers that shell's PID and kernel start identity under a shared
admission lock; archive/reconciliation holds the exclusive lock while stopping those roots and
their descendants. Every PID and sub-second start identity is rechecked immediately before a
signal. This covers Grok Build and its normal MCP/tool children even when a registered shell changes
process group or reparents after a manager crash. On macOS, the kernel deliberately hides the
environment of restricted executables, and the current platform rejects recursive `kqueue
NOTE_TRACK`; therefore an arbitrary restricted program that creates a new session and becomes
reparented after its registered shell has already exited is no longer observable or safely
attributable and can survive cleanup undetected. Such unmanaged daemonization is unsupported and
excluded from a green cleanup claim. Agent Sessions never guesses ownership or kills a process from
a PID, session number, or token hash alone. Installed macOS acceptance must still prove that real
Grok Build, MCP, and tool descendants leave no process or artifact residue after normal archive and
crash reconciliation.

Terminal turns durably queue a `GROK_LANE_TERMINAL` collection pointer for the configured owner.
The pointer contains a stable notice ID and exact `wait` command; it is never the answer. Its native
message ID is the same stable notice ID, so a retry after an ambiguous state write is deduplicated by
the destination peer.

## Remote lanes

An operator must explicitly enable remote lane execution on the destination federator. A healthy
destination advertises `grok-lane` only when its exact `grok-peer-lane` launcher is available. Use
`peer-federator lane --host HOST --product grok -- COMMAND ...`; every lifecycle command remains the
native Grok JSONL contract. Federation injects `--persistent` and a notify target back through the
source shadow for remote `run`, `start`, and `resume`, so callers must not override persistence or
notification flags. Every operation requires the hub and fails closed on disconnect.

## Permissions

Headless lanes cannot answer an interactive approval prompt. They therefore require explicit Grok
always-approve mode and publish `bypassPermissions`. Prompting modes fail at argument validation
instead of creating an unusable worker. Lifecycle ownership still requires an exact corroborated
local owner unless `--persistent` is selected; ownership never grants or downgrades permission.
Effective live permission comes from the resident roster, not argv inference.

Model, reasoning effort, cwd, plugin path, and permission switches are passed
as structured launcher options. Arbitrary native argv is not accepted on the
manager/control boundary.

## Acceptance gate

The PR is not complete until all applicable cells are executed from an
installed package on Linux and macOS, with blocked cells reported rather than
counted green:

- fail-closed resolver with chat-Grok first, proper Grok Build fallback, and
  direct/symlink/case-varied macOS application-bundle rejection;
- doctor, cold `run`, detached `start` + `wait`, status, interrupt, exact-UUID
  resume, name resume, list, `--mine`, archive, owner-exit archive, persistent
  lane, and auto-archive;
- idle peer message, reverse reply, message during an active turn, duplicate
  message ID, disconnect/reconnect, manager crash recovery, and outstanding
  result collection;
- bypass permission publication, rejection of default/prompting modes, live
  roster changes, and unauthorized MCP/control callers rejected;
- Codex-, Claude-, and Grok-owned local lanes, with no privilege escalation
  through ownership inference;
- full pairwise peer/lane messaging for every installed product, including
  Linux-to-macOS and macOS-to-Linux remote Grok lifecycle, collection, messaging, and archive;
- normal and forced cleanup with no manager, ACP, standard MCP/tool descendant,
  socket, launch record, registry row, or private directory resurrection, including
  the restricted-executable ancestry path on macOS;
- fresh normal and race suites, vet, lint, all supported cross-build/package
  archives, and archive-content verification including `grok-peer-lane`.

An enqueue-only observation is not delivery proof. A message cell passes only
when the destination visibly incorporates the exact marker and returns the
specified acknowledgement through Agent Sessions.
