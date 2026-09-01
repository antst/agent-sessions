# Linux Release Gate — `170b96c`

Date: 2026-08-30 UTC.

- Commit: `170b96c98750b3ed5fbea42ed629da9f505379dc`
- Tree: `3702f86b0d626428d51b0c7eadc81cabc7f0a30e`
- Command: `PATH=/usr/local/go/bin:$PATH ./scripts/release-final-gate linux <isolated-evidence-dir>`
- Isolated evidence directory: `/tmp/agent-sessions-linux-gate.TPT61E`
- Result: `release linux gate: PASS`

| Gate | Exact result |
|---|---|
| normal | all current packages passed; bridge `40.419s` in the separate uncached proof |
| race | all current packages passed; bridge `54.657s` in the separate uncached proof; no race report |
| vet | exit 0, no diagnostics |
| lint | exit 0, `0 issues.` |
| focused contracts | bridge, federator, launcher, qwenprofile, qwenreadiness passed |
| quickstart | host install rollback, hub install rollback, host/hub removal passed |
| federation | federator and current `agent-sessions` command packages passed |
| derived permission/prebuilt/nonmutation records | present and non-empty |

Before the aggregate gate, `go clean -testcache` preceded both the normal and race `scripts/test`
proofs. The gate ran from a clean checkout. This checked-in summary retains the exact candidate
identity and verdict; the transient detailed logs remain in the isolated directory named above.
