# US3: Unified Local Lane Acceptance

## Scope and execution identity

This record covers the daemon-owned local lane lifecycle, recovery, collection,
archive/cleanup debt, and the complete four-parent by four-target composition
matrix. The composition fixture uses two in-process daemon generations and 16
test-owned native-worker helper processes. It does not start, stop, or replace
the installed user service and does not invoke a legacy supervisor, shim,
lane-manager, host, or lane-watch process.

The accepted run completed at `2026-08-27T21:24:20Z` with:

- repository HEAD `2e54b62a94b4c309070061222fd38dcb88df778e` plus the feature worktree changes under test;
- branch `feature/unified-user-daemon`;
- `go version go1.26.5 linux/amd64`;
- Go environment `GOOS=linux`, `GOARCH=amd64`, `CGO_ENABLED=1`;
- `Linux 6.17.4-2-pve x86_64 GNU/Linux`.

The native PID/start pairs below were observed from the kernel process table by
the accepted run. They identify disposable test workers, not installed vendor
sessions.

## Commands and results

The two unified discriminators passed normally:

```sh
scripts/test-unified-lane-restart
scripts/test-unified-lane-composition
```

The restart discriminator reported one dispatch and zero redispatches for all
four products. Codex continued its exact active turn. Claude, Grok, and Qwen
each produced the specification's evidence-approved interrupted outcome, with
one collectable and resumable result and no second user daemon.

The composition wrapper now parses the Go test's structured cell records and
fails unless the set is exactly the 16 Cartesian parent/target pairs. For every
cell it requires an active pre-restart turn, a strictly newer recovered daemon
generation, the same live worker PID and process-start evidence, exactly one
dispatch and one reconnect, zero redispatches, and collection revision 1.

The relevant race coverage also passed:

```sh
AGENT_SESSIONS_UNIFIED_LANE_COMPOSITION=1 \
  go test -race ./internal/daemon \
  -run '^TestUnifiedLaneComposition$' -count=1

AGENT_SESSIONS_UNIFIED_LANE_RESTART_ACCEPTANCE=1 \
  go test -race ./internal/bridge \
  -run '^Test(CodexDaemonLaneReconnectsActiveTurnWithoutRedispatch|ClaudeDaemonLaneRestartRecordsEvidenceApprovedInterruption|GrokDaemonLaneRestartRecordsEvidenceApprovedInterruption|QwenDaemonLaneRestartRecordsEvidenceApprovedInterruption)$' \
  -count=1
```

Both packages returned `ok`; the daemon composition race run took `1.379s`
and the bridge restart race run took `1.039s`.

Focused normal and race runs also passed the 18 product-adapter tests covering
native start/load, active terminal wait, permission mapping, interjection,
interrupt, collection, archive, cleanup, exact-evidence reconnect, and the
permitted interrupted recovery. Six daemon-engine tests passed normally and
under race, covering parent-context admission, crash/reconstruction without a
second dispatch, terminal watching and notice delivery, idempotent archive,
durable cleanup debt, resource refusal, and completed-lane preservation without
redispatch.

## Individually observed composition cells

All rows crossed daemon generation `1 -> 2`, had
`dispatch/reconnect/redispatch = 1/1/0`, were collectable at collection revision 1, and retained the exact
listed native PID/start identity across the restart.

| Parent -> target | Parent attachment / session | Lane / turn | Native PID / proc-start | Restart -> terminal | Resumable / native evidence |
|---|---|---|---|---|---|
| codex -> codex | `f90366d29c977a929c1be6f148e87db7` / `parent-codex-0` | `lane-codex-to-codex` / `turn-codex-to-codex` | `3524429` / `1824748855` | continued -> completed | false / false |
| codex -> claude | `f90366d29c977a929c1be6f148e87db7` / `parent-codex-0` | `lane-codex-to-claude` / `turn-codex-to-claude` | `3524430` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| codex -> grok | `f90366d29c977a929c1be6f148e87db7` / `parent-codex-0` | `lane-codex-to-grok` / `turn-codex-to-grok` | `3524431` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| codex -> qwen | `f90366d29c977a929c1be6f148e87db7` / `parent-codex-0` | `lane-codex-to-qwen` / `turn-codex-to-qwen` | `3524432` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| claude -> codex | `db8069040735f9703059247ee86fef6d` / `parent-claude-1` | `lane-claude-to-codex` / `turn-claude-to-codex` | `3524450` / `1824748855` | continued -> completed | false / false |
| claude -> claude | `db8069040735f9703059247ee86fef6d` / `parent-claude-1` | `lane-claude-to-claude` / `turn-claude-to-claude` | `3524454` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| claude -> grok | `db8069040735f9703059247ee86fef6d` / `parent-claude-1` | `lane-claude-to-grok` / `turn-claude-to-grok` | `3524456` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| claude -> qwen | `db8069040735f9703059247ee86fef6d` / `parent-claude-1` | `lane-claude-to-qwen` / `turn-claude-to-qwen` | `3524457` / `1824748855` | evidence-approved-interrupted -> interrupted | true / true |
| grok -> codex | `cd8e21774b6bfe46b7e1f8ac3e7b2d3f` / `parent-grok-2` | `lane-grok-to-codex` / `turn-grok-to-codex` | `3524458` / `1824748855` | continued -> completed | false / false |
| grok -> claude | `cd8e21774b6bfe46b7e1f8ac3e7b2d3f` / `parent-grok-2` | `lane-grok-to-claude` / `turn-grok-to-claude` | `3524485` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |
| grok -> grok | `cd8e21774b6bfe46b7e1f8ac3e7b2d3f` / `parent-grok-2` | `lane-grok-to-grok` / `turn-grok-to-grok` | `3524486` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |
| grok -> qwen | `cd8e21774b6bfe46b7e1f8ac3e7b2d3f` / `parent-grok-2` | `lane-grok-to-qwen` / `turn-grok-to-qwen` | `3524488` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |
| qwen -> codex | `0ed50cbed27d05ad902cbe335da48434` / `parent-qwen-3` | `lane-qwen-to-codex` / `turn-qwen-to-codex` | `3524490` / `1824748856` | continued -> completed | false / false |
| qwen -> claude | `0ed50cbed27d05ad902cbe335da48434` / `parent-qwen-3` | `lane-qwen-to-claude` / `turn-qwen-to-claude` | `3524491` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |
| qwen -> grok | `0ed50cbed27d05ad902cbe335da48434` / `parent-qwen-3` | `lane-qwen-to-grok` / `turn-qwen-to-grok` | `3524509` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |
| qwen -> qwen | `0ed50cbed27d05ad902cbe335da48434` / `parent-qwen-3` | `lane-qwen-to-qwen` / `turn-qwen-to-qwen` | `3524513` / `1824748856` | evidence-approved-interrupted -> interrupted | true / true |

## Residue and conclusion

The composition test performed a process census both before and after the
generation transition: exactly 16 test-owned vendor workers were present and
zero processes or fixture artifacts named supervisor, shim, lane-manager,
`qwen-host`, `grok-host`, or lane-watch were present. Its final structured
result was:

```json
{"type":"unified.lane_composition.passed","cells":16,"active_turn_restart":true,"redispatch_count":0,"obsolete_processes":0}
```

After both scripts and both race runs exited, a direct process and temporary
root census found zero matching composition workers, zero retained
`agent-sessions-unified-lane-composition.*.log` files, and zero retained
`agent-sessions-lane-restart.*` roots. Thus all 16 active-turn cells passed
individually with no redispatch and no acceptance residue.
