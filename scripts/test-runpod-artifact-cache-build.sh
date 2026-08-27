#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
script=$root/scripts/runpod-artifact-cache-build.sh
report=$(mktemp -d)
trap 'rm -rf "$report"' EXIT
common_env='INFERCRANE_ARTIFACT_MODEL=org/model INFERCRANE_ARTIFACT_REVISION=0123456789abcdef0123456789abcdef01234567 INFERCRANE_RUNPOD_DATA_CENTER_ID=EU-RO-1 INFERCRANE_RUNPOD_VOLUME_GIB=200 INFERCRANE_RUNPOD_GPU_HOURLY_USD=0.74 INFERCRANE_RUNPOD_VOLUME_USD_PER_GB_MONTH=0.07 INFERCRANE_RUNPOD_MAX_PAID_SECONDS=3600 INFERCRANE_RUNPOD_VOLUME_RETENTION_HOURS=24'
plan=$(env $common_env INFERCRANE_E2E_REPORT_DIR="$report" "$script" plan)
printf '%s' "$plan" | jq -e '.mutation=="none" and .worst_case_cost_usd>0 and .model_identity=="org/model@0123456789abcdef0123456789abcdef01234567"' >/dev/null
if env $common_env INFERCRANE_E2E_REPORT_DIR="$report" RUNPOD_KEY_FILE=/missing "$script" build >/dev/null 2>&1; then
  echo 'build bypassed explicit paid-resource approval' >&2
  exit 1
fi
grep -Fq -- '--approve-paid-resources' "$script"
grep -Fq 'worst-case cost USD' "$script"
grep -Fq 'trap cleanup_pod EXIT INT TERM' "$script"
grep -Fq -- '--approve-volume-deletion' "$script"
grep -Fq 'RUNPOD_SECRET_' "$script"
echo 'RunPod artifact-cache plan, approval, budget, secret-reference, watchdog, and cleanup guards passed.'
