# Quickstart Validation Guide: Six-Product Support

This guide is the acceptance runbook for implementation candidates. Commands
are illustrative until the corresponding task lands; the contract and expected
outcomes are normative.

## 1. Prepare an Isolated Candidate

```bash
git worktree add /tmp/agent-sessions-six-products-test <candidate-sha>
cd /tmp/agent-sessions-six-products-test
git status --short
```

Expected: exact candidate SHA and empty porcelain. Do not run live product
tests from a dirty user checkout.

Record:

```bash
git rev-parse HEAD
git rev-parse HEAD^{tree}
go version
uname -a
```

## 2. Run Phase-0 Truth Gates

Before runtime interfaces freeze, execute the spike harness for:

```text
S0  historical base decoder proof; protocol-4 uniform rewrite is T125
S1  Kilo two-instance routing and attach parity
S2  DSH exact tuple, Cordis wake/steer, parent facade
S3  CodeBuddy wrapper/registry/socket ownership and restart isolation
S4  OpenCode/Kilo/Pi/OMP exact parent-session context
S5  legacy reachability and extract-and-freeze decision
S6  deterministic ten-product catalog projection
```

Expected: one structured evidence file per gate, exact native versions, no
credential content, and no unresolved red result. A red result updates the
contracts before implementation continues.

## 3. Validate the Derived Catalog

After building:

```bash
make build
bin/$(go env GOOS)-x64/agent-sessions catalog --json
./scripts/release-inventory host-aliases
./scripts/release-inventory plugins
```

Use the repository's platform directory spelling on arm64.

Expected:

- ten unique products;
- peer/lane aliases for all ten;
- exactly one integration asset root per product;
- CodeBuddy marked experimental until its Tencent cell passes;
- exact DSH tuple metadata;
- no shell-authored product list or projection drift.

## 4. Run Shared Contract Tests

```bash
go test ./internal/productruntime ./internal/localtransport \
  ./internal/component ./internal/productserver ./internal/structuredprocess
go test ./internal/daemon -run Lane
go test ./internal/federation
```

Repeat applicable packages with `-race` and fuzz the federation/component frame
decoders under the repository's bounded fuzz budget.

Expected:

- registry rejects missing/extra drivers and duplicates;
- component bootstrap/reconnect fails on wrong PID/start/ancestry;
- ledger crash matrix has no lost or duplicate accepted input;
- dispatch ambiguity is explicit and not replayed;
- one explicit opaque protocol-4 capability reaches only a ready exact destination;
- loopback client rejects redirect, proxy, non-loopback, oversized, and
  malformed responses.

## 5. Install in an Isolated Home

```bash
test_home=$(mktemp -d)
HOME="$test_home" XDG_CONFIG_HOME="$test_home/config" \
XDG_STATE_HOME="$test_home/state" XDG_DATA_HOME="$test_home/data" \
  make install PREFIX="$test_home/.local"
```

Expected: installed products receive exactly their catalog-derived integration;
absent products are structured skips. Re-running is idempotent. Induced failure
restores the exact prior host release and native registrations.

After the test, remove only the exact validated temporary home.

## 6. Doctor and Roster

```bash
agent-sessions catalog --json
agent-sessions doctor
agent-sessions roster
for product in opencode kilo pi omp codebuddy dsh; do
  "${product}-peer-lane" doctor --json </dev/null
done
```

Expected:

- no command waits for terminal stdin;
- native version and required feature keys are explicit;
- unsupported/absent products are fail-closed;
- CodeBuddy federation readiness reflects its experimental/account gate;
- DSH reports exact tuple and profile/plugin ownership;
- no peer secret or owned lane-server secret appears.

## 7. Peer Matrix

For each installed product:

```bash
<product>-peer -n <product>-accept -g six-product-accept
```

From a second grouped peer, use the common Agent Sessions tool to:

1. list and address the new peer;
2. send `IDLE_WAKE_<PRODUCT>` while it is idle;
3. start a slow native turn and send `BUSY_<PRODUCT>`;
4. have the new product send an acknowledgment outbound;
5. rename in the native product and inspect roster;
6. terminate and resume the same native session;
7. restart `agent-sessions.service` or the launchd agent and send again.

Expected: visible rendering, exact wake/steer-or-queue behavior, same native ID,
updated external name, and no keystroke injection.

Additional mandatory cases:

- Kilo: two attached TUIs, zero cross-delivery.
- CodeBuddy: daemon restart re-discovers the exact TUI, stale rows and recycled
  ports fail socket-to-PID/executable/ancestry checks, and peer versus owned
  authenticated lane endpoints remain distinct.
- DSH: Cordis plugin in the exact profile and socket under HOME/XDG.

## 8. Lane Matrix

For each product:

```bash
<product>-peer-lane start --name <product>-lane --cwd "$PWD" -- <briefing>
<product>-peer-lane status <thread-or-session-id>
<product>-peer-lane wait <thread-or-session-id>
<product>-peer-lane archive <thread-or-session-id>
```

Also run the same lifecycle through the structured MCP lane tool. During a slow
turn, deliver another input and verify a native steer or durable queued receipt.
Restart the daemon at every ledger transition using the deterministic fault
hooks, then prove exact recovery or explicit ambiguity.

Expected: one immutable native session, ordered receipt sequences, exact
terminal result, collection debt until collected, idempotent archive, and no
owned residue.

## 9. Parent Matrix

Inside each managed new-product TUI:

1. invoke the shipped Agent Sessions tool/command;
2. list peers;
3. send a direct message;
4. start one same-product lane and one different-product lane;
5. collect both;
6. observe both terminal notices in the visible TUI.

Run an adversarial tool call with a false native session ID.

Expected: normal calls bind to the exact component/product-native session;
false claims fail closed; shared daemon-wide MCP identity never substitutes for
per-session evidence.

## 10. Federation Matrix

Install the candidate host and hub on two trusted-network machines, configure
the hub through systemd and launchd service environments, and inspect roster.

For each ready product:

```bash
<product>-peer-lane --host <remote-host> doctor --json </dev/null
<product>-peer-lane --host <remote-host> run --name remote-<product> -- <briefing>
```

Expected: only advertised exact capabilities execute. Unknown products on an
protocol-mismatched hop are rejected before registration, never remapped. Groups and parent anchors
remain source-host canonical.

## 11. Full Gate

```bash
make lint
make test
make test-race
go vet ./...
for target in 'linux amd64' 'linux arm64' 'darwin amd64' 'darwin arm64'; do
  set -- $target
  make build GOOS="$1" GOARCH="$2"
done
```

Then run the real-product acceptance runner on Linux and a physical macOS
runner. Generate the release gate manifest and verify its commit/tree, native
versions, catalog projection, all capability cells, and absence of skipped
credit. The one allowed pending result is the explicitly labeled CodeBuddy
Tencent-authenticated model-turn cell.
