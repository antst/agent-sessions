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

After the wake client loads the session, Agent Sessions queries Grok's
official live session roster and publishes the effective two-class result:
`bypassPermissions` only when the exact resident session reports `yolo: true`,
otherwise `default`. It refreshes that value while the peer is live, including
after an in-TUI permission change. Missing or ambiguous live state fails
closed instead of guessing from argv.

Once the ACP bridge has loaded the exact session, the peer appears in
`list_peers` and can receive `send_message`. The installed `agent_sessions` MCP
server lets Grok list/message peers and invoke the existing `codex-peer-lane`
and `claude-peer-lane` launchers with process-attested lifecycle ownership.
The launcher pins that plugin process to the same selected native-runtime
binary as the private host, preventing mixed-revision host/MCP operation.
There is no native `grok-peer-lane` yet.

If the peer never appears, run `grok login`, confirm the installed plugin is
enabled with `grok plugin list --json`, and start a new `grok-peer`. The adapter
fails closed when cached CLI authentication or the official ACP contract is
unavailable; it never opens a login browser or falls back to PTY injection.

Implementation and verification details are recorded in
[`GROK-ADAPTER.md`](GROK-ADAPTER.md) and [`PROTOCOL.md`](PROTOCOL.md).
