# Linux user-service evidence

Date: 2026-08-31 UTC
Host: `pdev`, Linux x86_64
Source commit: `f96de00be296c80d113ae1e4c91ef695652b3ecd` plus the recorded dirty-tree implementation
Verdict: **PARTIAL — source and live service lifecycle green; login-cycle acceptance remains open**

This evidence does not label the dirty-tree build a signed release candidate. It records the live Linux
service work that can be completed without logging out the controlled user or authenticating a vendor
account. The separate four-real-product recovery gate remains blocked by Claude Code being logged out.

## Installed identity and sole authority

The managed install completed through `make install`. The selected installed binary and the built
binary were byte-identical. The first install after the service-controller cutover recorded:

```text
installed/built sha256 = 49c459c7a74c8269e28ee48e228af436aa25da3abbafc6a36d63dc3dab8c4f31
systemd MainPID        = 2197514
daemon generation      = 235
daemon process count   = 1
unit enabled/active    = yes/yes
```

After the readiness-race and release-metadata fixes, the final installed build was again installed by
the same transaction. `agent-sessions status` and `doctor` report schema
`agent-sessions.admin.v1`, `ready:true`, `service_state:"running"`,
`release_present:true`, `endpoint_present:true`, generation 242, four active attachments, and zero
cleanup debts. `pgrep` reported exactly one `.../agent-sessions daemon` process, PID 2210547.

Four same-release connector processes were present. They are stateless stdio MCP relays and not
service authorities. No connector retained the literal `@AGENT_SESSIONS_RELEASE_ID@` placeholder.

## Explicit stop, start, restart, and crash recovery

`agent-sessions daemon stop` delegated to systemd and left the unit `inactive` with no daemon process.
Both `agent-sessions status` and `codex-peer-lane list` returned nonzero while stopped, and a second
service-state check proved neither workflow bootstrapped the daemon.

The first live start attempt found that `systemctl start` can complete just before the control socket
is published. The CLI initially emitted success and an immediate status failed. The implementation was
changed so start and restart poll the metadata-only admin handshake for at most ten seconds before
reporting `completed:true`; `TestWaitForDaemonReadyBridgesServiceManagerSocketPublicationRace`
reproduces the ordering deterministically.

After reinstalling that fix, the exact live sequence passed:

```text
STOP_OK    status_rc=1 workflow_rc=1
START_OK   pid=2202776 generation=238
RESTART_OK pid=2203260 generation=239
CRASH_OK   pid=2203778 generation=240
```

The crash used `SIGKILL` only after `/proc/2203260/exe` resolved to the selected managed
`agent-sessions` binary. systemd's `Restart=on-failure` produced one successor PID and the admin
endpoint became ready at the next generation. Explicit stop did not restart, as required.

Throughout install, stop/start, restart, crash, removal, and reinstall, these external native process
identities remained live and unchanged:

```text
Codex App Server PID 998681
Qwen native PID      1967448
Grok native PID      1967455
```

No vendor process was signalled by the service unit (`KillMode=process`) or controller.

## Failed update preservation

An install was invoked with an invalid Claude executable (`CLAUDE=/bin/false`). Candidate validation
failed with exit 2 before service mutation. The live daemon remained PID 2203778, generation 240,
enabled, active, and reachable. This is the live counterpart to the transaction and controller
failure-injection tests; it does not substitute for a signed-release rollback exercise.

## Removal and reinstall

`make remove-all` stopped and disabled only `agent-sessions.service`, removed its unit, enablement
symlink, managed host integrations, aliases, and release tree, and preserved unified state/config.
The service was `inactive`/`not-found`, the host release and alias were absent, and no daemon process
remained. All three external native PIDs above survived.

`make install` then restored the integrations, exact unit, enablement symlink, selected release, and
one daemon. The installed and built binaries were byte-identical, the daemon reached generation 241,
and all four durable attachments were recorroborated. A final install with the release-metadata fix
reached generation 242 and the fixed admin schema shown above.

## Remaining Linux acceptance

- A real logout/login (or controlled fresh-user login) must still prove default-target activation.
- Four-real-product active-peer/active-lane recovery cannot be green while Claude Code reports
  `claude_logged_in:false` and `Claude Code is not authenticated`.
- The final signed clean-tree release must repeat the live service gate before publication credit.

Therefore T091 remains open despite all other live Linux service lifecycle legs passing.
