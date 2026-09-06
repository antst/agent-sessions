# Sessionbus OpenCode plugin

Install this package in exactly one supported OpenCode plugin location. The
same plugin serves lane and peer mode: a lane uses the wrapper's private socket,
while an interactive session publishes one peer from native lifecycle events.

The plugin registers the `sessionbus` tool and uses OpenCode's v2 session API
for native delivery. OpenCode must run without `--pure`, which disables external
plugins.

The development manifest links `../../bus` because the required
`@sessionbus/kit` context-bearing rehello and admitted-identity behavior is
newer than the published 0.1.0-pre.1 package. Extracted-package verification is
skipped until 0.1.0-pre.2 or an equivalent exact preview is published.
