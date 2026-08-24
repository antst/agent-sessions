# Qwen Code peer adapter

`qwen-peer` runs an ordinary interactive Qwen Code session with Agent Sessions
messaging and lane orchestration attached. Native Qwen remains the UI, transcript
owner, authentication provider, model selector, and permission manager.

Bare `qwen` is the explicit communication opt-out. It receives no Agent Sessions
identity merely because the plugin is installed.

## Native integration

The adapter uses Qwen Code 0.21.15 or newer and two supported native surfaces:

- protocol-v2 dual output (`--json-file`) supplies the exact native UUID, cwd,
  version, activity, and terminal events;
- native remote input (`--input-file`) accepts one JSONL `submit` record for an
  inbound AgentFrame.

The launcher allocates private `0700` state, creates regular `0600` input and
event files, and starts Qwen only after the selected profile passes the common
readiness engine. Publication waits for the exact `session_start` UUID, cwd,
version, protocol version, and event inventory. The stable delivery endpoint is
a real owner-only Unix socket. It is never a symlink and callers never need a
path-resolution workaround.

Profile and cwd identity use the shared adapter platform contract. On macOS,
the fixed `/tmp` and `/var` aliases are accepted only after resolving to the
expected `/private/...` system targets; arbitrary symlink components remain a
fail-closed error. Agent Sessions-owned sockets use the shared 103-byte macOS /
107-byte Linux pathname budget and compact runtime roots. Process environment
inspection that returns no entries is treated as unavailable, never as proof
that a live process is unrelated. See [ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md#shared-host-platform-primitives).

`qwen-peer --resume UUID_OR_UNIQUE_MANAGED_NAME` restores the exact Agent
Sessions catalog identity and Qwen transcript. `--continue` and
`--fork-session` are deliberately left to bare Qwen because neither identifies
one existing managed peer.

## Permissions

Qwen owns permission behavior in a peer exactly as it does normally:

- no flag keeps Qwen's native default;
- `--no-yolo` requests initial native `default` mode;
- `--yolo` requests initial native `yolo` mode;
- `--approval-mode MODE` passes that supported native initial mode through.

Those choices conflict with each other and are rejected before state mutation.
After launch, Qwen's normal UI may change mode. Agent Sessions records the
launch preference and an observed current mode when the native protocol exposes
one; it does not lock, emulate, or silently widen native permissions. Native
permission state never grants Agent Sessions identity or group authorization.

## Identity and messaging

The installed `agent_sessions` MCP is active only when all of these agree:

1. a per-launch unguessable capability (only its SHA-256 is persisted);
2. exact process ancestry and PID plus strong process-start identity;
3. the live Qwen session UUID and real delivery socket;
4. the selected `QWEN_HOME`/`QWEN_RUNTIME_DIR` identity and canonical cwd; and
5. the host-agent registration and group catalog.

Model-supplied IDs, names, paths, and permission labels are corroboration only.
The MCP supports grouped discovery, direct send, atomic multicast, named-group
broadcast, and all four lane products. An idle or busy Qwen TUI receives a
queued native submit without terminal keystroke injection.

## Cleanup

Normal exit, Ctrl+C, SIGTERM, wrapper death, native failure, and agent restart
converge on the same preparation journal. Cleanup re-attests exact process and
file identities, withdraws the participant, closes the socket, and removes only
the launch-owned input/event artifacts. PID reuse, changed files, path-type
changes, or ambiguous legacy symlinks retain explicit cleanup debt instead of
authorizing collateral deletion.

## Operator smoke

```bash
qwen-peer -n qwen-reviewer -g review
qwen-peer --resume qwen-reviewer -g review
```

From another managed peer in `review`, discover `qwen-reviewer`, exchange a
correlated direct message and reply, then broadcast to `review`. A simultaneous
bare `qwen` must remain absent from discovery. On exit, the participant row,
real session socket, and private launch files must disappear while the native
transcript and unrelated profile content remain.

See [QWEN-INSTALL.md](QWEN-INSTALL.md), [QWEN-LANES.md](QWEN-LANES.md), and
[ADAPTER-PROTOCOL.md](ADAPTER-PROTOCOL.md).
