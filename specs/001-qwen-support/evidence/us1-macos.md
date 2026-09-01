# US1 macOS: managed interactive Qwen

- Date: 2026-08-24
- Platform: macOS arm64
- Source commit: `6fb9f46cbe5747b6b8cb6ae2c02d72dca0aa0d0a`
- Qwen Code: `0.22.0`
- Selected authenticated profile: the owner's default `~/.qwen`, used under explicit authorization
- Selected extension: exact source payload `agent-sessions (0.2.4)`
- Selected test runtime: `/tmp/qwen-t031-runtime`, created empty with mode `0700`
- Credited runner: `scripts/test-qwen-contract`

## Result

The credited run exited 0 and emitted:

```json
{"type":"qwen.contract.passed","session_id":"67af75b9-c44e-4463-82e3-9f5101b609fc","elapsed_seconds":111.868,"delivery_socket_type":"unix","hub_round_trips":0}
```

The runner requires the published endpoint to exist and uses `lstat` to reject either a symlink or
anything other than a Unix socket. The result therefore proves a real non-symlink delivery socket,
not merely a labelled value. The real managed Qwen session also proved:

- exact selected-profile executable, version, extension, trust, authentication, and archive readiness;
- permission-conflict rejection before preparation, durable mutation, or native child creation;
- discovery, direct send/reply, named-group broadcast, and correlated delivery through the structured
  `agent_sessions` MCP;
- zero federation-hub round trips for the local messaging cell;
- inactivity of an unattested MCP process;
- Qwen-owned native approval-mode changes without Agent Sessions identity or group drift;
- exact native resume of session `67af75b9-c44e-4463-82e3-9f5101b609fc` with messaging restored;
- native-default, explicit non-yolo, yolo, and pass-through plan launch contracts; and
- normal exit and cleanup with zero retained contract root, runner process, worktree-binary process,
  or contract tmux server.

The selected extension remained installed for the subsequent lane and federation acceptance cells.
Metadata-only inspection recorded expected extension-manager and native usage/session bookkeeping;
the dedicated runtime contained the native session state. No credential or owner-profile file was
opened for comparison, hashed, copied, printed, or manually restored.

