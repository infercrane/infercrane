#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
script=$root/scripts/runpod-artifact-cache-build.sh
report=$(mktemp -d)
trap 'rm -rf "$report"' EXIT
mkdir -p "$report/bin"
REAL_CURL=$(command -v curl)
export REAL_CURL
# Fake Hugging Face metadata and a read-only RunPod REST v2 inventory. RunPod
# answers only at https://runpod.test/v2; any v1 path falls through and fails.
cat >"$report/bin/curl" <<'FAKE'
#!/bin/sh
output=
previous=
for argument do
  if [ "$previous" = -o ]; then output=$argument; fi
  previous=$argument
done
case "$*" in
  *huggingface.co/api/models*)
    printf '%s\n' '{"sha":"0123456789abcdef0123456789abcdef01234567","siblings":[{"rfilename":"config.json","size":1024},{"rfilename":"model.safetensors","size":107374182400}]}' >"$output"
    ;;
  *https://runpod.test/v2/network-volumes*)
    printf '%s' '{"networkVolumes":[{"id":"volume-1","name":"infercrane-artifact-a5d33b0cefe277aeeb41","size":200,"dataCenter":"EU-RO-1","type":"STANDARD"}],"pagination":{"nextCursor":null,"hasNextPage":false}}' >"$output"
    printf 200
    ;;
  *https://runpod.test/v2/pods*)
    printf '%s' '{"pods":[{"id":"pod-old","name":"infercrane-prefetch-a5d33b0cefe277aeeb41","status":"TERMINATED"}],"pagination":{"nextCursor":null,"hasNextPage":false}}' >"$output"
    printf 200
    ;;
  *runpod.test*)
    printf '%s' '{"title":"Not Found","status":404,"detail":"resource not found"}' >"$output"
    printf 404
    ;;
  *) exec "$REAL_CURL" "$@" ;;
esac
FAKE
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
if env $common_env PATH="$report/bin:$PATH" INFERCRANE_RUNPOD_PREFETCH_COMPUTE=cpu INFERCRANE_RUNPOD_PREFETCH_CPU_VCPUS=3 INFERCRANE_RUNPOD_CPU_HOURLY_USD=0.08 INFERCRANE_E2E_REPORT_DIR="$report" "$script" plan >/dev/null 2>&1; then
  echo 'plan accepted a vCPU count RunPod REST v2 rejects' >&2
  exit 1
fi
printf '%s\n' 'test-only-key' >"$report/runpod-key"
status=$(env $common_env PATH="$report/bin:$PATH" RUNPOD_KEY_FILE="$report/runpod-key" INFERCRANE_RUNPOD_REST_URL=https://runpod.test/v2 INFERCRANE_E2E_REPORT_DIR="$report" "$script" status)
printf '%s' "$status" | jq -e '.volume.id=="volume-1" and .volume.present and (.preparer.present|not)' >/dev/null || {
  echo "status did not read the RunPod REST v2 inventory: $status" >&2
  exit 1
}
grep -Fq 'https://api.runpod.io/v2' "$script"
grep -Fq -- '--approve-paid-resources' "$script"
grep -Fq 'worst-case cost USD' "$script"
grep -Fq 'exceeds authorized USD' "$script"
grep -Fq 'trap cleanup_pod EXIT INT TERM' "$script"
grep -Fq -- '--approve-volume-deletion' "$script"
grep -Fq 'RUNPOD_SECRET_' "$script"
echo 'RunPod GPU/CPU artifact-cache plan, approval, budget, secret-reference, watchdog, and cleanup guards passed.'
