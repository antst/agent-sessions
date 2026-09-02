# Internal DSH TUI Requirements

This note records what a future terminal UI for DeepSeek Harness must provide
before DSH can satisfy the Agent Sessions peer contract. It is an internal
design input, not a commitment to implement DSH peer mode in the current
release.

## Required product behavior

Requirements are ordered by their importance to a usable peer integration.

1. **CLI-owned lifecycle and identity.** One normal terminal command starts
   the UI. DSH issues a stable session UUID at or near startup, and closing the
   UI or disposing its root gives a truthful lifecycle boundary.
2. **Product-owned naming and resume.** A start-time name is written into
   DSH's own session store. Exact UUID resume is native. A native list or lookup
   returns UUID, title, working directory, and update time so a supplied name
   resolves to exactly one product session or fails truthfully with zero or
   multiple matches.
3. **Exact live-root extension context.** The TUI hosts or tolerates the Agent
   Sessions extension and exposes the exact current root/session to it. Each
   live root has created and disposed events. Each root can therefore hold one
   presence stream reporting `{uuid, name, groups, product}`. Multiple roots in
   one process are acceptable when their lifecycles remain distinct.
4. **Addressable inbound input.** The extension can deliver a peer message to
   one exact root and wake a turn without terminal keystrokes or screen
   scraping. A busy root either accepts a native steer or rejects the input
   truthfully.
5. **Promptless finite tool surface.** The model can invoke the bounded Agent
   Sessions operation vocabulary without an interactive approval prompt, with
   structured arguments and results.
6. **Live product events.** Native title changes and root replacement are
   observable so presence follows DSH's facts and never invents state.
7. **Ordinary terminal operability.** Provider, model, and permission choices
   are launch-scoped; pipes and TTY behavior work in a normal terminal and
   tmux; no browser-only control surface is required.

## What DSH already provides

- **Available headlessly:** ACP create, exact resume, prompt, cancel, and stop;
  stable product session identity; product-owned session storage; provider and
  model execution.
- **Available as substrate:** a plugin/profile ecosystem, structured tool
  registration, and root Agent created/disposed events capable of representing
  multiple concurrent roots.
- **Incomplete for a TUI:** session metadata exists, but a terminal front end
  must expose start-name plus unambiguous list/name/UUID resume as one coherent
  CLI contract.
- **Wrong surface:** the web UI demonstrates interactive multi-root operation,
  but its browser lifecycle is not the daily terminal peer topology.
- **Missing:** a maintained terminal TUI compatible with the pinned DSH
  exports. The genuine terminal front ends tested during integration were
  incompatible. Unrelated DeepSeek TUIs are not a substitute because they do
  not use DSH's sessions, ACP lifecycle, or plugin substrate.

The future implementation should therefore build only the terminal
presentation/lifecycle and the missing native name/list/resume and exact-root
extension wiring. It should reuse DSH's ACP, session store, model execution,
and plugin/tool facilities rather than reimplement them.
