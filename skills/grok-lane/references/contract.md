# Grok lane contract v1

## Commands

`run`, `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list [--all] [--mine]`, and
`doctor --json` form contract version 1. Non-persistent start/resume requires a corroborated live
Codex or Claude owner; plain shells and CI must pass `--persistent`.

Grok Build policy options are `-C/--cd`, `--model`, `--reasoning-effort`, `--timeout`, and the
fixed non-interactive `--permission-mode bypassPermissions`. The manager is the sole ACP driver.
It creates with `session/new`, resumes the same UUID with `session/load`, serializes
`session/prompt`, and interrupts with `session/cancel`. It never attaches to an interactive Grok
conversation.

## Events and outcomes

- `thread.started` or `thread.resumed`
- `turn.started`
- `lane.ready`: contract, product, lane identity, address, owner, and auto-archive fields
- `item.completed`: normalized `user_message` or `agent_message`
- `turn.completed`: the only collected terminal event; `status`, `outcome`, `exit`, `error`, and accounting distinguish every result
- `turn.interrupted`: immediate acknowledgement from the `interrupt` command, not the collectable result
- `lane.status`, `lane.list`, `lane.archived`, `lane.doctor`
- `error`; `timeout: true` distinguishes a collection bound

Outcomes are `completed` (0), `failed` (1), `timed_out` (124), and `interrupted` (130). A `wait`
collection timeout also exits 124 but emits no terminal event. Match final answers to the terminal
`turn_id`.

`resume` refuses active, queued, or uncollected terminal debt. Collect it with `wait` first. The
Agent Sessions lane UUID and Grok's ACP-created native UUID are both stable and exposed separately.
Resume of an archived lane restarts a sole-owner worker and loads the exact stored native UUID; it
does not use Grok title matching or scrape Grok's private conversation store.

Archive is bridge-owned: it retires Agent Sessions ownership and processes but preserves the native
Grok transcript. Resume starts a fresh ACP owner and uses `session/load`, not a native unarchive API.
A terminal turn queues a `GROK_LANE_TERMINAL` pointer with a stable native message ID; it is a
collection instruction, never the answer.

## Identity and messaging

The manager publishes only after ACP authentication, an exact resident roster row, live bypass
permission, and a direct `agent_sessions.list_peers` probe. The MCP launch token plus exact process
ancestry authorizes the lane; names and model-supplied session IDs never grant authority.
The local control socket is owner-only and every request must carry the exact stable lane session
ID; it is a same-UID lifecycle boundary, not an authority inferred from a lane name.

Inbound peer messages are durable queued turns. One manager owns one ACP writer, so no peer message
can create a concurrent prompt. Duplicate message IDs with identical content are idempotent;
conflicting fingerprints fail closed. Collected results remain acknowledged across resume.

Remote execution uses `peer-federator lane --host HOST --product grok --`. The destination must
advertise `grok-lane`; federation owns remote persistence and notification flags, while native
JSONL, collection, messaging, and archive semantics remain unchanged.
