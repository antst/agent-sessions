# CodeBuddy integration

Agent Sessions targets CodeBuddy Code **2.143.0**.

Interactive sessions report their product UUID, native title, launch groups,
and product `codebuddy` over the ordinary live-session connection. As with
every product, disconnect means gone; the daemon keeps no CodeBuddy session
registry, worker record, process identity, or socket-owner copy.

Lanes use the product's loopback HTTP job API. Open starts only the owned native
server. The first prompt creates the product session and returns its UUID; later
operations use that exact UUID. Start, detail, stream, reply, respawn, stop,
delete, and resume results come directly from CodeBuddy. A failed or busy
native operation is returned to the caller instead of entering a daemon queue.

The owned lane server uses its native password and CSRF conventions in memory.
No credential enters the durable lane-candidate table. Doctor checks the exact
CLI version, API shape, and a bounded native health/job round trip.

Tencent model-turn acceptance remains a real-product release cell; local API
fixtures do not claim it.
