# Contract: Federation Protocol 4

Federation is one trusted-environment wire protocol. The first host frame is
`hello` with exact version 4; every other version is rejected before
registration. Build strings are diagnostic.

## Hub

- A valid hello registers the connection immediately. A same-host reconnect
  replaces the old connection.
- A snapshot directly replaces that connection's live peer map.
- Every connected daemon receives the same complete in-memory roster.
- `lane_exec` carries exactly one explicit opaque capability and routes only to
  a live host advertising that capability.
- There is no compatibility marker, per-client filtering, generation
  promotion, prospective-roster admission, or durable hub state.

## Delivery

- Messages route only to live peers.
- Sender success is the destination carrier's acknowledgment.
- Unacknowledged daemon-to-hub frames remain in memory and are resent after a
  reconnect; they are never persisted.
- Shutdown stops new acceptance, drains accepted messages for at most two
  seconds, and then drops anything still pending.

## Trust

The environment is trusted. There is no in-code peer trust boundary. A future
daemon-to-hub app key is the only planned security item and has no present
protocol hooks.
