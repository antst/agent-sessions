---
description: Check whether the local Codex lane runtime is reachable and satisfies adapter contract 2
---

!`"${CLAUDE_PLUGIN_ROOT}/skills/codex-lane/scripts/lane-preflight"`

The JSON above is a read-only preflight report. Nothing was started, installed, or changed.

Report the result to the user:

- `summary: "ready"` — the runtime is reachable and satisfies contract 2. State the exact validated
  command in `invocation`; do not substitute a launcher from `PATH`.
- `runtime_found: false` — the bridge is not installed on this host. Point the user at
  `skills/codex-lane/references/install.md` inside this plugin. Do not attempt to install, build,
  or start anything. `launcher_found: true` with `runtime_found: false` means the launcher exists
  but the native runtime has not yet been initialized on this host.
- `runtime_found: true` with `contract_ok: false` — the runtime predates adapter contract 2. Report
  the gap using `contract_version`, `list_supported`, and `doctor_supported`, and say that lane
  orchestration should not proceed against it.
- `contract_ok: true` with `runtime_ready: false` — the CLI contract is compatible, but App Server
  or the peer supervisor is unreachable. Report the `doctor` object; do not start host services.

`peer_discovery_cli: false` only means the `claude` executable is not on `PATH`; lane messaging
itself is unaffected.

If the preflight script could not be run at all, say so plainly rather than guessing at the state
of the host.
