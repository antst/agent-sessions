# Contract: Uniform Federation Protocol 4

Federation is one complete wire protocol. The first host frame is `hello` with
the explicit exact `version` field set to 4. A protocol-4 hub accepts only 4;
every other value is rejected before registration, `hello_ok`, snapshot
admission, or roster publication. Build strings remain diagnostic and do not
participate in compatibility.

This exact-version rejection is the current forward-version mechanism. An N+1
participant against an N hub takes the same clean fail-closed path as any other
mismatch and never runs partially under N semantics. A later range-negotiated
protocol may be introduced only by defining an explicit first-frame range and
selecting one complete version before registration. Product, capability, build,
or binary-age inference is forbidden.

## Hub rules

- Capability strings are bounded, lower-case opaque tokens. Registration
  validates, sorts, and deduplicates them without product lookup.
- Every `lane_exec` carries exactly one explicit capability. Empty,
  duplicated, multiple, or invalid capability values fail closed. The hub has
  no product-to-capability compatibility map.
- The destination must advertise that exact capability. Source ownership,
  group, parent, generation, and readiness checks remain unchanged.
- Each ready daemon publishes a complete snapshot. Before replacing one
  snapshot, the hub computes the single prospective complete roster and checks
  its encoded byte, host-count, and peer-count bounds.
- A reconnecting same-host candidate remains pending through `hello_ok`. Its
  initial snapshot must pass the prospective uniform-roster check before an
  atomic promotion; only after promotion may the hub retire the prior live
  generation.
- Every admitted client receives the same roster object. There is no transport
  feature marker, old-host asymmetry, product filtering, or per-client roster.
- An inadmissible prospective roster does not replace the last-good snapshot,
  disconnect unrelated incumbents, or publish partial state.

## Destination rules

The destination accepts a lane request only when the product exists in its
catalog, its runtime registry exposes a lane driver, the explicit capability
matches the descriptor, live readiness passes, and parent/group/generation/
permission checks pass. Failure never falls back to a different product, host,
or inferred capability.

## Security and durability scope

The trusted-network/no-TLS/no-auth assumption is unchanged. Durable receipt
ownership, destination-owned receipt metadata, single-writer state, the lane
input ledger, the component protocol, and native-session write-once rules are
unchanged by the federation version boundary.

## Required cells

1. Every version mismatch, including N+1 against N, is rejected before
   registration and does not disturb an incumbent.
2. Clients with different capability sets receive byte-equivalent complete
   initial and updated rosters.
3. Prospective roster amplification, including from a higher-generation
   same-host reconnect, is rejected before promotion; the same-host last-good
   roster/connection and unrelated incumbents remain live.
4. Empty capability requests fail closed for every product, while one explicit
   unknown opaque capability routes only to an exact advertisement.
