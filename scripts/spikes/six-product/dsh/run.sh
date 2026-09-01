#!/usr/bin/env bash
set -euo pipefail

spike_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH= cd -- "$spike_dir/../../../.." && pwd -P)
output=${1:-"$repo_root/specs/004-six-product-support/evidence/phase0/S2-dsh.json"}
expected_commit=679fe9d3068b6362df867f8d78ce6708c4ce1342

if [[ $(git -C "$repo_root" rev-parse HEAD) != "$expected_commit" ]]; then
  echo "S2: wrong base commit" >&2
  exit 1
fi
command -v node >/dev/null
command -v pnpm >/dev/null

umask 077
runtime=$(mktemp -d /tmp/agent-sessions-dsh-s2.XXXXXX)
state_parent=${XDG_STATE_HOME:-"$HOME/.local/state"}/agent-sessions-spikes
mkdir -p -- "$state_parent"
chmod 700 -- "$state_parent"
state_root=$(mktemp -d "$state_parent/dsh-s2.XXXXXX")
case "$runtime" in /tmp/agent-sessions-dsh-s2.*) ;; *) echo "unsafe runtime root" >&2; exit 1;; esac
case "$state_root" in "$state_parent"/dsh-s2.*) ;; *) echo "unsafe state root" >&2; exit 1;; esac

cleanup() {
  if [[ ${DSH_S2_KEEP:-0} == 1 ]]; then
    printf 'S2 retained runtime=%s state=%s\n' "$runtime" "$state_root" >&2
    return
  fi
  find "$runtime" -depth -delete 2>/dev/null || true
  find "$state_root" -depth -delete 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

cp -- "$spike_dir/root-package.json" "$runtime/package.json"
cp -R -- "$spike_dir/plugin" "$runtime/plugin"
mkdir -p -- "$runtime/dshhome/profiles/acp" "$runtime/workspace"
cp -- "$spike_dir/profile/package.json" "$runtime/dshhome/profiles/acp/package.json"
cp -- "$spike_dir/profile/cordis.yml" "$runtime/dshhome/profiles/acp/cordis.yml"
cp -- "$spike_dir/profile/cordis.patch.yml" "$runtime/dshhome/profiles/acp/cordis.patch.yml"
cp -- "$spike_dir/sandbox-probe.sh" "$runtime/workspace/sandbox-probe.sh"
chmod 700 -- "$runtime/workspace/sandbox-probe.sh"

store="$runtime/pnpm-store"
pnpm install --dir "$runtime" --ignore-scripts --store-dir "$store" --reporter=silent
pnpm install --dir "$runtime/dshhome/profiles/acp" --ignore-scripts --store-dir "$store" --reporter=silent
plugin_archive=agent-sessions-dsh-s2-plugin-0.1.2-alpha.3.tgz
pnpm pack --dir "$runtime/plugin" --pack-destination "$runtime" >/dev/null
pnpm add --dir "$runtime/dshhome/profiles/acp" --ignore-scripts --store-dir "$store" --reporter=silent "$runtime/$plugin_archive"

dsh_bin="$runtime/node_modules/.bin/dsh"
control_socket="$state_root/component.sock"
plugin_log="$runtime/plugin-events.jsonl"
driver_result="$runtime/driver-result.json"
tuple_result="$runtime/tuple.json"
transcript="$driver_result.transcript.jsonl"

node "$spike_dir/validate-tuple.js" \
  "$runtime/dshhome/profiles/acp/package.json" \
  "$runtime/dshhome/profiles/acp/node_modules/@deepseek-ai/dsh-acp-app/package.json" \
  "$runtime/dshhome/profiles/acp/node_modules/@agent-sessions/dsh-s2-plugin/package.json" \
  "$tuple_result"

node "$spike_dir/acp-driver.js" \
  "$dsh_bin" \
  "$runtime/dshhome" \
  "$runtime/workspace" \
  "$control_socket" \
  "$plugin_log" \
  "$runtime/workspace/sandbox-probe.sh" \
  "$driver_result"

mkdir -p -- "$(dirname -- "$output")"
DSH_S2_BASE_COMMIT="$expected_commit" node "$spike_dir/evidence.js" \
  "$driver_result" \
  "$tuple_result" \
  "$plugin_log" \
  "$transcript" \
  "$output" \
  "$dsh_bin"

node -e 'const fs=require("fs"); const p=process.argv[1]; const e=JSON.parse(fs.readFileSync(p)); if(e.status!=="PASS") process.exit(1)' "$output"
printf 'S2 DSH PASS: %s\n' "$output"
