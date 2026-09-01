#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH= cd -- "$script_dir/../../../.." && pwd -P)
evidence_path=${S4_EVIDENCE_PATH:-$repo_root/specs/004-six-product-support/evidence/phase0/S4-component.json}
runtime_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-s4-runtime.XXXXXX")
package_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-s4-packages.XXXXXX")
package_prefix=${S4_PREINSTALLED_PREFIX:-$package_root/app}

cleanup() {
  if [[ ${KEEP_S4_SCRATCH:-0} == 1 ]]; then
    printf 'S4 runtime retained at %s\nS4 package prefix retained at %s\n' "$runtime_root" "$package_root" >&2
    return
  fi
  temp_root=$(CDPATH= cd -- "${TMPDIR:-/tmp}" && pwd -P)
  for entry in "$runtime_root" "$package_root"; do
    entry_parent=$(CDPATH= cd -- "$(dirname -- "$entry")" && pwd -P)
    entry_name=${entry##*/}
    case "$entry_name" in
      agent-sessions-s4-runtime.*|agent-sessions-s4-packages.*) ;;
      *) printf 'refusing unexpected S4 cleanup target: %s\n' "$entry" >&2; return 1 ;;
    esac
    if [[ $entry_parent != "$temp_root" ]]; then
      printf 'refusing S4 cleanup outside temp root: %s\n' "$entry" >&2
      return 1
    fi
    rm -rf -- "$entry"
  done
}
trap cleanup EXIT INT TERM

if [[ -z ${S4_PREINSTALLED_PREFIX:-} ]]; then
  mkdir -p "$package_prefix"
  (cd "$package_prefix" && npm init -y >/dev/null)
  for package in \
    opencode-ai@1.18.25 \
    @kilocode/cli@7.5.6 \
    @earendil-works/pi-coding-agent@0.84.4 \
    @oh-my-pi/pi-coding-agent@18.0.11 \
    bun@1.4.0
  do
    PNPM_HOME="$package_root/pnpm-home" XDG_CACHE_HOME="$package_root/cache" \
      pnpm --network-concurrency=1 --child-concurrency=1 --dir "$package_prefix" add "$package"
  done

  # pnpm 10 intentionally blocks dependency postinstall scripts unless approved.
  # Run only the two exact platform-binary installers required by this isolated probe.
  (cd "$package_prefix/node_modules/opencode-ai" && node postinstall.mjs)
  (cd "$package_prefix/node_modules/bun" && node install.js)
fi

mkdir -p "$(dirname -- "$evidence_path")"
cd "$repo_root"
node "$script_dir/run-probes.mjs" \
  --prefix "$package_prefix" \
  --runtime-root "$runtime_root" \
  --output "$evidence_path"

jq -e '
  .status == "pass" and
  (.products | length) == 4 and
  ([.products[].version] == ["1.18.25", "7.5.6", "0.84.4", "18.0.11"]) and
  ([.products[].native_session_id | length > 0] | all) and
  .bootstrap.capability_id_alone_is_insufficient and
  .bootstrap.components_inert_without_secret and
  .bootstrap.raw_secret_absent_from_artifacts and
  .component_vocabulary.product_specific_identity_sources_preserved
' "$evidence_path" >/dev/null

printf 'S4 evidence: %s\n' "$evidence_path"
