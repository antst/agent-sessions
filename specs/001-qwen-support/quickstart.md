# Quickstart Validation: Qwen Support

This guide is the end-to-end validation outline for the implementation. Managed Qwen preserves the
normal Qwen product experience and adds authenticated Agent Sessions communications plus the ability
to launch local or remote lanes for every supported product. Qwen continues to own its native tools,
UI, permission prompts, and in-session approval-mode changes.

## 1. Prerequisites and baseline

Use one real Linux host and one real macOS host with:

- the same exact Agent Sessions commit and plugin build on every hub, agent, and launcher;
- Qwen Code at or above `0.21.15`, plus every live native-contract probe;
- authenticated native Qwen profiles managed by Qwen itself;
- isolated Agent Sessions runtime/state roots and, for destructive acceptance, isolated explicit
  Qwen homes installed through the native extension command;
- no compatibility hub or fallback transport.

Record before mutation:

- exact Git commit/tree/signature and Go, repository linter, Qwen, Codex, Claude, and Grok versions;
- selected profile paths/value-or-absence and non-secret file hashes/mtimes;
- exact process identities, rows, sockets, listeners, active/archived lane counts, and unrelated
  native transcripts;
- current hub/agent identities and capabilities.

Do not copy, print, or rewrite credential values.

## 2. Source gate

Run the repository-managed commands on both Linux and macOS:

```bash
make test
make test-race
go vet ./...
make lint
```

Build all published targets:

```bash
make build GOOS=linux  GOARCH=amd64
make build GOOS=linux  GOARCH=arm64
make build GOOS=darwin GOARCH=amd64
make build GOOS=darwin GOARCH=arm64
```

Expected:

- no test, race, vet, linter, or build finding;
- every declared product appears in table completeness tests;
- clean worktree apart from the planned feature changes.

## 3. Isolated install and doctor

Install all product surfaces into an isolated prefix and install the Qwen plugin into the selected
test profile through Qwen's supported extension command. Then run:

```bash
qwen-peer-lane doctor --json -C <trusted-workspace>
qwen-peer-lane doctor --json --qwen-home <absolute-test-qwen-home> -C <trusted-workspace>
```

Expected ready environment:

- exact executable/package/version and selected profile;
- exact `agent-sessions` manifest, MCP server, and all four lane skills;
- trust ready; auth reported without secret values;
- ACP and native archive capabilities ready;
- the selected launch preference resolves to the expected initial native approval mode;
- no Qwen session, transcript, model turn, or leftover probe process.

Negative doctor cells deliberately exercise missing executable, version floor, unknown newer contract,
untrusted cwd, missing selected-profile plugin, auth unknown/unready, missing ACP capability, missing
archive capability, and an initial permission-mode mismatch. Each must fail with the actual
precondition.

## 4. Managed interactive peer

Launch a peer with Qwen's unmodified native initial approval behavior:

```bash
qwen-peer -n qwen-a -g qwen-acceptance -C <workspace>
```

Validate:

1. start a monotonic elapsed-time measurement immediately before the documented launch command and
   fail the cell unless launch, authorized discovery, one direct message/reply, and one named-group
   broadcast complete in under five minutes;
2. one native `session_start` with exact UUID/cwd/version/protocol;
3. one host participant with the selected launch preference and corroborated effective initial mode,
   correct groups, endpoint, and exact process identity;
4. Qwen's normal approval-mode controls remain usable, including entering and leaving yolo, without
   changing Agent Sessions identity, group membership, or messaging authority;
5. direct send/reply, explicit multicast, and named-group broadcast with correlated IDs, with
   transport instrumentation proving no extra hub round trip over the existing grouped route;
   separately inspect every live Codex, Grok, and Qwen session-stable published endpoint with
   `lstat`, require a Unix socket rather than a symlink, and complete correlated managed
   Claude-to-Codex and Claude-to-Grok deliveries through those exact paths without resolving them;
6. inbound delivery while idle and busy exactly once;
7. bare `qwen` may show installed surfaces but has no successful Agent Sessions operation;
8. exact UUID and unique-name resume preserve transcript/profile/groups and use the durable launch
   preference as the next initial-mode default;
9. profile mismatch, live duplicate, ambiguous name, continue/fork, invalid integration, and startup
   failure leave catalog/native state unchanged;
10. normal exit, Ctrl+C, SIGTERM, and wrapper SIGKILL converge without collateral.

Before crediting cleanup, seed one exactly owned legacy stable-socket symlink to a dead PID-bound
backend, one unrelated symlink, and one unrelated socket control. Reconciliation must remove only the
owned legacy pair, preserve both unrelated controls, and leave no attributable stable or backend
socket residue after normal exit and crash recovery.

Repeat the peer launch as four separate cells using: no permission option, `--no-yolo`, `--yolo`,
and native `--approval-mode plan` with no wrapper permission option. Corroborate initial native modes
as the Qwen default, `default`, `yolo`, and `plan` respectively; verify the exact tagged durable
launch preference; and resume each session without an override to prove the retained default. While
the yolo cell is live, use Qwen's normal UI to cycle away and back and verify Agent Sessions does not
interfere or misreport the launch preference as the current mode. Also attempt one wrapper/native
permission combination and one repeated/contradictory wrapper combination; each must return exit 2
before preparation, catalog mutation, native child creation, or profile bookkeeping.

## 5. Durable Qwen lane

From a live managed parent, run:

```bash
printf '%s\n' 'Return only QWEN_LANE_OK' | \
  qwen-peer-lane start --name qwen-lane-a --inherit-groups -C <workspace>
qwen-peer-lane wait <thread-id>
qwen-peer-lane status <thread-id>
qwen-peer-lane interrupt <thread-id>
qwen-peer-lane archive <thread-id>
qwen-peer-lane archive <thread-id>
qwen-peer-lane resume <thread-id>
```

Expected:

- distinct stable Agent Sessions and native Qwen IDs;
- one active ACP prompt, queued follow-ups, one terminal result, and no second collection debt;
- exact parent notification and private anchors, with explicit group inheritance only;
- interrupt reports the common interrupted outcome/exit contract;
- archive performs native Qwen archive only after worker/tool retirement;
- repeated archive is explicitly idempotent;
- resume performs native unarchive then exact ACP resume with both IDs unchanged;
- failed partial unarchive/resume re-archives or retains typed cleanup debt;
- the requested initial native approval mode is corroborated, while later Qwen-native changes are
  permitted and do not alter Agent Sessions routing authority.

Run owned-versus-persistent owner-exit pairs with auto-archive disabled so ownership is the only
variable.

Run separate lane starts using no permission option, `--no-yolo`, `--yolo`, and native
`--approval-mode plan`, and corroborate the same four initial-mode/launch-preference mappings through
ACP before publication. A wrapper/native permission combination and a repeated/contradictory wrapper
combination must fail with exit 2 before manager or worker creation. Execute these permission cells
on both Linux and macOS; later Qwen-native mode changes remain allowed and do not alter lane
identity, routing, ownership, or cleanup.

## 6. Crash and adversarial lifecycle

On both platforms, induce one controlled failure at each boundary:

- peer wrapper before event publication and after publication/before registration;
- input/event path replacement and changed type;
- lane manager, ACP worker, detached shell process group, MCP child, archive helper, and helper's
  preheated ACP child;
- host agent and supervisor restart;
- recycled PID/strong-start mismatch;
- duplicate collector and concurrent follow-up;
- native transcript externally archived/unarchived or opposite-state conflict.

At return and +1/+5/+10/+30 seconds, require zero attributable process, row, socket, temp setting,
input/event file, helper, worktree, pending notice, or unrecorded debt. Every unrelated baseline item
must survive. Ambiguous artifacts remain explicit debt and are not deleted.

## 7. Four-product composition and federation

Run all 16 parent-target contract cells and live-test every row/column involving Qwen. For inherited
groups, require exactly the explicit group plus source-parent and destination-child anchors.

Then connect the exact same build on Linux and macOS and run:

- Linux Qwen parent to macOS Qwen target;
- macOS Qwen parent to Linux Qwen target.

For each, execute the emitted `Collect:` line verbatim. Confirm the target exists only on the selected
host, returns the exact token to the exact source parent, archives cleanly, and leaves no destination
process/socket/helper residue. A disconnected/unready/non-capable target must fail before creation.

## 8. Release archive

Exercise the repository-owned build/package entrypoint used by the actual
`.github/workflows/ci.yml`, create every platform archive, verify checksums and the
eleven-executable/four-plugin/doc inventory from
[`contracts/federation-install.md`](contracts/federation-install.md), and perform one fresh prebuilt
installation without Go or a development checkout on each OS. Re-run doctor plus one peer and one
lane smoke cell from the installed archive. A test must fail if the workflow carries a private
hard-coded product list or can omit either Qwen executable or any plugin payload.

At the exact signed release commit, the final workflow run must emit immutable workflow artifact
`agent-sessions-v0.2.1-release-evidence-<full-commit-sha>` containing byte-stable
`agent-sessions-v0.2.1-release-evidence.json`. Validate every required field and digest from
[`contracts/release-evidence.md`](contracts/release-evidence.md) against normative
[`contracts/release-evidence.schema.json`](contracts/release-evidence.schema.json), including the
cross-field and canonical-byte rules, before creating the tag. Refuse tag creation if local or remote
`v0.2.1` already exists. The signed tag annotation must identify the workflow run, artifact name, and
JSON SHA-256. The tag-release job must require and verify that exact existing signed tag, refuse an
existing release or same-named release asset, retrieve the exact artifact by run identity, attach the
unchanged JSON to the new GitHub release, and include it with all four archives in `SHA256SUMS`.

Release only when:

- initial-mode mapping and corroboration pass on Linux and macOS, including native mode changes after
  publication;
- every source/live/install/federation cell is green at one signed exact commit;
- monitored owner credentials/settings/transcripts are unchanged;
- the release tag points to that exact commit and contains the required evidence trailers;
- the published JSON bytes and SHA-256 match the workflow artifact named by the signed tag.
