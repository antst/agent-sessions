# Installation

## Requirements and targets

Agent Sessions targets Linux and macOS on x86-64 and arm64. It requires Codex CLI with plugins,
hooks, and managed App Server support; Claude Code with local cross-session messaging; Qwen Code
0.21.15 or newer for Qwen peers/lanes; and Bash for
installation and maintenance scripts. Building from source requires Go 1.22 or newer. CI/release
artifacts include the eleven native binaries and require
neither Go nor Node.js on the destination host.

| Host | Bundled binary |
|---|---|
| Linux x86-64 | `bin/linux-x64/agent-session-runtime` |
| Linux arm64 | `bin/linux-arm64/agent-session-runtime` |
| macOS Intel | `bin/darwin-x64/agent-session-runtime` |
| macOS Apple Silicon | `bin/darwin-arm64/agent-session-runtime` |

Each platform directory also contains the distinct `peer`, `codex-peer`, `claude-peer`, `qwen-peer`,
`codex-peer-lane`, `claude-peer-lane`, `grok-peer`, `grok-peer-lane`, `qwen-peer-lane`, and `peer-federator`
executables. `peer-federator` remains a separately
operated process; installing the binary does not enable or load a federation service.

The release peer, lane, cleanup, and bidirectional federation matrix is exercised on real Linux and
macOS hosts. CI additionally cross-builds every release for Linux and macOS on x86-64 and arm64.

## Release archive installation

A `vX.Y.Z` tag whose base version matches the plugin manifest creates a Forgejo Release containing
four archives and `SHA256SUMS`. Choose exactly one archive for the destination host. Each archive
has one top-level directory and contains the matching native executables plus the Codex, Claude, Grok, and Qwen
plugin payloads, launchers, documentation, and installer; it deliberately omits Go source.

```bash
# Linux: verify the downloaded archive against the release checksum file.
sha256sum -c --ignore-missing SHA256SUMS
tar -xzf agent-sessions-vX.Y.Z-linux-x64.tar.gz
cd agent-sessions-vX.Y.Z-linux-x64

# After exiting every Codex client:
codex app-server daemon stop
make install-all
```

On macOS, use `shasum -a 256 -c --ignore-missing SHA256SUMS` and the matching `darwin-x64` or
`darwin-arm64` archive. The `.agent-sessions-prebuilt` marker makes `make build` and the install
targets use the packaged executable even if Go is present. Extracting an archive on the wrong OS
or architecture fails before installation with the missing platform name. `make install` installs
only the native runtime and Codex side; `make install-all` also installs the Claude orchestration,
trusted Grok MCP, and selected-profile Qwen Agent Plugins payloads.

## Source installation

```bash
git clone https://github.com/antst/agent-sessions.git ~/agent-sessions
cd ~/agent-sessions
make test-race
make install
```

For a release archive with the matching prebuilt binary, run only `make install` or
`make install-all`; Go is not needed.

By default, `make install`:

1. builds all eleven binaries under `bin/<platform>`;
2. copies the runtime plugin payload into `~/.local/libexec/agent-sessions`;
3. registers that installed tree's marketplace as `agent-sessions`;
4. installs `agent-sessions@agent-sessions` into Codex's plugin cache; and
5. creates command symlinks in `~/.local/bin` whose absolute targets are derived from the exact
   configured `INSTALL_ROOT`, not from an assumed prefix layout; and
6. starts the shared runtime only after App Server is stopped and no managed
   `grok-peer` TUI/private leader is live, without interrupting either product.

The first newly launched Codex session then asks for one-time approval of the installed plugin's
lifecycle hooks. Approve `agent-sessions@agent-sessions`; otherwise `SessionStart`,
`UserPromptSubmit`, and `Stop` do not run and owned-session registration plus fallback inbox
delivery remain incomplete. Ordinary threads execute the same globally installed hooks as silent
no-ops. This approval trusts the plugin hooks only—it does not change Codex's
sandbox or normal tool approval policy. A TUI that was already open during installation must be
restarted before it can load the new hook snapshot and present the prompt.

After the replacement is registered, the installer removes older `claude-code-peer` installations
from the repository, personal, and legacy `codex-messaging` marketplaces. This prevents both plugin
identities from loading the same hooks and MCP server after an upgrade; it does not remove the
user's `personal` marketplace.

Override the destination with `PREFIX=/another/prefix` or `INSTALL_ROOT=/another/libexec/path`.
Override the Codex executable with `CODEX=/path/to/codex`. Use `make dev-install` when you
intentionally want the native runtime, launchers, and marketplace to track a mutable source checkout. Run install
from a host terminal after exiting every Codex client, running
`codex app-server daemon stop`, and normally exiting every `grok-peer` TUI.
The installer refuses to replace any running App Server—even an idle one—because a separate
quiescence check followed by restart has an unavoidable race with native clients starting work.
It also refuses while a managed Grok launch record has any live or unverifiable
owner, host, private leader, or observer identity. Normal Grok TUI exit removes
that private process group automatically; see
[Grok leader shutdown](GROK-INSTALL.md#stop-leaders-safely).
Packagers can use `START_RUNTIME=0` to stage files without starting host services.

`make install` deliberately changes only the Codex/runtime side. To install the reusable Claude
orchestration skill plus Grok and Qwen MCP plugins, use `make install-all`; on a host where the runtime
is already installed, `make install-claude`, `make install-grok`, and `make install-qwen` update those surfaces
independently. The Claude target stages its cache-busted payload under a
versioned, immutable directory below `$(PREFIX)/share/agent-sessions/claude-marketplaces` before
updating the marketplace, so later native-only installs cannot change an active Claude plugin.
Use `make dev-install-claude` only when the Claude
marketplace should deliberately follow the mutable checkout. Claude plugin installation is explicit because it
changes the user's Claude Code marketplace and plugin settings. See
[CLAUDE-INSTALL.md](./CLAUDE-INSTALL.md).

The Grok target validates the local plugin and copies it into Grok's documented
auto-trusted user directory, `~/.grok/plugins/agent-sessions`, which allows its
native MCP command to execute as the current user. It migrates only a prior
single-plugin direct installation and keeps separate plugin data. Grok's
official trusted installer is used only to update the enabled-plugin setting;
that temporary registry row is then removed with `--keep-data`. Installation
fails unless `grok inspect --json` resolves exactly one enabled user plugin and
the exact staged `agent_sessions` MCP executable. See
[GROK-INSTALL.md](./GROK-INSTALL.md).

The Qwen target invokes Qwen's native Agent Plugins v1 extension manager in the
exact profile selected by presence-sensitive `QWEN_HOME` and
`QWEN_RUNTIME_DIR`. It verifies manifest/version/enabled-state drift, one
`agent_sessions` stdio MCP server, and all five skills before admitting a
managed launch. `make upgrade-qwen` is the same idempotent verified transaction;
`make remove-qwen` removes only that extension. Both refuse while a managed
Qwen process uses the selected profile. See [QWEN-INSTALL.md](./QWEN-INSTALL.md).

The equivalent source-linked development command is:

```bash
make -C ~/agent-sessions dev-install
```

Codex marketplace registration also accepts the Forgejo Git URL directly:

```bash
codex plugin marketplace add \
  https://github.com/antst/agent-sessions.git
codex plugin add agent-sessions@agent-sessions
```

The plugin must be installed, not merely checked out: managed App Server loads hooks and MCP
configuration from the installed plugin cache. Start a new TUI after installation and approve the
plugin hooks when prompted. A TUI that was already running retains its previous hook snapshot until
it is restarted.

The launcher consumes only `-n/--peer-name` and an explicit resume selector. It resolves the selector
to one UUID, owns the `--remote unix:// resume UUID` prefix, and supplies the canonical cwd only when
`-C/--cd` was absent. The remaining native Codex argv is appended unchanged, preserving relative
ordering without splitting variadic values. Model, profile, config, feature, sandbox, approval, search,
image, hook-trust, and display options may therefore appear before or after the input `resume` selector.
Explicit `--yolo` is also mirrored through the App Server before publication: at `thread/start` for a
fresh peer, and through `thread/resume` plus `thread/settings/update` for a resumed peer. This changes
durable thread settings, so later plain resumes remain full-access until those settings are changed.
For an externally isolated host, `codex-peer --yolo -n NAME`
remains an intentional opt-in.
Supported resume syntax is `codex-peer [GLOBAL_OPTIONS] resume [RESUME_OPTIONS] UUID_OR_NAME
[PROMPT_OR_OPTIONS]`; options may appear on either side of the input selector. A name selects the newest
usable exact-name session. Without explicit `--yolo`, resume preparation uses the existing thread's
effective App Server policy. Resume inherits the thread's canonical cwd;
an explicit `-C` is accepted only when it resolves to that same directory.

## Build and update commands

```bash
make lint          # verify config and run golangci-lint
make test          # shell checks and Go tests
make test-race     # race-enabled Go tests
make build         # current host, under bin/<platform>
make build GOOS=darwin GOARCH=arm64
make install-claude # install/update agent-sessions in Claude Code
make install-grok   # validate/trust/install the Grok MCP plugin
make install-qwen   # install/verify Qwen support in the selected profile
make upgrade-qwen   # idempotent verified Qwen update
make remove-qwen    # remove only Agent Sessions from the selected Qwen profile
make install-all    # native runtime plus Claude Code, Grok, and Qwen plugins
make reinstall     # new cachebuster, rebuild, reinstall
make repair-projection THREAD_ID=<uuid>          # inspect the known duplicate-ordinal failure
make repair-projection THREAD_ID=<uuid> APPLY=1  # back up and repair only that exact failure
make clean
```

The native launchers and installation bootstrap use `bin/<platform>/agent-session-runtime` directly.
Source installs build that executable before activation; packaged installs require the matching prebuilt
binary. There is no separate shell bootstrap or lazy shadow build.

## Runtime ownership

Managed App Server and the bridge supervisor are shared per user and `CODEX_HOME`, not per lane.
Each canonical `CODEX_HOME` has a hashed supervisor socket, version marker, exact interactive-owner
records,
lane registry, and retirement state, so two profiles under one Unix user cannot attach their hooks
or lanes to each other's App Server. Peer discovery sockets remain global per user because Claude
must see every profile in one local roster.
Both launchers start them idempotently, so no manual daemon start is required after reboot: the
first launcher used after boot starts everything. To make managed App Server durable before any
launcher invocation, Codex also provides:

```bash
codex app-server daemon bootstrap
```

Each advertised root thread runs the same binary in `shim` mode because Claude discovery is keyed
by a live PID. Child Codex subagents remain private and do not create extra registry entries.

The TUI owns an interactive peer's live lifetime. Normal exit of an attached peer removes its registry
and sockets,
unloads the App Server runtime and thread-scoped MCP children, and leaves the transcript resumable
but not messageable until another TUI resumes it. For a fresh root, `codex-peer` first creates the
thread using the real canonical cwd. Explicit UUID or unique-name resume resolves one authoritative,
unarchived thread. The wrapper binds that UUID to its PID/process-start
token and publishes the exact prepared owner before replacing the same process with a TUI resuming
the UUID. A committed zero-turn TUI that exits before SessionStart loses its shim but remains loaded;
its exact stale prepared-owner record authorizes one replacement resume without archive/unarchive,
which would race the replacement TUI and move the rollout into the archived session tree. Only a failed
fresh preparation before its publication commit is deleted. The supervisor therefore
performs the same cleanup on its next
five-second reconciliation tick when `SessionEnd` is skipped or the TUI dies with `SIGKILL`.
Ordinary threads have no capability record, publish no peer shim, and cannot use peer tools. The
public App Server and hook protocols identify a thread rather than a client attachment, so a plain
client explicitly resuming an already-authorized peer UUID cannot be distinguished from its owner.
Supervised lane cleanup uses its own durable owner
identity. If a shim or supervisor itself dies,
the existing startup/reconciliation sweep removes or replaces only bridge-owned stale transport;
dead registry PIDs are never considered reachable, and queued inbox messages are retained.

Persistent state defaults to `~/.local/state/claude-code-peer`. Runtime sockets use
`$XDG_RUNTIME_DIR/codex-claude-peer-<uid>` and fall back to a private system-temporary directory
when necessary. Startup rejects a symlink, non-directory, or directory owned by another uid and
requires mode `0700` before touching any socket or alias. Linux process identity comes from
`/proc`; macOS uses the kernel process table. Observation failures are distinct from proof of
death, so an unknown identity neither authorizes a new owner nor triggers destructive cleanup.

A version-changing update never restarts a running App Server. It requires the daemon to be
stopped, repeats that check after acquiring its cross-launch lock, and starts a fresh server with
the new plugin. The old peer supervisor is left intact until that start succeeds; the versioned
supervisor start then replaces it. Supervisor identity includes the SHA-256 captured from its
executable at startup, not only the plugin version. A same-version reinstall therefore replaces a
stale or deleted supervisor binary without restarting App Server, and every new `SessionStart`
performs the same identity check before registering its shim. When upgrading from the pre-profile layout, startup also
validates and retires the responsive legacy `supervisor.sock`; it refuses an unknown or
unresponsive owner instead of leaving two supervisors subscribed to one App Server. If a native client starts the server
while the updater is waiting, activation exits 75 without stopping either process. This removes
the non-atomic check-to-restart operation entirely. Same-version startup remains idempotent and
does not restart App Server. These rules are the same on Linux and macOS; a user can still bypass
them by invoking Codex's native `daemon restart` command directly.

If the fresh App Server starts but versioned peer-supervisor replacement fails, the updater emits
an explicit warning that messaging may be unavailable. The loaded-version/runtime markers are
already durable at that point, so the next `codex-peer`, `codex-peer-lane`, or direct
`agent-session-runtime bootstrap` retries only supervisor startup; it does not require another
server stop or plugin reinstall.

## Recovering the Codex 0.147 duplicate-ordinal projection failure

Codex 0.147 can append the same rollout ordinal twice if App Server is replaced during an active
turn. The canonical JSONL continues growing, but its derived `thread_history_*.sqlite` projection
stops at the duplicate and a resumed TUI renders stale history. This bridge prevents its updater
from creating that condition by requiring a cleanly stopped App Server for every version change.

For a host already affected, exit every Codex client and stop managed App Server first:

```bash
codex app-server daemon stop
cd ~/agent-sessions
make repair-projection THREAD_ID=<uuid>
make repair-projection THREAD_ID=<uuid> APPLY=1
```

The recovery command requires Python 3 only for this exceptional maintenance operation. Its dry
run and applied mode both refuse to run inside a Codex turn or while managed App Server is
reachable. It accepts only the proven three-record shape—`N-1`, duplicate `N-1`
`thread_settings_applied`, then `N`—backs up the complete SQLite database, advances only the
projection byte offset, and verifies that the rollout SHA-256 did not change. It never deletes or
rewrites canonical rollout JSONL. Unknown corruption is rejected rather than guessed at. Resume
through `codex-peer` after the repair; the materializer then catches the projection up to the
rollout tail.

## Verification

```bash
codex plugin list
codex app-server daemon version
codex-peer-lane --help
codex-peer -n reviewer
claude agents --json
```

See [CODEX-LANES.md](./CODEX-LANES.md) for Codex lane integration and the
[documentation index](./README.md) for the other product and operator guides.
