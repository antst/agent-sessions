# Pre-unification runtime inventory

This is the closed Agent Sessions runtime inventory frozen by T001 before process topology changes.
It describes `develop` commit `c056fbc5015d4ab0a673f66cac5404206f7bcee6` (subject `Merge Qwen
support`, tree `bb61de9f4ba4399cf0c62fb5b7a78a1896251189`). It is descriptive evidence,
not a design for another namespace or a compatibility promise for old executables.

The inventory is closed over shipped release roles, their documented service definitions, and their
durable records. Migration may inspect only these sources and corroborating exact identities. It must
not discover candidates through broad home-directory walks, process-name substring searches, or a
scalar process count. Native vendor processes are listed separately because Agent Sessions coordinates
but does not absorb or own them.

## 1. Shipped release inventory

[`scripts/release-inventory`](../../../scripts/release-inventory) is the baseline authority. A release
contains eleven separately built host-side executables, four plugin payloads, and four platforms.

| ID | Shipped executable | Current command role | Lifetime at baseline | Unified disposition |
|---|---|---|---|---|
| EXE-01 | `agent-session-runtime` | Internal multi-role runtime: bootstrap, supervisor, shim, App Server control, four lane engines, three detached lane managers, Grok/Qwen interactive hosts, hook, MCP, plugin, and release helpers | Mixed: long-lived authorities and short-lived helpers | All host code moves into the `agent-sessions` image; authority roles become daemon components and helper roles become local clients/connectors. |
| EXE-02 | `peer` | Generic product selector | Short-lived; execs a product launcher | Installed alias of `agent-sessions`. |
| EXE-03 | `codex-peer` | Managed Codex interactive launcher | Short-lived; execs native Codex after preparation | Installed alias of `agent-sessions`; remains a client/launcher. |
| EXE-04 | `claude-peer` | Managed Claude interactive lifecycle wrapper | Long-lived for the native Claude child | Installed alias of `agent-sessions`; lifecycle ownership moves into the daemon before the wrapper execs native Claude. |
| EXE-05 | `grok-peer` | Managed Grok interactive launcher | Short-lived; starts a host and execs native Grok | Installed alias of `agent-sessions`; host ownership moves into the daemon. |
| EXE-06 | `qwen-peer` | Managed Qwen interactive launcher | Short-lived; starts a host and execs native Qwen | Installed alias of `agent-sessions`; host ownership moves into the daemon. |
| EXE-07 | `codex-peer-lane` | Codex lane CLI | Short-lived client; may cause supervisor/shim work | Installed alias of `agent-sessions`; calls the daemon. |
| EXE-08 | `claude-peer-lane` | Claude lane CLI | Short-lived client; starts a detached manager | Installed alias of `agent-sessions`; calls the daemon. |
| EXE-09 | `grok-peer-lane` | Grok lane CLI | Short-lived client; starts a detached manager | Installed alias of `agent-sessions`; calls the daemon. |
| EXE-10 | `qwen-peer-lane` | Qwen lane CLI | Short-lived client; starts a detached manager | Installed alias of `agent-sessions`; calls the daemon. |
| EXE-11 | `peer-federator` | Central hub, per-host agent, diagnostics, remote-lane CLI, and lane watcher | Hub and agent are services; lane watcher is operation-lived | Host-agent behavior moves into the daemon. The hub moves to the distinct `agent-sessions-hub` image. |

The plugin payload inventory is exactly `codex` (`.agents`, `.codex-plugin`, `.mcp.json`, `hooks`,
`scripts`, `skills`), `claude` (`.claude-plugin`, `claude`), `grok` (`grok`), and `qwen` (`qwen`).
The release platforms are exactly Linux amd64, Linux arm64, Darwin amd64, and Darwin arm64.

## 2. Agent Sessions-owned long-lived and operation-lived roles

The source links below identify the baseline implementation seam. “Operation-lived” means a process
can remain for minutes or hours while a peer/lane/remote operation exists even though it is not an OS
service.

| ID | Process role and baseline command | Cardinality and owned responsibility | Implementation evidence | Unified disposition |
|---|---|---|---|---|
| PROC-01 | Central hub: `peer-federator hub` | One central deployment; TCP listener, host roster, group routing, delivery and remote-lane relay | [`cmd/peer-federator/main.go`](../../../cmd/peer-federator/main.go), `runHub`; [`internal/federator/hub.go`](../../../internal/federator/hub.go), `RunHub` | Remains central and separate as `agent-sessions-hub`; it is not a per-user host authority. |
| PROC-02 | Host routing/federation agent: `peer-federator agent` | One per configured host-agent instance; owns local catalog, group resolution, host-suffixed projections, local control socket, outbound hub connection, remote delivery and remote lane dispatch | [`internal/federator/agent.go`](../../../internal/federator/agent.go), `RunAgent`; [`internal/federator/route.go`](../../../internal/federator/route.go) | Embed once in the per-user daemon; preserve one-hub/multiple-agent behavior and global groups. |
| PROC-03 | Codex supervisor: `agent-session-runtime supervisor run --plugin-version V` | One per canonical `CODEX_HOME` profile key; owns App Server subscription, interactive ownership, Codex lanes, wakes/notices, and shim children | [`internal/bridge/supervisor.go`](../../../internal/bridge/supervisor.go), `nativeSupervisor.start` and `runSupervisorCommand` | Refactor into one daemon-owned Codex coordinator; no separate supervisor process or listener. |
| PROC-04 | Codex delivery shim: `agent-session-runtime shim ...` | One per managed interactive Codex peer or Codex lane; owns a session delivery socket, registry/state row, inbox and heartbeat | [`internal/bridge/runtime.go`](../../../internal/bridge/runtime.go), `runShimMain`, `newDaemon`, `daemon.start`; spawned by `nativeSupervisor.ensureShim` | Refactor `daemon` into an in-process attachment actor; no per-session process or socket. |
| PROC-05 | Claude peer lifecycle wrapper: `claude-peer ...` | One per managed Claude TUI; parents native Claude, prepares the exact native socket/profile, publishes/refreshes registration, enforces durable permission mode, and performs exact cleanup | [`internal/launcher/claude_peer.go`](../../../internal/launcher/claude_peer.go), `RunClaudePeer` | Move lifecycle/watch/registration into a daemon attachment actor; the launcher remains only until it execs native Claude. |
| PROC-06 | Grok interactive host: `agent-session-runtime grok-host ...` | One per managed Grok peer; owns launch record, control listener, private leader/ACP observer, roster/permission reconciliation, delivery and cleanup; embeds a `daemon` delivery endpoint | [`internal/bridge/grok.go`](../../../internal/bridge/grok.go), `grokHost.start`, `runGrokHostCommand`; launcher in [`internal/launcher/grok_peer.go`](../../../internal/launcher/grok_peer.go) | Become a daemon-owned Grok attachment actor; retain only native Grok children. |
| PROC-07 | Qwen interactive host: `agent-session-runtime qwen-host ...` | One per managed Qwen peer; owns native process admission, event/input handling, delivery, registration and cleanup; embeds a `daemon` endpoint | [`internal/bridge/qwen_host.go`](../../../internal/bridge/qwen_host.go), `runQwenHostCommand` | Become a daemon-owned Qwen attachment actor; retain only native Qwen children. |
| PROC-08 | Claude lane manager: `agent-session-runtime claude-lane-manager ...` | One detached process per live Claude lane; owns lane control socket/state/queue/turns/notices, native stream worker, registration and cleanup | [`internal/bridge/claude_lane.go`](../../../internal/bridge/claude_lane.go), `runClaudeLaneManager`, `claudeLaneManager.start` | Become a daemon-owned Claude lane actor; retain the native Claude worker. |
| PROC-09 | Grok lane manager: `agent-session-runtime grok-lane-manager ...` | One detached process per live Grok lane; owns lane control, ACP session/worker, turns, archive/cleanup, registration, and embedded delivery endpoint | [`internal/bridge/grok_lane_manager.go`](../../../internal/bridge/grok_lane_manager.go), `runGrokLaneManager`, `grokLaneManager.start` | Become a daemon-owned Grok lane actor; retain the native ACP worker. |
| PROC-10 | Qwen lane manager: `agent-session-runtime qwen-lane-manager ...` | One detached process per live Qwen lane; owns lane control, ACP session/worker, turns, archive helper/cleanup, registration, and embedded delivery endpoint | [`internal/bridge/qwen_lane_manager.go`](../../../internal/bridge/qwen_lane_manager.go), `runQwenLaneManager`, `qwenLaneManager.start` | Become a daemon-owned Qwen lane actor; retain the native ACP worker/helper only while required. |
| PROC-11 | Remote lane watcher: `peer-federator lane-watch ...` | One per in-flight remote lane CLI; holds a liveness FD, supervises the product lane process, and returns streams/exit | [`internal/federator/lane_watch.go`](../../../internal/federator/lane_watch.go); spawn path in [`internal/federator/lane.go`](../../../internal/federator/lane.go) | Dispatch directly from the embedded federation component to an in-process lane actor; remove watcher/CLI/manager chains. |
| PROC-12 | MCP stdio connector: `agent-session-runtime mcp` or `grok-mcp` | One vendor-spawned connector per MCP attachment; currently performs caller attestation, tool dispatch, routing, and lane launch in-process | [`internal/bridge/mcp.go`](../../../internal/bridge/mcp.go), `runMCPCommand`, `runGrokMCPCommand` | A vendor-required process may remain only as a stateless stdio relay to the daemon. It owns no catalog, delivery, lane, or cleanup authority. |

There is no separate Codex lane manager: `agent-session-runtime lane` uses the shared supervisor/App
Server, while each live Codex lane receives PROC-04. The common `bridge.daemon` type is also embedded
inside PROC-06, PROC-07, PROC-09, and PROC-10; that reuse does not make those distinct processes one
authority at baseline.

## 3. Native vendor process boundary

These processes may remain external. Their existence does not violate the future one-daemon census,
and migration/removal must never kill or rewrite them merely because Agent Sessions is changing.

| ID | Native process family | Agent Sessions relationship |
|---|---|---|
| NATIVE-01 | Codex App Server and interactive Codex TUI | Vendor-owned history/thread authority. Agent Sessions coordinates through App Server operations and exact thread/process identity. |
| NATIVE-02 | Claude TUI and Claude stream-json lane worker | Vendor-owned transcript, registry, permission and model execution. Agent Sessions supplies managed launch inputs and corroborates native rows/sockets. |
| NATIVE-03 | Grok TUI, private leader, ACP observer and lane ACP worker | Vendor-owned conversation/model execution. Agent Sessions owns only the launch/lane coordination records and exact cleanup boundary. |
| NATIVE-04 | Qwen TUI/daemon, ACP worker and bounded archive helper | Vendor-owned chats, profile, modes and model execution. Agent Sessions owns admission, routing and lane coordination only. |

## 4. Socket and listener inventory

| ID | Baseline endpoint | Owner and use | Source |
|---|---|---|---|
| SOCK-01 | Hub TCP listener, default `:7419` | PROC-01 federation handshake and routed frames | [`cmd/peer-federator/main.go`](../../../cmd/peer-federator/main.go) and [`internal/federator/hub.go`](../../../internal/federator/hub.go) |
| SOCK-02 | `<peer-federator-runtime>/agent.sock` | PROC-02 local registration, preferences, roster, delivery, diagnostics and lane dispatch | [`internal/federator/agent.go`](../../../internal/federator/agent.go), `RunAgent` |
| SOCK-03 | `<bridge-runtime>/supervisor-<profile-key>.sock` plus historical `supervisor.sock` | PROC-03 Codex control/status/delivery/lane authority | [`internal/bridge/supervisor.go`](../../../internal/bridge/supervisor.go), `resolveNativePaths` |
| SOCK-04 | `<bridge-runtime>/session-<session-key>.sock` | PROC-04 and embedded `daemon` peer/lane delivery endpoint; real `0600` socket in the current schema | [`internal/bridge/runtime.go`](../../../internal/bridge/runtime.go), `newDaemon` |
| SOCK-05 | `<host-agent-runtime>/cp-<session-key>.sock` | Native Claude messaging socket selected and cleanup-owned by PROC-05 | [`internal/federator/util.go`](../../../internal/federator/util.go), `ClaudePeerMessagingSocketPath` |
| SOCK-06 | `<grok-runtime>/g-<launch-key>/control.sock` and `leader.sock` | PROC-06 host control and native private leader | [`internal/bridge/grok.go`](../../../internal/bridge/grok.go), `resolveGrokHostPaths` |
| SOCK-07 | `<bridge-runtime>/cl-<session-key>.sock` | PROC-08 Claude lane control | [`internal/bridge/claude_lane.go`](../../../internal/bridge/claude_lane.go), `claudeLaneControlSocket` |
| SOCK-08 | Grok lane's `g-<launch-key>/control.sock` plus embedded `session-*.sock` | PROC-09 lane control and delivery | [`internal/bridge/grok_lane_manager.go`](../../../internal/bridge/grok_lane_manager.go) |
| SOCK-09 | `<bridge-runtime>/qw-<session-key>.sock` plus embedded `session-*.sock` | PROC-10 Qwen lane control and delivery | [`internal/bridge/qwen_lane.go`](../../../internal/bridge/qwen_lane.go), `qwenLaneControlSocket` |
| SOCK-10 | Vendor-native sockets and ACP stdio streams | NATIVE-01 through NATIVE-04; corroborated channels, not Agent Sessions authoritative listeners | Product adapters under [`internal/bridge`](../../../internal/bridge) |

The unified host target replaces SOCK-02 through SOCK-09 with one private local daemon endpoint and
in-process calls. SOCK-01 remains the separate central hub listener; SOCK-10 remains vendor-owned.

## 5. Durable and ephemeral root inventory

| ID | Baseline root or record family | Current contents/authority |
|---|---|---|
| STATE-01 | `${CLAUDE_PEER_DATA_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/claude-code-peer}` | Shared bridge root: `native-runtime-path`, `sessions/<key>/state.json` and inboxes, plus legacy global records. Derived in [`internal/bridge/supervisor.go`](../../../internal/bridge/supervisor.go). |
| STATE-02 | `STATE-01/profiles/<CODEX_HOME-key>/` | Per-Codex-profile `supervisor.json`, interactive owners, retired records, Codex `lanes`, Claude/Grok/Qwen lane records and logs, worktrees, wakes, notices, Grok launches/locks, and cleanup debt. |
| STATE-03 | `${XDG_STATE_HOME:-$HOME/.local/state}/agent-sessions/agents/<host>/` | PROC-02 authoritative host catalog `sessions.json`, durable name/group projection, preparation journals, cleanup debt, and product lifecycle roots. Derived by [`internal/federator/util.go`](../../../internal/federator/util.go), `DefaultStateDir`. |
| STATE-04 | `<Claude-config>/sessions/` and product-native profiles | Vendor-owned native registry/profile/transcript state consulted for corroboration; never an Agent Sessions migration or purge target. |
| STATE-05 | `$XDG_RUNTIME_DIR/codex-claude-peer-$UID`, compact `/tmp/ccp-$UID`, and shipped Darwin `TMPDIR` variants | Ephemeral bridge supervisor, session, and lane sockets/locks. The compact-root logic is in [`internal/bridge/runtime.go`](../../../internal/bridge/runtime.go) and [`internal/bridge/supervisor.go`](../../../internal/bridge/supervisor.go). |
| STATE-06 | `${XDG_RUNTIME_DIR:-system-temp}/peer-federator[-$UID]` | PROC-02 `agent.sock` and instance/registry locks. Derived by [`internal/federator/agent.go`](../../../internal/federator/agent.go), `DefaultRuntimeDir`. |
| STATE-07 | `<runtime>/agent-sessions-grok-$UID/g-<launch-key>/` | Grok private launch control/leader sockets and launch-owned runtime files. |
| STATE-08 | Product-selected Qwen runtime plus bounded `qwen-tools-*` roots | Qwen native runtime/events and temporary authenticated archive helper; vendor state remains outside Agent Sessions ownership. |

The authoritative migration-source closure is STATE-01, STATE-02, STATE-03, STATE-05, STATE-06, and
STATE-07 plus the exact service records below. STATE-04 and vendor-owned portions of STATE-08 are
corroboration/exclusion boundaries, never broad cleanup targets.

## 6. Service inventory

Only the peer-federator roles ship OS-service definitions at baseline. Supervisors, shims, interactive
hosts and lane managers are detached or peer/lane-owned processes, not systemd/launchd services.

| ID | Platform record | Baseline command/lifecycle | Unified disposition |
|---|---|---|---|
| SERVICE-01 | [`deploy/peer-federator/systemd/user/peer-federator-agent.service`](../../../deploy/peer-federator/systemd/user/peer-federator-agent.service) | `peer-federator agent`, restarted on failure | Legacy host-agent migration candidate; replaced by the one standard `agent-sessions` user service. |
| SERVICE-02 | [`deploy/peer-federator/systemd/user/peer-federator-hub.service`](../../../deploy/peer-federator/systemd/user/peer-federator-hub.service) | `peer-federator hub`, restarted on failure | Replaced independently by `agent-sessions-hub`; never part of host migration. |
| SERVICE-03 | [`deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example`](../../../deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example) | Example `net.antst.peer-federator.agent` job | Legacy host-agent candidate if installed; replaced by the standard Agent Sessions LaunchAgent. |
| SERVICE-04 | [`deploy/peer-federator/launchd/net.antst.peer-federator.hub.plist.example`](../../../deploy/peer-federator/launchd/net.antst.peer-federator.hub.plist.example) | Example `net.antst.peer-federator.hub` job | Replaced independently by the hub LaunchAgent. |

## 7. Short-lived command inventory

These are behavior surfaces, not independent authorities: the four peer launch commands; four lane
commands (`doctor`, `run`, `start`, `resume`, `wait`, `status`, `list`, `interrupt`, `archive` as
applicable); `peer-federator` diagnostics/hosts/lane client; runtime bootstrap, App Server safety,
hook, launch preparation/bind, Grok safety/plugin verification, Qwen plugin install/remove, and
release packaging/evidence commands. Their current dispatch is closed by
[`internal/bridge/runtime.go`](../../../internal/bridge/runtime.go),
[`internal/launcher`](../../../internal/launcher), and
[`cmd/peer-federator/main.go`](../../../cmd/peer-federator/main.go).

After unification they remain short-lived calls into the user-managed daemon or hub and must fail when
that required service is unavailable. They must not start, stop, restart, replace, or supervise the
daemon as a side effect of a peer, lane, messaging, plugin, connector, or federation workflow.

## 8. Closure statement

The complete baseline runtime candidate set is PROC-01 through PROC-12, SOCK-01 through SOCK-10,
STATE-01 through STATE-08, SERVICE-01 through SERVICE-04, EXE-01 through EXE-11, and NATIVE-01
through NATIVE-04. PROC-01/SOCK-01 and SERVICE-02/SERVICE-04 belong to the separate central hub.
NATIVE-01 through NATIVE-04 and vendor portions of SOCK-10/STATE-04/STATE-08 are explicit non-targets.
All remaining authority roles are converged into one per-user-host daemon; there is no unnamed
product, profile, environment, namespace, or process class to add during implementation without first
amending this baseline evidence.
