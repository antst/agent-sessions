# Installation and service lifecycle

Agent Sessions installs two executable roles from one release:

- `agent-sessions`: the single host image. One foreground instance runs per OS user-host. The
  installed `peer`, product peer, and product lane command names are aliases of this exact image.
- `agent-sessions-hub`: the distinct central federation hub image. It has no local product adapters,
  attachments, lanes, connectors, or host runtime authority.

Linux and macOS archives are built for x86-64 and arm64. Each platform archive contains exactly these
two executables, all four optional connector payloads, service definitions, documentation, and the
release manifest. Building from source requires Go 1.22 or newer; a matching release archive uses its
prebuilt executables.

## Host installation

```sh
make install-all
```

`make install` and `make dev-install` are host-install aliases. Clean installs and steady-state unified
upgrades use this host transaction:

1. validates and stages one immutable host release;
2. discovers Codex, Claude, Grok, and Qwen without treating an absent product as an aggregate failure;
3. persists the resolved native executable inventory and prepares recoverable connector changes for
   the installed subset;
4. installs the standard user service definition and atomically selects `host/current`;
5. commits connector changes from that immutable release before performing one service-manager start
   or restart;
6. verifies the exact binary identity, generation, one private endpoint, state schema, product
   readiness, local routing, and configured federation state; and
7. commits the release journal and confirms the already-applied connector journal.

Explicit connector operations remain strict:

```sh
make install-codex
make install-claude
make install-grok
make install-qwen
make remove-qwen
```

They use each vendor's supported installer. They never copy, inspect, or repair credentials and do
not manufacture a byte-clean uninstall by editing vendor state. Native installers may retain their
own non-secret bookkeeping.

The installed host service is:

- Linux: `agent-sessions.service`, a systemd user service;
- macOS: `net.antst.agent-sessions`, a launchd user agent.

The service manager owns lifetime. The daemon runs `agent-sessions daemon` in the foreground and does
not fork or supervise another copy. Peer, lane, connector, MCP, and federation workflow commands never
start, stop, repair, or replace it. When it is unavailable they fail before accepting work.

## Hub installation

Install the one central hub separately:

```sh
make install-hub HUB_LISTEN=:7419
```

This installs only `agent-sessions-hub`, its non-secret configuration, and its own systemd or launchd
service. It does not install the host service, command aliases, connectors, or vendor integrations.
Host and hub releases, current selections, locks, journals, services, readiness decisions, removal,
and purge roots are disjoint even when both roles use the same OS account.

Upgrading a protocol-compatible host never restarts the hub. Upgrading the hub never restarts a host.
Their release versions and source commits may differ; network interoperability requires only exact
hub-protocol-version equality. See [FEDERATION.md](FEDERATION.md).

## First installation of the unified daemon

Version 0.3 is a deliberate greenfield boundary from the unreleased split-runtime prototypes. Before
the first install, close every old peer and lane, stop all old Agent Sessions processes and services,
and remove or archive the old Agent Sessions-owned state and installation roots. The installer does
not inventory, adopt, drain, retire, or repair the old topology.

Vendor credentials, profiles, settings, transcripts, and native history belong to the vendor clients
and are not part of this reset. Do not delete or rewrite them. After the clean first install, ordinary
0.3 upgrades use the durable release transaction and preserve unified daemon state. The first managed
Codex launch asks the unified daemon to start the external App Server through Codex's supported
vendor command; installation itself does not start an unused vendor process.

## Routine restart and upgrade

Steady-state upgrades use the normal immutable host transaction. The service
manager performs one restart. The daemon restores its catalogs, attachments, deliveries, lane actors,
and embedded federation-client connection before opening the next generation.

Native vendor processes remain external and are not stopped merely because Agent Sessions restarts.
Codex, Claude, Grok, and Qwen adapters re-corroborate their existing native actors. Accepted messages
are not duplicated. Active lane turns reconnect when the native protocol proves that safe; an
adapter-specific unsupported case records exactly one explicit interrupted, collectable, resumable
outcome.

Explicit stop or disable suppresses service-manager restart until the user explicitly starts or
enables the service. Sending a signal is not the supported persistent stop on launchd.

## Status and troubleshooting

```sh
agent-sessions status --json
agent-sessions doctor --json
agent-sessions-hub status --json
agent-sessions-hub doctor --json
```

Status and doctor report bounded non-secret identity, generation, endpoint, service, product,
attachment, lane, federation, and debt metadata. They never include message, prompt,
result, tool, credential, or transcript content and never start an unavailable service.

Common diagnoses:

- `unavailable`: start the already-installed role through systemd-user or launchd; do not retry a
  workflow expecting it to bootstrap the daemon.
- `refused`: close every exact peer/lane or removal blocker named by the command, then retry
  the same supported operation.
- `incompatible`: install a build supporting the recorded state or matching hub protocol; no downgrade
  or alternate carrier is attempted.
- `retryable`: satisfy the recorded identity predicate and retry. Do not delete sockets, journals, or
  state by hand.
- `start Codex App Server`: inspect `codex app-server daemon version`, run the vendor start command
  directly for its native diagnostic when needed, and retry. The peer command never bootstraps the
  service itself; the unified daemon owns the lazy coordination step.
- Codex history appears blank through the App Server: run Codex's own `codex migrate-rollouts` command.
  Agent Sessions does not rewrite vendor history projections.

## Removal and purge

Normal host removal is state-preserving:

```sh
make remove
```

It refuses with zero mutation while any managed attachment or lane is active. Once quiescent, it stops
the exact host service, removes supported connector registrations, command links, selected releases,
and verified disposable runtime artifacts. It preserves Agent Sessions configuration, catalogs,
completed lane metadata, cursors, federation configuration, and cleanup debt.
It never removes native sessions, credentials, profiles, settings, or transcripts.

Hub removal is independent:

```sh
make remove-hub
```

It stops and removes only the hub service, selected hub release, and disposable hub runtime state.
Remote hosts continue running; hub configuration and durable hub metadata are preserved.

Deleting preserved Agent Sessions state is always a separate offline, revision-bound operation:

```sh
make purge-inspect PURGE_PLAN=/absolute/path/host-plan.json
make purge PURGE_PLAN=/absolute/path/host-plan.json
make purge-hub-inspect PURGE_PLAN=/absolute/path/hub-plan.json
make purge-hub PURGE_PLAN=/absolute/path/hub-plan.json
```

Apply revalidates the plan revision, file type, UID, root containment, and current identity before each
deletion. It refuses a running role, changed identity, link ambiguity, vendor-owned path, other-role
state, or unenumerated target. Interrupted purge is journaled and idempotently retryable.

## Development checks

```sh
make test
make test-race
go vet ./...
make lint
make build-release-platform GOOS=linux GOARCH=amd64
```

The complete command, option, environment, JSON, and exit contract is generated in [CLI.md](CLI.md).
