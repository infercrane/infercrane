#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$project_root/compose.production.yaml"
runpod_compose_file="$project_root/compose.production.runpod.yaml"
aws_compose_file="$project_root/compose.production.aws.yaml"
kubernetes_compose_file="$project_root/compose.production.kubernetes.yaml"
fixture_key=$(mktemp)
fixture_aws=$(mktemp -d)
fixture_kubeconfig=$(mktemp)
base_rendered=$(mktemp)
runpod_rendered=$(mktemp)
aws_rendered=$(mktemp)
kubernetes_rendered=$(mktemp)
trap 'rm -rf "$fixture_aws"; rm -f "$fixture_key" "$fixture_kubeconfig" "$base_rendered" "$runpod_rendered" "$aws_rendered" "$kubernetes_rendered"' EXIT

chmod 600 "$fixture_key"
printf '%s\n' 'test-only-runpod-key' >"$fixture_key"

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
  docker compose -f "$compose_file" config >"$base_rendered"

grep -q 'INFERCRANE_ENV: production' "$base_rendered"
grep -q 'no-new-privileges:true' "$base_rendered"
grep -q -- '- ALL' "$base_rendered"
if grep -Eqi 'runpod|skypilot|RUNPOD_KEY_FILE' "$base_rendered"; then
  echo 'base production Compose is coupled to RunPod or SkyPilot' >&2
  exit 1
fi
if grep -Eq 'fake-vllm|fake-router|runpod-fault-proxy|infercrane-runpod-acceptance-key' "$base_rendered"; then
  echo 'production Compose includes a development or acceptance-only component' >&2
  exit 1
fi

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
RUNPOD_KEY_FILE="$fixture_key" \
  docker compose -f "$compose_file" -f "$runpod_compose_file" config >"$runpod_rendered"

grep -q 'INFERCRANE_SKYPILOT_API: enabled' "$runpod_rendered"
grep -q 'target: /run/secrets/runpod-api-key' "$runpod_rendered"
grep -q 'source: skypilot-state' "$runpod_rendered"
grep -q 'source: runpod-state' "$runpod_rendered"
if grep -Eq 'fake-vllm|fake-router|runpod-fault-proxy|infercrane-runpod-acceptance-key' "$runpod_rendered"; then
  echo 'RunPod production overlay includes a development or acceptance-only component' >&2
  exit 1
fi

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
AWS_CONFIG_DIR="$fixture_aws" \
INFERCRANE_AWS_ROLE_ARN=arn:aws:iam::123456789012:role/test \
INFERCRANE_AWS_EXTERNAL_ID=test-only \
INFERCRANE_AWS_REGION=eu-central-1 \
INFERCRANE_AWS_SUBNET_ID=subnet-test \
INFERCRANE_AWS_SECURITY_GROUP_IDS=sg-test \
INFERCRANE_AWS_AMI_ID=ami-test \
INFERCRANE_AWS_INSTANCE_TYPE=g6e.xlarge \
INFERCRANE_AWS_GPU=L40S \
INFERCRANE_AWS_INSTANCE_PROFILE_ARN=arn:aws:iam::123456789012:instance-profile/test \
INFERCRANE_AWS_WORKER_SECRET_ARN=arn:aws:secretsmanager:eu-central-1:123456789012:secret:test \
INFERCRANE_AWS_IMAGE_DIGEST=example.invalid/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  docker compose -f "$compose_file" -f "$aws_compose_file" config >"$aws_rendered"
grep -q 'INFERCRANE_AWS_ROLE_ARN: arn:aws:iam::123456789012:role/test' "$aws_rendered"
grep -q 'target: /home/app/.aws' "$aws_rendered"
if grep -Eqi 'runpod|skypilot' "$aws_rendered"; then
  echo 'AWS production overlay is coupled to RunPod or SkyPilot' >&2
  exit 1
fi

printf '%s\n' 'apiVersion: v1' >"$fixture_kubeconfig"
INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
KUBECONFIG_FILE="$fixture_kubeconfig" \
INFERCRANE_KUBERNETES_CONTEXT=test-cluster \
INFERCRANE_KUBERNETES_IMAGE_DIGEST=example.invalid/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  docker compose -f "$compose_file" -f "$kubernetes_compose_file" config >"$kubernetes_rendered"
grep -q 'INFERCRANE_KUBERNETES_CONTEXT: test-cluster' "$kubernetes_rendered"
grep -q 'target: /run/secrets/kubeconfig' "$kubernetes_rendered"
if grep -Eqi 'runpod|skypilot|INFERCRANE_AWS_' "$kubernetes_rendered"; then
  echo 'Kubernetes production overlay is coupled to another provider' >&2
  exit 1
fi

echo 'provider-neutral, RunPod, AWS, and Kubernetes production Compose configurations passed'
