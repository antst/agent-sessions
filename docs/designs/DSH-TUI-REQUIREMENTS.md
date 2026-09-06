# Internal DSH TUI requirements

This is the internal product contract for a future DeepSeek Harness terminal UI, including dashi.
The shipped Sessionbus DSH profile already implements lane mode as a native protocol-v1 client;
the TUI must supply the interactive peer lifecycle without replacing DSH's session or agent core.

## Required product behavior

1. **CLI-owned lifecycle and identity.** One normal terminal command starts the UI. DSH supplies a
   stable session ID, and closing or replacing a root produces an observable lifecycle boundary.
2. **Product-owned naming and resume.** Start-time names are written through DSH's session
   controller and flushed to its store. Exact-ID resume and session listing return native ID, title,
   cwd, and update time.
3. **Exact live-root plugin context.** The TUI exposes its current root to the Sessionbus plugin
   and emits a root-changed event. Each root holds its own protocol-v1 presence connection after the
   app-ready signal and closes it in the plugin disposer.
4. **Addressable native input.** `Agent.steer` delivers ordinary inbound messages at the next step
   boundary or starts work while idle. `Agent.followup` creates a tracked lane-style turn when that
   distinction is needed. The product's receipt and consumption events carry DSH message IDs.
5. **Promptless finite tools.** The model can call the closed Sessionbus operation vocabulary
   without a new interactive approval prompt. Host-supplied native identity, not a model argument,
   identifies the caller.
6. **Live product events.** Title, model, cwd, root replacement, input receipt, user consumption, and
   terminal turn reasons are observable from DSH's public Cordis services.
7. **Ordinary terminal operation.** Provider, model, reasoning, and permission choices remain native
   launch facts. Pipes, TTY operation, tmux, SIGINT, and SIGTERM behave as normal terminal surfaces.

## Native protocol responsibilities

The TUI integration speaks [Native Sessionbus Presence Protocol v1](../specs/NATIVE-PEER-PROTOCOL.md)
directly over `presence.sock`:

- `session.hello` reports DSH's native ID, stored title, launch groups, product `dsh`, and live info;
- `session.update` follows product title/info changes;
- structured `message.deliver` is rendered once and submitted through `Agent.steer`;
- peer and lane tool requests use the first-class v1 methods; and
- one connection is held per live root, so EOF means that root is gone.

The DSH installation's Cordis singleton services must remain singleton. Every `@deepseek-ai/*`
service, `cordis`, and `cordis-plugin-loader` used by the integration is therefore a peer dependency,
matching the shipped lane profile and dashi packaging.

## Reuse, not replacement

DSH already owns session creation/resolution, persistence, rename, model selection, permission
presets, user-message creation, inbox receipt, turn correlation, interruption, and flush. The TUI
adds presentation, current-root lifecycle, and the native presence connection. It must not open the
session store directly, invent message IDs, infer a turn from timing, or allow two processes to
write one session.

Candidate inspection uses `sessionController.list`, which does not claim a writer. Root creation and
resume use `sessionController.create` and `resolveAgent`. Archive cancels with the inbox preserved,
waits for idle, flushes, and exits the app so the owning process releases the session.
