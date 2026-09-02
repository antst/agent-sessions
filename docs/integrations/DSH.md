# DeepSeek Harness (DSH)

DSH is supported as a headless lane engine only. It has no Agent Sessions peer
launcher, web profile, or installed Agent Sessions plugin.

The supported native tuple is exact:

- `@deepseek-ai/dsh@0.1.2-alpha.3`
- `@deepseek-ai/dsh-acp-app@0.1.2-alpha.3`
- `pnpm@10.28.1`

`dsh-peer-lane` drives `dsh --profile acp` through DSH's ACP surface. The lane
supports creating or exactly resuming a native session, sending a prompt,
waiting, interrupting, and archiving. DSH creates its own shipped `acp` profile;
Agent Sessions neither installs nor snapshots a DSH profile.

`default` maps to DSH `workspace-write`; explicit `bypassPermissions` maps to
`danger-full-access`. Doctor checks the exact native tuple and runs a live,
keyless ACP probe. Peer-mode DSH is intentionally unsupported because the
available DSH front ends do not provide a compatible terminal peer surface.
