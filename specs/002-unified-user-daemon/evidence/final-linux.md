# Final Linux Candidate Evidence

Date: 2026-08-30 UTC.

Status: **PARTIAL — the exact signed candidate passes every automated Linux gate and installed Codex,
Grok, and Qwen smoke/restart/resume/cleanup checks; authenticated Claude and the complete
schema-recorded 202-cell run remain open.**

This evidence does not grant aggregate credit to unexecuted acceptance IDs. In particular, the
candidate is not declared fully Linux-accepted while the native Claude CLI reports logged out and the
repository lacks one authoritative result record for every applicable matrix cell.

## Candidate identity

- Branch: `feature/unified-user-daemon-v2`
- Candidate commit: `170b96c98750b3ed5fbea42ed629da9f505379dc`
- Candidate tree: `3702f86b0d626428d51b0c7eadc81cabc7f0a30e`
- Parent: `2caef741b29f18272b79e76ce1cb664c3498dea9`
- Subject: `Harden exact acceptance credit`
- Candidate signature: good ED25519 signature,
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Checkout before and after candidate validation: clean.
- Host: `Linux pdev 6.17.4-2-pve x86_64`
- Toolchain: `go version go1.26.5 linux/amd64`
- Installed release: `0.3.0`
- Source, installed alias, and installed-current `agent-sessions` SHA-256:
  `3da9c32c448022c76618d663f6927af67f5ee85ff332da84de6a3bb054e66e79`
- Installed path:
  `/home/antst/.local/libexec/agent-sessions/host/releases/0.3.0/bin/agent-sessions`

## Exact-candidate Linux gates

The following were run uncached at the candidate commit and tree above:

| Gate | Result |
|---|---|
| `go test ./... -count=1` | PASS; all packages green |
| `go clean -testcache` then `./scripts/test` | PASS; bridge `40.419s` |
| `go clean -testcache` then `RACE=1 ./scripts/test` | PASS; bridge `54.657s`; zero race reports |
| `go vet ./...` | PASS |
| `make lint` | PASS; `0 issues.` |
| `./scripts/release-final-gate linux` | PASS in isolated `/tmp/agent-sessions-linux-gate.TPT61E` |

The release gate also passed focused bridge/federator/launcher/Qwen contracts, host and hub install and
removal transactions, federation current-command packages, and its permission, prebuilt-install, and
owner-nonmutation projections. The normal/race command inventory contains exactly the current command
packages `cmd/agent-sessions` and `cmd/agent-sessions-hub`; deleted split command packages are absent.

## Four-platform package proof

Two isolated package passes at this exact tree used explicit `GOOS` and `GOARCH` for all four targets.
Every second archive was byte-identical to the corresponding first archive. Each archive contained
exactly `agent-sessions` and `agent-sessions-hub`; the repository-only cleanup script and cleanup
contract were absent.

| Archive | SHA-256 | Binary format |
|---|---|---|
| `agent-sessions-0.3.0-darwin-arm64.tar.gz` | `3a5e77814cf416d96e3b23e3e2381de6531f06dfb0503dcf8cb85d6152e28fc1` | Mach-O arm64 |
| `agent-sessions-0.3.0-darwin-x64.tar.gz` | `4ba8389a505ea20f1054907549b90c7894bfd4c37fc29b023761aa4f312ed0d8` | Mach-O x86_64 |
| `agent-sessions-0.3.0-linux-arm64.tar.gz` | `b87f82144e993264fa64da4666b94a6d1136de2d212fac6e45697e9363dec81c` | ELF aarch64 |
| `agent-sessions-0.3.0-linux-x64.tar.gz` | `46179de2d7f6f67b791721cf4de0c4c0fb895ac9a6ea6a57cb7816edb08e05ad` | ELF x86-64 |

An earlier invocation incorrectly used an unsupported `PLATFORM` variable and produced host binaries;
it was explicitly rejected and receives no evidence credit. The Darwin rows above prove only package
inventory, reproducibility, and binary format—not macOS execution.

## Installed service and topology

- `make install` installed the exact signed candidate and returned success.
- `agent-sessions.service` is active/running, main PID `1893270`, generation `228`.
- The service executable is the `current/bin/agent-sessions daemon` image under the installed release.
- All peer and lane aliases for Codex, Claude, Grok, and Qwen resolve to that installed image.
- The live steady-state Agent Sessions executable census contains one daemon and one connector owned
  by the running Codex App Server; no split runtime or per-product host executable remains.
- No live `agent-session-runtime`, `peer-federator`, `claude-code-peer`, `codex-messaging`, or
  product-host image was found.
- Installation and daemon restarts preserved Codex App Server PID `998681`, started
  `2026-08-29 13:51:38`; vendor infrastructure was not restarted by installation.
- `agent-sessions status` after recovery reports one active attachment and service state `running`.

`KillMode=process` preserves vendor/App Server descendants across daemon restart. The installed restart
cells below prove the intended continuation/interruption behavior and absence of a duplicate daemon.

## Installed native readiness

| Product | Native identity | Installed daemon result |
|---|---|---|
| Codex | `codex-cli 0.151.0` | contract 2, `ready:true` |
| Claude | `2.1.251` | `logged_in:false`, `auth_method:none`, `ready:false` |
| Grok | `grok 1.0.13` | contract 2, `ready:true` |
| Qwen | `0.22.3` | contract 2, integration/parser/ACP/archive contracts ready |

Claude's row is an unresolved RED investigation, not a product pass and not `N/A`. Claude was
authenticated earlier on this host, but its credential file was rewritten at
`2026-08-30 19:00:49.794544884`, eight seconds before an earlier Agent Sessions service install/restart.
No credential contents were read or hashed. The exact `170b96c` installation preserved the already
logged-out file metadata; validation of individual plugin lifecycle commands in the logged-out state
also left it unchanged. Authentication must be restored and the logged-in install path isolated before
this row can receive credit.

## Real installed exact-candidate evidence

Fresh persistent Codex, Grok, and Qwen lanes were started from the installed image. Each inherited the
parent's `peer-dev` group and permission mode, sent a unique token through the structured Agent
Sessions messaging surface, completed with exit 0, was collected exactly once, archived, and
disappeared from `list_peers`:

- `MX170_CODEX_TO_OWNER_OK`
- `MX170_GROK_TO_OWNER_OK`
- `MX170_QWEN_TO_OWNER_OK`

The daemon was then restarted from PID `1888402` to PID `1893270` during one accepted active turn per
product, while Codex App Server PID `998681` remained unchanged:

- Codex continued the same accepted turn ID, completed with exit 0, and delivered
  `MX170_CODEX_RESTART_AFTER`.
- Grok produced one collectable `interrupted` result, exit 130, diagnostic
  `Agent Sessions daemon restarted during the accepted turn`.
- Qwen produced the same one-collectable-interruption contract, exit 130.
- Grok and Qwen were resumed successfully by stable name and by public lane UUID; Codex also resumed
  successfully by stable name and public lane UUID. The native session identity remained stable in
  each product and every destination-visible resume token arrived.
- Public lane UUID was proven to be the Agent Sessions thread UUID, not the vendor-native session ID:
  attempts using the vendor IDs correctly returned `lane was not found` and received no pass credit.
- All three lanes were finally archived and disappeared from `list_peers`.

The final local peer list contained only the intentional remote macOS validator; none of the exact
candidate test lanes remained addressable.

## Cleanup-tool boundary

`scripts/cleanup-pre-unification` remains a repository-only, controlled-host, pre-install tool. It is
not referenced by standard install, `install-all`, removal, or production runtime surfaces and is
excluded from release archives. A plan-only invocation after unified-host installation correctly
refused with `refuse pre-unification cleanup after unified host installation`; it was not worked around
and performed no cleanup mutation.

## Open Linux acceptance

1. Reauthenticate Claude Code in the owner's normal profile, isolate the logged-in plugin lifecycle
   commands using metadata-only checks, run an idempotent install, and prove authentication/history
   preservation. Then run the same fresh/message/restart, name/UUID resume, queue, archive, cleanup,
   parent/target, and interactive-session cells for Claude.
2. Materialize schema-complete result records for every applicable one of the 202 matrix IDs. The
   manifest, runner, and validator exist, but the repository still lacks a complete authoritative
   per-cell result set.
3. Run the independent operator quickstart from a clean committed checkout and retain its exact
   per-cell result and residue records. Automated transaction quickstart coverage is green but is not
   a substitute for the real-product matrix.

Until those items close, this candidate is suitable for continued local Linux testing but is **not yet
declared fully Linux-accepted for rollout to the central host**.
