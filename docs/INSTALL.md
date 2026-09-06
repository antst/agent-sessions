# Installation

Sessionbus supports Linux and macOS on amd64 and arm64. A source build requires Go 1.24 or
newer. DSH integration also requires pnpm 10.28.1. See [Products](PRODUCTS.md) for the accepted
native product versions.

## Install a host

From a source checkout:

```sh
git clone https://github.com/antst/sessionbus.git
cd sessionbus
make test
make install
```

`install`, `install-all`, `dev-install`, and `dev-install-all` use the same host transaction. It:

1. builds or validates the two release binaries;
2. stages an immutable release under `~/.local/libexec/sessionbus`;
3. installs the available product integrations and aliases under `~/.local/bin`;
4. selects the release atomically; and
5. installs and restarts one user-managed Sessionbus service.

The binaries are `sessionbus` and the optional `sessionbus-hub`. Every peer, lane, connector,
doctor, and lifecycle alias resolves to the same `sessionbus` image. Product CLIs, profiles,
credentials, settings, histories, and session stores are never copied into the release.

An unavailable product is skipped without blocking the other integrations. A native product still
owns login and first-run setup; complete those through the product itself before starting its first
managed peer or lane.

Codex's App Server daemon starts its MCP helper from one installed configuration shared by every
interactive session. Set those peers' groups when installing the host:

```sh
make install CODEX_GROUPS='["project","review"]'
```

The default is `[]`. Codex cannot carry a different group set from each TUI launch to that helper,
so `codex-peer -g/--group` reports the install setting instead of discarding the requested group.

## Verify the installation

```sh
sessionbus status
sessionbus doctor
sessionbus roster
sessionbus catalog --json
```

On Linux:

```sh
systemctl --user status sessionbus.service --no-pager
journalctl --user -u sessionbus.service -n 100 --no-pager
```

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.sessionbus"
```

There must be exactly one service-managed daemon for the current user. Product-launched
`sessionbus connector ...` processes are stateless clients of that daemon and may be present in
any number while managed products are open.

Lifecycle commands delegate to the native user service manager:

```sh
sessionbus daemon start
sessionbus daemon stop
sessionbus daemon restart
```

Peer, lane, connector, messaging, and federation commands require the service to be running. They
do not start a replacement daemon.

## Installed product integrations

The host transaction uses each product's supported installation surface. It projects the
release-owned OpenCode and Kilo plugin files into those products' installation roots. Managed Pi
and OMP launches project their extension assets from the selected immutable release on every
launch. DSH alone is packed and installed into the `sessionbus` DSH profile with DSH's native
plugin command; its profile manifest therefore points at release-owned bytes.

Fresh launches receive one uniform context:

```text
SESSIONBUS_PRODUCT
SESSIONBUS_SESSION_NAME
SESSIONBUS_GROUPS
SESSIONBUS_SESSION_ID       # only when the native identity is known
```

Tools-only connectors may also receive `SESSIONBUS_LANE_CAPABILITY` and
`SESSIONBUS_HOST_BINARY`. A fresh product-generated identity deliberately receives neither a
provisional session ID nor a provisional private anchor. The integration learns the product ID
natively and the daemon derives the final anchor once.

## Federation hub

Install the independent hub on the central host:

```sh
make install-hub
systemctl --user status sessionbus-hub.service --no-pager
```

Host daemons read optional configuration from `~/.config/sessionbus/service.env`:

```sh
SESSIONBUS_HUB=hub.example:7419
SESSIONBUS_HOST_NAME=workstation-a
```

The default hub listener is `:7419`; Linux may override it through
`~/.config/sessionbus/hub.env`. See [Federation](FEDERATION.md).

## Update and removal

`make reinstall` refreshes integration manifests and performs the same validated host transaction.
A successful update changes the selected release and restarts exactly the Sessionbus user
service. systemd's normal control-group behavior terminates the daemon and its lane worker tree.
The product-owned Codex App Server runs outside that service and remains available to live Codex
clients; other product session state remains in the product and returns through confirmed resume.
Connectors from a previous install refresh after their next completed tool response. Requests already received when the refresh is pending are answered with a stale-connector error; the connector then refreshes once, and retries use the new image.

Remove Sessionbus while preserving all Sessionbus and product state:

```sh
make remove-all
```

Delete the Sessionbus state and configuration roots only with the explicit destructive target:

```sh
make purge-all
```

Remove the independent hub with `make remove-hub`. No removal target deletes product profiles,
credentials, histories, transcripts, or session stores.
