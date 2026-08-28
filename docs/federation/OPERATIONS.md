# Operations

## Topology

Run exactly one `agent-sessions` daemon per OS user. Its embedded federation
component is local-only unless the host configuration names one outbound hub.
Reusing a host ID replaces the old hub connection; the fixed control-endpoint
lock rejects a second daemon.

The daemon publishes exactly one synthetic service row in the configured
Claude `sessions/` registry. Participating peer adapters are registered through
the private control socket and do not create public router/shadow rows.

## Start and diagnose

```sh
agent-sessions status --json
agent-sessions doctor --json
```

For federation:

```sh
agent-sessions-hub --listen :7419
agent-sessions-hub doctor --json
agent-sessions status --json
agent-sessions lane --host workstation-b --product codex -- doctor --json
```

Host `doctor` requires the local daemon and its attachment authority. Host
`status` reports the embedded federation state; hub status/doctor are owned by
the independent hub binary. Remote lane commands fail while disconnected
instead of falling back locally or returning stale host data.

## Product sessions

Use `codex-peer`, `claude-peer`, `grok-peer`, or `qwen-peer` to opt in. Pass repeatable
`-g NAME` or `--group NAME` options on a fresh launch. Resume without group/yolo overrides
restores the catalog; explicit values replace it. `peer resume SESSION_UUID`
uses the catalogued product adapter.

`claude-peer` uses the same configured native Claude profile as ordinary
Claude. Its exact UUID can move between ordinary and managed attachments
without transcript copying. The host agent is the sole writer of the one
service row. Bare `claude` is not registered with Agent Sessions, and install
never changes the operator's default `crossSessionInbound` policy.
Claude does not publish live Shift+Tab permission changes in its native row.
Consequently a managed peer's advertised permission class is fixed at launch:
constrained peers disable in-session bypass through a launch-only overlay, and
explicit bypass peers remain conservatively labelled bypass until restart.
Managed Claude profiles and secure-storage overrides must use absolute paths so
detached, local, and remote workers cannot reinterpret them under another cwd.
Unreleased development builds that stored Claude transcripts in an Agent
Sessions-private native profile are not copied into the shared profile; there
is no collision-safe automatic migration for those development-only sessions.

## Remote lane execution

Remote lanes require `remote_lanes_enabled` and a `hub_address` in the unified
host daemon configuration. The daemon advertises only product adapters that
are ready in that generation. After installing or repairing a connector,
restart the user-managed `agent-sessions` service to publish the new readiness
snapshot; there is no separately managed federation-agent process.

Every parent product can select every target product. The source parent is
agent-attested; it is independent from `--product`, which chooses only the
destination lane adapter. Parent group inheritance is opt-in per launch.

Remote structured input is capped at 1 MiB before durable source acceptance.
Remote work uses the same in-process lane engine, adapter state machine, and
vendor-native lifecycle as local work; it has no separate CLI-process pool or
federation-specific capacity namespace. Supply the destination `-C`/`--cd`
path explicitly when needed.

## Linux systemd user service

```sh
make install
make install-hub HUB_LISTEN=:7419
systemctl --user daemon-reload
systemctl --user enable --now agent-sessions.service
systemctl --user enable --now agent-sessions-hub.service
```

On the hub host, configure and enable the supplied hub unit. Permit TCP 7419
only inside the trusted network.

## macOS launchd

```sh
make install
make install-hub HUB_LISTEN=:7419
plutil -lint ~/Library/LaunchAgents/net.antst.agent-sessions.plist
plutil -lint ~/Library/LaunchAgents/net.antst.agent-sessions-hub.plist
launchctl bootstrap "gui/$(id -u)" \
  ~/Library/LaunchAgents/net.antst.agent-sessions.plist
```

The supported role transaction stages the exact launchd definition, enables login start, and
starts or restarts only that selected role through the shared service lifecycle engine. Host and
hub definitions, release roots, and service labels remain disjoint.

## Failure and upgrades

Local routing and the durable catalog remain available during a hub outage.
Host daemons reconnect with bounded backoff and republish their current attachment snapshot.
Daemon generation recovery re-corroborates native attachments and active lane state, so a daemon
restart does not require a peer restart. Exiting connectors detach exact identities; stale native
evidence is withdrawn by the owning adapter without a second registration authority.

Upgrade the hub and host daemons independently. Protocol mismatches fail at
hello/probe, and no protocol-2 flat/shadow fallback is attempted.

Before release, run `make lint`, `make test`, `make test-race`, the grouped
two-host federation integration, and installed Linux/macOS parent×target lane
acceptance.
