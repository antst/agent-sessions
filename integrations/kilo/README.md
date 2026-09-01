# Agent Sessions Kilo plugin

Install `agent-sessions.mjs` in exactly one Kilo-supported plugin location. Use
the release-managed global registration or one project plugin, never both
`.opencode/plugins` and `.kilo/plugins`, because Kilo 7.5.6 loads both.

The plugin activates only for an Agent Sessions-owned isolated `kilo serve`
process. Managed peer mode requires a full `kilo attach`; `--mini` is not an
Agent Sessions peer surface.

Exact resume is selected only from the daemon-prepared native `ses_*` and cwd:
the isolated server must return that exact session and the full attach is
rendered with `--session <id>`. User overrides, `--continue`, and latest-session
fallback are rejected.
