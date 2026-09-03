#!/usr/bin/env bash
set -euo pipefail

readonly expected_hf_home="/model-cache"
readonly expected_hf_hub_cache="/model-cache/hub"
readonly expected_inference_port="30000"
readonly expected_health_port="30001"
readonly expected_health_path="/ping"

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

/usr/bin/python3 /opt/infercrane/health_shim.py &
health_pid=$!

/usr/bin/python3 -m sglang.launch_server \
  --model-path Qwen/Qwen3.8-27B-FP8 \
  --served-model-name Qwen/Qwen3.8-27B-FP8 \
  --revision 017b9c7af6b5689d5dd426a76e0bc077eb5ca20a \
  --host 0.0.0.0 \
  --port 30000 \
  --tp-size 1 \
  --context-length 18432 \
  --mem-fraction-static 0.90 \
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
