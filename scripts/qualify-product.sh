#!/bin/sh
set -u

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command_name=${1:-status}
shift || true
approval=false
case "$command_name" in local|nightly|runpod|aws|gcp|kubernetes|status|report|reset) ;; *)
  echo "usage: $0 local|nightly|runpod|aws|gcp|kubernetes|status|report|reset [--approve-paid-resources]" >&2; exit 2;;
esac
while [ "$#" -gt 0 ]; do
  case "$1" in --approve-paid-resources) approval=true;; *) echo "unknown argument: $1" >&2; exit 2;; esac
  shift
done

commit=$(git -C "$root" rev-parse HEAD) || exit
short=$(git -C "$root" rev-parse --short HEAD) || exit
dirty=false
[ -z "$(git -C "$root" status --porcelain)" ] || dirty=true
state_root=${INFERCRANE_PRODUCT_QUALIFICATION_DIR:-"$root/.infercrane/product-qualification"}
state="$state_root/$commit"
gates="$state/gates"
mkdir -p "$gates"

record() {
  gate=$1 status=$2 reason=$3 log=${4:-}
  jq -n --arg gate "$gate" --arg status "$status" --arg reason "$reason" --arg commit "$commit" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg log "$log" \
    '{schema_version:1,gate:$gate,status:$status,reason:$reason,commit:$commit,generated_at:$generated_at,log:(if $log=="" then null else $log end)}' \
    >"$gates/$gate.json"
}

run_gate() {
  gate=$1; shift
  evidence="$gates/$gate.json" log="$gates/$gate.log"
  if [ -f "$evidence" ] && [ "$(jq -r '.commit+":"+.status' "$evidence")" = "$commit:PASSED" ]; then
    echo "==> $gate (already passed for $short)"
    return 0
  fi
  echo "==> $gate"
  started=$(date +%s)
  if "$@" >"$log" 2>&1; then
    record "$gate" PASSED "completed in $(( $(date +%s)-started ))s" "$log"
    echo "<== $gate (passed, $(( $(date +%s)-started ))s)"
  else
    result=$?
    record "$gate" FAILED "command exited $result after $(( $(date +%s)-started ))s" "$log"
    echo "<== $gate (failed; $log)" >&2
    tail -n 80 "$log" >&2
    return "$result"
  fi
}

run_contracts() {
  output="$state/adapter-contracts.json"
  args=""
  [ "$dirty" = false ] || args="--allow-dirty"
  # shellcheck disable=SC2086
  go run "$root/tools/contract-qualifier" --output "$output" $args
}

run_supply_chain() {
  make -C "$root" deadcode
  make -C "$root" audit
  make -C "$root" candidate-artifacts RELEASE_CANDIDATE_TAG="${INFERCRANE_RELEASE_CANDIDATE_TAG:-v1.0.0-rc.1}"
}

run_product_journeys() {
  for journey in offline first-value modules adoption reliability; do
    INFERCRANE_PRODUCT_ACCEPTANCE_RUN_ID="$short-product" \
      "$root/scripts/product-acceptance.sh" "$journey" || return
  done
}

run_local() {
  [ "$dirty" = false ] || { echo "whole-product qualification requires a clean worktree so evidence is commit-addressable" >&2; return 2; }
  failed=0
  run_gate developer-environment "$root/scripts/dev-check.sh" full || failed=1
  run_gate product-journeys run_product_journeys || failed=1
  run_gate adapter-contracts run_contracts || failed=1
  run_gate simulated-clouds "$root/scripts/test-simulated-clouds.sh" || failed=1
  run_gate supply-chain run_supply_chain || failed=1
  report >/dev/null
  return "$failed"
}

run_nightly() {
  [ "$dirty" = false ] || { echo "scheduled qualification requires a clean worktree" >&2; return 2; }
  failed=0
  run_gate continuous-fuzz "$root/scripts/test-fuzz.sh" || failed=1
  run_gate reliability-soak "$root/scripts/test-reliability-soak.sh" || failed=1
  run_gate network-chaos "$root/scripts/test-network-chaos.sh" || failed=1
  run_gate kubernetes-scale "$root/scripts/test-kubernetes-kwok.sh" || failed=1
  run_gate kubernetes-version-matrix "$root/scripts/test-kubernetes-versions.sh" || failed=1
  report >/dev/null
  return "$failed"
}

require_paid() {
  [ "$approval" = true ] || { echo "$command_name requires --approve-paid-resources" >&2; exit 2; }
  [ "$dirty" = false ] || { echo "real-infrastructure evidence requires a clean worktree" >&2; exit 2; }
}

run_runpod() {
  require_paid
  if [ ! -r "${RUNPOD_KEY_FILE:-}" ]; then
    for gate in runpod-serverless-real runpod-serverless-faults-real runpod-elastic-real runpod-elastic-faults-real; do
      record "$gate" BLOCKED_ACCESS "RUNPOD_KEY_FILE is not readable"
    done
    return 3
  fi
  if [ -z "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ]; then
    record runpod-serverless-real BLOCKED_ACCESS "serverless template ID is missing"
    record runpod-serverless-faults-real BLOCKED_ACCESS "serverless template ID is missing"
  fi
  run_id=${INFERCRANE_QUALIFICATION_RUN_ID:-"$(date -u +%Y%m%dT%H%M%SZ)-$short"}
  failed=0
  if [ -n "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ]; then
    INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-serverless" run_gate runpod-serverless-real \
      "$root/scripts/release-acceptance.sh" serverless --approve-paid-resources || failed=1
    INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-serverless" "$root/scripts/release-acceptance.sh" cleanup >/dev/null 2>&1 || failed=1
    INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-serverless-faults" run_gate runpod-serverless-faults-real \
      "$root/scripts/release-acceptance.sh" serverless-faults --approve-paid-resources || failed=1
    INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-serverless-faults" "$root/scripts/release-acceptance.sh" cleanup >/dev/null 2>&1 || failed=1
  else
    failed=1
  fi
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-elastic" run_gate runpod-elastic-real \
    "$root/scripts/release-acceptance.sh" elastic-qualify --approve-paid-resources || failed=1
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-elastic" "$root/scripts/release-acceptance.sh" cleanup >/dev/null 2>&1 || failed=1
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-elastic-faults" run_gate runpod-elastic-faults-real \
    "$root/scripts/release-acceptance.sh" elastic-faults --approve-paid-resources || failed=1
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-elastic-faults" "$root/scripts/release-acceptance.sh" cleanup >/dev/null 2>&1 || failed=1
  return "$failed"
}

run_portable() {
  provider=$1 gate=$2
  require_paid
  [ -r "${INFERCRANE_V1_PROVIDER_ENV_FILE:-}" ] || { record "$gate" BLOCKED_ACCESS "INFERCRANE_V1_PROVIDER_ENV_FILE is not readable"; return 3; }
  [ -d "${INFERCRANE_V1_SPEC_DIR:-}" ] || { record "$gate" BLOCKED_ACCESS "INFERCRANE_V1_SPEC_DIR is not readable"; return 3; }
  [ -r "${INFERCRANE_V1_API_KEY_FILE:-}" ] || { record "$gate" BLOCKED_ACCESS "INFERCRANE_V1_API_KEY_FILE is not readable"; return 3; }
  export INFERCRANE_ACCEPTANCE_RUN_ID=${INFERCRANE_ACCEPTANCE_RUN_ID:-"$(date -u +%Y%m%dT%H%M%SZ)-$short-$provider"}
  run_gate "$gate" "$root/scripts/portable-provider-acceptance.sh" "$provider" --approve-paid-resources
}

report() {
  tmp="$state/report.tmp"
  jq -n --slurpfile manifest "$root/qualification/product-gates.json" \
    --arg commit "$commit" --argjson dirty "$dirty" --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{schema_version:1,commit:$commit,dirty:$dirty,generated_at:$generated_at,gates:$manifest[0].gates}' >"$tmp"
  for file in "$gates"/*.json; do
    [ -f "$file" ] || continue
    jq --slurpfile evidence "$file" --arg commit "$commit" \
      '.gates |= map(if .id == $evidence[0].gate and $evidence[0].commit == $commit then . + {status:$evidence[0].status,reason:$evidence[0].reason,evidence:$evidence[0].log} else . end)' \
      "$tmp" >"$tmp.next"
    mv "$tmp.next" "$tmp"
  done
  jq '.gates |= map(if has("status") then . else . + {status:(if .tier=="local" or .tier=="scheduled" then "NOT_RUN" else if .tier=="human" then "HUMAN_REQUIRED" else "REAL_INFRA_REQUIRED" end end),reason:"no commit-bound evidence"} end)
      | .summary = (reduce .gates[] as $g ({}; .[$g.status] = ((.[$g.status] // 0) + 1)))
      | .verdict = (if any(.gates[]; .status=="FAILED") then "FAILED" else if all(.gates[] | select(.tier=="local"); .status=="PASSED") then "LOCAL_QUALIFIED" else "INCOMPLETE" end end)' "$tmp" >"$state/report.json" || return
  rm -f "$tmp"
  {
    echo "# InferCrane product qualification"
    echo
    echo "Commit: \`$commit\`"
    echo
    echo "Verdict: **$(jq -r .verdict "$state/report.json")**"
    echo
    echo '| Gate | Tier | Status | Reason |'
    echo '|---|---|---|---|'
    jq -r '.gates[] | "| `\(.id)` | \(.tier) | **\(.status)** | \(.reason | gsub("\\|"; "\\|")) |"' "$state/report.json"
  } >"$state/report.md"
  jq . "$state/report.json"
  echo "evidence=$state"
}

case "$command_name" in
  local) run_local;;
  nightly) run_nightly;;
  runpod) run_runpod; result=$?; report; exit "$result";;
  aws) run_portable aws aws-real; result=$?; report; exit "$result";;
  gcp) run_portable gcp gcp-real; result=$?; report; exit "$result";;
  kubernetes) run_portable kubernetes kubernetes-gpu-real; result=$?; report; exit "$result";;
  status|report) report;;
  reset) rm -rf "$state"; echo "removed qualification evidence for $commit";;
esac
