# Agent Sessions host deployment assets

This directory belongs exclusively to the per-user, per-host `agent-sessions`
runtime. It will contain the authoritative source-release version and the
systemd user-service and launchd user-agent assets for the host daemon.

Host installation owns the host release selection, service lifecycle,
configuration, connector payloads, local runtime endpoint, and host-side
durable state. It must not install, start, stop, restart, upgrade, remove, or
purge the central `agent-sessions-hub` deployment.

The host daemon embeds the existing host federation-agent responsibilities. It
connects to one separately operated central hub, but no standalone host
federation-agent service remains in the unified steady state.

`VERSION` is the sole declared source-release authority. Release `0.3.0`
intentionally starts the incompatible unified-daemon and federation protocol
generation; deployed host and hub versions may still differ when their exact
hub-protocol versions match.
