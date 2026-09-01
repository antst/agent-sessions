# Kilo Phase-0 S1 spike

This spike pins the real `@kilocode/cli` package at `7.5.6` and proves the
peer topology required by the six-product design. Kilo's server, attached TUI,
HTTP/SSE APIs, MCP client, background-process manager, session store, and tool
execution are native. Only the model is replaced by a deterministic local
OpenAI-compatible fixture.

Run from anywhere inside the repository:

```sh
scripts/spikes/six-product/kilo/run.sh
```

The harness creates a mode-0700 temporary root, installs the exact npm version,
uses isolated `HOME` and XDG roots, starts two password-authenticated loopback
servers, and attaches one full TUI to each server. Passwords exist only in
process/tmux memory. Successful cleanup stops native background jobs, TUIs,
servers, event readers, and the model fixture, then removes the scratch root.
Set `KILO_S1_KEEP=1` only when diagnosing a failure.

The executable assertions cover:

- exact A-to-A and B-to-B `/tui/*` routing with zero cross-delivery;
- idle wake and busy-turn queue ordering through the full attached TUI;
- native busy/idle completion events;
- session rename, visible title update, and resume of the same native ID;
- MCP connection on both isolated servers;
- native `background_process` attribution to the exact TUI session; and
- the negative `--mini` result described below.

Exact Kilo parent context is intentionally not reprobed here. The harness
requires the canonical S4 evidence and verifies that it contains a passing
Kilo 7.5.6 `shell.env.sessionID` match before S1 starts. That avoids duplicating
the component-identity spike.

## Topology decision

Peer mode must use **one authenticated `kilo serve` plus one full
`kilo attach` TUI per peer**. Kilo 7.5.6's `/tui/*` controller routes are
server/TUI control operations, so isolated servers give an exact session
boundary and remove ambiguous multi-TUI routing.

`kilo attach --mini` is not a supported peer surface. At the pinned version it
can read/resume a session, but `/tui/append-prompt` plus `/tui/submit-prompt`
neither renders nor submits into mini. The harness records that negative
explicitly; wrappers must launch the full TUI.

The mock MCP process speaks the real stdio MCP framing, and the model fixture
can emit a real `background_process` tool call. Neither helper implements or
replaces a Kilo protocol.
