# Grok lane contract v1

The command surface is `run`, `start`, `resume`, `wait`, `status`, `interrupt`, `archive`, `list
[--all] [--mine]`, and `doctor --json`. Required events are `thread.started`, `thread.resumed`,
`turn.started`, `lane.ready`, `item.completed`, `turn.completed`, `lane.status`, `lane.list`,
`lane.archived`, `lane.doctor`, and `error`. `interrupt` immediately emits `turn.interrupted` as a
control acknowledgement; the eventual collectable terminal result is still `turn.completed`.

Outcomes and exits are `completed`/0, `failed`/1, `timed_out`/124, and `interrupted`/130. A wait
timeout also exits 124 without terminalizing the turn. Exactly one collector may acknowledge a
turn. Resume refuses uncollected debt and uses ACP `session/load` with the exact stored native Grok
UUID; the Agent Sessions lane UUID remains the stable lifecycle/message identity.

The lane manager is the sole ACP driver. It publishes only after authentication, exact resident
roster identity, live bypass permission, and a direct Agent Sessions MCP probe. Peer messages form
durable serialized turns; duplicate IDs are idempotent and conflicts fail closed.
Its local owner-only control socket rejects a request unless it carries the exact stable lane
session ID. This is a same-UID lifecycle boundary; names and model-supplied IDs grant nothing.

Archive is bridge-owned and preserves the native transcript for a fresh ACP owner to `session/load`;
there is no native Grok archive/unarchive call. `GROK_LANE_TERMINAL` is a durable collection pointer
with a stable native message ID, not answer content. Remote lifecycle uses
`peer-federator lane --host HOST --product grok --`, requires advertised `grok-lane`, and retains
this same JSONL and collection contract.
