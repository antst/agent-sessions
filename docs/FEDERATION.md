# Peer Federator

`peer-federator` makes every live Claude-compatible peer on one trusted LAN host appear as a local
peer on every other participating host. It is session-type agnostic: native interactive Claude
sessions, interactive Codex sessions, and Codex lanes are all ordinary registry records and Unix
socket endpoints to this daemon.

It deliberately remains a separate binary and service from `agent-session-runtime`. The local
runtime owns Codex/App Server and lane lifecycle; `peer-federator` only virtualizes the local
Claude registry and peer socket protocol over the network.

## Architecture

Each host agent reads its local `$CLAUDE_CONFIG_DIR/sessions` registry and sends a live snapshot to
one hub. For every remote peer in the hub roster, the agent launches a tiny local shadow process.
The shadow has a real PID, a numeric Claude registry record, and a connectable Unix socket, so
unmodified Claude and Codex discovery treat it exactly like a local peer. Frames written to that
socket travel through the hub to the actual peer socket on the destination host.

The contract is intentionally small:

- trusted isolated network;
- plain TCP and JSON lines;
- live peers only;
- no credentials, encryption, access policy, offline queue, or HA;
- a hub disconnect removes all remote shadows, cancels active remote command proxies, and rejects
  new remote spawns while local messaging remains untouched.

## Run

Build once on each host:

```sh
make build
```

Start a hub on one stable VLAN host:

```sh
peer-federator hub --listen :7419
```

Start one agent per user/Claude registry on every host, including the hub host:

```sh
peer-federator agent \
  --hub 10.2.17.1:7419 \
  --host workstation-a \
  --name workstation-a
```

Remote peer names are published as `name--host`. The double dash stays within Claude's bare
teammate-name grammar; `@` cannot be used because native `SendMessage` interprets it as special
mention syntax. Local peers retain their original short names.
Only live, connectable records are exported, and records created by the federator are never
re-exported.

The first native name-based send may return a confirmation address such as
`reviewer--workstation-a [a1b2c3]`; resend once with that full value. This is the same confirmation
step used for local peers. Replies through the `from` socket on an incoming message do not need
that confirmation.

Run the hub and agents under the host's normal process supervisor. Agents reconnect with bounded
backoff after a hub restart. If the hub is unavailable, federated shadows disappear; local peers
and local messaging continue normally.

Check a host before enabling its service:

```sh
PEER_FEDERATOR_HUB=10.2.17.1:7419 peer-federator doctor
peer-federator status
```

`doctor` reports live sessions that lack messaging sockets. Those sessions cannot be federated:
interactive Codex requires the `agent-sessions` integration, and Claude Code must be launched with
its peer-messaging feature enabled. See [federation/OPERATIONS.md](federation/OPERATIONS.md) for installation,
systemd/launchd examples, failure behavior, and rolling upgrades.

## Remote lanes

Remote execution is disabled by default. Enabling it authorizes every host connected to this
trusted hub to execute native lane commands as the agent's OS user; those commands can run tools
and read files allowed by the selected lane policy. Enable it only where that trust is intended:

```sh
peer-federator agent ... --enable-remote-lanes
# or PEER_FEDERATOR_ENABLE_REMOTE_LANES=true in agent.env
```

Enabled agents advertise the native lane launchers they can execute. List connected destinations:

```sh
peer-federator hosts
```

Run any native lane command on one of them while preserving its stdout, stderr, and exit code:

```sh
printf '%s\n' 'Inspect the repository and report the failing tests.' |
  peer-federator lane --host workstation-b --product codex -- \
    start --name remote-review -C /srv/project -

peer-federator lane --host workstation-b --product codex -- \
  wait remote-review --timeout 300
```

Use `--product claude` for `claude-peer-lane`. Remote `run`, `start`, and `resume` are made
persistent and receive a notify target pointing back to the originating live Codex/Claude session.
Do not pass `--persistent`, `--notify`, `--no-notify`, or `--no-auto-archive` yourself on those
three commands. The destination cleanup fuse cannot be disabled remotely. Other native flags and
subcommands pass through unchanged. Remote auto-archive delays are capped at 86,400 seconds and
each destination runs at most 32 remote lane CLI processes concurrently. Pass the native
`-C`/`--cd` option on `run` or `start` when the remote working directory matters; otherwise the
command inherits the destination agent service's working directory. `resume` retains its lane's
established cwd. Remote stdin is capped at 1 MiB; `--prompt-file` refers to an already-existing
destination file and does not transfer a local file.

The originating session is inferred only when a live Codex or Claude process is corroborated in
the caller's ancestry. Detached or non-agent automation must pass `--source-session` explicitly.

The product path never uses SSH and destination agents expose no spawn listener. Every request is
local CLI → local Unix socket → connected hub → destination agent. If the local agent cannot see
the hub, the command fails closed. Losing the hub cancels a still-blocking proxy; a detached lane
whose `start` already returned may continue under its native local supervisor, but it cannot
receive cross-host messages or controls until federation reconnects.

The proxy buffers a bounded amount of output per request. A caller that stops reading
stdout/stderr is cancelled rather than allowing one command to consume unbounded memory.

## Development and releases

```sh
make lint
make test
make test-race
```

Forgejo CI runs lint, normal tests, race tests, and all four Linux/macOS architecture builds
concurrently, then publishes archives plus `SHA256SUMS` for version tags only after every job
passes. `make install-systemd-user` also installs both environment templates under
`~/.config/peer-federator/` without replacing active `.env` files. On macOS,
`make install-launchd-user` installs the binary and both `.plist.example` templates under
`~/Library/LaunchAgents` without replacing or loading active `.plist` files. The repository
version is in `VERSION`; a release tag must be exactly `v$(cat VERSION)`.

## Scope

The daemon federates discovery, messaging, and native lane CLI streams. It does not implement
Codex App Server or Claude worker semantics itself: the installed `codex-peer-lane` and
`claude-peer-lane` commands remain authoritative on the destination. A remote lane also appears as
an ordinary federated peer and can be messaged through the hub.
