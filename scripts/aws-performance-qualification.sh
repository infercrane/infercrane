#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
model=${1:-}
[ -n "$model" ] || { echo "usage: $0 mistral|deepseek|granite --approve-paid-resources" >&2; exit 2; }
[ "${2:-}" = "--approve-paid-resources" ] || {
  echo "AWS performance qualification creates paid GPU resources; pass --approve-paid-resources" >&2
  exit 1
}

case "$model" in
  mistral)
    spec=aws-mistral-7b.yaml
    features="tools structured"
    ;;
  deepseek)
    spec=aws-deepseek-r1-distill-7b.yaml
    features=none
    ;;
  granite)
    spec=aws-granite-8b.yaml
    features=none
    ;;
  *) echo "unknown AWS qualification model: $model" >&2; exit 2 ;;
esac

: "${INFERCRANE_V1_PROVIDER_ENV_FILE:?set the private AWS qualification environment file}"
: "${INFERCRANE_V1_API_KEY_FILE:?set the worker/control-plane API key file}"
run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-aws-$model-performance}

INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" \
INFERCRANE_V1_SPEC_DIR="$root/examples" \
INFERCRANE_V1_VLLM_SPEC="$spec" \
INFERCRANE_V1_RUNTIMES=vllm \
INFERCRANE_V1_VLLM_FEATURES="$features" \
INFERCRANE_V1_CONCURRENCY_SWEEP=true \
  "$root/scripts/portable-provider-acceptance.sh" aws --approve-paid-resources
