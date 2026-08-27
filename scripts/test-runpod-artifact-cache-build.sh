#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
script=$root/scripts/runpod-artifact-cache-build.sh
report=$(mktemp -d)
trap 'rm -rf "$report"' EXIT
mkdir -p "$report/bin"
real_curl=$(command -v curl)
printf '%s\n' '#!/bin/sh' \
  'case "$*" in' \
  '  *huggingface.co/api/models*)' \
  '    output=' \
  '    while [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; output=$1; fi; shift; done' \
  '    printf '\''{"sha":"0123456789abcdef0123456789abcdef01234567","siblings":[{"rfilename":"config.json","size":1024},{"rfilename":"model.safetensors","size":107374182400}]}\n'\'' > "$output"' \
  '    ;;' \
  '  *) exec '"$real_curl"' "$@" ;;' \
  'esac' > "$report/bin/curl"
chmod +x "$report/bin/curl"
common_env='INFERCRANE_ARTIFACT_MODEL=org/model INFERCRANE_ARTIFACT_REVISION=0123456789abcdef0123456789abcdef01234567 INFERCRANE_RUNPOD_DATA_CENTER_ID=EU-RO-1 INFERCRANE_RUNPOD_VOLUME_GIB=200 INFERCRANE_RUNPOD_GPU_HOURLY_USD=0.74 INFERCRANE_RUNPOD_VOLUME_USD_PER_GB_MONTH=0.07 INFERCRANE_RUNPOD_MAX_PAID_SECONDS=3600 INFERCRANE_RUNPOD_VOLUME_RETENTION_HOURS=24'
plan=$(env $common_env PATH="$report/bin:$PATH" INFERCRANE_E2E_REPORT_DIR="$report" "$script" plan)
printf '%s' "$plan" | jq -e '.mutation=="none" and .worst_case_cost_usd>0 and .prefetch_compute=="gpu" and .model_identity=="org/model@0123456789abcdef0123456789abcdef01234567" and .artifact.exact_revision_bytes==107374183424 and .artifact.required_volume_gib==111' >/dev/null
cpu_plan=$(env $common_env PATH="$report/bin:$PATH" INFERCRANE_RUNPOD_PREFETCH_COMPUTE=cpu INFERCRANE_RUNPOD_CPU_HOURLY_USD=0.08 INFERCRANE_E2E_REPORT_DIR="$report" "$script" plan)
printf '%s' "$cpu_plan" | jq -e '.mutation=="none" and .prefetch_compute=="cpu" and .prefetch_resource=="cpu5c,cpu3c" and .price_evidence.preparer_hourly_usd==0.08' >/dev/null
if env $common_env INFERCRANE_E2E_REPORT_DIR="$report" RUNPOD_KEY_FILE=/missing "$script" build >/dev/null 2>&1; then
  echo 'build bypassed explicit paid-resource approval' >&2
  exit 1
fi
if env $common_env PATH="$report/bin:$PATH" INFERCRANE_RUNPOD_VOLUME_GIB=100 INFERCRANE_E2E_REPORT_DIR="$report" "$script" plan >/dev/null 2>&1; then
  echo 'plan accepted a volume smaller than the exact revision plus reserve' >&2
  exit 1
fi
grep -Fq -- '--approve-paid-resources' "$script"
grep -Fq 'worst-case cost USD' "$script"
grep -Fq 'exceeds authorized USD' "$script"
grep -Fq 'trap cleanup_pod EXIT INT TERM' "$script"
grep -Fq -- '--approve-volume-deletion' "$script"
grep -Fq 'RUNPOD_SECRET_' "$script"
echo 'RunPod GPU/CPU artifact-cache plan, approval, budget, secret-reference, watchdog, and cleanup guards passed.'
