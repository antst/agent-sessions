# Real v0.3.0 federation compatibility probe

This spike compiles a helper against the exact pre-feature source at
`679fe9d3068b6362df867f8d78ce6708c4ce1342` (`v0.3.0`) and runs that binary
against the current `internal/federation` hub and host implementation. The
current baseline host advertises one peer for each original product (Codex,
Claude, Grok, and Qwen), alongside a live filtered OpenCode peer.

```sh
./scripts/spikes/six-product/federation-old/run.sh
RACE=1 ./scripts/spikes/six-product/federation-old/run.sh
```

The runner exports the old commit with `git archive`, copies only the stable
probe entrypoint into that source tree, and builds it there. The old process
therefore imports the real v0.3.0 `internal/federation` package rather than a
model of its validator. All build caches and extracted source live under one
`mktemp` directory and are removed on exit. It does not install or start a user
service and does not write user configuration or runtime state.

The supported mixed-version topology is intentionally one-way: the **new hub
must distribute rosters**. It can see the transport marker and remove complete
new-product peer rows for an unmarked protocol-3 client. An old hub cannot make
that per-client distinction. A new-product snapshot sent to the strict old hub
is rejected at old snapshot admission, so an old hub with a new peer plus a
strict old host is unsupported; it must not be treated as an upgrade topology.
