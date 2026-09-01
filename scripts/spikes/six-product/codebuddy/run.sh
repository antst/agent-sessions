#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../../../.." && pwd -P)
evidence=${EVIDENCE_OUT:-"$repo_root/specs/004-six-product-support/evidence/phase0/S3-codebuddy.json"}
scratch=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-codebuddy-s3.XXXXXX")

cleanup() {
  if [[ ${KEEP_SCRATCH:-0} == 1 ]]; then
    printf 'CodeBuddy S3 scratch retained at %s\n' "$scratch" >&2
    return
  fi
  case "$(cd "$(dirname "$scratch")" && pwd -P)/$(basename "$scratch")" in
    /tmp/agent-sessions-codebuddy-s3.*|/private/tmp/agent-sessions-codebuddy-s3.*)
      rm -rf -- "$scratch"
      ;;
    *)
      printf 'refusing to remove unexpected scratch path\n' >&2
      ;;
  esac
}
trap cleanup EXIT

umask 077
mkdir -p "$scratch/prefix" "$scratch/npm-cache"
npm install \
  --prefix "$scratch/prefix" \
  --cache "$scratch/npm-cache" \
  --no-audit \
  --no-fund \
  --save-exact \
  @tencent-ai/codebuddy-code@2.143.0 \
  >"$scratch/npm-install.log" 2>&1

codebuddy="$scratch/prefix/node_modules/.bin/codebuddy"
[[ -x "$codebuddy" ]]
integrity=$(node - "$scratch/prefix/package-lock.json" <<'NODE'
const lock = JSON.parse(require("node:fs").readFileSync(process.argv[2], "utf8"));
const entry = lock.packages?.["node_modules/@tencent-ai/codebuddy-code"];
process.stdout.write(entry?.integrity || "");
NODE
)

node "$repo_root/scripts/spikes/six-product/codebuddy/probe.mjs" \
  "$codebuddy" \
  "$scratch" \
  "$evidence" \
  "$integrity"

node - "$evidence" <<'NODE'
const evidence = JSON.parse(require("node:fs").readFileSync(process.argv[2], "utf8"));
if (evidence.schema !== "agent-sessions.six-product-spike.v1") throw new Error("bad schema");
if (evidence.gate !== "S3-codebuddy") throw new Error("bad gate");
if (!evidence.assertions.exact_worker_correlation) throw new Error("worker correlation failed");
if (!evidence.assertions.zero_wrong_session_delivery) throw new Error("wrong-session safety failed");
if (evidence.account_gated.credit !== "pending-never-pass") throw new Error("bad account credit");
if (evidence.result.status !== "red-contract-change-required") throw new Error("truth gate was not reported red");
NODE

