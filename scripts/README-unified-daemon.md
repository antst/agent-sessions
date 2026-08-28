# Unified daemon acceptance assets

The unified-daemon acceptance commands live in `scripts/` and validate the
existing Agent Sessions behavior after its runtime responsibilities move into
one `agent-sessions` daemon per OS user on each host.

The planned acceptance entry points cover:

- `test-unified-service`: host installation, service control, upgrade,
  rollback, removal, purge, and single-process/single-endpoint invariants;
- `test-unified-peers`: Codex, Claude, Grok, and Qwen peer attachment,
  discovery, grouped messaging, restart, and cleanup;
- `test-unified-lane-composition`: the complete parent-to-target lane matrix;
- `test-unified-lane-restart`: accepted-turn restart, recovery, collection,
  archive, and cleanup behavior;
- `test-unified-stress`: durable admission and real resource-failure behavior;
  and
- the existing grouped federation acceptance under `scripts/federation/`,
  updated to exercise embedded host federation against the independently
  deployed `agent-sessions-hub`.

Acceptance scripts may create test-owned native profiles, sessions, lanes, and
other exact resource identities, but they never start a second Agent Sessions
host daemon for the same OS user. They use the already running standard user
service and existing groups. Host acceptance must not mutate hub lifecycle;
hub acceptance must not mutate host lifecycle.

The host and hub are distinct deployment roles even when tested on one
machine. A passing matrix must therefore identify both exact builds and prove
federation interoperability through protocol-version equality rather than
assuming equal release strings or commits.
