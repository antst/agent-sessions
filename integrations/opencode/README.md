# Agent Sessions OpenCode plugin

Install `agent-sessions.mjs` in exactly one supported OpenCode plugin location.
The release installer owns the global registration; do not also copy it into a
project `.opencode/plugins` directory or the component and tool will load twice.

The plugin is inert unless an Agent Sessions managed wrapper supplies the
one-time component bootstrap environment. It registers the `agent_sessions`
tool, injects exact session context through `shell.env`, and uses only the
documented plugin SDK.

