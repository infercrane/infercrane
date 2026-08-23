#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
deployment=${1:-}
[ -n "$deployment" ] || { echo "usage: $0 DEPLOYMENT --approve-load [--output DIR]" >&2; exit 2; }
shift

approved=false
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --approve-load) approved=true ;;
    --output)
      shift
      [ "$#" -gt 0 ] || { echo "--output requires a directory" >&2; exit 2; }
      output=$1
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
[ "$approved" = true ] || { echo "concurrency sweep sends sustained inference load; pass --approve-load" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "required command not found: $1" >&2; exit 1; }; }
need jq
cli_binary=${INFERCRANE_BENCHMARK_CLI:-infercrane}
if [ ! -x "$cli_binary" ]; then
  cli_binary=$(command -v "$cli_binary" 2>/dev/null || true)
fi
[ -n "$cli_binary" ] && [ -x "$cli_binary" ] || { echo "InferCrane CLI is not executable: ${INFERCRANE_BENCHMARK_CLI:-infercrane}" >&2; exit 1; }

case "$deployment" in *[!A-Za-z0-9_.-]*) echo "deployment name contains unsupported characters" >&2; exit 2;; esac
run_id=${INFERCRANE_BENCHMARK_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
case "$run_id" in *[!A-Za-z0-9_.-]*) echo "benchmark run ID contains unsupported characters" >&2; exit 2;; esac
output=${output:-"$root/.infercrane/performance/$run_id-concurrency-sweep"}
mkdir -p "$output/results"
chmod 700 "$output" "$output/results"

run_cli() {
  if [ -n "${INFERCRANE_BENCHMARK_CONTEXT:-}" ]; then
    "$cli_binary" --context "$INFERCRANE_BENCHMARK_CONTEXT" "$@"
  else
    "$cli_binary" "$@"
  fi
}

concurrencies=${INFERCRANE_BENCHMARK_CONCURRENCIES:-"1 8 32 128"}
requests=${INFERCRANE_BENCHMARK_SWEEP_REQUESTS:-512}
input_tokens=${INFERCRANE_BENCHMARK_SWEEP_INPUT_TOKENS:-512}
output_tokens=${INFERCRANE_BENCHMARK_SWEEP_OUTPUT_TOKENS:-256}
random_seed=${INFERCRANE_BENCHMARK_SWEEP_RANDOM_SEED:-17}
streaming=${INFERCRANE_BENCHMARK_SWEEP_STREAMING:-true}
ttft_slo=${INFERCRANE_BENCHMARK_TTFT_SLO_MS:-0}
tpot_slo=${INFERCRANE_BENCHMARK_TPOT_SLO_MS:-0}
latency_slo=${INFERCRANE_BENCHMARK_LATENCY_SLO_MS:-0}
max_error_rate=${INFERCRANE_BENCHMARK_SWEEP_MAX_ERROR_RATE:-0.05}
revision=${INFERCRANE_BENCHMARK_REVISION:-active}

case "$requests:$input_tokens:$output_tokens:$random_seed" in
  *[!0-9:]*) echo "request, token, and seed inputs must be non-negative integers" >&2; exit 2;;
esac
[ "$requests" -ge 1 ] && [ "$input_tokens" -ge 1 ] && [ "$output_tokens" -ge 1 ] || { echo "requests and token counts must be positive" >&2; exit 2; }
case "$streaming" in true|false) ;; *) echo "INFERCRANE_BENCHMARK_SWEEP_STREAMING must be true or false" >&2; exit 2;; esac
case "$ttft_slo:$tpot_slo:$latency_slo:$max_error_rate" in
  *[!0-9.:]*) echo "SLO and error-rate inputs must be non-negative decimal numbers" >&2; exit 2;;
esac

printf '%s\n' "InferCrane concurrency sweep · $deployment · revision $revision"
printf '%s\n' "Fixed workload · requests=$requests · input=$input_tokens · output=$output_tokens · streaming=$streaming · seed=$random_seed"
for concurrency in $concurrencies; do
  case "$concurrency" in *[!0-9]*|'') echo "concurrency values must be positive integers" >&2; exit 2;; esac
  [ "$concurrency" -ge 1 ] && [ "$concurrency" -le "$requests" ] || { echo "concurrency $concurrency must be 1..$requests" >&2; exit 2; }
  result="$output/results/c$concurrency.json"
  printf '%s\n' "==> concurrency $concurrency"
  run_cli benchmark "$deployment" --requests "$requests" --concurrency "$concurrency" \
    --input-tokens "$input_tokens" --output-tokens "$output_tokens" --random-seed "$random_seed" \
    --streaming="$streaming" --revision "$revision" --ttft-slo-ms "$ttft_slo" \
    --tpot-slo-ms "$tpot_slo" --latency-slo-ms "$latency_slo" --output json >"$result"
  chmod 0600 "$result"
  jq -e --argjson requests "$requests" --argjson concurrency "$concurrency" \
    --argjson input_tokens "$input_tokens" --argjson output_tokens "$output_tokens" \
    --argjson random_seed "$random_seed" --argjson streaming "$streaming" '
      .request_count == $requests and
      .workload.request_count == $requests and
      .workload.concurrency == $concurrency and
      .workload.input_tokens == $input_tokens and
      .workload.output_tokens == $output_tokens and
      .workload.random_seed == $random_seed and
      .workload.streaming == $streaming and
      (.reproduction_command | contains("${INFERCRANE_API_KEY}"))
    ' "$result" >/dev/null
  jq -e --argjson maximum "$max_error_rate" '((.failed / .request_count) <= $maximum)' "$result" >/dev/null || {
    echo "concurrency $concurrency error rate exceeded $max_error_rate" >&2
    exit 1
  }
  jq -r '"<== c=\(.workload.concurrency) · errors=\(.failed)/\(.request_count) · TTFT p95=\(.ttft_p95_ms // "unavailable")ms · TPOT p95=\(.tpot_p95_ms // "unavailable")ms · tok/s=\(.output_token_throughput // "unavailable")"' "$result"
done

jq -s --arg run_id "$run_id" --arg deployment "$deployment" --arg revision "$revision" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson max_error_rate "$max_error_rate" '
  {
    schema_version: 1,
    campaign: "same-workload-concurrency-sweep",
    run_id: $run_id,
    deployment: $deployment,
    revision: $revision,
    generated_at: $generated_at,
    max_error_rate: $max_error_rate,
    evidence_class: "measured",
    workload: (.[0].workload | {request_count,input_tokens,output_tokens,random_seed,streaming,ttft_slo_ms,tpot_slo_ms,latency_slo_ms}),
    results: (sort_by(.workload.concurrency) | map({id,model_identity,runtime,runtime_version,runtime_configuration,provider,region,gpu,gpu_count,compute_mode,workload,request_count,succeeded,failed,duration_seconds,request_throughput,output_token_throughput,ttft_p50_ms,ttft_p95_ms,tpot_p50_ms,tpot_p95_ms,latency_p50_ms,latency_p95_ms,goodput,gpu_utilization,cost_metadata,reproduction_command}))
  }
' "$output"/results/*.json >"$output/sweep.json"
chmod 0600 "$output/sweep.json"
printf '%s\n' "$output/sweep.json"
