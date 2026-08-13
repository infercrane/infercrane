#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
dockerfile="$root/images/vllm-runpod/Dockerfile"
workflow="$root/.github/workflows/runtime-image.yml"

grep -Fq 'ARG VLLM_BASE_IMAGE=' "$dockerfile"
grep -Fq 'FROM ${VLLM_BASE_IMAGE}' "$dockerfile"
grep -Fq 'ARG VLLM_VERSION=' "$dockerfile"
grep -Fq 'importlib.metadata.version("vllm")' "$dockerfile"

grep -Fq 'vllm_base_image:' "$workflow"
grep -Fq 'VLLM_VERSION=${{ inputs.vllm_version' "$workflow"
grep -Fq 'VLLM_BASE_IMAGE=${{ inputs.vllm_base_image' "$workflow"
grep -Fq 'vllm/vllm-openai@sha256:[0-9a-f]{64}' "$workflow"

echo "runtime image publication contract passed"
