#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../../../.." && pwd -P)"
expected_base="679fe9d3068b6362df867f8d78ce6708c4ce1342"
actual_base="$(git -C "$repo_root" rev-parse HEAD)"
if [[ "$actual_base" != "$expected_base" ]]; then
  printf 'legacy-audit: HEAD %s, want %s\n' "$actual_base" "$expected_base" >&2
  exit 1
fi

go_bin="${GO_BIN:-/usr/local/go/bin/go}"
if [[ ! -x "$go_bin" ]]; then
  printf 'legacy-audit: go binary is not executable: %s\n' "$go_bin" >&2
  exit 1
fi

scratch="$(mktemp -d)"
cleanup() { rm -rf "$scratch"; }
trap cleanup EXIT

mkdir -p "$scratch/bin"
GOBIN="$scratch/bin" PATH="$(dirname "$go_bin"):$PATH" \
  "$go_bin" install golang.org/x/tools/cmd/deadcode@v0.36.0
PATH="$(dirname "$go_bin"):$PATH" \
  "$scratch/bin/deadcode" ./cmd/agent-sessions ./cmd/agent-sessions-hub \
  >"$scratch/deadcode-linux.txt"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 PATH="$(dirname "$go_bin"):$PATH" \
  "$scratch/bin/deadcode" ./cmd/agent-sessions ./cmd/agent-sessions-hub \
  >"$scratch/deadcode-darwin.txt"

PATH="$(dirname "$go_bin"):$PATH" \
  "$go_bin" run ./scripts/spikes/six-product/legacy/audit.go \
  -root "$repo_root" \
  -deadcode-linux "$scratch/deadcode-linux.txt" \
  -deadcode-darwin "$scratch/deadcode-darwin.txt"
