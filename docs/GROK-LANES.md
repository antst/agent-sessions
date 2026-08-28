# Grok lanes

A Grok lane is a durable daemon-owned Agent Sessions lane backed by a vendor-owned Grok ACP session.
`grok-peer-lane` is an alias of the canonical `agent-sessions` image and a client of the running
daemon. ACP coordination is in process; there is no Grok lane manager, host listener, or remote watcher
chain.

## Lifecycle

```sh
grok-peer-lane start --name research --cd /srv/project --prompt-file prompt.txt
grok-peer-lane status --name research --json
grok-peer-lane wait --name research --json
grok-peer-lane interrupt --name research
grok-peer-lane resume --name research --prompt-file followup.txt
grok-peer-lane archive --name research
grok-peer-lane list --mine --json
grok-peer-lane doctor --json
```

`run` combines start and wait. The daemon commits the turn before ACP dispatch, records the exact
native session/turn identity, publishes one terminal notice, and advances collection once. Resume
loads the corroborated native session; interrupt and archive are exact-revision operations.

Grok owns the ACP session, roster, leader/observer, authentication, transcript, and native archive
behavior. The daemon owns parent context, existing global groups, permission proof, lane/turn state,
terminal notice, cursor, archive coordination, and cleanup debt.

## Parent, groups, and permissions

The child always receives its own and its parent's private groups. Other parent groups are copied only
with `--inherit-groups`; explicit groups remain in the same global multi-host space. Product, process
session, profile, and host identities are exact lifecycle facts, not access namespaces.

The adapter maps the requested permission class to Grok's native controls and records the observed
effective mode. It does not invent a parallel policy engine.

## Restart

The daemon recovers the lane actor from the durable transaction and reconnects only when Grok exposes
the same supported ACP session/turn identity. It does not redispatch accepted work. If the active turn
cannot be reconnected safely, the daemon records one explicit interrupted, collectable result and
retains the native session reference for resume.

## Archive and cleanup

Archive and cleanup revalidate the exact owner, leader, ACP session, turn, and daemon revision. A
changed process group or session becomes debt. Grok credentials, plugins, settings, transcripts, and
native session data remain untouched.

Remote Grok lanes are dispatched directly to the destination daemon through the hub using the same
lane engine and result notice. There is no SSH or fallback local execution.
