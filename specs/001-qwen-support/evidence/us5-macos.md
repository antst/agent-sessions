# US5 macOS: no-Go prebuilt install and smoke

- Date: 2026-08-24
- Platform: macOS arm64
- Commit: `b8bc0136ca37de484588d2e3ce4a978f186a19a7`
- Tree: `2e314a145d99f907ccfe71b568f27c1417395805`
- Version: `0.2.4`
- Qwen Code: `0.22.0`
- Credited archive: `agent-sessions-0.2.4-darwin-arm64.tar.gz`
- Archive SHA-256: `bbc8ceee9fe9dc1fb143d1f32f94104b54faa35bad4defd01f20f5546d7fa12c`
- Archive size: 24,037,066 bytes
- Credited roots: `/private/tmp/t077-b8bc` and `/private/tmp/t077-unauth`, both removed
- Verdict: GREEN

## Package and Go-absent installation

The archive was the exact output already produced by the `b8bc013` macOS
S-tier; it was not rebuilt for this cell. It contained the prebuilt marker,
the authoritative eleven executables, four plugin roots, and Makefile. Inside
the explicitly constrained child environment, `command -v go` returned no
result, `go version` failed, `gofmt` was unavailable, and `GOROOT` was unset.
The extracted build selected `Using packaged prebuilt binaries`, then installed
into fresh isolated prefix/install roots.

| Executable | SHA-256 |
| --- | --- |
| `agent-session-runtime` | `1d9d294418b71bdd9586ccfba16fd2a49b55cfa8d1aa6854188176262ba3a52c` |
| `claude-peer` | `6fea0a413e2d1240cc9a93dd888f8a4c896f4612494e8c4100b00388ba0dd5c3` |
| `claude-peer-lane` | `9eb88cf34eb2e9b5052072845e2a4fd520328e362ad97cdf61ddb8675d902475` |
| `codex-peer` | `e4292179ccc12c16843c9065dccd1f92bc5c726ef66cd50704968cc7fcbfe9e0` |
| `codex-peer-lane` | `0e3b69c737a5a5ff46415ac7c15a7e6092f50c68a57526a4cd5b2b67732b6b3a` |
| `grok-peer` | `e0342f2f8b045f4e220abe3ef82dd05fc53bde0298a204bd0738e53e4c503016` |
| `grok-peer-lane` | `c69ccc960353983fa49e15b970985dcc5d28dfe2b217e50c23dcc55660fbd2a9` |
| `peer` | `0d3ba7e51097ea1ad45f3e7f708a5526f99c59c99df63ced6605da731f2af1b6` |
| `peer-federator` | `2851e56eff1a3075f4305145e7643332e689f69e318f28aaeac68fac84135e62` |
| `qwen-peer` | `c5aed46c8013333002397721fc7c1d8bcfbe261b094fa3413c292afd2b2e24e6` |
| `qwen-peer-lane` | `9558eb417fb473a55cce26108d648042887a2da17210e1cfcbf542cd79e10d6b` |

Every executable was mode `0755`. The packaged public
`qwen/scripts/native-entry` was a regular non-symlink executable, mode `0755`,
665 bytes, SHA-256
`47be7badd2066d6d4201602c7b84711e70bc2e2a5a066eefbcee341671b2430d`.
The packaged MCP command was exactly `./scripts/native-entry`.

## Packaged install and removal

Fresh unauthenticated profile `/private/tmp/t077-unauth` began empty and
contained no credential-shaped file. Packaged installation succeeded and
verified the relative MCP command and executable entry point. Packaged removal
then succeeded and left the extension absent; only native Qwen extension
bookkeeping remained. No native session was launched against this profile, so
the destructive half of the rehearsal never touched the authorized profile.

The already-authorized real profile was used only with a fresh dedicated Qwen
runtime. Exact package verification took the supported source-mismatch
reconciliation path because Qwen records the local extension source. The
payload was never left absent outside the native uninstall/install transaction.

## Packaged interactive peer

The isolated packaged agent published service row
`agent-e4cf4e87806040d34d6a` as `agent-sessions--t077`, PID `20812`, version
`0.2.4`, with socket `/tmp/t077-b8bc/agent-runtime/agent.sock` and no hub.
Managed peer `t077-peer` made one allow-once structured `identity` call and no
other tool call. Its exact result was:

```text
t077-peer — uds:/tmp/t077-b8bc/xdg-runtime/codex-claude-peer-501/session-c201908da1739fb9e1ea.sock
```

Native transcript `3a482048-7731-4b96-ba46-745b623558f3.jsonl` contained 13
records. The verifier displaced Qwen's suggestion placeholder, confirmed the
literal `/quit` input, and submitted it once; the session exited cleanly.

## Packaged persistent lane

Doctor reported ready for Agent Sessions `0.2.4`, Qwen contract `1`, package
identity, native provider, ACP, archive, trust, and integration. The packaged
persistent lane had:

| Field | Exact value |
| --- | --- |
| Lane/session/thread | `aa3e8a78-26c0-4e15-85e2-3c12fc583789` |
| Turn | `be50a138-918d-4899-bd7f-cebaf82fa22b` |
| Native Qwen session | `24f69403-f57b-436f-9a4e-eacc84dc3d71` |
| Groups | `session:t077/aa3e8a78-26c0-4e15-85e2-3c12fc583789` |
| Terminal token | `T077_PACKAGED_LANE_OK` |
| Outcome | `completed`, exit `0`, stop reason `end_turn` |
| Final state | explicitly archived; native archive `archived` |

Its five-record native transcript existed only beneath the isolated runtime;
the owner Qwen profile acquired zero new chats.

## Confounds and cleanup

Six harness/setup events are disclosed and receive no independent credit:

1. An omitted isolated prefix made the first installer read the wrong plugin
   root and fail before mutation.
2. Without isolated `CODEX_HOME`, install preflight correctly returned exit 75
   rather than stop an owner App Server.
3. The isolated Codex package tree had to be staged and its `current` link kept
   inside the test root.
4. Two lane starts correctly refused missing stable ownership and then missing
   grouped host agent; the credited persistent lane used the isolated no-hub
   agent.
5. The interactive peer was launched to satisfy a requested missing evidence
   cell before a later no-rerun message arrived. It was explicitly credited,
   allowed to finish once, and was not retried.
6. A Qwen suggestion placeholder defeated the verifier's idle-text detector.
   The verifier credits the native transcript/result and clean parent exit, not
   that detector.

The first deletion gate stopped on 56 overbroad filename matches: 55 public
Codex marketplace/plugin files and one Qwen token-count usage ledger. There
was no `auth.json`. A narrowed credential-store matcher found zero precise
credential shapes. Both roots then passed exact uid, mode, realpath, device,
open-file, process, escaping-symlink, and ownership gates before deletion.
Final exact-executable counts were zero for all eleven packaged products,
tmux, and Qwen. Packaged supervisor/App Server controls stopped cleanly; the
agent was signalled only after exact executable re-attestation.

## Owner profile and source-pointer classification

The real profile remains enabled at exact `0.2.4`, with command
`./scripts/native-entry` and all eight public payload files matching
`b8bc013`. T077 first restored its pre-cell source pointer to:

```text
/private/tmp/claude-501/-Users-antst-work-ai-agent-sessions/e6e009e6-3428-4477-9d77-a5e2947767ec/scratchpad/mx-target/qwen
```

The stable owner checkout at `/Users/antst/work/ai/agent-sessions` was then
inspected read-only. It is a clean detached v0.2.3-era tree with no `qwen/`
payload at all, so the exact-payload gate correctly refused to repoint or
mutate it. The current source exists and the installed copy is functional, but
the metadata points into the verifier's disposable scratchpad; a later native
extension update would fail after that scratchpad is reclaimed. This is a
known pre-existing development-install source-lifetime debt, not residue from
the deleted packaged roots. It remains pending a stable Qwen-bearing checkout
or real release installation; no owner checkout was changed to hide it.

An owner Codex authentication-file mtime changed concurrently while unrelated
owner Codex processes were live. It was observed by metadata only, was not
attributed to the rehearsal, and was not restored.

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```
