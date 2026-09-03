# Troubleshooting

## Check the one daemon

Start with the public diagnostics:

```sh
agent-sessions status
agent-sessions doctor
agent-sessions roster --json
```

On Linux:

```sh
systemctl --user status agent-sessions.service --no-pager
journalctl --user -u agent-sessions.service -n 100 --no-pager
pgrep -a -u "$(id -u)" -f '/agent-sessions daemon$'
```

On macOS:

```sh
launchctl print "gui/$(id -u)/net.antst.agent-sessions"
tail -n 100 ~/.local/state/agent-sessions/logs/agent-sessions.stderr.log
```

There should be exactly one service-managed daemon. Zero or more
`agent-sessions connector ...` processes are normal: products start stateless connector clients for
their live MCP connections. Do not start another daemon to compensate for a connector failure.

Restart through the service manager:

```sh
agent-sessions daemon restart
agent-sessions doctor
```

systemd must terminate the whole daemon-owned control group. Left-over App Server, leader, server,
connector, or lane-profile processes after restart indicate an unclean service stop.

## A peer is missing

1. Confirm the product process is still open.
2. Run `agent-sessions doctor` and inspect the product-specific result.
3. Check that the peer and intended destination share a group from this invocation.
4. Restart only the affected product if it retained an older integration manifest.

Presence is live-only. EOF removes the peer immediately, and a daemon restart begins with an empty
roster until clients reconnect. No registry row or product transcript can make a disconnected peer
live.

If a source manifest still displays `@AGENT_SESSIONS_RELEASE_ID@`, the product is loading source or
stale plugin metadata rather than the selected immutable release. Run `make reinstall`, then restart
only that product.

## Resume cannot find a name

Name selection follows [Products](PRODUCTS.md). Name-capable products receive the selector natively.
ID-only products are resolved from their product session list. With duplicate names, use the native
picker or choose the exact native ID; without a terminal the wrapper prints the product-provided
candidates and exits.

An offline lane appears under `list --all` only if the caller shares a recorded group and the
product confirms the exact ID. A stale candidate row intentionally produces no result.

## A lane is not live after restart

That is expected for daemon-owned workers. The product session survives in its native store:

```sh
PRODUCT-peer-lane list --all
PRODUCT-peer-lane resume NAME_OR_ID --prompt-file follow-up.md
```

Archive also preserves the product session. It closes only the live driver topology. If resume from
a different directory fails, pass the intended current invocation directory with `--cd`; Agent
Sessions does not remember the old cwd.

## Product errors and permission failures

Agent Sessions returns native product and filesystem errors verbatim. Diagnose login, provider,
model, approval, or working-directory failures in the product first. Omitting model, agent, effort,
or permission on resume deliberately restores the product's own default rather than the prior
invocation.

Headless permission is never silently widened. For DSH, `--yolo` selects the product's
`danger-full-access` preset; normal mode selects the installed noninteractive workspace-write preset,
where escalation is rejected instead of prompting forever.

## Message delivery failed

Delivery requires a live visible destination. A synchronous rejection is the destination product's
answer; the daemon does not queue or retry it. Check the destination's native busy/input behavior,
then send again only if the caller chooses to retry.

If a tool or command contradicts Agent Sessions' own documentation and the `gh` CLI is authorized,
the MCP server instructions and fallback internal error provide the issue-report guidance. Include
the exact command, observed behavior, and expected behavior; do not report a documented product
limitation as an Agent Sessions defect.
