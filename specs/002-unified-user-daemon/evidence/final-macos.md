# macOS Hosted Acceptance — Greenfield Runtime Candidate 37e1977

## Attributable identity

- Commit: `37e1977356299f6cd741685df566835e80c96abf`
- Tree: `4bd400c4e8b1494e1cd0ea3b10c9de1e85307bbd`
- Subject: `Restore lazy Codex App Server startup`
- Signature key: `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Hosted runtime head: `37e1977356299f6cd741685df566835e80c96abf`
- Hosted workflow: [`33181867525`](https://github.com/antst/agent-sessions/actions/runs/33181867525)

The hosted workflow ran at the exact runtime candidate; no intermediate evidence-only head was used.

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
