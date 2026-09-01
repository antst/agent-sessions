#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../../../.." && pwd -P)
expected_old_commit=679fe9d3068b6362df867f8d78ce6708c4ce1342
old_ref=${OLD_REF:-$expected_old_commit}
old_commit=$(git -C "$repo_root" rev-parse "$old_ref^{commit}")
if [[ "$old_commit" != "$expected_old_commit" ]]; then
  printf 'federation-old: old ref %s resolves to %s, want %s\n' \
    "$old_ref" "$old_commit" "$expected_old_commit" >&2
  exit 1
fi

go_bin=${GO:-}
if [[ -z "$go_bin" ]]; then
  go_bin=$(command -v go || true)
fi
if [[ -z "$go_bin" && -x /usr/local/go/bin/go ]]; then
  go_bin=/usr/local/go/bin/go
fi
if [[ -z "$go_bin" || ! -x "$go_bin" ]]; then
  printf 'federation-old: go executable not found\n' >&2
  exit 1
fi

scratch=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-federation-old.XXXXXX")
cleanup() { rm -rf -- "$scratch"; }
trap cleanup EXIT

mkdir -p \
  "$scratch/old-source/scripts/spikes/six-product/federation-old/probe" \
  "$scratch/bin" "$scratch/go-cache" "$scratch/mod-cache" "$scratch/go-tmp"
git -C "$repo_root" archive "$old_commit" | tar -x -C "$scratch/old-source"
cp "$repo_root/scripts/spikes/six-product/federation-old/probe/main.go" \
  "$scratch/old-source/scripts/spikes/six-product/federation-old/probe/main.go"

race_args=()
case ${RACE:-0} in
  0) ;;
  1) race_args=(-race) ;;
  *) printf 'federation-old: RACE must be 0 or 1\n' >&2; exit 1 ;;
esac

export GOCACHE="$scratch/go-cache"
export GOMODCACHE="$scratch/mod-cache"
export GOTMPDIR="$scratch/go-tmp"
export GOENV=off

(
  cd "$scratch/old-source"
  "$go_bin" build "${race_args[@]}" -trimpath \
    -o "$scratch/bin/federation-old-probe" \
    ./scripts/spikes/six-product/federation-old/probe
)

cd "$repo_root"
AGENT_SESSIONS_REAL_OLD_FEDERATION_BINARY="$scratch/bin/federation-old-probe" \
AGENT_SESSIONS_REAL_OLD_FEDERATION_COMMIT="$old_commit" \
  "$go_bin" test "${race_args[@]}" ./internal/federation \
    -run '^TestRealPreFeatureProtocolThreeHostAgainstCurrentHub$' -count=1 -v

printf 'federation-old: PASS old=%s current=%s race=%s\n' \
  "$old_commit" "$(git -C "$repo_root" rev-parse HEAD)" "${RACE:-0}"
