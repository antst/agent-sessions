# S6 catalog/install projection spike

This directory is a phase-0 fixture, not production code. It models the four
existing and six proposed products in one staged Go executable and derives four
stable projections from that single descriptor set:

- `catalog`
- `release-inventory`
- `install-plan`
- `acceptance-matrix`

The fixture adds explicit `parent` capability data, uses one bounded lower-case
token validator for product IDs and federation capabilities, records aliases,
payloads, runtime/install strategy keys, support state, native baselines, DSH's
exact pnpm tuple, doctor features, and real-product acceptance requirements.
CodeBuddy's projection explicitly separates its credential-free managed peer
path (native registry plus literal-loopback process attestation and the constant
`X-CodeBuddy-Request: 1` CSRF header) from Agent Sessions-owned lane servers,
whose product password remains memory-only.

Run:

```sh
scripts/spikes/six-product/catalog/run.sh
```

The runner builds a staged executable, proves byte-stable output, verifies each
derived consumer view, checks a pinned digest, demonstrates that a changed
projection fails verification, and rejects a shell-authored product list.
