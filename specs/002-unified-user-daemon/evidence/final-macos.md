# macOS Hosted Acceptance — Greenfield Runtime Candidate 08e915d

## Attributable identity

- Commit: `08e915da6b91ddb54eb4790054d1fc818d4b4dec`
- Tree: `dd448d5e9c3d0a412fee88f001cf15ed03cfe755`
- Subject: `Establish greenfield unified daemon install`
- Signature key: `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Hosted evidence head: `76f11f3ad5a5296c9d6fa65765ace82dc65990bf`
- Hosted workflow: [`33178722467`](https://github.com/antst/agent-sessions/actions/runs/33178722467)

The evidence-only head is the direct child of the runtime candidate and does not change its runtime,
service, connector, package, or test tree.

## Hosted Darwin result

The exact candidate is green on the hosted macOS runner. The workflow completed successfully with:

- normal tests;
- race tests with no race-detector finding;
- `go vet ./...`;
- macOS lint;
- the clean-user launchd installed-service fixture, including install, restart, crash recovery,
  explicit stop, connector rollback, removal, and purge;
- package and authoritative inventory contracts; and
- both `darwin-arm64` and `darwin-x64` release builds.

The hosted run validates the greenfield launchd transaction in a clean user context and does not
compose results from the older `8afd94f` runtime.

## Remaining controlled-host check

Installation on the owner's physical Mac remains deliberately uncredited until its old Agent
Sessions peers and prototype roots are quiescent and the exact candidate can be installed without
reading or mutating vendor credentials, history, settings, or transcripts. This is an operational
acceptance check, not a hosted code or launchd gate failure.
