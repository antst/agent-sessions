# Final Constitution Review — Unified User Daemon

**Runtime candidate**: signed commit
`08e915da6b91ddb54eb4790054d1fc818d4b4dec`, tree
`dd448d5e9c3d0a412fee88f001cf15ed03cfe755`.

**Review status**: **PENDING CROSS-PLATFORM CLOSURE**. Linux is green. Hosted CI and exact-candidate
macOS acceptance have not yet been credited.

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

## Pending before final PASS

- hosted Linux/macOS workflow results at the exact candidate;
- hosted clean-user launchd service acceptance at the exact candidate;
- independent physical-Darwin normal/race/vet/lint and nonmutation checks; and
- final comparison of hosted archive artifacts with the declared two-image inventory.

The older `8afd94f` cross-platform evidence is historical and is not silently composed with this
candidate. T094 and T095 remain open until these pending checks are complete.
