# Baseline macOS Evidence

Status: **BLOCKED — no acceptance credit**.

The required target is a real macOS host running the exact detached baseline
`c056fbc5015d4ab0a673f66cac5404206f7bcee6`. The intended command set is identical to
`baseline-linux.md`: normal, race, vet, lint, mapped packages, then the checked manifest validators.

On 2026-08-29 UTC this resumed Linux session was not an attested Agent Sessions peer:

```text
agent_sessions is inactive outside an attested peer session
```

Therefore it could not send the execution brief to the existing Mac validation peer. The two
configured owner SSH names were also unavailable from this host:

```text
ssh macbook: Could not resolve hostname macbook
ssh mbp-lan: Could not resolve hostname mbp-lan
```

No command ran on macOS, no prior transcript was reused, and no historical aggregate “green” was
credited. T012 remains open. Local regression-freeze work that does not extract or replace production
behavior may continue, but no product cutover, deletion, or ready claim may pass this missing platform
gate.
