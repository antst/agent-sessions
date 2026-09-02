# Contract: Local Daemon Control

One private same-user endpoint serves short-lived launchers, hooks, MCP relays, running-daemon
status/doctor requests, and external product coordination calls. Service start, stop, and restart are
performed through systemd or launchd and do not depend on this endpoint.

## Roles

- `admin`: same-user metadata, status, and doctor operations while the daemon is running
- `launcher`: preparation, adoption, refresh, detach, and lane commands
- `hook`: exact managed event for a prepared/adopted attachment
- `connector`: MCP relay for one product and optional exact launch capability

Model-facing roles expose no daemon administration.

## Shared rules

- bounded framed requests and responses with correlation IDs;
- exact daemon generation and same-user peer credentials;
- idempotency keys for externally retryable mutations;
- preparation commits before native authority is published;
- no generic session ID supplied by a model can adopt an attachment;
- a bare connector may initialize and discover tools but receives only the inactive result for calls;
- reconnect after daemon restart never implies attachment adoption;
- errors are cause-specific, carry the handler or product failure to the same-user caller, and are non-successful.

## Product evidence

The protocol carries tagged product evidence defined by the port map. Shared fields include product,
profile, cwd, launch ID, PID/start, and generation. Codex App Server/thread evidence, Claude
registry/socket evidence, Grok launch-token/leader ancestry, and Qwen capability/artifact ancestry remain
distinct variants. The daemon rejects missing, mixed, stale, or cross-product variants.
