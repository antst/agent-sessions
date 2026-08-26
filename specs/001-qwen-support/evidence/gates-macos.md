# Qwen macOS rehearsal gate

- Date: 2026-08-24
- Platform: macOS arm64, stock Darwin temporary directory
- Commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Parent: `5f881f771d349cc4d8f1c51c61dfcedc17a0adb2`
- Signature: good SSH signatures on both commits from
  `SHA256:lgAnkhJdgKV1odY8EpHWrEpCwDRVj0NWAJijtWvpeXU`
- Version: `0.2.4`
- Evidence class: pre-candidate rehearsal, not tagged-commit evidence
- Verdict: GREEN

## Source gate

The verifier fetched the exact pushed commit, checked out a detached clean
worktree, verified the supplied tree and two-commit chain, and read the delta
before executing it. Every repository-owned gate exited 0:

```text
make test
make test-race
go vet ./...
make lint
make build-release-platform  # linux-x64, linux-arm64, darwin-x64, darwin-arm64
```

The run started at `2026-08-24T22:24:06Z` and finished at
`2026-08-24T22:28:27Z`. Normal and race suites contained zero top-level
failures; the race suite emitted zero data-race findings. Vet emitted no
diagnostic, repository-managed lint reported `0 issues.`, and all four
authoritative `0.2.4` rehearsal archives built. The worktree remained clean.
Go was `1.26.5`; the repository-managed golangci-lint was `2.12.2`.

## Same-version integration reconciliation

The selected authorized Qwen profile contained the rejected predecessor's
same-version MCP command:

```text
${extensionPath}${/}scripts${/}native-entry
```

With zero live Qwen processes, `make dev-install-qwen` took the native scoped
uninstall/install reconciliation path and passed exact post-verification. All
eight installed public payload files matched the `b8bc013` source. The final
MCP command is exactly `./scripts/native-entry`, with argument `mcp`; the entry
is a regular, non-symlink, executable mode-`0755` file.

From fresh workspace `/private/tmp/qwen-cleanws-b8bc`, outside the repository
and containing no `.mcp.json`, native Qwen reported:

```text
agent_sessions: /Users/antst/.qwen/extensions/agent-sessions/scripts/native-entry mcp (stdio) - Connected
```

`qwen extensions list` independently reported enabled extension
`agent-sessions (0.2.4)` and MCP server `agent_sessions`. Qwen therefore
resolved the contained relative command to the installed extension path; no
repository fallback supplied the registration.

## Exact-runtime managed-parent discriminator

The driver asserted that the worktree binary directory was absent from
`PATH`, launched managed parent `disc-b8bc` on an isolated no-hub agent, and
approved only the structured `identity` and `list_peers` calls once each.
Both tools existed and returned normally. `identity` reported the managed Qwen
session and its real stable Unix socket; `list_peers` correctly reported no
shared peer for the lone parent.

Primary runtime proof came from the exact parent process tree, not tool prose.
The MCP server descendant of Qwen parent PID `13658` was PID `13873`, whose
executable was the exact rehearsal build:

```text
/private/tmp/claude-501/-Users-antst-work-ai-agent-sessions/e6e009e6-3428-4477-9d77-a5e2947767ec/scratchpad/mx-target/bin/darwin-arm64/agent-session-runtime
```

The owner-installed runtime under `/Users/antst/.local/libexec/` did not
appear anywhere in that tree. As corroboration, the loaded lane schema used
the current product-neutral `supported-product lane lifecycle` description,
and the identity schema named a Qwen session rather than the older Codex-only
wording. The parent exited through `/quit`; its tmux session and process tree
were empty afterward.

## Cleanup and owner boundary

The isolated no-hub agent was re-attested immediately before `SIGTERM` by PID
`1576`, exact executable, process start, and argv; it exited and removed its
agent socket. Five disposable command-form profiles, the clean workspace, and
the discriminator scaffolding were each validated as test-owned, non-live,
non-symlink roots before exact removal. Final exact-executable counts were zero
for the host agent, runtime, Qwen peer, and all four lane launchers.

The authorized owner Qwen extension remains installed at exact version
`0.2.4` with command `./scripts/native-entry`. A concurrent owner Codex
authentication-file mtime change was observed by metadata only; the file was
not read, copied, hashed, diffed, attributed to this cell, or restored. The
verifier retained earlier explicitly preserved federation and Qwen contract
transcript roots because they were outside this teardown authorization and had
no live references.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
