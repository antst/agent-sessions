# Grok peer adapter — implementation contract and verification record

This records the implemented adapter contract and the evidence required to
change it safely.

Antigravity is parked. Do not follow that approach. Do not import `agy-*`.
The AGY design was: launch token, global hooks, inbox, inject at the next
human invocation. That is lossless mail, not a peer. A message to an idle
agent must start work with no one typing. Queue-until-prompt failed.

Follow **Codex**.

```
codex-peer          grok-peer
     |                   |
 App Server           private grok leader
     |                   |
 TUI + supervisor     TUI + ACP waker
     |                   |
 thread/turn/start    x.ai/interject
```

`codex-peer` owns App Server and starts turns through it. `grok-peer` must
own a private leader (`grok agent leader --leader-socket <private>`) and
start turns through the resident TUI actor with Grok's official ACP extension
`x.ai/interject`. Isolated `grok` (no `--leader`) has
no external inject, same class of hole that parked AGY.

---

## Wake gate — passed on Grok 1.0.4

The blocking feasibility gate passed on 2026-08-15 with Grok 1.0.4
(`d846eb93d9`) on Linux: a private leader accepted idle and in-turn work for an
already-open TUI with no terminal input. The original probe attached a second
ACP client with `session/load` and then used `session/prompt`. A later installed
startup exposed why that must not be the product contract: if the TUI's
`session/new` and the second client's `session/load` overlap, Grok can replace
the resident actor and reclaim the TUI's still-starting MCP process scope.

The implemented transport therefore uses Grok's official `x.ai/interject`
extension. It addresses the existing resident actor directly and handles both
states natively: an idle interjection starts a turn, while a mid-turn
interjection is queued by Grok. The observer never calls `session/load` or
`session/prompt`, so it cannot become a second owner of the TUI session.

The leader socket is **not** an ACP socket. It uses Grok's private leader
framing and registration handshake. ACP clients must connect through Grok's
official stdio bridge:

```text
1. Start:  grok --permission-mode default agent leader --leader-socket $SOCK \
             --no-exit-on-disconnect --relay-on-demand --no-auto-update
2. Attach: grok --leader --leader-socket $SOCK
3. Start:  grok --no-auto-update --permission-mode default --leader-socket $SOCK \
             agent --leader stdio
4. Over the bridge's stdin/stdout: initialize; authenticate with the
   advertised cached_token method; observe the exact resident row through
   `_x.ai/sessions/list`; verify the exact `agent_sessions` plugin through a
   direct read-only `_x.ai/mcp/call` to `agent_sessions.list_peers`; submit
   `_x.ai/interject` with the TUI session ID, text, and durable message ID.
5. Pass: the already-open idle TUI starts a turn with no keystrokes. While a
   turn is active, Grok incorporates the interjection at its next safe point in
   that same turn and the attached TUI remains usable afterward.
```

Use a private socket under a temp dir, never `~/.grok/leader.sock`.
`--sandbox` other than off refuses leader; probe without a sandbox.

The ACP calls use protocol version 1 and newline-delimited JSON-RPC. The exact
sequence is `initialize` with `protocolVersion: 1` and explicit disabled
filesystem/terminal client capabilities, authentication using the advertised `cached_token` method,
The extension request is encoded as method `_x.ai/interject` with direct params
`{sessionId, text, interjectionId}`. The nested generic extension envelope is
not valid for this call in Grok Build 1.0.4.
If cached authentication is unavailable, fail closed and direct the user to
`grok login`; never open a browser from the adapter. Do not use `session/new`,
`session/load`, `session/prompt`, or `session/resume` from the observer. The
bridge must tolerate extension notifications while waiting for the matching
JSON-RPC response.

---

## Implemented architecture

The product is the leader + waker. Hooks are not the delivery path.

### What to build

- `cmd/grok-peer` + `internal/launcher/grok_peer.go`
  - Strip only `-n` / `--peer-name`.
  - For a fresh conversation, generate a UUID before starting children and
    pass it to the TUI with Grok's documented `--session-id`. For the initial
    implementation, managed resume accepts an exact UUID only; reject title,
    bare `--resume`, and `--continue` rather than scraping Grok's private
    storage or parsing a human table.
    This preselection is the readiness observer: the TUI does not need to mint
    or report an unknown ID, and no SessionStart hook is involved.
  - Start a private leader, then `exec grok --leader --leader-socket …`
    with the managed exact session selector and remaining args.
  - Reject `--no-leader`, caller `--leader-socket`, and non-`off`
    `--sandbox`. Fail closed; do not launch an isolated TUI.
  - Passthrough (plain `Exec`, no leader): `--help`, `--version`, and
    subcommands `agent`, `completions`, `dashboard`, `doctor`, `du`,
    `export`, `help`, `inspect`, `leader`, `login`, `logout`, `mcp`,
    `memory`, `models`, `plugin`, `sessions`, `setup`, `trace`, `update`,
    `version`, `worktree`, `wrap`.
- ACP waker: spawn and supervise the official `agent --leader stdio` bridge as
  a second client of that leader. It speaks the existing shim
  control `action=wake` / `wake_status` used by `supervisorOwnsWake` in
  `runtime.go`. Initialize once, authenticate with `cached_token`, observe the
  exact resident session through the roster, and deliver idle or in-turn work
  through `x.ai/interject`. One message id, one durable owner, same as Codex
  `queueWake`. Never attach this observer with `session/load`.
- Readiness: the launcher may exec after the private leader is listening, but
  the host must not publish a registry row until the persistent ACP bridge has
  observed exactly one resident roster row for the preselected session ID. A
  fresh TUI creates that UUID; the bridge retries observation while the exact
  owner is alive. Publication additionally requires a successful direct,
  read-only `_x.ai/mcp/call` to `agent_sessions.list_peers`. Grok 1.0.4's MCP
  catalog omits plugin-only clients, so catalog presence is not a readiness
  signal. Do not retry OAuth or mutate MCP configuration. Unrelated MCP
  failures do not block peer readiness.
- Policy: the TUI alone receives the user's native argv and configuration.
  Start the infrastructure-only leader and ACP bridge with explicit
  `--permission-mode default` so their own configuration cannot widen a
  prompting TUI. The observer's roster and interjection requests do not set
  `_meta.yoloMode` or `_meta.autoMode`; a peer message is not approval. Query Grok's
  official FleetView ACP extension (`_x.ai/sessions/list` wrapping
  `x.ai/sessions/list`) and require exactly one row for the session with
  `resident: true` and a boolean `yolo`. Publish `bypassPermissions` only when
  that live value is true, and refresh it while the peer is live so an in-TUI
  permission change cannot leave stale authority metadata. A missing,
  malformed, duplicate, or dormant row is a fail-closed readiness failure.
  Use the live mode captured immediately before an injected `x.ai/interject` as
  an explicitly labelled snapshot until the first successful post-acceptance
  roster refresh. Grok may defer that refresh for the generated turn, so MCP
  calls use the snapshot rather than blocking on the actor. Bound that
  authority to 30 minutes: if roster recovery remains broken, later MCP and
  lane authorization fails closed rather than inheriting stale privilege.
  Other ACP contention is retryable and supplies no permission class.
- Publish the live session into Claude's registry (`entrypoint: "grok"`)
  so `send_message` can address it. Reuse the existing shim; point
  `supervisorSocket` at the waker.
- MCP `grok-mcp` only after the session is process-attested against the live
  owner (TUI pid + proc-start), host, and private leader. An unguessable launch
  token is written before children start and inherited only by that launch.
  Model-supplied `session_id` is optional corroboration; it does not grant.
  The launcher also propagates its exact selected native-runtime path into the
  TUI environment so the cached Grok plugin cannot invoke a different installed
  revision.
- A process-attested Grok session may own `codex-peer-lane` and
  `claude-peer-lane` children. Use the same deepest-live-ancestor rule as Codex
  and Claude; a model-provided session ID is never authority.
- `fromProduct` / entrypoint allowlist: add `"grok"` next to
  `codex|claude` on this branch. Grep those strings. Do not add `agy`.

### What not to build

- Hook-driven delivery as the product. Grok ignores SessionStart and
  UserPromptSubmit stdout. Stop cannot start an idle turn. Do not
  recreate AGY `injectSteps` / inbox-as-wake.
- Inbox+Stop except as the Codex-style fallback after the waker has
  *not* claimed ownership. An accepted ACP wake must not also be
  Stop-injected.
- `grok-peer-lane` and `--product grok` federation. Grok owning existing
  Codex/Claude lanes is in scope; a native Grok lane product is not.
- `grok -p --resume` against a live pager (dual writer).
- Binding or adopting `~/.grok/leader.sock`.
- Scraping `~/.grok/sessions` or parsing `grok sessions` human output to
  emulate native title resolution.
- PTY/tmux keystroke injection.
- Polling `/loop` or a blocking MCP wait to fake wake.
- Anything from `feature/antigravity-support` `agy-*`.

### Owner and cleanup

Launcher pid becomes the TUI pid (`exec`). That process is the owner. A leader
started before the exec remains a child of the launcher-turned-TUI; the waker's
parent depends on which runtime process supervises it. Do not rely on this
incidental process shape for cleanup. Persist the exact owner, host, leader,
and waker pid/proc-start identities, cwd, name, permission mode, private socket
paths, and selected session ID. The raw launch token is an inherited process
capability and must never appear in argv, readiness output, logs, or disk;
persist only its SHA-256 digest. On owner death, stop only this launch's private
leader and waker and remove its registry row. One launch binds one preselected
Grok `sessionId`. A stale pid with a mismatched proc-start is dead, regardless
of pid reuse.

### Tests

- Launcher: `-n` stripped; `--no-leader` / `--leader-socket` / non-`off`
  `--sandbox` error; passthrough subcommands do not start a leader.
- Waker: fake ACP records one `x.ai/interject` on `action=wake` for an idle
  resident session and asserts that the observer never calls `session/load` or
  `session/prompt`; inbox stays empty; the same message id is not interjected
  twice; ACP failure *before* claim returns an error (shim may inbox).
- Busy delivery: hold one turn active, submit an interjection, and prove Grok
  incorporates it once at a safe point without replacing the resident actor;
  the attached TUI remains usable afterward.
- Lifecycle: waker disconnect/reconnect, normal TUI exit, killed TUI, failed
  authentication, and leader death all converge without a published stale peer.
- Command drift: extract Grok's real top-level command list and require every
  command to be classified, skipping only when `grok` is unavailable.
- Linux and macOS live smokes, including Darwin's shorter Unix-socket path
  budget.
- `make lint`, `make test`, race tests, vet, and all supported cross-builds.
- Readiness and policy: no registry row before exact live roster attestation
  and a successful direct `agent_sessions.list_peers` readiness call; no
  yolo/auto metadata in observer requests; infrastructure
  clients are explicitly neutral; argv, user config, admin policy, and in-TUI
  changes are reflected through the roster's effective `yolo` value.
- Attestation: empty/mismatched session IDs cannot grant authority; a live
  Grok owner can invoke existing Codex/Claude lane launchers, while an
  unrelated process cannot.

### Smoke (the same bar as the gate)

```bash
grok-peer -n grok-reviewer   # terminal A, then leave it idle
# from an attested Codex or Claude peer:
send_message target=grok-reviewer message=GROK-PEER-WAKE
```

Terminal A starts that turn with no typing, remains the attached TUI afterward,
and accepts the user's next prompt. Repeat while a first turn is active and
prove the interjection is incorporated exactly once. If either path fails, the
PR is not done. The implementation PR must repeat this smoke on an installed build; the
design probe is evidence of feasibility, not evidence that the adapter is
correct.

---

## Later, not this draft

`grok-peer-lane` and Grok federation. Only after interactive wake is real.

Do not substitute a hook adapter if a future Grok release breaks this contract.
Fail the installed smoke and revisit the ACP/leader integration instead.
