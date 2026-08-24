# US5 deployment-example review

Reviewed at `2026-08-23T21:13:37Z` on Linux amd64 before the T082 version freeze. The source
baseline was `4402e9abe2499d1d99e81ad68ec7935b3c6665b9`; the examples already contained the T061
Qwen changes and were not edited during this review.

## Inputs

| Example | SHA-256 |
|---|---|
| `deploy/peer-federator/systemd/user/agent.env.example` | `a023042b4343e23dcd3ff6f4de3403d1d3bf72900ff795834c1b69703505d44d` |
| `deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example` | `213d409b2c551cf1738b5223695bf6c77f2684b17411882eb79d7eedaf5d7316` |

Tool labels: Go `go1.26.5 linux/amd64`, Qwen Code `0.22.0`. The repository binary still
reported `0.2.0`, as required before T082 takes exclusive ownership of the `0.2.4` version change.

## Validation

- `systemd-analyze verify deploy/peer-federator/systemd/user/peer-federator-agent.service`: PASS.
- `bash -n deploy/peer-federator/systemd/user/agent.env.example`: PASS.
- Python `plistlib.load` of the launchd example: PASS. `ProgramArguments` selects `agent`, includes
  the hub and host, and does not opt into `--enable-remote-lanes`; the environment contains exactly
  the documented Qwen launcher/profile variables in addition to the template's fixed launch data.
- `go test ./internal/qwenprofile -count=1`: PASS. This proves unset profile variables preserve the
  native default, explicit values are canonical absolute paths without mutable symlink ambiguity,
  set-empty values fail, and both variables participate in profile identity.
- Focused launcher and federator Qwen/profile tests: PASS. The agent consumes
  `PEER_FEDERATOR_QWEN_LANE`, withholds Qwen capability until the shared readiness engine succeeds,
  and retains the one-agent runtime lock.
- Every runtime lane role (`lane`, `claude-lane`, `grok-lane`, `qwen-lane`) returned help directly.
  The review exposed and fixed a class defect where wrapper help attempted runtime/plugin activation
  first; `TestLaneHelpDoesNotActivateRuntime` now requires help to use read-only runtime discovery,
  while `TestLaneCommandsStillActivateRuntime` preserves activation for real commands.

## Contract conclusions

- The examples open no inbound TCP listener. The agent owns one private local control socket and an
  optional outbound hub connection; only the separately configured hub command listens.
- Remote lane execution remains explicitly disabled in the systemd example and omitted from the
  launchd arguments. Enabling it is an operator trust decision.
- Omitting `QWEN_HOME` and `QWEN_RUNTIME_DIR` selects Qwen's native profile. Supplying them binds
  readiness, plugin verification, peer/lane launch, and federation advertisement to the exact same
  profile. No example copies or embeds credentials.
- The optional Qwen launcher override is product-specific configuration, while the runtime, state,
  hub, host, and listener contracts remain shared with the other products.

Result: PASS. The T061 examples match the frozen behavioral contracts and need no further edit.
