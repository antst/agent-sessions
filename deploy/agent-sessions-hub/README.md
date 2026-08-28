# Agent Sessions central hub deployment assets

This directory belongs exclusively to the separately deployed central
`agent-sessions-hub` service. The hub is not a host runtime authority: it owns
no local vendor adapter, managed attachment, lane manager, connector, host
configuration, or host runtime endpoint.

Hub installation owns a distinct release selection, service lifecycle,
non-secret configuration, listener, routing state, transaction journal, and
purge boundary. It must not install, start, stop, restart, upgrade, remove, or
purge any per-host `agent-sessions` daemon.

Host and hub builds may come from different repository commits or release
versions. They interoperate only when their explicitly declared federation
protocol versions match; release version, build identity, installation time,
and advertised host capabilities do not couple their lifecycle transactions.

The service definitions in this directory select only
`hub/current/bin/agent-sessions-hub`. The install transaction replaces
`@PREFIX@`, `@LISTEN@`, and `@STATE_ROOT@` from the exact hub role state; it
never reads or changes the host role's selection or configuration.
