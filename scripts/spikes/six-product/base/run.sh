#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../../../.." && pwd -P)
cd "$repo_root"

go_binary=$(command -v go || true)
if [[ -z "$go_binary" && -x /usr/local/go/bin/go ]]; then
  go_binary=/usr/local/go/bin/go
fi
if [[ -z "$go_binary" ]]; then
  echo 'go executable not found' >&2
  exit 1
fi

test "$(git rev-parse HEAD)" = "679fe9d3068b6362df867f8d78ce6708c4ce1342"
test "$(git rev-parse HEAD^{tree})" = "90dbec40e4114aa164c16d8d3fd958467f3051aa"
test "$(git rev-parse origin/main)" = "$(git rev-parse HEAD)"

if rg -n 'DisallowUnknownFields' internal/federation cmd/agent-sessions-hub; then
  echo 'federation path unexpectedly rejects additive fields' >&2
  exit 1
fi

"$go_binary" run ./scripts/spikes/six-product/base
"$go_binary" test ./internal/federation ./cmd/agent-sessions-hub
