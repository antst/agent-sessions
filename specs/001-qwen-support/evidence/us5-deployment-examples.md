# US5 deployment-example review

Revalidated on 2026-08-24 on Linux amd64 before the T082 version freeze. The reviewed behavior tree
is parented by `642829eca0e18bd6949689563998fce78e30e02d`. The T059A correction separates the native
Qwen executable used for readiness from the `qwen-peer-lane` launcher used for dispatch.

## Inputs

| Example | SHA-256 |
|---|---|
| `deploy/peer-federator/systemd/user/agent.env.example` | `f0939e0aa0a0573feed52039d638ac133e4de82938196a36134e719c52f20429` |
| `deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example` | `f0c534497a2638baca0a9026fb8513675ba41b7270e6500b4b055965c3351f68` |

Tool labels: Go `go1.26.5 linux/amd64`, Qwen Code `0.22.0`, Agent Sessions `0.2.4`.

## Validation

- `systemd-analyze verify deploy/peer-federator/systemd/user/peer-federator-agent.service`: PASS.
- `bash -n deploy/peer-federator/systemd/user/agent.env.example`: PASS.
- Python `plistlib.load` of the launchd example: PASS. `ProgramArguments` selects `agent`, includes
  the hub and host, and does not opt into `--enable-remote-lanes`; the environment contains exactly
  the documented Qwen launcher, independent native executable, and profile variables in addition to
  the template's fixed launch data.
- `go test ./internal/qwenprofile -count=1`: PASS. This proves unset profile variables preserve the
  native default, explicit values are canonical absolute paths without mutable symlink ambiguity,
  set-empty values fail, and both variables participate in profile identity.
- Focused launcher and federator Qwen/profile tests plus `scripts/federation/test`: PASS. The agent
  consumes `PEER_FEDERATOR_QWEN_LANE` only as the dispatch launcher, resolves native Qwen separately
  from `--qwen-bin` / `QWEN_PEER_QWEN_BIN` or the service path, runs the sole readiness engine against
  that native executable, propagates the exact native path into remote Qwen workers, withholds Qwen
  capability on failure, and retains the one-agent runtime lock.
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
- The optional Qwen launcher and native-client overrides are independent product-specific
  configuration, while the runtime, state, hub, host, and listener contracts remain shared with the
  other products. Neither example conflates `qwen-peer-lane` with native `qwen`.

Result: PASS. The T061/T059A examples match the corrected behavioral contract.
