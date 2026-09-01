# Phase-0 S4: shared component native identity

This isolated spike installs the exact pinned native products into temporary
directories and proves:

- OpenCode 1.18.25 and Kilo 7.5.6 provide the exact native session ID to one
  shared plugin through `shell.env.sessionID`, and the ID reaches the shell
  environment for that exact session;
- Pi 0.84.4 and OMP 18.0.11 provide the exact native session ID to one shared
  extension through both `session_start` and registered-tool contexts, matching
  the products' real JSONL RPC `get_state.sessionId`;
- the planned component v1 frame vocabulary carries those product-specific
  witnesses without replacing them with daemon-wide identity;
- a public capability ID without its ephemeral bootstrap value leaves every
  component inert, while the exact memory-only secret activates it without
  appearing in evidence artifacts.

The deterministic local model only emits the tool call needed to exercise the
real Pi/OMP registered-tool path. No product protocol, hook, session store,
shell execution, or RPC implementation is mocked.

Run from any directory:

```bash
scripts/spikes/six-product/component-context/run.sh
```

Set `KEEP_S4_SCRATCH=1` only for debugging. The default removes all package and
runtime scratch directories. No native credentials or user profile paths are
used.
