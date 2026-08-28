# Final Constitution Review — Unified User Daemon

**Runtime candidate**: signed commit
`37e1977356299f6cd741685df566835e80c96abf`, tree
`4bd400c4e8b1494e1cd0ea3b10c9de1e85307bbd`.

**Review status**: **PENDING CONTROLLED-MAC INSTALLATION**. Local Linux and the complete hosted
Linux/macOS matrix are green at the exact runtime tree.

## Satisfied at the exact Linux candidate

- One `agent-sessions` user daemon owns attachments, delivery, lanes, embedded federation-client
  behavior, recovery, and diagnostics.
- `agent-sessions-hub` is the sole separate central-hub image; network interoperability depends only
  on exact hub protocol equality, not SHA or release equality.
- Global groups remain the only collaboration boundary in one uniform multi-host space.
- The release inventory and all four archives contain exactly two executable images.
- Pre-0.3 inventory, adoption, conversion, drain, retirement, fallback reads, migration CLI, tests,
  and acceptance assets are absent from the shipped tree.
- First installation is explicitly operator-established greenfield state; vendor profiles,
  credentials, settings, transcripts, and native histories remain outside that boundary.
- Connector mutations are prepared transactionally, retargeted to the selected immutable release,
  committed before service restart, and restored with configuration/service/alias state on failure.
- Normal, race, vet, lint, installed systemd service, federation, binary-pair, peer, lane, stress,
  removal, purge, and four-platform package gates are green.
- The installed exact candidate has one daemon, one endpoint, zero obsolete processes, and healthy
  metadata-only status/doctor output.
- A stopped Codex App Server is started lazily by the unified daemon on the first managed Codex
  attachment; the peer wrapper remains a thin client, the managed TUI uses `--remote unix://`, and
  the external vendor daemon remains available for subsequent sessions.
- Hosted workflow run
  [`33181867525`](https://github.com/antst/agent-sessions/actions/runs/33181867525) passed Linux and
  macOS normal, race, vet, lint, clean-user installed-service, package-contract, inventory, and all
  four release-build jobs at the exact runtime head.

## Pending before final PASS

- install the exact candidate on the controlled physical Mac after its old Agent Sessions processes
  and prototype roots are quiescent;
- prove the physical-host nonmutation boundary for vendor credential, history, settings, and
  transcript stores; and
- record the resulting one-daemon/no-obsolete-process census.

The older `8afd94f` cross-platform evidence is historical and is not silently composed with this
candidate. T094 and T095 remain open only for the controlled physical-Mac installation and final
constitution sign-off.
