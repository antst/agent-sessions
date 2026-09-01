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

The hub does not infer a capability from a product through a closed switch.
The requesting host supplies the product already present on the current wire;
its runtime registry resolves the expected capability before sending.

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

- Old protocol-3 hub or host may drop a capability it does not know. The new
  product is unavailable through that hop; existing products continue.
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
- malformed/oversized/duplicate hostile-client fuzz;
- old-build omission behavior;
- destination registry rejection;
- generation fencing, group admission, parent attestation, disconnect, and
  receipt dedup regressions;
- Linux and macOS service environment projection.
