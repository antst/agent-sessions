# US1 Linux: managed interactive Qwen

- Date: 2026-08-23
- Platform: Linux amd64
- Qwen Code: `0.22.0`
- Selected authenticated profile: `/home/antst/.qwen`
- Selected extension: `agent-sessions (0.2.4)` from this worktree
- Selected test runtime: a private `0700` directory under `/tmp`
- Credited runner: `QWEN_TEST_HOME=/home/antst/.qwen QWEN_TEST_RUNTIME_DIR=<private-runtime> ./scripts/test-qwen-contract`

## Result

The credited run exited 0 and emitted:

```json
{"delivery_socket_type":"unix","elapsed_seconds":111.292,"hub_round_trips":0,"session_id":"568133e7-a96f-46fb-b972-e0ed7b205622","type":"qwen.contract.passed"}
```

The real managed Qwen session proved all of the following:

- exact official package/version and selected-profile readiness;
- permission-conflict rejection before child creation or state mutation;
- post-readiness native-authentication failure rollback;
- publication under the requested name/group with a real Unix socket, not a symlink;
- Qwen `agent_sessions` discovery, direct send, group broadcast, inbound delivery, and correlated reply;
- a direct unattested MCP process receives `inactive outside an attested peer session`;
- native Shift+Tab permission-mode mutation remains Qwen-owned;
- normal cleanup removes the managed registration/socket;
- native resume retains session `568133e7-a96f-46fb-b972-e0ed7b205622` and restores structured messaging;
- native-default, explicit `--no-yolo`, `--yolo`, and pass-through `plan` launch contracts publish and clean;
- zero federation hub round trips were required.

After the run there were no live `qwen`/`qwen-peer` processes and no retained
`qwen-contract.*` harness root. The selected profile's `settings.json`,
`installation_id`, extension directory, skill directory, and output-language
file retained their pre-run metadata. Qwen legitimately appended native session,
tip-history, usage, and cleanup bookkeeping. No credential value was read,
copied, printed, or altered.

## Discarded harness attempt

The first attempt completed managed discovery/send/broadcast/reply but timed out
asking a bare language model to select a forbidden tool. That was not credited.
The runner now invokes the installed MCP server directly from an unattested
process, making the security assertion independent of model choice and network
latency. The complete corrected runner then passed.
