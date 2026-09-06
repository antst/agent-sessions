# Sessionbus OpenCode plugin

Install this package in exactly one supported OpenCode plugin location. The
same plugin serves lane and peer mode: a lane uses the wrapper's private socket,
while an interactive session publishes one peer from native lifecycle events.

`sessionbus-opencode-install` transactionally adds the exact package specifier
to the `plugin` array in the user OpenCode config. During development, pass a
product-verified local or tarball specifier:

```sh
sessionbus-opencode-install --specifier file:/absolute/path/to/integrations/opencode
```

Run `sessionbus-opencode-install --remove` to remove the managed entry. The
installer follows OpenCode's native global-config selection, removes its owned
string or tuple entry from every merged JSON/JSONC file, and preserves comments,
config keys, and unrelated plugin entries. OpenCode resolves the configured
package; the installer does not copy plugin source into its config directory.

The plugin registers the `sessionbus` tool and uses OpenCode's v2 session API
for native delivery. OpenCode must run without `--pure`, which disables external
plugins.

The development manifest links `../../bus` because the required
`@sessionbus/kit` context-bearing rehello and admitted-identity behavior is
newer than the published 0.1.0-pre.1 package. Extracted-package verification is
skipped until 0.1.0-pre.2 or an equivalent exact preview is published.
