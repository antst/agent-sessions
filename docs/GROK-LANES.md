# Grok Build worker lanes

`grok-peer-lane` is the headless worker companion to `grok-peer`. It gives an
orchestrator a durable, named Grok Build session that can receive peer
messages, execute turns without a TUI, emit normalized JSONL results, resume
by exact identity, and archive cleanly.

This document is the implementation contract for the initial lane PR. Until
the acceptance section is green, the binary is experimental and must not be
advertised by federation.

## Ownership boundary

One lane manager owns one Grok ACP process and one Grok conversation. The
manager is the sole ACP driver for that conversation from creation through
archive. It must never load, prompt, cancel, or otherwise attach to a session
owned by an interactive `grok` or `grok-peer` process.

The supported ACP lifecycle is:

```text
spawn: grok --session-id UUID agent --no-leader stdio
initialize
authenticate
session/new                         # start/run
session/load                        # exact lane resume only
session/prompt                      # launcher and peer turns, serialized
session/cancel                      # interrupt
shutdown + verified process cleanup # archive
```

The UUID is selected before process start so the lane's plugin and native MCP
receive one immutable launch identity. Names are local lifecycle aliases;
resume resolves them only through Agent Sessions lane state. It never scrapes
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
turn.completed | turn.failed
```

Unknown ACP notifications are retained as diagnostics but never interpreted
as terminal results. A turn completes only from the corresponding ACP prompt
response or a protocol-defined terminal error.

## Peer messaging and attestation

The lane loads the installed `agent_sessions` Grok plugin in its own session.
Its MCP process is authorized by a per-launch capability plus exact process
identity and ancestry. Model-supplied session IDs, lane names, socket paths,
and permission labels are corroboration only and never grant authority.

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

## Permissions

Headless lanes cannot answer an interactive approval prompt. The initial
implementation must expose an explicit Grok permission option and choose a
documented non-interactive default verified against the installed Grok Build
version. Owner-derived bypass is allowed only from a process-attested local
owner; an explicit caller choice wins. Effective live permission comes from
the resident roster, not argv inference, and is refreshed before it authorizes
lane inheritance or outgoing peer labels.

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
- default and bypass permission publication, explicit-mode precedence, live
  roster changes, and unauthorized MCP/control callers rejected;
- Codex-, Claude-, and Grok-owned local lanes, including terminal notices and
  no privilege escalation through ownership inference;
- full pairwise peer/lane messaging for every installed product, plus remote
  Grok lane cells only after Grok federation is explicitly implemented;
- normal and forced cleanup with no manager, ACP, MCP, socket, launch record,
  registry row, or private directory resurrection;
- fresh normal and race suites, vet, lint, all supported cross-build/package
  archives, and archive-content verification including `grok-peer-lane`.

An enqueue-only observation is not delivery proof. A message cell passes only
when the destination visibly incorporates the exact marker and returns the
specified acknowledgement through Agent Sessions.
