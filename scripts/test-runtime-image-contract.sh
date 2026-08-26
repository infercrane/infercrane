#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
dockerfile="$root/images/vllm-runpod/Dockerfile"
workflow="$root/.github/workflows/runtime-image.yml"

grep -Fq 'ARG VLLM_BASE_IMAGE=' "$dockerfile"
grep -Fq 'FROM ${VLLM_BASE_IMAGE}' "$dockerfile"
grep -Fq 'ARG VLLM_VERSION=' "$dockerfile"
grep -Fq 'RUN python3 -c' "$dockerfile"
grep -Fq 'importlib.metadata.version("vllm")' "$dockerfile"

grep -Fq 'vllm_base_image:' "$workflow"
grep -Fq 'pull_request:' "$workflow"
grep -Fq 'default: 0.22.0' "$workflow"
grep -Fq 'default: vllm/vllm-openai@sha256:0fec7ec5f3e6bc168e54899935fb0557da908a4832a1dbc88e2debcf2f889416' "$workflow"
grep -Fq "type=raw,value=v\${{ inputs.vllm_version || '0.22.0' }}" "$workflow"
grep -Fq 'VLLM_VERSION=${{ inputs.vllm_version' "$workflow"
grep -Fq 'VLLM_BASE_IMAGE=${{ inputs.vllm_base_image' "$workflow"
grep -Fq 'vllm/vllm-openai@sha256:[0-9a-f]{64}' "$workflow"
grep -Fq "push: \${{ github.event_name != 'pull_request' }}" "$workflow"

echo "runtime image publication contract passed"
