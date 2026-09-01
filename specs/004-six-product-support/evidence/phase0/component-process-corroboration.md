# Optional Component Process Corroboration Evidence

Status: **FABLE-APPROVED NARROW AMENDMENT**

Amendment round: **1**

The component wire remains protocol version `1` and contract revision
`agent-sessions.component.v1-r1`. No frame, envelope, durable record, or state
transition is added.

`bootstrap` and `reconnect` retain `process_start` and `strong_start` as an
optional all-or-none pair:

- omitting both is accepted;
- supplying exactly one is rejected as an invalid frame;
- supplying both requires an exact match with independently captured live
  process identity;
- kernel socket credentials plus daemon-captured process identity, executable,
  and ancestry remain authoritative;
- a foreign process is rejected even when both claim fields are omitted.

The shared JavaScript client remains active when neither launch environment
entry exists and omits both keys from bootstrap and reconnect payloads. A
partial environment pair remains inert and never connects.

Closure tests cover omitted, matching, partial, mismatch, and omitted-claim
foreign-process cases for both Bootstrap and Reconnect, plus shared-client wire
omission and partial-environment fail-closed behavior.
