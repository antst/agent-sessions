# Native Qwen Contract

## Version and admission

- Syntax/protocol floor: Qwen Code `0.21.15`.
- Version alone never admits an operation.
- Doctor and launch perform operation-specific probes; a newer client with a missing, ambiguous, or
  changed contract is rejected.
- The exact executable and selected profile are stable for the full peer/lane transaction.

## Interactive dual-output v2

Managed new launch supplies:

```text
--session-id <expected-uuid>
--chat-recording=true
--input-file <private-regular-file>
--json-file <private-regular-file>
--mcp-config <private-authoritative-config>
```

Managed resume replaces `--session-id` with exact `--resume <uuid>`.

The first structured event must be `system/session_start`. Admission requires:

- exact expected native session UUID;
- canonical cwd;
- admitted Qwen version;
- dual-output protocol v2 and required event inventory;
- exact lifecycle process still live;
- selected profile and integration unchanged;
- exact preservation of the selected native approval-mode request, including an unmodified native
  default when no option was supplied; current mode remains unknown unless Qwen exposes it.

Input records are complete JSON objects, one per line. The writer opens only the attested regular
file, serializes one delivery once, fsyncs/flushes according to the native contract, and rechecks
inode/type/body cursor. Truncation, replacement, changed type, or unexpected cursor becomes debt.

The adapter's published session-stable delivery endpoint is itself a private Unix socket. It is not
a symlink to a PID-named backend, and senders must not need `readlink`, canonicalization, or another
transport-specific workaround before connecting. Replacement and deletion still require exact
session, process-start, and artifact ownership; a legacy Codex/Grok alias is removed only after
its exact stale backend is corroborated.

## Stdio ACP lane

Transport is newline-delimited JSON-RPC over private stdio. Required initialize evidence includes:

- agent name `qwen-code` and admitted version;
- protocol version 1;
- new/load/resume/list/prompt/cancel/update behavior used by the manager;
- mode/config reporting used for permission corroboration;
- supported MCP injection.

The manager uses:

- `session/new` for first native identity;
- `session/resume` for transcript-preserving reattachment;
- `session/prompt` for one active turn;
- `session/update` to aggregate agent output and tool/plan state;
- `session/cancel` to interrupt;
- `session/set_mode` when required by the native ACP workflow; later native mode changes are Qwen
  behavior rather than an Agent Sessions policy violation.

It does not use routine `session/load`, concurrent prompts, native continue, or title lookup.

One prompt is active at a time. Later work is durably queued. A terminal stop reason is observed once,
normalized once, and collected once.

## Native archive helper

Archive/unarchive uses only a bounded helper launched after the active ACP worker and tool tree are
quiescent:

```text
qwen serve --bare \
  --hostname 127.0.0.1 \
  --port 0 \
  --require-auth \
  --token <fresh-random-token> \
  --no-web \
  --workspace <canonical-cwd>
```

Environment includes exact profile/runtime roots and strips weakening or unrelated managed
capabilities. The helper must:

1. publish an OS-assigned loopback endpoint;
2. require bearer auth on every request, including health;
3. report protocol/capability v1 with `session_archive` and exact workspace;
4. accept an authenticated exact-UUID archive or unarchive request;
5. return exactly one of the documented idempotent success states for that UUID;
6. terminate with its preheated ACP child and leave no process/socket/temp residue.

`archived` and `alreadyArchived` are archive success. `unarchived` and `alreadyActive` are unarchive
success. `notFound`, conflict, transition race, workspace mismatch, capability mismatch, transport
failure, or response ambiguity is not success.

The helper is never a federation listener, peer transport, or persistent service.

## Profile contract

- Unset `QWEN_HOME` means the native default profile.
- Explicit `QWEN_HOME` is canonical and absolute. Mutable symlink components
  fail closed; fixed platform-owned aliases such as macOS `/tmp` and `/var`
  resolve to their native targets.
- `QWEN_RUNTIME_DIR` value or absence is part of exact transcript identity.
- Launch never copies/migrates credentials, settings, skills, plugins, or transcripts.
- Qwen-owned counters/caches/bookkeeping inside the selected profile are allowed and inventoried
  separately from Agent Sessions mutations.

## Live probe contract

Session-free probes may inspect executable/package version, command-specific help, parser-only
failures, extension inventory, trust, and ACP `initialize`. They must not send session/new, load,
resume, prompt, or create a transcript. Interactive `session_start`, live MCP connection, and provider
authentication are admitted only during the intended managed launch.
