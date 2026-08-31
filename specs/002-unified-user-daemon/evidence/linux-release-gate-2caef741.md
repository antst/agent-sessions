# Linux Release Gate — `2caef741`

Date: 2026-08-30 UTC.

- Commit: `2caef741b29f18272b79e76ce1cb664c3498dea9`
- Tree: `19fc82f7b6a13a07974fd20663377ad71e5e3c08`
- Command: `PATH=/usr/local/go/bin:$PATH ./scripts/release-final-gate linux <isolated-evidence-dir>`
- Result: `release linux gate: PASS`

| Gate | Exact result |
|---|---|
| normal | all current packages passed; bridge `40.443s` |
| race | all current packages passed; bridge `54.647s`; no race report |
| vet | exit 0, no diagnostics |
| lint | exit 0, `0 issues.` |
| focused contracts | bridge, federator, launcher, qwenprofile, qwenreadiness passed |
| quickstart | host install rollback, hub install rollback, host/hub removal passed |
| federation | federator and current `agent-sessions` command packages passed |
| derived permission/prebuilt/nonmutation records | present and non-empty |

The gate ran from a clean checkout. The evidence directory was isolated under `/tmp`; this checked-in
summary retains the candidate identity and exact verdict without retaining transient build output.
