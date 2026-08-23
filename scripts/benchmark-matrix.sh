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
[ "$approved" = true ] || { echo "benchmark matrix sends sustained inference load; pass --approve-load" >&2; exit 1; }

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
output=${output:-"$root/.infercrane/performance/$run_id"}
mkdir -p "$output/results"
chmod 700 "$output" "$output/results"

run_cli() {
  if [ -n "${INFERCRANE_BENCHMARK_CONTEXT:-}" ]; then
    "$cli_binary" --context "$INFERCRANE_BENCHMARK_CONTEXT" "$@"
  else
    "$cli_binary" "$@"
  fi
}

profiles=${INFERCRANE_BENCHMARK_PROFILES:-"interactive balanced throughput buffered long-context long-generation overload"}
ttft_slo=${INFERCRANE_BENCHMARK_TTFT_SLO_MS:-0}
tpot_slo=${INFERCRANE_BENCHMARK_TPOT_SLO_MS:-0}
latency_slo=${INFERCRANE_BENCHMARK_LATENCY_SLO_MS:-0}
overload_max_error_rate=${INFERCRANE_BENCHMARK_OVERLOAD_MAX_ERROR_RATE:-0.05}
revision=${INFERCRANE_BENCHMARK_REVISION:-active}

case "$ttft_slo:$tpot_slo:$latency_slo:$overload_max_error_rate" in
  *[!0-9.:]*) echo "SLO and error-rate inputs must be non-negative decimal numbers" >&2; exit 2;;
esac

printf '%s\n' "InferCrane performance matrix · $deployment · revision $revision"
for profile in $profiles; do
  case "$profile" in balanced|buffered|interactive|long-context|long-generation|overload|throughput) ;;
    *) echo "unsupported benchmark profile in matrix: $profile" >&2; exit 2;;
  esac
  result="$output/results/$profile.json"
  printf '%s\n' "==> $profile"
  run_cli benchmark "$deployment" --profile "$profile" --revision "$revision" \
    --ttft-slo-ms "$ttft_slo" --tpot-slo-ms "$tpot_slo" --latency-slo-ms "$latency_slo" \
    --output json >"$result"
  chmod 0600 "$result"
  jq -e --arg profile "$profile" '
    .workload.profile == $profile and
    .workload.profile_version == "benchmark-profile-v1" and
    .request_count >= .workload.concurrency and
    .succeeded > 0 and
    (.reproduction_command | contains("${INFERCRANE_API_KEY}"))
  ' "$result" >/dev/null
  if [ "$profile" = overload ]; then
    jq -e --argjson maximum "$overload_max_error_rate" '
      ((.failed / .request_count) <= $maximum)
    ' "$result" >/dev/null || {
      echo "overload error rate exceeded $overload_max_error_rate" >&2
      exit 1
    }
  else
    jq -e '.failed == 0' "$result" >/dev/null || {
      echo "$profile workload returned failed requests" >&2
      exit 1
    }
  fi
  jq -r '"<== \(.workload.profile) · c=\(.workload.concurrency) · TTFT p95=\(.ttft_p95_ms // "unavailable")ms · TPOT p95=\(.tpot_p95_ms // "unavailable")ms · tok/s=\(.output_token_throughput // "unavailable") · goodput=\(.goodput // "unavailable")"' "$result"
done

jq -s --arg run_id "$run_id" --arg deployment "$deployment" --arg revision "$revision" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson overload_max_error_rate "$overload_max_error_rate" '
  {
    schema_version: 1,
    profile_version: "benchmark-profile-v1",
    run_id: $run_id,
    deployment: $deployment,
    revision: $revision,
    generated_at: $generated_at,
    overload_max_error_rate: $overload_max_error_rate,
    evidence_class: "measured",
    results: map({id, model_identity, runtime, runtime_version, runtime_configuration, provider, region, gpu, gpu_count, compute_mode, workload, request_count, succeeded, failed, duration_seconds, request_throughput, output_token_throughput, ttft_p50_ms, ttft_p95_ms, tpot_p50_ms, tpot_p95_ms, latency_p50_ms, latency_p95_ms, goodput, gpu_utilization, cost_metadata, reproduction_command})
  }
' "$output"/results/*.json >"$output/matrix.json"
chmod 0600 "$output/matrix.json"
printf '%s\n' "$output/matrix.json"
