# CodeBuddy S3 truth spike

This spike exercises the real pinned `@tencent-ai/codebuddy-code@2.143.0`
binary in an isolated home. It does not use a Tencent account and therefore
never credits model-token completion.

It verifies:

- two interactive TUI workers publish distinct PID/session/loopback endpoint
  records;
- the documented session reply API accepts exact-session input and rejects an
  ID belonging to another worker endpoint;
- concurrent replies use the native busy-safe endpoint;
- exact socket-to-PID ownership, executable/start identity, and wrapper ancestry
  are required in addition to the untrusted registry URL;
- stale rows, PID reuse, and endpoint/port recycling are rejected;
- the same TUI is rediscovered and re-attested across a CodeBuddy daemon or
  Agent Sessions controller restart, with no persistent peer sidecar; and
- whether the native interactive-worker record actually contains the password
  assumed by the proposed credential-sidecar design.

The spike separately proves the lane case: an Agent Sessions-owned
`codebuddy --serve --auth password` receives an ephemeral
`CODEBUDDY_GATEWAY_PASSWORD`, requires it for the API, and does not persist it
in the isolated lane home. Peer endpoints and lane servers are intentionally
different authority/authentication surfaces.

Run from the repository root:

```sh
scripts/spikes/six-product/codebuddy/run.sh
```

The script writes only the redacted evidence record
`specs/004-six-product-support/evidence/phase0/S3-codebuddy.json`. Package,
state, TUI transcript, and HTTP response scratch data live in a private
`mktemp` directory and are removed on exit unless `KEEP_SCRATCH=1` is set.
Raw worker records, generated lane passwords, and native output are never
printed.
