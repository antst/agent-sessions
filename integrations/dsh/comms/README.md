# Agent Sessions communications for DSH

This plugin presents each live top-level DSH session, provides `identity`,
`peers.list`, and `message.send`, and delivers through native `Agent.steer`.
Bundles load it through the included patch. Without launch context or plugin
groups, a root still appears under its native id and title with no groups. The
plugin does not implement the native lane protocol.

The native DSH session title is the sole Agent Sessions name. A launch name
renames it first. Groups follow patch/profile, launch environment, then additive
runtime commands. Launch name/groups apply without a session id; when present,
the session id pins only the UUID. `/agent-sessions group g [g...]` applies at the next existing
reconnect (about two seconds); confirm acceptance in the roster. It refuses
while a call or delivery is in flight: retry when idle. `/agent-sessions groups`
lists the current set. ACP/headless sessions have no command router and use only
patch/profile or launch environment values.
