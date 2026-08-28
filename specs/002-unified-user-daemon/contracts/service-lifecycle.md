# Contract: User Service and Release Lifecycle

## Authority

The OS user service manager is the only steady-state owner of daemon lifetime:

- Linux: `agent-sessions.service` in the systemd user manager
- macOS: `net.antst.agent-sessions` in the user's launchd domain

The daemon runs in the foreground and does not fork, daemonize, or supervise a replacement of itself.
Peer, lane, MCP, plugin, hook, and federation workflow commands do not start or repair it.

The central network hub is a separate `agent-sessions-hub` service and executable. It is not a second
host runtime authority, owns no local product integration, and is never started, stopped, upgraded, or
removed by host installation or workflow commands. One hub deployment serves multiple independently
managed host daemons.

## Shared lifecycle implementation

Host and hub have independent service instances and independently invoked transactions, but their
mechanics are implemented once. A shared service-control package consumes an immutable role descriptor
for executable, arguments, service name, configuration path, and readiness probe. A shared release
transaction package owns locking, immutable staging, pointer commit, journaling, service transition,
readiness, rollback, removal, and revision-bound purge. Host hooks add connector and first-migration
steps; hub hooks add only hub configuration and network readiness. Neither role imports the other's
orchestration package.

## Service definitions

### Linux

- `Type=simple`
- `ExecStart` uses the stable host-role release selection and `agent-sessions daemon`
- `Restart=on-failure`
- enabled in `default.target`
- runtime directory supplied by the user manager and owned mode `0700`
- non-secret configuration loaded from the Agent Sessions user configuration path

`systemctl --user stop agent-sessions.service` is an explicit stop and is not reversed by
`Restart=on-failure`. `disable` suppresses login start. `start` and `enable --now` are explicit user
actions.

### macOS

- label `net.antst.agent-sessions`
- `ProgramArguments` uses the stable host-role release selection and `agent-sessions daemon`
- `RunAtLoad=true`
- `KeepAlive=true` for unexpected process exit
- logs go through the configured user-service output path and obey metadata-only logging

An explicit stop uses `launchctl bootout gui/$UID/net.antst.agent-sessions`, because merely signalling a
`KeepAlive` process is not a persistent stop. Start uses `launchctl bootstrap` with the installed plist.

## Login, crash, sleep, and logout behavior

- Enabled service starts at user login.
- Unexpected daemon exit is restarted by the service manager within the acceptance budget.
- Explicit stop/disable keeps the daemon absent until explicit start/enable.
- macOS sleep/wake and network changes do not create another service instance; the existing process or
  its one service-manager replacement reconnects native adapters and federation.
- Logout follows the platform user-service lifecycle. Durable accepted work remains recoverable at the
  next service start.

## Hub-only service definitions

The explicit `install-hub` target installs only `bin/agent-sessions-hub`, non-secret hub configuration,
and one hub service definition:

- Linux: `agent-sessions-hub.service`, `ExecStart` uses the exact installed
  `agent-sessions-hub --listen ADDRESS`, and the service manager owns restart behavior.
- macOS: `net.antst.agent-sessions-hub`, whose `ProgramArguments` use the exact installed
`hub/current/bin/agent-sessions-hub --listen ADDRESS`.

It does not install or enable `agent-sessions.service`, command aliases, vendor connectors, native
product integrations, host configuration, or a host runtime root. `remove-hub` stops and removes only
the exact hub service, binary selection, immutable releases not referenced by the host role, and
hub-owned disposable runtime
state after exact identity checks. It preserves non-secret hub configuration and durable hub metadata
for reinstall and does not touch any host daemon, vendor state, or remote host. `purge-hub` is the only
operation that may delete that preserved hub state, using an offline revision-bound plan. Hub
install/upgrade/remove/purge never invokes host service-manager operations, and host lifecycle never
invokes hub lifecycle.

## Release layout

```text
$PREFIX/libexec/agent-sessions/
├── host/
│   ├── current -> releases/<host-release-id>/
│   ├── releases/<release-id>/{bin/agent-sessions,connectors,docs,manifest.json}
│   └── transactions/<transaction-id>.json
└── hub/
    ├── current -> releases/<hub-release-id>/
    ├── releases/<release-id>/{bin/agent-sessions-hub,docs,manifest.json}
    └── transactions/<transaction-id>.json
```

`release-id` contains the declared version plus a content identity so same-version development builds
cannot alias different bytes. A staged release is immutable after validation. Host and hub use disjoint
role-owned release roots, current selections, transaction directories, install locks, service
transitions, readiness decisions, rollback, removal, and purge roots. User-facing host command names
resolve to the one `host/current/bin/agent-sessions` image. The central service executes the distinct
`hub/current/bin/agent-sessions-hub` image and is never an argv0 alias of the host image. Thus a
co-located host and hub can select unrelated builds while sharing the same lifecycle implementation.

The authoritative source-release version is `deploy/agent-sessions/VERSION`; the old
`deploy/peer-federator/VERSION` is removed. Each host and hub binary also records its exact build
identity and federation protocol version. A source archive describes both binaries it contains, but a
deployed host and hub may originate from unrelated commits or releases of this repository.
Software-version interoperability depends only on exact hub-protocol-version equality, never on equal
release strings or commit identities.

## Clean host installation

1. Acquire the host-role install lock.
2. Validate archive/platform, complete product inventory, binary identity, connector payloads, service
   definition, configuration schema, and writable owned destinations.
3. If the estate predates the unified daemon, verify the operator-stopped maintenance window through a
   read-only closed inventory before any persistent install mutation.
4. Discover installed native products. Mark absent products unavailable; do not fail aggregate install.
5. Stage and fsync an immutable release on the same filesystem as the host-role current selection.
6. Prepare recoverable vendor connector transactions for each installed Codex marketplace, Claude
   marketplace, Grok plugin, and Qwen extension without reading credential values.
7. Install the standard service definition and preserve existing non-secret configuration.
8. Atomically select `host/current`.
9. Enable and start the user service once.
10. Require readiness proof: exact runtime/release identity, generation, endpoint, state schema, local
   routing, product readiness, and configured federation state.
11. Commit the install journal and all prepared connector mutations together.

The `install-all` target invokes the shared release engine with the host role and implements only this
host transaction. It never installs, enables, starts, restarts, or removes the hub service.

For a first migration, the operator first closes every legacy peer and lane, stops every responsive
legacy supervisor, product manager, and federation authority through the old supported lifecycle, and
prevents replacement legacy launches until installation completes. The installer fails closed naming
any remaining live legacy authority, including one reporting zero shims. It never stops, signals, or
restarts legacy authority; after absence is proven it may adopt metadata and retire exact artifacts.

If any step before service start fails, the system publishes no daemon authority. If readiness fails,
the installer disables/stops the candidate, restores exact prior connector/config state, and reports
the attributable cause.

## Upgrade transaction

1. Acquire the selected role's install lock and recover or finish that role's prior journal.
2. Read the current daemon generation and service-manager state.
3. If the current estate predates the unified daemon, verify the operator-stopped maintenance window
   through a read-only closed inventory before any persistent upgrade mutation.
4. Offline-validate and stage the complete successor release.
5. Prepare Codex, Claude, Grok, and Qwen connector changes that apply to the installed product subset
   and record exact per-product rollback metadata.
6. Atomically switch the selected role's current release to the staged release.
7. Ask the service manager for one restart transaction.
8. Verify the successor is the only authority and reports the exact target release/generation.
9. Commit connector changes and transaction journal; retain the prior release until commit.

There is never a deliberate interval with two authoritative host daemons. If successor readiness fails,
the transaction stops the candidate, restores that role's prior current selection and owned state, starts
the previous service image, and verifies the previous generation is usable. This recovery is part of
the failed administrative transaction; only a ready generation is committed as authority.

That previous-unified-image restart rule does not apply to first migration. First-migration rollback
leaves the unified candidate and all legacy authorities stopped, restores only installer-changed
release/state/connector/service surfaces, and directs the operator to retry unified installation or
manually relaunch the old supported lifecycle. It performs no live handoff or compatibility drain.

Steady-state upgrade does not require closing supported managed native interactive sessions. Adapters
reconstruct them after restart. Active lane behavior follows the adapter restart contract.

A host and hub upgrade use the same transaction engine with different role descriptors and hooks; they
do not share a lock, invocation, service transition, or readiness decision. A host upgrade reconnects
to the existing hub after restart. When both sides declare the same
federation protocol version, it does not request, require, or cause hub lifecycle work. Hub upgrade
follows its own immutable staging and service-manager transaction and leaves every host daemon running;
protocol-matching hosts reconnect without local upgrade. Protocol mismatch fails before host
registration or work acceptance and names the exact version required at that connection boundary.

## First migration gate

Before the first unified service becomes ready, the staged runtime inventories legacy Agent Sessions
state and authorities. The gate:

- refuses before mutation if any exactly corroborated managed legacy peer or lane remains active;
- names every exact peer/lane and requires the operator to close it before retrying;
- does not implement live legacy peer, attachment, or lane handoff;
- ignores stale scalar counts after their alleged exact owners are proven absent;
- records unknown/conflicting identity as retryable debt;
- does not stop a native vendor session or unrelated process;
- adopts the existing host identity, global groups, session catalog, lane state, messages/notices, and
  federation configuration;
- requires the operator to stop every responsive legacy Agent Sessions authority through its old
  supported lifecycle and proves their absence before any unified authority is published;
- retires every old Agent Sessions supervisor, shim, host, lane manager, local routing/federation agent,
  service job, and listener before declaring the daemon ready.

The central hub is not a migration target. An existing host federation agent is.

## Status and doctor

The daemon status contract includes:

- exact release/runtime identity, generation, PID/start identity, endpoint, service unit, and state;
- configured host/hub identity and federation connection state;
- product readiness and non-secret profile identities;
- managed attachment/lane counts and exact blocker references;
- migration/install transaction state and cleanup debt;
- no message, prompt, result, transcript, tool content, or credential value.

Doctor validates the service-manager job, endpoint/record/process agreement, state schema, product
readiness, configured hub-protocol match, and exact debt remediation. It never starts the service.

## Explicit stop and restart

- `stop`: service-manager stop/bootout; daemon closes admission, finishes or checkpoints owned durable
  transitions, closes the one endpoint, and exits. Native interactive sessions are not terminated.
- `restart`: service-manager restart; same release, next generation; accepted work recovers idempotently.
- `disable`: suppress login start and stop/bootout when requested by the user.
- `start`: starts only the already installed current release for that service role through the service manager.

## Normal host removal

1. Query the running daemon for exact active peer/lane blockers and exact removal targets.
2. If any managed attachment or lane is active, fail without mutation and list every blocker.
3. Acquire the install lock and journal the removal.
4. Disable and stop/bootout the service; verify the exact daemon exited.
5. Remove Codex, Claude, Grok, and Qwen connector registrations/payloads through their supported
   product installers, reporting native bookkeeping that the vendor retains.
6. Remove the service definition, command links, host current selection, host releases, and disposable
   runtime endpoint after exact ownership checks.
7. Preserve Agent Sessions configuration, durable metadata, and migration/cleanup debt.
8. Verify zero Agent Sessions host daemon or obsolete long-lived role remains.

The operation is idempotent and resumes from its journal after interruption.

## Normal hub removal

Hub removal invokes the same removal engine with the hub role. It journals the operation, stops and
verifies the exact hub service, removes its service definition, hub current selection and releases,
and disposable endpoints/spools, then verifies no hub process remains. It does not query or stop
remote hosts and does not remove host state, connector payloads, vendor data, hub configuration, or
durable hub metadata. Reinstall restores the preserved hub identity and configuration.

## Explicit purge

Host purge and `purge-hub` are separate from normal removal and are available only when the selected
role's service is removed/quiescent.
It first emits an exact plan containing Agent Sessions-owned paths and current revisions. Apply requires
that plan/revision and revalidates filesystem type, UID, root containment, and current identity before
each deletion.

Purge is an offline administrative mode of the selected canonical executable, invoked from a verified
release archive or repository build after normal removal. It does not require, contact, or start the
daemon or hub.
Packaged/source lifecycle targets must make that tool available even when the installed command links
and releases have already been removed.

Purge excludes:

- every vendor credential and authentication store;
- vendor profiles and settings;
- native transcripts/history and archived sessions;
- unrelated service-manager jobs and logs;
- data owned by the other deployment role.

Partial purge records remaining debt and is safely retryable.

## Packaging and optional products

- Release archives remain available for linux-x64, linux-arm64, darwin-x64, and darwin-arm64.
- Each contains the canonical `agent-sessions` host binary, the separate `agent-sessions-hub` binary,
  all four optional connector payloads, service definitions, and documentation.
- Aggregate host install probes each native product and installs only available integrations; it does
  not install the hub.
- Hub-only install requires no native product, connector, vendor profile, or host daemon and installs
  only the hub binary/service/configuration. `remove-hub` preserves hub configuration and durable
  metadata; `purge-hub` deletes only its exact revision-bound hub targets.
- Explicit product install/validation commands remain strict when the selected native product is absent
  or unready.
- `deploy/agent-sessions/VERSION` and one generated source-release manifest drive the two binary
  identities, connectors, service files, release archives, help, and status for that build. Runtime
  network interoperability is independently driven by the hub-protocol version; advertised host
  capabilities remain per-operation availability, not release coupling.
