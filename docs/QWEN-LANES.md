# Qwen lanes

A Qwen lane is a durable daemon-owned Agent Sessions lane backed by a vendor-owned Qwen ACP/native
session. `qwen-peer-lane` is an alias of the canonical `agent-sessions` image and a client of the
already-running daemon. Native observation and input run as adapter goroutines; no Qwen host, lane
manager, delivery listener, or remote watcher exists.

## Lifecycle

```sh
qwen-peer-lane start --name analysis --cd /srv/project --prompt-file prompt.txt
qwen-peer-lane status --name analysis --json
qwen-peer-lane wait --name analysis --json
qwen-peer-lane interrupt --name analysis
qwen-peer-lane resume --name analysis --prompt-file followup.txt
qwen-peer-lane archive --name analysis
qwen-peer-lane list --mine --json
qwen-peer-lane doctor --json
```

`run` combines start and wait. The daemon commits a turn before native dispatch, records exact Qwen
session/turn and event/input evidence, emits one terminal notice, and advances collection once. Resume,
interrupt, and archive use the same exact durable identity.

Qwen owns the TUI/daemon/ACP worker, authentication, transcript, event/input and archive stores. The
daemon owns parent context, existing global groups, permission proof, lane/turn state, notices,
collection cursor, archive coordination, and cleanup debt.

## Parent, groups, profiles, and permissions

The child always receives its private group and its parent's private group. Other parent groups are
copied only with `--inherit-groups`; explicit `--group` values remain global across hosts.

`QWEN_HOME` and `QWEN_RUNTIME_DIR` select the exact native profile and runtime artifacts. They do not
create another Agent Sessions daemon, state root, routing namespace, or access boundary. Permission
mode is translated to native Qwen controls and the observed effective value is stored.

## Restart

The daemon reconstructs the lane actor only from the admitted native session, event/input artifacts,
and live ancestry. Accepted work is never redispatched. A supported Qwen session contract reconnects;
otherwise the active turn becomes exactly one explicit interrupted, collectable, resumable result.

## Archive and cleanup

Archive uses Qwen's native store and verifies the exact result before committing daemon state. Cleanup
revalidates the profile, ancestry, native artifacts, session/turn, and revision. It never deletes Qwen
credentials, profile settings, transcripts, native archive data, or unrelated files.

Remote Qwen lanes use the same destination daemon lane engine through the central hub. There is no
SSH or alternate carrier fallback.
