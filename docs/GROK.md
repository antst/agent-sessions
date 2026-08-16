# Grok interactive peers

`grok-peer` runs the ordinary Grok TUI while keeping that exact conversation
reachable by Agent Sessions. It uses a private Grok leader and Grok's official
ACP bridge, so a message starts a turn in an idle TUI without terminal input.
Messages arriving during a turn are serialized and run afterward in the same
TUI.

## Install

Install and log in to the Grok Build CLI first. Then install Agent Sessions and
explicitly trust its local Grok plugin payload:

```bash
make install
make install-grok
```

`grok-peer` validates candidates and skips unrelated chat/desktop executables
also named `grok`. Plugin installation uses the same auto-discovery. On a host
with multiple valid candidates, pin the intended Grok Build executable with
`make install-grok GROK=/absolute/path/to/grok`; a pinned path is still
validated and never falls through silently.

`make install-all` also installs the Codex and Claude integrations. The installer
copies the validated payload to Grok's documented auto-trusted user location,
`~/.grok/plugins/agent-sessions`. Grok 1.0.4 has no direct command for enabling
an auto-discovered user plugin, so the installer briefly registers the source
through Grok's trusted plugin installer, then removes only that registry row
with `--keep-data`. This lets the official CLI update its enabled-plugin
configuration without leaving a second installation. Finally, it requires
`grok inspect --json` to resolve exactly one enabled user plugin at that path
and exactly one `agent_sessions` MCP command at the staged native entry. This allows the
`agent_sessions` MCP server to execute the installed native runtime as your
user; review this repository before granting it. The installer migrates the
older direct-install registry entry because Grok can list that entry as enabled
while omitting its MCP server from a live session. Start a new Grok process (or
reload its plugins) after changing the plugin installation.

Both `make install-grok` and `make install-all` refuse to replace the payload
while any managed `grok-peer` owner, host, private leader, or observer is live
or cannot be verified. `GROK_USER_PLUGIN_ROOT` overrides are accepted only when
their final path component is exactly `agent-sessions`; the installer will not
remove a broader user plugin directory.

## Use

```bash
grok-peer -n reviewer
grok-peer --always-approve -n trusted-reviewer
grok-peer --yolo -n trusted-reviewer
grok-peer --resume 12345678-1234-4234-8234-123456789abc
```

`-n` / `--peer-name` is the only Agent Sessions option. Other options stay in
their original order and are parsed by Grok. Fresh sessions receive a UUID
before the TUI starts; the exit/session UI can be used to retain that UUID.

The initial adapter deliberately accepts only an exact UUID for managed
resume. Bare `--resume`, title resume, `--continue`, and `--fork-session` must
be run with native `grok`; they are rejected rather than resolved by scraping
Grok's private storage. Do not open the same UUID concurrently in another
native Grok process.

Managed peers require the private leader, so caller-supplied `--leader`,
`--no-leader`, and `--leader-socket` are rejected. Grok leaders require
`--sandbox off`; the launcher supplies that transport requirement. This means
the private leader is not OS-sandboxed, independently of whether the TUI asks
before tool use. The infrastructure-only leader and wake client run with an
explicit neutral permission mode so user configuration cannot make them widen
the attached TUI. The TUI itself keeps the user's native argv, configuration,
and admin policy. A peer wake is not approval.

After the wake client observes the TUI-owned session in Grok's official live
session roster, Agent Sessions publishes the effective two-class result:
`bypassPermissions` only when the exact resident session reports `yolo: true`,
otherwise `default`. It refreshes that value while the peer is live, including
after an in-TUI permission change. Missing or ambiguous live state fails
closed instead of guessing from argv.

The same roster is the authoritative coarse activity source. A resident
`working` actor is published as `busy`, `needs_input` as `waiting`, and `idle`
as `idle`; these values are updated from global roster-change notifications
and reconciled by a one-second read-only roster poll. Completed, dormant,
dead, removed, nonresident, missing, duplicate, or unknown actors withdraw the
peer instead of leaving stale discovery state. FleetView does not expose a
load-free fine-grained shell/tool state, so this adapter does not invent
`shell` from generic `working` activity.

The bridge snapshots that live permission class immediately before submitting
an interjection and keeps the labelled snapshot until the first successful
roster refresh after actor acceptance. Grok can defer roster replies while the
interjected model turn is running; during that interval the snapshot prevents
the session's own MCP tools from deadlocking behind the same ACP stream. An
in-TUI permission change made during that interval becomes authoritative after
the turn releases the roster and the refresh completes. The snapshot expires
after 30 minutes even if roster recovery never succeeds; later MCP and lane
authorization then fail closed instead of inheriting an indefinitely stale
permission class.

Once the ACP bridge sees exactly one resident row and a direct read-only
`x.ai/mcp/call` to `agent_sessions.list_peers` succeeds, the peer appears in
`list_peers`. That probe exercises actual plugin discovery, process startup,
MCP initialization, and launch attestation; failures in unrelated MCP servers
do not block it. Delivery uses Grok's
official `x.ai/interject` extension; the wake client deliberately does not
`session/load` the TUI-owned session, because a second load can replace the
resident actor while the TUI is still starting its MCP processes. The installed `agent_sessions` MCP
server lets Grok list/message peers and invoke the existing `codex-peer-lane`
and `claude-peer-lane` launchers with process-attested lifecycle ownership.
The launcher pins that plugin process to the same selected native-runtime
binary as the private host, preventing mixed-revision host/MCP operation.
Native headless Grok worker lanes are being added through `grok-peer-lane`;
they own separate ACP sessions and never attach to or concurrently write an
interactive `grok-peer` conversation. See [GROK-LANES.md](GROK-LANES.md).

Grok's immediate `queued` interjection response is not delivery proof: version
1.0.4 returns it even if a stale resident handle's actor mailbox has closed.
Agent Sessions waits for the matching live actor notification before recording
`actor_accepted`. This proves that the actor began handling the message, while
the matrix's destination-visible reply proves the turn completed. Grok does not
deduplicate repeated interjection IDs; after an ambiguous post-write timeout,
the durable wake remains `in_flight`, is logged, and is never replayed
automatically because replay could duplicate model work.

The private leader and ACP observer are background infrastructure, so their
stdout and stderr are isolated from the interactive TUI. A foreign MCP failure
therefore remains attributed to its server in Grok's `/mcp` view and session
`events.jsonl` instead of appearing as an unrelated top-level fatal in the
prompt. If a managed infrastructure process itself exits, `grok-peer` reports
the failing role without copying private child output into terminal, control,
or durable wake records. Agent Sessions never copies raw third-party output
into model-visible protocol data.

If the peer never appears, run `grok login`, confirm `grok inspect --json`
shows one enabled `agent-sessions` plugin with `scope: "user"` at
`~/.grok/plugins/agent-sessions` and its `agent_sessions` MCP target under that
same directory, and start a new `grok-peer`. The adapter
fails closed when cached CLI authentication or the official ACP contract is
unavailable; it never opens a login browser or falls back to PTY injection.

## Stop leaders safely

A `grok-peer` launch owns a private leader. Exit that Grok TUI normally before
installing a different Agent Sessions version; the host then terminates its
private ACP client and leader and removes their private sockets. That leader is
not the user's ordinary discoverable Grok leader, so a normal peer exit does
not disturb unrelated Grok clients.

For an ordinary shared Grok leader, inspect before stopping it:

```bash
grok leader list
grok leader kill
```

`grok leader kill` stops all discoverable leaders and can disconnect other
ordinary Grok clients. Do not use it merely to close one healthy `grok-peer`.
If a crashed peer leaves a process that is absent from `grok leader list`, first
verify its full command, PID, and process start identity, then send that exact
orphan `SIGTERM`; reserve `SIGKILL` for a process that does not terminate.

Implementation and verification details are recorded in
[`GROK-ADAPTER.md`](GROK-ADAPTER.md) and [`PROTOCOL.md`](PROTOCOL.md).
