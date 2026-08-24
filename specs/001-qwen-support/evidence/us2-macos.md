# US2 macOS: durable Qwen lane lifecycle

- Date: 2026-08-24
- Platform: macOS arm64
- Source commit: `6fb9f46cbe5747b6b8cb6ae2c02d72dca0aa0d0a`
- Qwen Code: `0.22.0`
- Agent Sessions version: `0.2.4`
- Selected authenticated profile: the owner's default `~/.qwen`, used under explicit authorization
- Selected test runtime: `/tmp/qwen-t045-runtime`, created empty with mode `0700`
- Credited runner: `scripts/test-qwen-lane-contract`
- Result: `Qwen lane contract: PASS` (exit 0)

## Credited lifecycle evidence

The real Qwen lane runner proved:

- selected-profile doctor readiness, native archive capability, workspace trust, and profile identity;
- conflicting wrapper/native permission choices fail with exit 2 before a manager, worker, or durable
  lane exists;
- completed native-default, explicit non-yolo, yolo, and pass-through plan turns;
- exact live follow-up/resume, active-turn interrupt, native archive idempotence, archived
  unarchive/resume, and final re-archive;
- exact manager and ACP-worker crash reconciliation without broad process discovery; and
- no active lane, manager, worker, delivery socket, contract root, runner process, worktree-binary
  process, or process referring to the dedicated runtime after cleanup.

Independent inspection found all seven native test chats under the dedicated runtime's
`chats/archive/` directory and none in its active chat root. The archived native session IDs were:

```text
f578cfe5-51c8-4489-a16e-2d1703bcfeb3
74fa3227-1177-44f4-9a9c-65d0aff5ae2d
2b442aec-28fc-43e6-92b9-dc5a43656941
64d97e01-fa24-4241-b0d7-0f6705fbfd64
209b883e-90fa-46d0-b79d-9131916ec946
78882384-2681-4ce7-8db2-1b49c5d886c1
ab292d12-8e09-44e4-8dbd-bf501179fdc0
```

During the deliberate manager/ACP-worker crash cells, native Qwen logged eighteen instances of
`ACP child process shutdown failed` / `daemon shutdown incomplete`. Those diagnostics align with the
runner intentionally terminating the process that Qwen then attempted to retire. They are recorded
as native crash diagnostics rather than a RED because every authoritative retirement, archive, and
zero-residue assertion passed.

Metadata-only profile inspection recorded expected extension-manager, usage, and log-cleanup
bookkeeping. The selected extension remained installed for federation acceptance. No credential or
owner-profile file was opened for comparison, hashed, copied, printed, or manually restored.

