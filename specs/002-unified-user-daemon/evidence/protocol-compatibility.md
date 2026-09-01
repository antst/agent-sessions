# Federation Protocol Compatibility Evidence

## Acceptance boundary

T098 requires unrelated-build, equal-protocol interoperability plus independent host and hub
upgrades and pre-registration rejection of a mismatched protocol. The reusable production-binary
harness is `scripts/federation/binary_pair_test.py`.

The harness has two modes:

- With no binary arguments, it builds four byte-distinct production executables from the current
  tree. This is a local wiring and upgrade smoke test, not unrelated-build acceptance credit.
- With all four `--host-a`, `--host-b`, `--hub-a`, and `--hub-b` paths plus
  `--require-prebuilt`, it refuses to build substitutes and exercises operator-supplied binaries.
  That is the mode required for T098 acceptance evidence.

## Local same-tree production-binary smoke

Environment: Linux x86_64, 2026-08-31.

Command:

```bash
PATH=/usr/local/go/bin:$PATH python3 scripts/federation/binary_pair_test.py
```

Result: PASS.

```json
{"host_builds":["binary-pair-host-a","binary-pair-host-b"],"host_generations":[1,1,2],"host_hashes":["5745a6807afff075aab8209e5aa2c62eb35706c8442ec7de6c83496d24d880c2","4d86e9353e9de2325e09c673c2d5c05cecd84fa8a9e04ae3f0ffe01482bc4137"],"hub_builds":["binary-pair-hub-a","binary-pair-hub-b"],"hub_hashes":["d3dd2b6aa6abbc74b7b2829269cb32cfa923b0357ffdba8a170487077e2ee176","76c997d232f1f00cbaa0086f4ab5323e89c7b57fcfc9dad0bc835e27db938702"],"mismatch_refused_before_registration":true,"mode":"same-tree-smoke","protocol":3,"status":"passed","type":"federation.binary_pair"}
```

This proves that the shipped command paths use the daemon-owned `internal/federation` engine and
that the harness detects binary identity, observes host generation replacement, survives an
independent hub restart, and rejects protocol 4 before registration. It does not prove unrelated
source/build interoperability or macOS operation.

## Unrelated-build acceptance (pending)

Run the following with four independently produced, trusted, executable images. The paths must not
be symlinks; the harness verifies that each host pair and hub pair has different SHA-256 content.

```bash
python3 scripts/federation/binary_pair_test.py \
  --require-prebuilt \
  --host-a /path/to/build-a/agent-sessions \
  --host-b /path/to/build-b/agent-sessions \
  --hub-a /path/to/build-a/agent-sessions-hub \
  --hub-b /path/to/build-b/agent-sessions-hub
```

Record the metadata-only JSON result for Linux and macOS here. Until those runs exist, T098 remains
unchecked and no unrelated-build acceptance claim is made.
