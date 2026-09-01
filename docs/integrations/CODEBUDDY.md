# CodeBuddy integration

CodeBuddy support targets exactly `@tencent-ai/codebuddy-code` 2.143.0 and is
experimental. The complete offline PEER, LANE, and PARENT protocol paths are
implemented. The Tencent-authenticated real model-turn acceptance cell remains
pending and must never be reported as passed from offline evidence.

## Two distinct HTTP surfaces

The managed interactive peer is the user's product-owned TUI. CodeBuddy writes
an interactive worker row under its native session registry and exposes a
literal-loopback per-session endpoint. The endpoint has no peer password. It
requires only the constant `X-CodeBuddy-Request: 1` CSRF header; that header is
not authentication and is never represented as a durable credential.

The lane is a different surface. Agent Sessions starts and supervises a
per-lane `codebuddy --serve --auth password` process on literal loopback. Its
generated password is passed through `CODEBUDDY_GATEWAY_PASSWORD` as sensitive
environment material, retained only by the live client, excluded from durable
state and diagnostics, and checked for accidental native persistence by the
integration doctor. A fresh lane open proves only the health and ownership of
that server and returns an explicitly unbound native-session reference. The
pinned product does not expose a native session until the first job is
accepted, so health is never misreported as native-session authority.

There is no CodeBuddy component, sidecar, peer secret, or shared peer/lane
endpoint.

## Peer launch and adoption

`codebuddy-peer` execs the pinned native executable with:

```text
--session-id <managed-id>
--strict-mcp-config
--mcp-config <installed integrations/codebuddy/mcp.json>
```

It also supplies `AGENT_SESSIONS_SESSION_ID` and
`AGENT_SESSIONS_PRODUCT=codebuddy`. No bootstrap secret is accepted.

The registry URL is only a claim. Adopt, refresh, delivery, and rename perform
the following bounded proof again:

1. select exactly one credential-free `kind:"interactive"` registry row for
   the expected native session and cwd;
2. capture the row PID's strong process-start identity;
3. prove the literal-loopback listening socket is owned by that exact PID;
4. verify the native executable/Node entrypoint and wrapper ancestry;
5. query `/api/v1/sessions/live` and require the exact session ID;
6. re-read the registry identity/digest and process identity to close row,
   PID, and port-reuse races.

Linux uses `/proc/net/tcp{,6}` plus the exact PID's fd table. The shipped
`CGO_ENABLED=0` macOS build invokes the OS `/usr/sbin/lsof` in bounded field
mode and requires the exact PID, listener address, fd, device, and node to be
identical in two complete snapshots. Optional cgo builds use
`proc_pidinfo(PROC_PIDLISTFDS)` and
`proc_pidfdinfo(PROC_PIDFDSOCKETINFO)` with the same double-snapshot rule.
Missing commands, ambiguous output, changed descriptors/nodes, and
unsupported or unverifiable platforms fail closed. The pure-Go parser and
race cases are covered offline; physical macOS ownership proof remains a
pending evidence cell and receives no acceptance credit. A daemon restart
simply re-discovers and re-attests the still-live product worker; it does not
reconnect a sidecar.

Inbound idle and busy messages use the supported
`POST /api/v1/sessions/{id}/reply` endpoint. A successful driver result means
the endpoint returned `delivered:true` for the exact live session. Rename uses
`POST /api/v1/sessions/{id}/rename` and requires the native 204 response.

## Lane lifecycle

Lane mechanics use only pinned self-described API operations:

- dispatch: `POST /api/v1/jobs`;
- follow-up/steer: `POST /api/v1/jobs/{id}/reply`;
- completion: `GET /api/v1/jobs/{id}` plus
  `GET /api/v1/jobs/{id}/stream` SSE;
- interrupt: `POST /api/v1/jobs/{id}/stop`;
- wake a saved reply: `POST /api/v1/jobs/{id}/respawn`;
- exact resume: `POST /api/v1/jobs/resume`;
- archive: guarded `DELETE /api/v1/jobs/{id}`.

Input is opened from the daemon receipt spool and its exact length and SHA-256
are reverified before native I/O. On a fresh unbound lane, the driver snapshots
the supported job list, assigns the receipt an exact secret-free marker, and
posts the first job. The product-generated session UUID in that authoritative
response becomes the lane's native identity; Agent Sessions atomically stores
the receipt acceptance, native dispatch handle, and lane binding before it
commits delivery. Fresh lane launch omits both `--session-id` and
`AGENT_SESSIONS_SESSION_ID`; no managed lane ID or caller value is presented as
native authority. If the POST may have written but its response is lost or
malformed, retry lists jobs and accepts only one new job with the exact marker
and cleaned cwd. It never replays the POST or adopts a pre-existing, unmarked,
malformed, or ambiguous job.

After that one-time binding, every accepted detail, respawn, follow-up, and
resume job must retain the exact product session UUID and cleaned lane cwd.
The first binding turn uses the exact product-returned job ID as its durable
native acceptance anchor. For later replies and respawns, CodeBuddy reuses that
job ID, so Agent Sessions assigns a fresh receipt-derived driver handle; an old
turn reference therefore cannot steer, wait on, or interrupt a later respawn.
SSE events wake authoritative polling but never replace the terminal job detail
with intermediate or prompt text. An immediate job reply is native steer
acceptance; because that endpoint supplies no message ID, the driver does not
invent one. A `saved:true` reply has already mutated CodeBuddy pending state;
the driver therefore consumes it through exact respawn and returns acceptance.
If respawn cannot be proven after the save, delivery is ambiguous and is never
reported unsupported or automatically replayed. Durable resume construction
requires a recovery source that reconstructs the exact
cwd and permission inputs; resume rejects any substituted native session or
cwd. Guarded archive refusal is cleanup debt, not success. Once native DELETE
is confirmed, its ephemeral progress is retained so a failed server-close
proof retries shutdown without deleting the job again; archive succeeds only
after authoritative owned-process exit.

Default permission mode maps to CodeBuddy `default`. The broader
`bypassPermissions` policy fails closed unless the lane was explicitly granted
the sandbox-only bypass mode; only then is `CODEBUDDY_IS_SANDBOX=1` supplied.
Peer settings are never changed.

## Parent connector

The installed MCP configuration runs:

```text
agent-sessions connector codebuddy --release-identity <installed-release>
```

The daemon uses kernel local-peer identity plus the connector's exact process
start and ancestry to select one active CodeBuddy attachment. A claimed native
session ID is corroborating only. False IDs, ambiguous ancestry, cross-target
processes, and component-binding/sidecar claims are rejected. Rename and resume
retain authority only after the new live evidence is re-attested.

## Doctor and readiness

Doctor first requires `codebuddy --version` to equal 2.143.0. Feature and
integration depths then start an isolated Agent Sessions-owned authenticated
server, verify health, and inspect the self-served OpenAPI title, version,
required paths, HTTP methods, and operation IDs. Integration depth snapshots
jobs, dispatches one exact marked offline job, corroborates its new product
session ID, cwd, and marker through the job list, and requires guarded deletion.
This keyless protocol round trip was reproduced on the pinned binary, but it is
not model-turn credit. The binary's `/runs` implementation rejects the generic
message body described by its own OpenAPI, so that route receives no readiness
credit. Doctor stops the owned process and scans the isolated native state for
the generated password. It checks the supported job-list and dispatch schemas
needed for deferred binding, but does not require `/sessions/live` to contain
an ID on an unbound fresh server.

The report exposes `tencent-model-turn:false` in every offline result. Offline
readiness can be green while catalog support remains experimental and
federation advertisement remains gated by central support-state policy.

## Failure categories

Missing processes/endpoints map to `Unavailable` or `Stale`; wrong credentials
to `Unauthorized`; row/socket/session ambiguity to `AmbiguousSession`; API
shape drift to `Protocol`/`Incompatible`; an unprovable respawn after a saved
busy input to `AmbiguousSession`; unsafe archive refusal to `CleanupDebt`.
Native detail is bounded and redacted, and no endpoint, registry payload,
prompt body, or secret is copied into product-owned durable state.
