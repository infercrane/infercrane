#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

if "$root/scripts/benchmark-concurrency-sweep.sh" production >"$temporary/unapproved.log" 2>&1; then
  echo "concurrency sweep ran without explicit load approval" >&2
  exit 1
fi
grep -Fq 'pass --approve-load' "$temporary/unapproved.log"

cat >"$temporary/infercrane" <<'EOF'
#!/bin/sh
set -eu
requests="" concurrency="" input_tokens="" output_tokens="" random_seed="" streaming=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --requests) requests=$2; shift 2 ;;
    --concurrency) concurrency=$2; shift 2 ;;
    --input-tokens) input_tokens=$2; shift 2 ;;
    --output-tokens) output_tokens=$2; shift 2 ;;
    --random-seed) random_seed=$2; shift 2 ;;
    --streaming=*) streaming=${1#*=}; shift ;;
    *) shift ;;
  esac
done
jq -n --argjson requests "$requests" --argjson concurrency "$concurrency" \
  --argjson input_tokens "$input_tokens" --argjson output_tokens "$output_tokens" \
  --argjson random_seed "$random_seed" --argjson streaming "$streaming" '{
  id:("bench-c"+($concurrency|tostring)), model_identity:"meta-llama/Llama-3.1-8B-Instruct@commit",
  runtime:"vllm", runtime_version:"qualified", runtime_configuration:{args:[]}, provider:"fixture",
  region:"local", gpu:"fixture", gpu_count:1, compute_mode:"elastic",
  workload:{request_count:$requests,concurrency:$concurrency,input_tokens:$input_tokens,output_tokens:$output_tokens,random_seed:$random_seed,streaming:$streaming,ttft_slo_ms:0,tpot_slo_ms:0,latency_slo_ms:0},
  request_count:$requests,succeeded:$requests,failed:0,duration_seconds:10,
  request_throughput:10,output_token_throughput:100,ttft_p50_ms:10,ttft_p95_ms:20,
  tpot_p50_ms:2,tpot_p95_ms:3,latency_p50_ms:100,latency_p95_ms:200,goodput:null,
  gpu_utilization:72,cost_metadata:{available:true,currency:"USD",hourly:1.2,source:"fixture",observed_at:"2026-08-23T00:00:00Z"},
  reproduction_command:"aiperf --api-key ${INFERCRANE_API_KEY}"
}'
EOF
chmod +x "$temporary/infercrane"

INFERCRANE_BENCHMARK_CLI="$temporary/infercrane" \
INFERCRANE_BENCHMARK_RUN_ID=fixture \
  "$root/scripts/benchmark-concurrency-sweep.sh" production --approve-load --output "$temporary/evidence" >/dev/null

jq -e '
  .schema_version == 1 and .campaign == "same-workload-concurrency-sweep" and
  .evidence_class == "measured" and (.results | length) == 4 and
  .workload == {request_count:512,input_tokens:512,output_tokens:256,random_seed:17,streaming:true,ttft_slo_ms:0,tpot_slo_ms:0,latency_slo_ms:0} and
  [.results[].workload.concurrency] == [1,8,32,128] and
  ([.results[].model_identity] | unique) == ["meta-llama/Llama-3.1-8B-Instruct@commit"] and
  ([.results[].workload | del(.concurrency,.revision_selector,.direct_revision_validation,.endpoint_type,.server_token_count,.profile,.profile_version)] | unique | length) == 1
' "$temporary/evidence/sweep.json" >/dev/null

echo "benchmark concurrency sweep safety and evidence contract passed"
