# Contract: Baseline Port Map

`baseline-port-map.yml` is the authoritative machine-readable inventory. This Markdown file is its
review projection and explanatory contract. A Markdown row alone cannot authorize implementation or
deletion.

Every behavior moved from `c056fbc` must have a row before implementation and before deletion. Rows
begin as an inventory; they are expanded with exact tests and new symbols during implementation.

| ID | Baseline symbols | Captured invariant | Intended owner | Mandatory proof before deletion |
|---|---|---|---|---|
| SH-CLI | `internal/launcher/peer_context.go`, product parsers | Groups, names, permissions, native argument delimiter and ordering | shared parser skeleton plus product option tables | all old parser tests and installed argv parity |
| SH-IDENTITY | `internal/procinfo`, `internal/pathidentity`, identity callers | Exact process start/strong-start/ancestry and no-follow filesystem identity | shared process and filesystem identity packages | old identity/PID-reuse/path-type tests plus `S-05` |
| SH-MSG | `internal/bridge/mcp.go`, `internal/federator/groups.go`, routing code | AgentFrame, global groups, discovery/send/multicast/broadcast/reply | daemon delivery plus shared federation package | old group/message tests and destination-ack matrix |
| C-LAUNCH | `internal/launcher/codex_peer.go`, `internal/bridge/launch.go` | lazy App Server, start/resume, name/UUID, cwd, owner and argv behavior | Codex adapter called by daemon | all old Codex launcher/launch tests plus installed lifecycle |
| C-SUP | `internal/bridge/supervisor.go`, `appserver.go`, `hook.go` | App Server coordination, hook attestation, delivery, recovery, cleanup | daemon Codex coordinator | old supervisor/hook/App Server tests plus restart and hook matrices |
| CL-LAUNCH | `internal/launcher/claude_peer.go` | exact profile namespaces, settings merge, gate, native row/socket, late selection, permission refresh, cleanup | shared transaction skeleton plus Claude adapter | every old Claude launcher test and installed Claude lifecycle |
| CL-MCP | Claude paths in `internal/bridge/mcp.go` | native registry/socket and ancestry attestation, direct delivery | stateless relay plus daemon Claude adapter | old Claude MCP/group tests and bare/managed hook matrix |
| G-LAUNCH | `internal/launcher/grok_peer.go` | executable discovery, args, permission, token, TUI/host handoff | shared transaction skeleton plus Grok adapter | every old Grok launcher test and installed Grok lifecycle |
| G-HOST | `internal/bridge/grok.go` | exact owner/host/leader/MCP ancestry, ACP roster, wake/interjection, late selection | daemon Grok coordinator with only required stateless vendor helper | old Grok host/MCP tests and real ancestry/delivery matrix |
| Q-LAUNCH | `internal/launcher/qwen_peer.go` | profile/runtime, readiness, args, permission, capability, dual output, rollback | shared transaction skeleton plus Qwen adapter | every old Qwen launcher test and installed Qwen lifecycle |
| Q-HOST | `internal/bridge/qwen*.go` | daemon/ACP ancestry, event/input evidence, delivery, archive, cleanup | daemon Qwen coordinator | all old Qwen host/authorization/archive tests and real matrix |
| L-SHARED | `internal/bridge/lane*.go` | durable turn, notices, collection, cleanup, parent context | daemon shared lane engine | old common lane tests plus 4×4 matrix |
| L-PRODUCT | product lane/manager files | native launch/resume/permission/archive/interrupt differences | product lane adapters | old product lane tests plus all applicable parent/target cells |
| FED | `internal/federator/*`, `cmd/peer-federator` | one hub, host agent, global groups, routing, reconnect, remote lanes | daemon host component plus `agent-sessions-hub` | old federation tests and cross-host matrix |
| INSTALL | Makefile, package/release/plugin installers | optional products, transactional connectors, exact removal and owner-state preservation | shared host/hub install and service implementation | old install/release tests and real Linux/macOS service cells |

## Completion rule

Each YAML entry stores its current highest completed status. Status moves monotonically across manifest
revisions, and the validator applies cumulative predicates: a later status is invalid unless its fields
and evidence also satisfy every earlier applicable gate. A separate list of historical scalar statuses
is intentionally not authoritative. The validator rejects implementation when `old_symbols`,
`old_tests`, `invariant`, `new_owner`, or `replacement_tests` is empty. Deletion requires status
`removable`, exact `new_symbols`, all linked acceptance-cell results, and evidence paths. Deletion review
must show that every deleted old symbol belongs to a row whose focused, installed, and cleanup evidence
is green. Aggregate test output is insufficient.
