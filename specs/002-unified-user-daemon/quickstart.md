# Quickstart Validation: Unified User Daemon

This guide describes the end-to-end acceptance flow after implementation. It is not an instruction to
run unimplemented commands against the current installed estate.

## Safety prerequisites

- Use a dedicated acceptance OS user or an owner-approved real user-host baseline.
- Do not start an additional daemon when that user already has one.
- Record exact process, service, endpoint, state-root, connector, native-profile metadata, and unrelated
  process baselines before mutation.
- Never read, copy, print, diff, or hash credential-bearing profile content.
- Use the actual installed Codex, Claude, Grok, and Qwen clients only for the products available on the
  host. Missing products are valid aggregate-install cells.
- For live release-candidate credit, Linux and macOS must validate the candidate binaries from the
  same signed commit and tree. The network-interoperability cells additionally use separately
  identified builds of this repository from unrelated commits; software-version interoperability
  depends only on their declared hub-protocol versions.

## 1. Repository gate

```sh
make test
make test-race
go vet ./...
make lint

release_root="$(mktemp -d)"
for pair in 'linux amd64' 'linux arm64' 'darwin amd64' 'darwin arm64'; do
  set -- $pair
  make build-release-platform \
    GOOS="$1" GOARCH="$2" \
    RELEASE_VERSION="$(cat deploy/agent-sessions/VERSION)" \
    RELEASE_OUTPUT_DIR="$release_root"
done
```

Before replacing any implementation, freeze the closed baseline functional-cell inventory in
`specs/002-unified-user-daemon/evidence/baseline-functional-cells.md`. It must name the exact existing
test or live command for every peer, lane, group, host-suffix, permission, archive, collection,
cleanup, resume, 16-cell composition, and federation behavior, plus its expected functional result.
Final acceptance reruns and reports every row individually; an aggregate suite result cannot stand in
for an unnamed or skipped cell.

Expected:

- normal and race suites pass with no race report;
- vet and the repository-managed linter pass on both Linux and macOS CI runners;
- all four archives build both `agent-sessions` and `agent-sessions-hub` from the authoritative release
  inventory;
- no test launches a second long-lived daemon for the same OS user.

## 2. Clean install with optional products

Run separate clean-user cells with zero, one, several, and all four native products available:

```sh
make install-all
```

Expected:

- missing native products are reported as unavailable and do not fail aggregate installation;
- one immutable release and one stable host-role current selection are installed;
- all installed host command aliases resolve to the exact `agent-sessions` image;
- `agent-sessions-hub` is a distinct image and its service is not installed or enabled by
  `make install-all`;
- only available product connectors are installed;
- one standard user service is installed and enabled;
- one daemon generation becomes ready.

### Linux service proof

```sh
systemctl --user status agent-sessions.service
agent-sessions status --json
agent-sessions doctor --json
```

### macOS service proof

```sh
launchctl print "gui/$UID/net.antst.agent-sessions"
agent-sessions status --json
agent-sessions doctor --json
```

Expected status fields include exact binary/runtime identity, generation, PID/start identity, one
endpoint, host/hub configuration, product readiness, attachments, lanes, federation, migration, and
debt. Output contains no message, prompt, result, transcript, tool, or credential content.

Validate the canonical command inventory before live work:

```sh
agent-sessions help --json
agent-sessions status --help
agent-sessions doctor --help
agent-sessions migrate --help
agent-sessions remove --help
agent-sessions purge --help
agent-sessions-hub --help
codex-peer --help
claude-peer --help
grok-peer --help
qwen-peer --help
codex-peer-lane --help
claude-peer-lane --help
grok-peer-lane --help
qwen-peer-lane --help
```

Expected: the checked `docs/CLI.md`, machine-readable inventory, generated help, installed aliases,
and instantiated parsers agree exactly on every Agent Sessions command, option, environment binding,
stable JSON field, and exit class. Native passthrough options are marked as vendor-owned rather than
misrepresented as Agent Sessions options.

## 3. Explicit-stop discriminator

### Linux

```sh
systemctl --user stop agent-sessions.service
peer resume <known-session-id>       # expected nonzero: daemon unavailable
systemctl --user start agent-sessions.service
```

### macOS

```sh
launchctl bootout "gui/$UID/net.antst.agent-sessions"
peer resume <known-session-id>       # expected nonzero: daemon unavailable
launchctl bootstrap "gui/$UID" "$HOME/Library/LaunchAgents/net.antst.agent-sessions.plist"
```

Expected:

- workflow failure does not recreate the service or endpoint;
- the service remains absent for at least 30 seconds after explicit stop;
- explicit start restores the same configured host, groups, catalog, and debt;
- no native fallback carrier is attempted.

## 4. Four-product interactive peer restart

Start one managed peer from each available product in the same explicit group:

```sh
codex-peer  -g unified-acceptance
claude-peer -g unified-acceptance
grok-peer   -g unified-acceptance
qwen-peer   -g unified-acceptance
```

From the model-facing Agent Sessions tools, prove:

1. each peer lists the other three;
2. direct send and correlated reply work in both directions;
3. explicit multicast resolves all destinations before dispatch;
4. named-group broadcast reaches exactly the other group members;
5. a peer in another group stays hidden;
6. equal display names remain disambiguated by exact identity/host suffix.

Restart only the daemon through the service manager while all four native TUIs remain open. Repeat the
matrix.

Then stage a distinct validated host release with the same local state contract and hub-protocol
version and run the supported install/upgrade transaction while the same four native TUIs remain open.
Repeat the matrix again and compare exact native PID/start and session identities to the pre-upgrade
baseline.

Expected:

- native process/session identities are unchanged;
- local messaging recovers within 10 seconds;
- accepted messages are neither lost nor delivered twice;
- the installer-driven upgrade publishes one successor daemon generation without terminating or
  replacing any of the four native peer processes;
- exact process census shows one Agent Sessions host daemon, no supervisor, shim, product host, lane manager,
  routing agent, or separate federation agent;
- vendor-required MCP relays, if present, own no listener or durable Agent Sessions state.

### Mixed-workload capacity discriminator

```sh
scripts/test-unified-stress
```

Run 100 simultaneous managed attachments distributed across Codex, Claude, Grok, and Qwen, including
concurrent production, development, and test groups under the same OS user and daemon. Verify every
attachment has an exact identity, existing global-group authorization yields the same recipients as
the baseline, the process census contains one daemon and one listener, accepted messages remain
durable, and restart creates neither a duplicate delivery nor a duplicate lane turn. This is not a
quota test: actual resource exhaustion must fail before acceptance with its specific cause.

## 5. Bare-session opt-out

With integrations installed, start ordinary `codex`, `claude`, `grok`, and `qwen` sessions without the
managed wrappers. Invoke the visible Agent Sessions MCP surface where the vendor exposes it.

Expected: every bare session receives the inactive/unavailable result, publishes no attachment, and
causes no daemon-admin or credential/profile mutation.

## 6. Complete lane matrix

For every parent product and target product, exercise:

```sh
<target>-peer-lane start --name <unique-name> -g unified-acceptance -C <workspace> -
<target>-peer-lane wait <session-or-name>
<target>-peer-lane resume <session-or-name> -
<target>-peer-lane wait <session-or-name>
<target>-peer-lane status <session-or-name>
<target>-peer-lane archive <session-or-name>
```

Use the model-facing lane tool for the corresponding local and remote parent paths. In every one of
the 16 parent-target cells, wait until that cell's turn is durably accepted and active, restart the
daemon, and record that cell's exact recovery outcome before proceeding to the next cell. A restart
performed for one parent-target pair does not provide restart credit for another pair.

Expected:

- all 16 parent-target combinations preserve parent/group/permission semantics;
- supported reconnectable turns continue without redispatch;
- each evidence-approved non-reconnectable native pipe produces exactly one interrupted, collectable,
  resumable result;
- concurrent collect is idempotent;
- archive uses the native product history and leaves no Agent Sessions manager process/socket.

## 7. One hub, multiple host daemons

On the one designated central hub deployment, use the hub-only install path:

```sh
make install-hub HUB_LISTEN=:7419
agent-sessions-hub --help
agent-sessions-hub status --json
agent-sessions-hub doctor --json
```

Verify this installs/enables only `agent-sessions-hub` and its service: no `agent-sessions` host daemon,
host aliases, connector payloads, vendor probing, or host runtime state. Configure at least three host
daemons to connect to that hub. Do not start a separate federation agent. Exercise cross-host peer
messaging and every remote lane target, then restart each host daemon independently.

Exercise three network-interoperability cells:

1. hub and hosts from the same build and protocol version;
2. independently built hub and hosts from unrelated repository commits that declare the same
   protocol version;
3. a fixture with a mismatched protocol version.

During cell 2, upgrade one host through the normal host install transaction. Record the exact hub PID,
process start, executable/build identity, service state, and other host connections before and after;
all remain unchanged. Cell 3 fails during handshake before registration or work acceptance and reports
the coordinated deployment requirement.

Expected:

- global groups and host-suffixed peer names select the same recipients as the baseline;
- host capability advertisement comes from the daemon's ready adapters;
- either host recovers federation within 30 seconds after restart or sleep/wake;
- no local fallback or duplicate remote lane occurs;
- the hub remains the one existing `agent-sessions-hub` and does not gain host-daemon responsibilities;
- protocol-preserving host upgrades never restart, replace, install, or upgrade the hub;
- protocol-preserving hub upgrades leave host daemons running and reconnect them without local
  upgrade.

After the federation cells, validate `make remove-hub` against a disposable hub installation. It stops
and removes only the exact hub service, selected binary/releases, and disposable runtime state; it
preserves hub configuration and durable metadata and leaves every host daemon, vendor profile, and
remote host unchanged. Reinstall and prove the same hub identity/configuration returns. Then generate
an offline revision-bound hub purge plan with `make purge-hub-inspect PURGE_PLAN=/tmp/hub-purge.json`,
apply it with `make purge-hub PURGE_PLAN=/tmp/hub-purge.json`, and prove apply deletes every enumerated
hub-owned target and nothing owned by the host role, a vendor, or a remote host. In a co-located fixture,
select different host and hub releases and prove either role can upgrade, roll back, remove, and reinstall
without changing the other role's selection, service process, or readiness.

## 8. Legacy migration discriminator

On a disposable acceptance user, construct or preserve a real pre-unification estate containing:

- old/new runtime-root spellings;
- two responsive supervisors;
- one stale shim count with no matching process;
- two exact live managed blockers (one peer and one lane);
- one Grok/Qwen host or lane manager;
- one old host federation agent and configured hub identity;
- unrelated native and control processes.

Inspect without mutation:

```sh
agent-sessions migrate inspect --json
```

Expected:

- every exact live managed peer and lane is named;
- the stale count is non-blocking after owner absence is proven;
- unknown identity is explicit debt;
- unrelated processes are excluded.

After the operator closes every named managed peer and lane, rerun the normal install/upgrade
transaction. Do not test or implement live handoff from the legacy processes.

Expected:

- catalog, global groups, names, lane state, collection cursors, notices, hub/host configuration, and
  debt migrate;
- vendor transcripts/credentials/profiles remain untouched;
- every old Agent Sessions authority/listener is retired;
- one ready daemon endpoint remains;
- rollback restores the prior usable authority if successor readiness is fault-injected before commit.

## 9. Upgrade transaction and crash injection

Stage a successor with a distinct release identity and inject failures at:

1. archive/manifest validation;
2. connector preparation;
3. current-pointer commit;
4. service stop/restart;
5. successor state recovery;
6. adapter readiness;
7. legacy retirement;
8. transaction-journal finalization.

Expected: failures before committed readiness leave or restore the previous usable release; failures
after durable acceptance retain exact recoverable debt; no sample at return or 1/5/10/30 seconds finds
a mixed-version authoritative estate.

## 10. Observability content canaries

Seed unique canary content into peer messages, lane inputs/results, tool arguments/results, and test
vendor transcripts. Exercise both host and hub through normal, debug, rejected-operation, crash, and
service-manager restart paths while collecting every registered operational sink:

- daemon and hub stdout/stderr and structured logs;
- systemd user journal or launchd-managed stdout/stderr;
- human and JSON status/doctor output;
- metrics and traces, including disabled/empty projections when no exporter is configured;
- crash reports and bounded recovery diagnostics.

Expected: no sink contains any canary content or credential value, every sink is present in the closed
test-owned observability manifest assembled from the host-core, service-manager, and hub fragments,
and the bounded metadata still identifies the exact operation,
state, revision, timing, and non-secret failure cause needed for diagnosis. Run this on real installed
Linux and macOS services; an in-process redaction unit test alone receives no acceptance credit.
The manifest is acceptance evidence only; it does not create a production sink registry or runtime
subsystem.

## 11. Removal and purge

With one managed peer or lane active:

```sh
make remove
```

Expected: nonzero refusal listing every exact blocker and zero mutation.

After quiescence:

```sh
make remove
make purge-inspect PURGE_PLAN="$PWD/purge-plan.json"
```

Expected normal removal:

- service, releases, command aliases, connectors, and ephemeral runtime artifacts are gone;
- Agent Sessions configuration and durable metadata remain for reinstall;
- all vendor credentials, profiles, transcripts, and native sessions remain untouched.

Apply purge only with the exact revision-bound plan produced by the offline inspection target, for
example `make purge PURGE_PLAN="$PWD/purge-plan.json"`. The target uses the canonical executable from
the verified release/source tree and must not reinstall or start the daemon. Verify it deletes every
enumerated Agent Sessions-owned target and no vendor-owned or unrelated artifact.

## 12. Final acceptance record

For Linux and macOS record:

- exact commit, tree, signature identity, release/runtime identity;
- Go, linter, Codex, Claude, Grok, and Qwen versions;
- normal/race/vet/lint/four-build results;
- service-manager, install/upgrade/rollback, migration, peer, lane, federation, removal results;
- exact attributable process/socket/file residue and preserved-state evidence;
- any rejected, skipped, or confounded evidence separately.

Release-candidate credit requires both platforms green at the same exact signed commit with no waiver.
That evidence rule does not require already deployed network participants to share its commit; their
software versions interoperate solely by exact hub-protocol-version equality, while ordinary identity,
routing, group, and operation checks still apply.
