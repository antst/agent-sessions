#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  internal/bridge/runtime.go|internal/federator/agent.go|internal/federator/hub.go)
    victim="$1"
    ;;
  *)
    printf 'usage: %s {internal/bridge/runtime.go|internal/federator/agent.go|internal/federator/hub.go}\n' "$0" >&2
    exit 2
    ;;
esac

repo_root="$(cd "$(dirname "$0")/../../../.." && pwd -P)"
scratch="$(mktemp -d)"
cleanup() { rm -rf "$scratch"; }
trap cleanup EXIT

git -C "$repo_root" archive HEAD | tar -x -C "$scratch"
mv "$scratch/$victim" "$scratch/.s5-removed.go"

set +e
output="$(
  cd "$scratch" &&
  PATH=/usr/local/go/bin:$PATH go test \
    ./internal/bridge ./internal/federator ./internal/launcher ./cmd/agent-sessions 2>&1
)"
status=$?
set -e

printf 'victim=%s status=%d\n' "$victim" "$status"
printf '%s\n' "$output" | sed -n '/undefined:/p' | sed -n '1,12p'
if (( status == 0 )); then
  printf 'legacy-audit: removal unexpectedly compiled; full gates are still required before deletion\n' >&2
  exit 1
fi
