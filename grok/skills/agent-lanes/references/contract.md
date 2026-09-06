# Codex, Claude, Grok, and Qwen lane contracts

All products expose `run`, `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list`, and
`doctor`. Codex is contract 2; Claude and Grok are contract 1. All emit `lane.ready`, normalized
`item.completed`, `turn.completed`, `lane.status`, `lane.list`, and `lane.archived` JSONL.

Codex runs on the shared App Server and preserves caller-selected sandbox, approval, model, effort,
web, config, schema, and worktree policy. Claude owns a native stream worker, supports cost/budget
fields, and has no transcript archive API; lane archive retires the worker while leaving its exact
Claude session resumable. Grok owns one ACP driver, persists its native UUID, and uses bridge-owned
archive before exact `session/load` resume.

One collector owns a lane result cursor. Resume refuses uncollected debt. Ordinary Sessionbus
messages wake or steer live workers, but a terminal notification is only a collection pointer.
