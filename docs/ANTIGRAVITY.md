# Antigravity integration

`agy-peer` launches a native Antigravity CLI conversation as an Agent Sessions
peer. It preserves Agy's process and argument semantics: the launcher removes
only `-n/--peer-name`, creates a process-bound launch token, and then replaces
itself with `agy` using the remaining argv unchanged.

The launcher fails closed unless the selected executable exposes the headless
Agy CLI contract. It probes executable `agy` candidates on `PATH` with the
non-launching `--help` command, skips desktop Antigravity/GUI shims, and falls
back to `~/.local/bin/agy` when necessary. `AGY_PEER_AGY_BIN` may select a
specific executable, but the same validation applies and a GUI launcher is
rejected.

```bash
agy-peer -n reviewer
agy-peer -n batch-review -p "Review the current change"
agy-peer -n reviewer --conversation <conversation-uuid>
```

Use Agy's documented `--conversation` or `--continue` forms rather than a
synthetic `resume` subcommand. A peer name is an address for a live bridge, not
an Agy session index: repeat `-n` when a resumed conversation should keep a
particular advertised name.

Informational Agy subcommands such as `models`, `plugin`, `agents`, `install`,
and `update`, plus help/version flags, pass through without peer activation.
`--dangerously-skip-permissions` is reflected as the bridge's bypass permission
class; the launcher does not otherwise choose Agy policy.

## Install

The native payload is staged by `make install`. Activate the Agy plugin with:

```bash
make install-agy
# or install every product surface
make install-all
```

Installation stages the plugin directly in Agy's global plugin directory,
`~/.gemini/config/plugins/agent-sessions`, and records the same
skills/MCP/hooks entry that Agy writes to `config/import_manifest.json`. It
deliberately does not invoke `agy plugin install`: current macOS builds
delegate that subcommand to the desktop application, which can open or update
the GUI. Existing import entries and unknown manifest metadata are preserved.

Before starting Agy, the launcher prepends its installed native-runtime
directory to `PATH`. The plugin's stdio MCP entry therefore resolves the exact
`agent-session-runtime` installed beside `agy-peer`, even when that directory
was absent from the caller's original `PATH`.

Use `make dev-install-agy` to stage the plugin directly from the current
checkout. The plugin contains Antigravity's `plugin.json`, hook
configuration, MCP registration, and two skills. Validate it directly with
`make validate-agy`.

Antigravity may independently ask whether the current project is trusted before
it loads project-local configuration, hooks, or execution policy. That is Agy's
workspace trust boundary; it is not an Agent Sessions permission request.

## Identity and authorization

`agy-peer` captures its own PID and process-start token before `exec agy`. The
native runtime stores a random launch token bound to that exact process. The
first attested Agy hook attaches the token to one `conversationId`; the token
cannot authorize another conversation, a dead owner, a reused PID, or an
ordinary Agy process.

The shim publishes `entrypoint: "agy"` in Claude's local registry and exposes a
stable UDS reply address while the Agy process lives. Owner-process death removes
the shim, registry row, socket, and launch record. Globally installed hooks and
MCP inventory are intentional no-ops outside an attested `agy-peer` launch.

Inside the conversation the MCP server is named `agent_sessions` and exposes:

- `list_peers`
- `send_message`
- `check_inbox`
- `identity`
- `rename_session`

The MCP server derives the current `session_id` from the process-attested launch
record. A model-supplied ID is optional: it can corroborate that identity but
cannot grant authority or select another conversation.

## Delivery boundaries

PreInvocation injects startup context and queued peer messages. PostInvocation
continues the current execution when a message arrived before that boundary;
Stop can keep the loop alive for messages queued while the turn was finishing.

Antigravity 1.1.13 exposes no documented external operation that wakes a fully
idle interactive CLI. A message sent after the conversation is already sitting
idle remains durable in its Agent Sessions inbox and is injected on the next
real invocation. `check_inbox` is recovery-only, not a polling loop. The adapter
does not claim instant idle wake where the host product provides none.

## Codex and Claude lanes from Agy

The `agent-lanes` skill teaches Agy to use the existing `codex-peer-lane` and
`claude-peer-lane` contract-2 CLIs. No third lifecycle engine is involved. The
Agy launch token, ancestry, live shim, and conversation ID corroborate Agy as the
lane owner, so a default lane is interrupted/archived when its Agy owner exits
and its terminal notice targets that Agy conversation. Use `--persistent` only
when the caller explicitly wants a lane to outlive the Agy process.

The same skill supports remote Codex and Claude lanes through `peer-federator lane`;
it never falls back to SSH or silently changes the destination.

There is no `agy-peer-lane` in this release. A durable Agy worker lifecycle and
federated Agy lane capability require a separately verified headless/session
contract and are intentionally deferred.
