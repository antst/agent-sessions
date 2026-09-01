# CodeBuddy integration payload

This payload is loaded only by the managed `codebuddy-peer` wrapper through
CodeBuddy's supported `--mcp-config` and `--strict-mcp-config` flags.

It does not install a component, sidecar, peer password, daemon service, or
global product setting. The interactive TUI remains product-owned and is
adopted from its native worker registry only after fresh socket-to-PID,
executable/start, ancestry, and live-session checks. The authenticated
`codebuddy --serve` lane is a separate Agent Sessions-owned process whose
generated password exists only in memory.

The files under `.codebuddy/` provide instructions only. Authority always
comes from the live attested connector.
