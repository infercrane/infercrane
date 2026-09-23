#!/usr/bin/env bash
set -euo pipefail

readonly model="Qwen/Qwen3.8-27B-FP8"
readonly revision="017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
readonly artifact_root="/runpod-volume/infercrane/qwen38"
readonly artifact_model="${artifact_root}/model"
readonly artifact_manifest="${artifact_root}/manifest.json"
readonly local_model="/tmp/infercrane-qwen38-model"
readonly provider_key_file="/run/infercrane/openrouter-api-key"
readonly metrics_key_file="/run/infercrane/metrics-api-key"
readonly base_runtime_image="lmsysorg/sglang@sha256:06e4f2ed21afde4ff513cda65070124e727ba23ccaeff7712b8c40e1097d611f"
readonly local_compile_cache="/tmp/infercrane-qwen38-compile-cache"

if (( $# != 0 )); then
  echo "refusing mutable container arguments: this recipe has a pinned launch vector" >&2
  exit 64
fi

if [[ "${INFERCRANE_OPENROUTER_MODE:-serve}" == "prepare-model" ]]; then
  unset TRANSFORMERS_OFFLINE HF_HUB_OFFLINE
  exec python3 /opt/infercrane/prepare_model.py
fi
if [[ "${INFERCRANE_OPENROUTER_MODE:-serve}" != "serve" ]]; then
  echo "unsupported INFERCRANE_OPENROUTER_MODE" >&2
  exit 64
fi
if [[ -z "${INFERCRANE_OPENROUTER_API_KEY:-}" ]]; then
  echo "INFERCRANE_OPENROUTER_API_KEY is required" >&2
  exit 64
fi

umask 077
printf '%s' "$INFERCRANE_OPENROUTER_API_KEY" > "$provider_key_file"
unset INFERCRANE_OPENROUTER_API_KEY
export INFERCRANE_OPENROUTER_API_KEY_FILE="$provider_key_file"
if [[ -n "${INFERCRANE_OPENROUTER_METRICS_KEY:-}" ]]; then
  printf '%s' "$INFERCRANE_OPENROUTER_METRICS_KEY" > "$metrics_key_file"
  unset INFERCRANE_OPENROUTER_METRICS_KEY
  export INFERCRANE_OPENROUTER_METRICS_KEY_FILE="$metrics_key_file"
fi

if [[ ! -d /runpod-volume ]] || [[ ! -w /runpod-volume ]]; then
  echo "persistent volume /runpod-volume is unavailable or read-only" >&2
  exit 73
fi
mkdir -p "$HF_HUB_CACHE"

python3 - "$artifact_manifest" "$artifact_model" "$revision" <<'PY'
import hashlib
import json
import pathlib
import sys

manifest_path = pathlib.Path(sys.argv[1])
model_path = pathlib.Path(sys.argv[2])
expected_revision = sys.argv[3]
try:
    manifest = json.loads(manifest_path.read_text())
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit(f"artifact manifest is unavailable or invalid: {exc}")
if manifest.get("model") != "Qwen/Qwen3.8-27B-FP8":
    raise SystemExit("artifact manifest model identity does not match")
if manifest.get("revision") != expected_revision:
    raise SystemExit("artifact manifest revision does not match")
if not model_path.is_dir():
    raise SystemExit(f"model artifact is missing at {model_path}")
if manifest.get("schema_version") != "infercrane.dev/model-artifact/v1":
    raise SystemExit("artifact manifest schema is unsupported")
entries = manifest.get("files")
if not isinstance(entries, list) or not entries:
    raise SystemExit("artifact manifest has no files")
expected_paths = set()
for entry in entries:
    relative = pathlib.PurePosixPath(str(entry.get("path", "")))
    if relative.is_absolute() or not relative.parts or ".." in relative.parts:
        raise SystemExit(f"artifact manifest contains an unsafe path: {relative}")
    relative_text = relative.as_posix()
    if relative_text in expected_paths:
        raise SystemExit(f"artifact manifest contains a duplicate path: {relative_text}")
    expected_paths.add(relative_text)
    item = model_path.joinpath(*relative.parts)
    if not item.is_file() or item.is_symlink():
        raise SystemExit(f"artifact file is missing or not regular: {relative_text}")
    expected_size = entry.get("bytes")
    if not isinstance(expected_size, int) or expected_size < 0 or item.stat().st_size != expected_size:
        raise SystemExit(f"artifact file size mismatch: {relative_text}")
    digest = hashlib.sha256()
    with item.open("rb") as handle:
        for chunk in iter(lambda: handle.read(8 << 20), b""):
            digest.update(chunk)
    if digest.hexdigest() != entry.get("sha256"):
        raise SystemExit(f"artifact file digest mismatch: {relative_text}")
actual_paths = {
    item.relative_to(model_path).as_posix()
    for item in model_path.rglob("*")
    if item.is_file()
}
if actual_paths != expected_paths:
    raise SystemExit("artifact directory and manifest file sets differ")
PY

compute_capability="$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader | head -n 1 | tr -d '[:space:].')"
if [[ -z "$compute_capability" ]]; then
  echo "GPU compute capability is unavailable" >&2
  exit 69
fi
gpu_arch="sm${compute_capability}"
cuda_version="$(python3 -c 'import torch; print(torch.version.cuda or "unknown")')"
if [[ -n "${INFERCRANE_COMPILE_CACHE_RELEASE:-}" ]]; then
  if [[ -z "${INFERCRANE_COMPILE_CACHE_MANIFEST_SHA256:-}" ]]; then
    echo "INFERCRANE_COMPILE_CACHE_MANIFEST_SHA256 is required with a compiled-cache release" >&2
    exit 64
  fi
  if [[ ! "${INFERCRANE_RUNTIME_IMAGE_DIGEST:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "INFERCRANE_RUNTIME_IMAGE_DIGEST must pin the final provider image when restoring a compiled-cache release" >&2
    exit 64
  fi
  python3 /opt/infercrane/compile_cache.py restore \
    --release-dir "${artifact_root}/compile-cache/releases/${INFERCRANE_COMPILE_CACHE_RELEASE}" \
    --manifest-sha256 "$INFERCRANE_COMPILE_CACHE_MANIFEST_SHA256" \
    --destination "$local_compile_cache" \
    --runtime-image "$INFERCRANE_RUNTIME_IMAGE_DIGEST" \
    --model-revision "$revision" \
    --gpu-arch "$gpu_arch" \
    --cuda-version "$cuda_version"
else
  mkdir -p "$local_compile_cache"
  echo "starting without an immutable compiled-cache release from ${base_runtime_image}" >&2
fi
export SGLANG_CACHE_DIR="${local_compile_cache}/sglang"
export TRITON_CACHE_DIR="${local_compile_cache}/triton"
export TORCHINDUCTOR_CACHE_DIR="${local_compile_cache}/inductor"
export DG_JIT_CACHE_DIR="${local_compile_cache}/deep-gemm"
export SGLANG_DG_CACHE_DIR="$DG_JIT_CACHE_DIR"
export FLASHINFER_CACHE_DIR="${local_compile_cache}/flashinfer"
export FLASHINFER_WORKSPACE_BASE="${local_compile_cache}/flashinfer-workspace"
export TVM_FFI_CACHE_DIR="${local_compile_cache}/tvm-ffi"
export TILELANG_CACHE_DIR="${local_compile_cache}/tilelang"
export CUTE_DSL_CACHE_DIR="${local_compile_cache}/cute-dsl"
export CUDA_CACHE_PATH="${local_compile_cache}/cuda"
mkdir -p "$SGLANG_CACHE_DIR" "$TRITON_CACHE_DIR" "$TORCHINDUCTOR_CACHE_DIR" \
  "$DG_JIT_CACHE_DIR" "$FLASHINFER_CACHE_DIR" "$FLASHINFER_WORKSPACE_BASE" \
  "$TVM_FFI_CACHE_DIR" "$TILELANG_CACHE_DIR" "$CUTE_DSL_CACHE_DIR" "$CUDA_CACHE_PATH"

# The persistent volume is the source of truth; local disk avoids serving
# weights over network storage. The marker is valid only for this revision.
if [[ ! -f "$local_model/.infercrane-ready-$revision" ]]; then
  rm -rf "$local_model"
  mkdir -p "$local_model"
  cp -a "$artifact_model"/. "$local_model"/
  touch "$local_model/.infercrane-ready-$revision"
fi

python3 /opt/infercrane/health_shim.py &
health_pid=$!

python3 -m sglang.launch_server \
  --model-path "$local_model" \
  --served-model-name "$model" \
  --host 127.0.0.1 \
  --port 30000 \
  --tp-size 1 \
  --context-length 262144 \
  --mem-fraction-static 0.90 \
  --kv-cache-dtype fp8_e4m3 \
  --max-running-requests 12 \
  --enable-metrics \
  --reasoning-parser qwen3 \
  --tool-call-parser qwen3_coder \
  --speculative-algorithm NEXTN \
  --speculative-num-steps 3 \
  --speculative-eagle-topk 1 \
  --speculative-num-draft-tokens 4 \
  --cuda-graph-bs-decode 1 2 4 8 12 16 24 32 33 \
  --cuda-graph-bs-prefill 256 512 1024 2048 4096 8192 &
runtime_pid=$!

edge_args=(
  --min-in-flight 4
  --initial-in-flight 8
  --max-in-flight 16
  --admission-window 32
  --target-ttft 3s
  --max-prefill-tokens-in-flight "${INFERCRANE_MAX_PREFILL_TOKENS_IN_FLIGHT:-65536}"
  --qualified-output-tps "${INFERCRANE_QUALIFIED_OUTPUT_TPS:-0}"
  --input-price-per-million "${INFERCRANE_INPUT_PRICE_PER_MILLION_USD:-0.10}"
  --output-price-per-million "${INFERCRANE_OUTPUT_PRICE_PER_MILLION_USD:-2.20}"
  --gpu-hourly-cost "${INFERCRANE_GPU_HOURLY_COST_USD:-0}"
)
if [[ -n "${INFERCRANE_OPENROUTER_METRICS_KEY_FILE:-}" ]]; then
  edge_args+=(--metrics-key-file "$INFERCRANE_OPENROUTER_METRICS_KEY_FILE")
fi
/usr/local/bin/infercrane-openrouter-edge "${edge_args[@]}" &
edge_pid=$!

terminate() {
  trap - TERM INT
  kill -TERM "$edge_pid" "$runtime_pid" "$health_pid" 2>/dev/null || true
  wait "$edge_pid" "$runtime_pid" "$health_pid" 2>/dev/null || true
}
trap terminate TERM INT

set +e
wait -n "$edge_pid" "$runtime_pid" "$health_pid"
status=$?
set -e
terminate
exit "$status"
