# S2: DSH exact-tuple and Cordis truth spike

Run from the repository or any directory:

```sh
scripts/spikes/six-product/dsh/run.sh
```

The spike installs `@deepseek-ai/dsh` and `@deepseek-ai/dsh-acp-app` at exactly
`0.1.2-alpha.3` with pnpm into a validated temporary root. It loads a real
Cordis protocol-driver plugin into a real ACP profile. Only the model adapter is
deterministic; no DSH, ACP, Cordis, tool, sandbox, or session protocol is
mocked.

It proves:

- exact tuple validation and fail-closed rejection of a mismatched member;
- `ctx.agents` enumeration;
- idle `followup`, busy `steer`, and turn completion;
- the selected native registered-tool parent facade and exact `exec.agent` ID;
- a component socket under HOME is visible from the bwrap sandbox while `/tmp`
  is masked, with native `DSH_SESSION_ID` corroboration;
- ACP cancellation is a notification, while a request is rejected with
  `-32601`;
- a second ACP prompt during a live request is rejected with `-32602`, so the
  shared durable lane ledger must queue it;
- projection cache state is not usable as real-time liveness.

No API key is read. The runner removes both its `/tmp` root and its validated
HOME state/socket root after recording bounded evidence.
