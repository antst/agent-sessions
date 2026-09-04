# Federation protocol version 4

Protocol 4 is the complete newline-delimited JSON host/hub contract. Every connection begins with
an exact-version `hello`; the hub replies `hello_ok`. A different version is rejected rather than
downgraded. Build strings are diagnostic only and do not affect interoperability.

The hello identifies one daemon by stable `host_id`, display `host_name`, build, and its catalogued
lane capability tokens. Capability tokens are opaque to the hub. Invalid, duplicate, or oversized
input is rejected at the bounded decoder.

## Roster

Whenever membership changes, the hub broadcasts one complete roster to every connected daemon.
Every recipient receives the same sorted host and live-peer projection, including unknown valid
capability tokens. A host replaces its prior remote projection atomically; rosters are not deltas.

A peer projection contains its global and native IDs, product-owned display fields, status,
working directory, permission, groups, parent, and source host. Product is opaque speaker metadata
carried verbatim, not launch authority; Entrypoint is its compatibility fallback. Each is a 1–64 byte
token starting with a lowercase ASCII letter and followed only by lowercase ASCII letters, digits,
or non-leading, non-trailing, non-consecutive hyphens. The daemon never interprets these labels as
paths, commands, selectors, or capabilities. Grouped protocol 1 peers require a source-host private anchor.

## Deliveries

Message requests carry one bounded Agent Sessions frame, source ID, target host, and request ID.
The hub admits only a live source owned by the sending host and routes only to a connected target
host. The destination applies normal group visibility and returns a correlated success or error.

One request ID represents one accepted operation. Reconnect may resend a still-unacknowledged frame
from memory; destination correlation prevents a duplicate result. Neither host nor hub persists the
frame.

## Remote lanes

A lane request carries exactly one product capability, bounded argv, optional input, and the live
parent context. The hub verifies that the destination advertised the requested capability. The
destination daemon revalidates connection, source ownership, capability, arguments, and concurrency
before invoking its normal lane engine. Stdout, stderr, exit, and errors return as correlated bounded
frames.

The wire supports at most 32 concurrent inbound remote lane runs per host, 256 arguments, 512 KiB
of argument data, 1 MiB of input, and 2 MiB per federation frame.

## Lifetime

EOF removes a host and causes a new complete roster broadcast. Reconnection always starts with a
fresh hello. During graceful host shutdown, the daemon stops admission, drains accepted work for
up to two seconds, returns available acknowledgements, and then exits. All federation queues,
correlation maps, and roster state are process memory only.
