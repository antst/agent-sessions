# Grok interactive peers

`grok-peer` runs the ordinary Grok TUI while keeping that exact conversation
reachable by Agent Sessions. It uses a private Grok leader and Grok's official
ACP bridge, so a message starts a turn in an idle TUI without terminal input.
Messages arriving during a turn are serialized and run afterward in the same
TUI.

## Install

Install and log in to the Grok Build CLI first. Then install Agent Sessions and
explicitly trust its local Grok plugin:

```bash
make install
make install-grok
```

`grok-peer` validates candidates and skips unrelated chat/desktop executables
also named `grok`. Plugin installation uses the same auto-discovery. On a host
with multiple valid candidates, pin the intended Grok Build executable with
`make install-grok GROK=/absolute/path/to/grok`; a pinned path is still
validated and never falls through silently.

`make install-all` also installs the Codex and Claude integrations. Grok's
`--trust` allows the `agent_sessions` MCP server to execute the installed native
runtime as your user; review this repository before granting it. Start a new
Grok process (or reload its plugins) after changing the plugin installation.

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

The bridge snapshots that live permission class only while submitting the
brief interjection RPC, so an MCP call that begins before its acknowledgement
does not deadlock against the same ACP stream. The periodic live roster refresh
continues during the model turn; an in-TUI permission change is not pinned for
the duration of that turn.

Once the ACP bridge sees exactly one resident row and a cached `x.ai/mcp/list`
snapshot reports the local `agent_sessions` stdio server ready with
`send_message` enabled, the peer appears in `list_peers`. Failures in unrelated
MCP servers do not block it. Delivery uses Grok's
official `x.ai/interject` extension; the wake client deliberately does not
`session/load` the TUI-owned session, because a second load can replace the
resident actor while the TUI is still starting its MCP processes. The installed `agent_sessions` MCP
server lets Grok list/message peers and invoke the existing `codex-peer-lane`
and `claude-peer-lane` launchers with process-attested lifecycle ownership.
The launcher pins that plugin process to the same selected native-runtime
binary as the private host, preventing mixed-revision host/MCP operation.
There is no native `grok-peer-lane` yet.

If the peer never appears, run `grok login`, confirm the installed plugin is
enabled with `grok plugin list --json`, and start a new `grok-peer`. The adapter
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
