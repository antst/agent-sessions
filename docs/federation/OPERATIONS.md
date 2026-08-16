# Operations

## Topology

Run exactly one host agent per OS user/runtime directory. A local-only agent is
fully supported. Add one outbound hub connection only when cross-host routing
or remote lanes are needed. Reusing a host ID replaces the old hub connection;
the runtime-directory lock rejects a second local agent.

The agent publishes exactly one synthetic service row in the configured
Claude `sessions/` registry. Participating peer adapters are registered through
the private control socket and do not create public router/shadow rows.

## Start and diagnose

```sh
peer-federator agent --host workstation-a
peer-federator doctor
peer-federator status
```

For federation:

```sh
peer-federator hub --listen :7419
peer-federator agent --hub 10.2.17.1:7419 --host workstation-a
peer-federator doctor --hub 10.2.17.1:7419
peer-federator hosts
```

`doctor` requires the local agent and registry. It checks hub compatibility
only when `--hub` is supplied. `status` reports local/remote peer and host
counts; there is no shadow count in protocol 3. `hosts` fails while the hub is
disconnected instead of returning stale data.

## Product sessions

Use `codex-peer`, `claude-peer`, or `grok-peer` to opt in. Pass repeatable
`--group NAME` options on a fresh launch. Resume without group/yolo overrides
restores the catalog; explicit values replace it. `peer resume SESSION_UUID`
uses the catalogued product adapter.

`claude-peer` uses a deterministic private `CLAUDE_CONFIG_DIR` for its native
session registry and projects the one host-agent service row there. Secure
credential storage remains rooted at the user’s public Claude config. Bare
`claude` is not modified or registered.

## Remote lane execution

Remote lanes are disabled unless `--enable-remote-lanes` or
`PEER_FEDERATOR_ENABLE_REMOTE_LANES=true` is set. The agent searches `PATH` and
`~/.local/bin` for all three target launchers. Exact paths can be supplied with
`PEER_FEDERATOR_CODEX_LANE`, `PEER_FEDERATOR_CLAUDE_LANE`, and
`PEER_FEDERATOR_GROK_LANE`.

Every parent product can select every target product. The source parent is
agent-attested; it is independent from `--product`, which chooses only the
destination lane adapter. Parent group inheritance is opt-in per launch.

Each destination accepts at most 32 concurrent remote lane CLI processes.
Remote stdin is capped at 1 MiB and auto-archive at 86,400 seconds. Supply the
destination `-C`/`--cd` path explicitly when needed. `--prompt-file` names a
destination-local file; it does not transfer data.

## Linux systemd user service

```sh
make install-systemd-user
cp ~/.config/peer-federator/agent.env.example ~/.config/peer-federator/agent.env
systemctl --user daemon-reload
systemctl --user enable --now peer-federator-agent.service
```

On the hub host, configure and enable the supplied hub unit. Permit TCP 7419
only inside the trusted network.

## macOS launchd

```sh
make install-launchd-user
cp ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist.example \
  ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
plutil -lint ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
launchctl bootstrap "gui/$(id -u)" \
  ~/Library/LaunchAgents/net.antst.peer-federator.agent.plist
```

The install target updates example templates only; it never overwrites or
loads an active plist.

## Failure and upgrades

Local routing and the durable catalog remain available during a hub outage.
Agents reconnect with bounded backoff and republish their current registered
peers. A peer adapter periodically refreshes registration, so an agent restart
does not require a peer restart. Exiting adapters unregister exact identities;
the agent also removes stale registrations after PID/socket revalidation.

Upgrade the hub first, then one agent at a time. Protocol mismatches fail at
hello/probe, and no protocol-2 flat/shadow fallback is attempted.

Before release, run `make lint`, `make test`, `make test-race`, the grouped
two-host federation integration, and installed Linux/macOS parent×target lane
acceptance.
