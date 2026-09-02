# CodeBuddy integration payload

This payload is loaded only by the managed `codebuddy-peer` wrapper through
CodeBuddy's supported `--mcp-config` and `--strict-mcp-config` flags.

It does not install a component, sidecar, daemon service, or global product
setting. The connector reports the product-owned session over the shared
`presence.sock` stream. Incoming messages are passed to CodeBuddy's own live
worker endpoint for that exact UUID.

The files under `.codebuddy/` provide instructions only. Disconnecting the
connector removes the session from Agent Sessions immediately.
