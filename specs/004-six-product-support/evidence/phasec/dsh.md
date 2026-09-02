# DSH scope

DSH is lane-only by owner ruling. Peer mode is unsupported and uncredited.

The genuine DSH terminal front ends available for the pinned
`0.1.2-alpha.3` release were probed against the real product and were
incompatible with its exports. The web surface works but is not the selected
daily peer interface. The former Agent Sessions Cordis/web profile, plugin,
peer alias, peer launcher strategy, and peer-presence claims were therefore
deleted instead of being maintained beside an unsuitable product surface.

The retained scope is the headless ACP lane using the exact native tuple:

- `@deepseek-ai/dsh@0.1.2-alpha.3`
- `@deepseek-ai/dsh-acp-app@0.1.2-alpha.3`
- `pnpm@10.28.1`

DSH owns and creates its shipped `acp` profile. Agent Sessions installs no DSH
profile or plugin. Lane creation, exact resume, prompt, wait, interrupt, and
archive remain implemented in `internal/products/dsh`. Real-model lane credit
is pending the lane-mode end-to-end pass.
