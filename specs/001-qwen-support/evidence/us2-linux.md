# US2 Linux: durable Qwen lane lifecycle

- Date: 2026-08-23
- Platform: Linux amd64
- Qwen Code: `0.22.0`
- Agent Sessions version: `0.2.4`
- Selected authenticated profile: `/home/antst/.qwen`
- Command: `QWEN_TEST_HOME=/home/antst/.qwen QWEN_TEST_RUNTIME_DIR=<private-runtime> ./scripts/test-qwen-lane-contract`
- Result: `Qwen lane contract: PASS` (exit 0)

## Credited evidence

The real Qwen lane runner proved:

- doctor readiness, native archive capability, workspace trust, and selected-profile identity;
- conflicting wrapper/native permission choices fail with exit 2 before a worker or durable lane exists;
- completed native-default, explicit non-yolo, yolo, and pass-through plan turns;
- follow-up work resumes on the exact live Agent Sessions/native transcript;
- an active turn interrupts with normalized status/outcome `interrupted` and wait exit 130;
- exact manager SIGKILL is reconciled through forced archive, retiring its worker and owned endpoints/tool roots;
- exact ACP-worker SIGKILL is observed by the manager and archived without broad process discovery;
- native archive is idempotent;
- archived unarchive/resume preserves the exact transcript identity;
- final archive leaves no active lane, owned manager/worker, delivery socket, or test root.

The isolated Codex App Server printed warnings that it would not create helper
PATH aliases beneath `/tmp`; each warning explicitly said it was proceeding.
The lane contract completed successfully, so those warnings are not classified
as Qwen or Agent Sessions failures.

No Qwen credential value was read, copied, printed, or changed. After cleanup,
no `qwen`, `qwen-peer`, `qwen-lane-manager`, or Qwen ACP worker process from the
runner remained, and its `qwen-lane-contract.*` root was absent.
