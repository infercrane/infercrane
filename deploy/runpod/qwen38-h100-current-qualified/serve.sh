#!/usr/bin/env bash
set -euo pipefail

readonly expected_hf_home="/model-cache"
readonly expected_hf_hub_cache="/model-cache/hub"
readonly expected_inference_port="30000"
readonly expected_health_port="30001"
readonly expected_health_path="/ping"
readonly expected_model_revision="017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
readonly artifact_root="/runpod-volume/infercrane"
readonly artifact_model_path="${artifact_root}/model"
readonly artifact_manifest="${artifact_root}/manifest.json"
readonly local_model_path="/tmp/infercrane-qwen38-model"

if (( $# != 0 )); then
  echo "refusing mutable container arguments: this provisional recipe has a pinned launch vector" >&2
  exit 64
fi

require_exact_environment() {
  local name="$1"
  local expected="$2"
  local actual="${!name:-$expected}"
  if [[ "$actual" != "$expected" ]]; then
    echo "refusing mutable ${name}: expected ${expected}" >&2
    exit 64
  fi
  export "${name}=${expected}"
}

require_exact_environment HF_HOME "$expected_hf_home"
require_exact_environment HF_HUB_CACHE "$expected_hf_hub_cache"
require_exact_environment PORT "$expected_inference_port"
require_exact_environment PORT_HEALTH "$expected_health_port"
require_exact_environment HEALTH_CHECK_PATH "$expected_health_path"

if [[ ! -L /model-cache ]] || [[ "$(readlink /model-cache)" != "/runpod-volume" ]]; then
  echo "refusing unpinned cache layout: /model-cache must point to /runpod-volume" >&2
  exit 64
fi
if [[ ! -d /runpod-volume ]] || [[ ! -w /runpod-volume ]]; then
  echo "RunPod network-volume path /runpod-volume is unavailable or read-only" >&2
  exit 73
fi
if ! /usr/bin/python3 -c 'import os, sys; sys.exit(0 if os.path.ismount("/runpod-volume") else 1)'; then
  echo "refusing ephemeral cache: /runpod-volume is not a mounted RunPod network volume" >&2
  exit 73
fi
mkdir -p "$HF_HUB_CACHE"

# The network volume is the durable artifact source, but loading tens of
# gigabytes of weights directly from network storage makes every worker start
# unnecessarily slow. Stage the exact, pre-qualified revision onto the
# worker's local container disk before SGLang touches it. RunPod FlashBoot can
# retain this local state when the same worker is revived.
/usr/bin/python3 - "$artifact_manifest" "$expected_model_revision" <<'PY'
import json
import pathlib
import sys

manifest_path = pathlib.Path(sys.argv[1])
expected_revision = sys.argv[2]
try:
    manifest = json.loads(manifest_path.read_text())
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit(f"artifact manifest is unavailable or invalid: {exc}")
if manifest.get("model") != "Qwen/Qwen3.8-27B-FP8":
    raise SystemExit("artifact manifest model identity does not match")
if manifest.get("revision") != expected_revision:
    raise SystemExit("artifact manifest revision does not match")
PY

if [[ ! -d "$artifact_model_path" ]]; then
  echo "exact model artifact is missing at ${artifact_model_path}" >&2
  exit 73
fi
if [[ ! -f "$local_model_path/.infercrane-ready-$expected_model_revision" ]]; then
  rm -rf "$local_model_path"
  mkdir -p "$local_model_path"
  cp -a "$artifact_model_path"/. "$local_model_path"/
  touch "$local_model_path/.infercrane-ready-$expected_model_revision"
fi

export HF_HUB_OFFLINE=1
export TRANSFORMERS_OFFLINE=1

/usr/bin/python3 /opt/infercrane/health_shim.py &
health_pid=$!

/usr/bin/python3 -m sglang.launch_server \
  --model-path "$local_model_path" \
  --served-model-name Qwen/Qwen3.8-27B-FP8 \
  --host 0.0.0.0 \
  --port 30000 \
  --tp-size 1 \
  --context-length 18432 \
  --mem-fraction-static 0.90 \
  --max-running-requests 8 \
  --cuda-graph-max-bs 8 \
  --disable-piecewise-cuda-graph \
  --enable-metrics \
  --reasoning-parser qwen3 \
  --tool-call-parser qwen3_coder \
  --speculative-algorithm NEXTN \
  --speculative-num-steps 3 \
  --speculative-eagle-topk 1 \
  --speculative-num-draft-tokens 4 &
model_pid=$!

terminate() {
  trap - TERM INT
  kill -TERM "$model_pid" "$health_pid" 2>/dev/null || true
  wait "$model_pid" "$health_pid" 2>/dev/null || true
}
trap terminate TERM INT

set +e
wait -n "$model_pid" "$health_pid"
status=$?
set -e

kill -TERM "$model_pid" "$health_pid" 2>/dev/null || true
wait "$model_pid" "$health_pid" 2>/dev/null || true
exit "$status"
