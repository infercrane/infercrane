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
grep -Fq 'default: 0.22.1' "$workflow"
grep -Fq 'default: vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7' "$workflow"
grep -Fq "type=sha,prefix=candidate-sha-" "$workflow"
grep -Fq 'needs: scan-upstream-vllm' "$workflow"
grep -Fq 'Publish the scanned semantic version tag' "$workflow"
grep -Fq 'org.opencontainers.image.version=v${{ inputs.vllm_version' "$workflow"
grep -Fq 'Remove the superseded vulnerable v0.22.0 package version' "$workflow"
grep -Fq 'VLLM_VERSION=${{ inputs.vllm_version' "$workflow"
grep -Fq 'VLLM_BASE_IMAGE=${{ inputs.vllm_base_image' "$workflow"
grep -Fq 'vllm/vllm-openai@sha256:[0-9a-f]{64}' "$workflow"
grep -Fq "push: \${{ github.event_name != 'pull_request' }}" "$workflow"

echo "runtime image publication contract passed"
