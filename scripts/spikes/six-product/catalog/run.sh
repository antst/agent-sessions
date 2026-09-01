#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../../.." && pwd -P)
spike_root="$root/scripts/spikes/six-product/catalog"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-catalog-spike.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

go_bin=${GO:-}
if [[ -z "$go_bin" ]]; then
  go_bin=$(command -v go || true)
fi
if [[ -z "$go_bin" && -x /usr/local/go/bin/go ]]; then
  go_bin=/usr/local/go/bin/go
fi
if [[ -z "$go_bin" ]]; then
  printf 'catalog-spike: go executable not found\n' >&2
  exit 1
fi

"$go_bin" test ./scripts/spikes/six-product/catalog
"$go_bin" build -o "$scratch/agent-sessions-catalog" ./scripts/spikes/six-product/catalog

"$scratch/agent-sessions-catalog" validate >/dev/null
"$scratch/agent-sessions-catalog" catalog >"$scratch/catalog-a.json"
"$scratch/agent-sessions-catalog" catalog >"$scratch/catalog-b.json"
cmp "$scratch/catalog-a.json" "$scratch/catalog-b.json"

for view in release-inventory install-plan acceptance-matrix; do
  "$scratch/agent-sessions-catalog" "$view" >"$scratch/$view.json"
  "$scratch/agent-sessions-catalog" verify "$view" "$scratch/$view.json" >/dev/null
done

actual_digest=$("$scratch/agent-sessions-catalog" digest catalog)
expected_digest=$(tr -d '\n' <"$spike_root/testdata/catalog.sha256")
if [[ "$actual_digest" != "$expected_digest" ]]; then
  printf 'catalog-spike: catalog digest drift: got %s, want %s\n' "$actual_digest" "$expected_digest" >&2
  exit 1
fi

printf '{}\n' >"$scratch/drifted.json"
if "$scratch/agent-sessions-catalog" verify catalog "$scratch/drifted.json" >/dev/null 2>&1; then
  printf 'catalog-spike: drifted projection unexpectedly verified\n' >&2
  exit 1
fi

if rg -n '(^|[[:space:]])(codex|claude|grok|qwen|opencode|kilo|pi|omp|codebuddy|dsh)([[:space:]]|$)' "$spike_root/run.sh" >/dev/null; then
  printf 'catalog-spike: shell runner contains a product-authored inventory\n' >&2
  exit 1
fi

printf 'S6 catalog projection PASS digest=%s\n' "$actual_digest"
