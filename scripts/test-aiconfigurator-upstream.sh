#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

command -v docker >/dev/null 2>&1 || {
  echo "docker is required for the optional AIConfigurator upstream contract" >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  echo "jq is required for the optional AIConfigurator upstream contract" >&2
  exit 1
}

cat >"$temporary/input.json" <<'EOF'
{
  "schema_version": "infercrane.optimizer.estimator-input/v1",
  "required_version": "0.11.0",
  "required_plotext_version": "5.3.2",
  "model_path": "Qwen/Qwen3-8B",
  "system": "l40s",
  "backend": "vllm",
  "database_mode": "HYBRID",
  "target_concurrency": 8,
  "input_tokens": 512,
  "output_tokens": 128,
  "ttft_ms": 250,
  "tpot_ms": 30,
  "prefix_tokens": 0,
  "top_n": 3,
  "enable_chunked_prefill": true
}
EOF

docker run --rm --platform linux/amd64 -i \
  -v "$root/internal/optimizer/aiconfigurator_adapter.py:/opt/infercrane/adapter.py:ro" \
  python:3.12-slim sh -ec '
    python -m pip install --disable-pip-version-check --quiet \
      aiconfigurator==0.11.0 plotext==5.3.2
    exec python /opt/infercrane/adapter.py
  ' <"$temporary/input.json" >"$temporary/output.json"

jq -e '
  .schema_version == "infercrane.optimizer.estimator-output/v1" and
  .source == "aiconfigurator" and
  .source_version == "0.11.0" and
  .evidence_class == "modeled" and
  .model_path == "Qwen/Qwen3-8B" and
  .system == "l40s" and
  .backend == "vllm" and
  (.result_digest | test("^sha256:[0-9a-f]{64}$")) and
  (.candidates | length) > 0 and
  ([.candidates[].mode] | all(. == "aggregated" or . == "disaggregated"))
' "$temporary/output.json" >/dev/null

echo "AIConfigurator 0.11.0 upstream contract passed for Qwen3-8B on modeled L40S/vLLM"
echo "Evidence class: modeled only; this command did not benchmark a GPU"
