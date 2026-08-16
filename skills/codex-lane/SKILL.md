---
name: codex-lane
description: Spawn, supervise, message, collect, resume, and archive durable local or remote Codex worker lanes from any Agent Sessions parent. Use when delegating work to Codex, including a Codex parent launching another Codex lane.
---

# Codex lane

Use `codex-peer-lane` for the Codex target. Parent product and target product
are independent: a Codex, Claude, or Grok parent may use this same target CLI.

Run `codex-peer-lane doctor --json` and `codex-peer-lane list --all`; require
contract version 2 and a ready runtime. For another host use
`peer-federator lane --host HOST --product codex --` after checking `hosts`.
Never fall back to SSH or silently run locally.

Pipe the briefing on stdin:

```sh
codex-peer-lane start --name review-a - < brief.md
codex-peer-lane wait review-a --timeout 300 > result.jsonl
```

The child always joins its own private group and the immediate parent’s private
group. Parent groups are not copied by default. Add `--inherit-groups` only
when the parent deliberately wants all its current groups propagated. Use
repeatable `--group NAME` for child-specific groups. `--no-inherit-groups`
explicitly resets optional inheritance while preserving the parent anchor;
omission on resume restores the prior choice.

`lane.ready` is not the answer. Collect once with `wait`, match the final
`turn.completed` and final agent message by turn ID, and report outcome/exit.
Terminal notices are collection pointers. Collect outstanding debt before
`resume`. Use `interrupt` for active work and `archive` when orchestration is
finished. `--persistent` changes lifecycle ownership only; it does not remove
the communication parent anchor.

For a remote launch federation supplies the attested source parent context and
returns terminal notices through grouped routing. The selected destination
target remains the native Codex adapter.
