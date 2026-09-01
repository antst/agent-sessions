# Contract: Protocol-3 Opaque Product Capabilities

No wire field or version changes. Protocol remains 3.

## Hub Rules

The hub treats host capability strings as bounded opaque identifiers:

- non-empty lower-case ASCII token syntax;
- fixed maximum length, count, and aggregate bytes;
- sorted and deduplicated;
- no product lookup at registration;
- exact target-advertisement check before forwarding `lane_exec`;
- group/source/parent/generation checks remain unchanged.

The hub does not generally infer a capability from a product through a closed
switch. The sole exception is the frozen protocol-3 compatibility map for an
empty `lane_exec` capability: `codex`, `claude`, `grok`, and `qwen` map to their
original lane capabilities. Empty capabilities for new or unknown products are
rejected. The reserved `federation-peer-products` transport marker is never
lane-dispatchable.

New protocol-3 hosts advertise `federation-peer-products` in the existing
`Host.Capabilities` field. A new hub sends complete opaque-product peer rows
only to marked clients. For unmarked clients it removes the whole peer row
unless the effective product is one of the frozen original four; groups,
targets, and other peer fields are not partially projected.

## Destination Rules

The destination accepts a lane request only when:

1. product exists in its data catalog;
2. runtime registry has a lane driver for that product;
3. requested capability exactly equals a descriptor capability;
4. doctor currently reports required readiness;
5. parent/group/generation/permission checks pass.

Unknown or not-ready products return explicit unsupported/unavailable errors.
There is no fallback to another product or local host.

## Mixed Builds

- A pre-feature host tolerates the unknown feature capability but rejects an
  entire roster containing an unknown `Peer.Product`; the new hub's per-client
  filtering preserves original-four federation in this mixed topology.
- The new hub must be the roster distributor. A pre-feature hub cannot perform
  this filtering and therefore cannot safely connect a strict pre-feature host
  to a topology containing new-product peers.
- A new host never assumes an old host supports a new product merely because
  both speak protocol 3.
- No retroactive compatibility shim, product alias, or version downgrade is
  introduced.

## Security Scope

The documented trusted-network assumption remains. This feature does not add
TLS or authentication and must not describe federation as safe on an untrusted
network. Authentication/encryption is separate tracked design work.

## Tests

- live `internal/federation` hub integration, not legacy federator;
- opaque known/unknown capability pass-through and exact target matching;
- malformed/oversized/duplicate hostile-client fuzz, including prospective
  per-client post-filter roster size before last-good snapshot replacement;
- old-host marker asymmetry: unknown host capability is tolerated while an
  unknown peer product is rejected;
- live mixed-build filtering: an unmarked host stays connected, receives the
  complete original-four roster with no partial new-peer leak, and a marked
  host receives the full roster on initial publication and updates;
- destination registry rejection;
- generation fencing, group admission, parent attestation, disconnect, and
  receipt dedup regressions;
- Linux and macOS service environment projection.
