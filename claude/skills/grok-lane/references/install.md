# Install or repair Grok lane support

Do not install or replace host services from an agent session. Ask the operator to exit managed
clients, then run from a host shell:

```sh
make install
make install-grok
grok-peer-lane doctor --json
```

The Grok Build coding CLI is required. A chat-only binary with the same `grok` name is rejected.
On a host with two products, pass the coding CLI explicitly during installation:

```sh
make install-grok GROK=/absolute/path/to/grok-build
```
