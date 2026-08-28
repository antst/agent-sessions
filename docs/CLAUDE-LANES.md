# Claude lanes

A Claude lane is a durable daemon-owned Agent Sessions lane backed by a vendor-owned Claude
stream-JSON worker and native session. `claude-peer-lane` is an alias of the canonical
`agent-sessions` image and a client of the already-running daemon; no detached Claude lane manager or
control socket exists.

## Lifecycle

```sh
claude-peer-lane start --name implementation --cd /srv/project --prompt-file prompt.txt
claude-peer-lane status --name implementation --json
claude-peer-lane wait --name implementation --json
claude-peer-lane interrupt --name implementation
claude-peer-lane resume --name implementation --prompt-file followup.txt
claude-peer-lane archive --name implementation
claude-peer-lane list --mine --json
claude-peer-lane doctor --json
```

`run` combines start and wait. The daemon commits the turn before starting the native worker, records
exact UUID/process/stream evidence, emits one terminal notice, and advances collection once. Resume,
interrupt, and archive operate on the exact durable lane/turn revision.

Claude owns its transcript, authentication namespace, native permission behavior, resume selection,
and stream worker. The daemon owns parent context, existing global groups, effective permission mode,
lane/turn state, terminal notices, collection cursor, archive coordination, and cleanup debt.

## Parent, groups, and permissions

Every child gets its private group and its parent's private group. Other parent groups are copied only
with `--inherit-groups`; explicit `--group` values remain global across hosts. No profile, host, or
product namespace is introduced.

The adapter passes the selected permission mode as a launch-only Claude overlay and records the proven
effective class. It does not alter the shared Claude profile's permission or cross-session defaults.

## Restart

The lane actor is a daemon goroutine. If the native stream worker exposes sufficient stable identity,
restart reconnects without duplicate dispatch. Where real platform evidence proves inherited streams
cannot be recovered safely, the daemon records exactly one explicit `interrupted` terminal result that
is collectable and resumable. It does not keep an Agent Sessions proxy process solely to preserve a
pipe.

## Archive and cleanup

Archive and cleanup revalidate the exact Claude UUID, process/stream identity, profile/cwd, and daemon
revision. Only Agent Sessions-owned lane records and connector artifacts may be retired. Claude
credentials, secure-storage entries, settings, native registry, and transcripts remain vendor-owned.

Remote Claude lanes use the same destination daemon lane engine through the central hub. There is no
remote watcher or SSH/direct-listener fallback.
