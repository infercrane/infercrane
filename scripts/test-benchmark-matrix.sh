#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

if "$root/scripts/benchmark-matrix.sh" production >"$temporary/unapproved.log" 2>&1; then
  echo "benchmark matrix ran without explicit load approval" >&2
  exit 1
fi
grep -Fq 'pass --approve-load' "$temporary/unapproved.log"

cat >"$temporary/infercrane" <<'EOF'
#!/bin/sh
set -eu
profile=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = --profile ]; then profile=$2; shift 2; continue; fi
  shift
done
case "$profile" in
  interactive) requests=256; concurrency=1 ;;
  balanced|buffered) requests=256; concurrency=8 ;;
  throughput) requests=512; concurrency=32 ;;
  long-context|long-generation) requests=64; concurrency=4 ;;
  overload) requests=512; concurrency=128 ;;
  *) exit 2 ;;
esac
jq -n --arg profile "$profile" --argjson requests "$requests" --argjson concurrency "$concurrency" '{
  id:("bench-"+$profile), model_identity:"mistralai/Mistral-7B-Instruct-v0.3@commit",
  runtime:"vllm", runtime_version:"qualified", runtime_configuration:{args:[]}, provider:"fixture",
  region:"local", gpu:"fixture", gpu_count:1, compute_mode:"elastic",
  workload:{profile:$profile,profile_version:"benchmark-profile-v1",concurrency:$concurrency},
  request_count:$requests,succeeded:$requests,failed:0,duration_seconds:10,
  request_throughput:10,output_token_throughput:100,ttft_p50_ms:10,ttft_p95_ms:20,
  tpot_p50_ms:2,tpot_p95_ms:3,latency_p50_ms:100,latency_p95_ms:200,goodput:null,
  gpu_utilization:null,cost_metadata:{available:false},
  reproduction_command:"aiperf --api-key ${INFERCRANE_API_KEY}"
}'
EOF
chmod +x "$temporary/infercrane"

INFERCRANE_BENCHMARK_CLI="$temporary/infercrane" \
INFERCRANE_BENCHMARK_RUN_ID=fixture \
  "$root/scripts/benchmark-matrix.sh" production --approve-load --output "$temporary/evidence" >/dev/null

jq -e '
  .schema_version == 1 and .profile_version == "benchmark-profile-v1" and
  .evidence_class == "measured" and (.results | length) == 7 and
  ([.results[].workload.concurrency] | sort) == [1,4,4,8,8,32,128] and
  ([.results[].model_identity] | unique) == ["mistralai/Mistral-7B-Instruct-v0.3@commit"]
' "$temporary/evidence/matrix.json" >/dev/null

echo "benchmark matrix safety and evidence contract passed"
