# Installation

## Requirements

Agent Sessions supports Linux and macOS on x86-64 and arm64. Building from
source requires Go 1.22 or newer. The host may have any subset of Codex, Claude
Code, Grok Build, and Qwen Code installed; unavailable products are skipped by
`make install-all` and do not prevent the other adapters from working.

Supported native-client baselines for this release are:

| Product | Minimum supported | Acceptance version |
|---|---:|---:|
| Codex CLI | 0.151.0 | 0.151.0 |
| Claude Code | 2.1.251 | 2.1.251 |
| Grok Build | 1.0.13 | 1.0.13 |
| Qwen Code | 0.21.15 | 0.22.3 |

Qwen readiness enforces its semantic version floor. Codex, Claude, and Grok
currently validate required native capabilities instead of comparing a version
string, but versions older than these documented baselines are unsupported.

A release archive contains exactly two executable images for its platform:

- `agent-sessions`, used by the user service and by every installed peer, lane,
  hook, connector, status, and doctor alias;
- `agent-sessions-hub`, the optional central network hub.

The product aliases are symlinks to the same `agent-sessions` image. Vendor
CLIs, the Codex App Server, transcripts, histories, profiles, and credentials
remain vendor-owned and are not release payloads.

On macOS the rendered launchd agent starts in the user's home directory and
records the exact available product executables resolved by the installer,
plus a bounded user-tool `PATH`. The candidate plist is validated before the
loaded agent is replaced. Rerun `make install` after moving or replacing a
native product executable so launchd receives the new absolute path.

## Install a host

From a source checkout:

```sh
git clone https://github.com/antst/agent-sessions.git ~/agent-sessions
cd ~/agent-sessions
make test-race
make install-all
```

From a release archive, verify `SHA256SUMS`, extract the archive matching the
host platform, and run `make install-all` from its top-level directory. The
prebuilt marker makes installation use the packaged image without requiring Go.

The host installation:

1. validates the release and every product connector payload before mutation;
2. stages an immutable host release under `~/.local/libexec/agent-sessions`;
3. switches the selected release atomically;
4. installs aliases under `~/.local/bin`;
5. installs each available native product integration;
6. installs and enables `agent-sessions.service` on Linux or
   `net.antst.agent-sessions` under launchd on macOS; and
7. performs one validated restart of only that Agent Sessions service.

Installation does not stop or restart the Codex App Server, a vendor TUI, a
vendor lane process, or the central hub. Already-open product sessions may keep
their prior plugin snapshot until restarted, but daemon restart/recovery is
designed to preserve their native process and attachment identity.

`make install`, `make dev-install`, and their `*-all` forms use the same unified
host transaction. Explicit product-only legacy install paths are not part of
the version 0.3 operational surface.

## Verify a host

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster
systemctl --user status agent-sessions.service
```

Explicit lifecycle commands remain service-manager operations even when
invoked through the multi-call binary:

```sh
agent-sessions daemon start
agent-sessions daemon stop
agent-sessions daemon restart
```

Peer, lane, hook, connector, messaging, and federation workflows never invoke
those operations and never bootstrap a stopped daemon.

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
```

Then start a fresh real session for each installed product and run its lane
doctor. A native product login or first-run prompt is a product prerequisite,
not something Agent Sessions copies into an alternate profile.

The expected process census is one service-managed `agent-sessions daemon` for
the current user. Zero or more `agent-sessions connector ...` processes may
also appear while product clients have MCP servers open. Those are stateless
stdio relays to the existing daemon, not duplicate daemons. See
[Troubleshooting](TROUBLESHOOTING.md) for exact checks and recovery guidance.

## Configure federation

The installer writes
`~/.config/agent-sessions/service.env.example` when it is absent and preserves
an existing `service.env`. To join the one central hub:

```sh
cp ~/.config/agent-sessions/service.env.example \
  ~/.config/agent-sessions/service.env
${EDITOR:-vi} ~/.config/agent-sessions/service.env
systemctl --user restart agent-sessions.service
```

Set:

```sh
AGENT_SESSIONS_HUB=hub.example:7419
# Optional display name; stable identity defaults to the daemon catalog host.
AGENT_SESSIONS_HOST_NAME=workstation-a
```

On macOS restart with
`launchctl kickstart -k "gui/$(id -u)/net.antst.agent-sessions"`.
See [FEDERATION.md](FEDERATION.md) for the protocol and remote lane surface.

## Install the hub

On the central host:

```sh
make install-hub
systemctl --user status agent-sessions-hub.service --no-pager
```

The installer enables and starts `agent-sessions-hub.service` on Linux or
`net.antst.agent-sessions-hub` under launchd on macOS. The default listen address
is `:7419`; Linux reads an optional `AGENT_SESSIONS_HUB_LISTEN` value from
`~/.config/agent-sessions/hub.env`.

If the central machine also runs peers, install the normal host daemon as well;
the two images are independent processes with different ownership. Host and hub
builds may come from unrelated commits when their hub protocol versions match.

## Update and rollback

An explicit update stages and validates the replacement before changing the
selected pointer. Failed validation leaves the prior selected release and
service state intact. A successful host update restarts exactly one
`agent-sessions` user service. The service manager owns daemon lifetime; peer,
lane, hook, MCP, and federation workflows never start or stop it.

Version 0.3 is a greenfield boundary for the unreleased prototype. The standard
installer contains no old-supervisor discovery, adoption, draining, or cleanup.
For the three controlled development hosts only, the repository retains the
separately reviewed `scripts/cleanup-pre-unification` utility. It is not shipped,
is not reachable from any Make target, and has no authority over vendor-owned
profiles, credentials, transcripts, or histories.

## Removal

Remove only the host integration, service, aliases, and installed releases:

```sh
make remove-all
```

This preserves unified Agent Sessions state/config and all vendor profiles,
credentials, transcripts, histories, and settings. To also delete the unified
Agent Sessions state and configuration roots, use the explicit destructive form:

```sh
make purge-all
```

Remove the independent hub without touching host state with `make remove-hub`.
The pre-unification cleanup utility remains a separate one-time repository tool;
none of these standard removal targets invokes it.

For daemon, connector, and service diagnostics, see
[Troubleshooting](TROUBLESHOOTING.md).
