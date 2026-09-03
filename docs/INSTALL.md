# Installation

Agent Sessions supports Linux and macOS on amd64 and arm64. A source build requires Go 1.22 or
newer. DSH integration also requires pnpm 10.28.1. See [Products](PRODUCTS.md) for the accepted
native product versions.

## Install a host

From a source checkout:

```sh
git clone https://github.com/antst/agent-sessions.git
cd agent-sessions
make test
make install
```

`install`, `install-all`, `dev-install`, and `dev-install-all` use the same host transaction. It:

1. builds or validates the two release binaries;
2. stages an immutable release under `~/.local/libexec/agent-sessions`;
3. installs the available product integrations and aliases under `~/.local/bin`;
4. selects the release atomically; and
5. installs and restarts one user-managed Agent Sessions service.

The binaries are `agent-sessions` and the optional `agent-sessions-hub`. Every peer, lane, connector,
doctor, and lifecycle alias resolves to the same `agent-sessions` image. Product CLIs, profiles,
credentials, settings, histories, and session stores are never copied into the release.

An unavailable product is skipped without blocking the other integrations. A native product still
owns login and first-run setup; complete those through the product itself before starting its first
managed peer or lane.

## Verify the installation

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
agent-sessions catalog --json
```

On Linux:

```sh
systemctl --user status agent-sessions.service --no-pager
journalctl --user -u agent-sessions.service -n 100 --no-pager
```

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
```

There must be exactly one service-managed daemon for the current user. Product-launched
`agent-sessions connector ...` processes are stateless clients of that daemon and may be present in
any number while managed products are open.

Lifecycle commands delegate to the native user service manager:

```sh
agent-sessions daemon start
agent-sessions daemon stop
agent-sessions daemon restart
```

Peer, lane, connector, messaging, and federation commands require the service to be running. They
do not start a replacement daemon.

## Installed product integrations

The host transaction uses each product's supported installation surface. It installs product
plugins or extensions from the selected immutable Agent Sessions release, not from a temporary
directory. The DSH adapter is packed once and installed into the `agent-sessions` DSH profile with
DSH's native plugin command; its profile manifest therefore points at release-owned bytes.

Fresh launches receive one uniform context:

```text
AGENT_SESSIONS_PRODUCT
AGENT_SESSIONS_SESSION_NAME
AGENT_SESSIONS_GROUPS
AGENT_SESSIONS_SESSION_ID       # only when the native identity is known
```

Tools-only connectors may also receive `AGENT_SESSIONS_LANE_CAPABILITY` and
`AGENT_SESSIONS_HOST_BINARY`. A fresh product-generated identity deliberately receives neither a
provisional session ID nor a provisional private anchor. The integration learns the product ID
natively and the daemon derives the final anchor once.

## Federation hub

Install the independent hub on the central host:

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

Host daemons read optional configuration from `~/.config/agent-sessions/service.env`:

```sh
AGENT_SESSIONS_HUB=hub.example:7419
AGENT_SESSIONS_HOST_NAME=workstation-a
```

The default hub listener is `:7419`; Linux may override it through
`~/.config/agent-sessions/hub.env`. See [Federation](FEDERATION.md).

## Update and removal

`make reinstall` refreshes integration manifests and performs the same validated host transaction.
A successful update changes the selected release and restarts exactly the Agent Sessions user
service. systemd's normal control-group behavior terminates the daemon and its lane worker tree.
The product-owned Codex App Server runs outside that service and remains available to live Codex
clients; other product session state remains in the product and returns through confirmed resume.

Remove Agent Sessions while preserving all Agent Sessions and product state:

```sh
make remove-all
```

Delete the Agent Sessions state and configuration roots only with the explicit destructive target:

```sh
make purge-all
```

Remove the independent hub with `make remove-hub`. No removal target deletes product profiles,
credentials, histories, transcripts, or session stores.
