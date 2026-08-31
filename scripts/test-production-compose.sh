#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$project_root/compose.production.yaml"
runpod_compose_file="$project_root/compose.production.runpod.yaml"
aws_compose_file="$project_root/compose.production.aws.yaml"
gcp_compose_file="$project_root/compose.production.gcp.yaml"
kubernetes_compose_file="$project_root/compose.production.kubernetes.yaml"
fixture_key=$(mktemp)
fixture_aws=$(mktemp -d)
fixture_gcloud=$(mktemp -d)
fixture_kubeconfig=$(mktemp)
base_rendered=$(mktemp)
runpod_rendered=$(mktemp)
aws_rendered=$(mktemp)
gcp_rendered=$(mktemp)
kubernetes_rendered=$(mktemp)
production_project="infercrane-production-compose-test-$$"
stop_production_stack() {
  INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
  INFERCRANE_URL=https://infercrane.invalid \
  INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
  INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
    docker compose -p "$production_project" -f "$compose_file" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
cleanup() {
  stop_production_stack
  rm -rf "$fixture_aws" "$fixture_gcloud"
  rm -f "$fixture_key" "$fixture_kubeconfig" "$base_rendered" "$runpod_rendered" "$aws_rendered" "$gcp_rendered" "$kubernetes_rendered"
}
trap cleanup EXIT

chmod 600 "$fixture_key"
printf '%s\n' 'test-only-runpod-key' >"$fixture_key"

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
  docker compose -f "$compose_file" config >"$base_rendered"

grep -q 'INFERCRANE_ENV: production' "$base_rendered"
grep -q 'INFERCRANE_HOSTED_AUTH_JWT_KEY:' "$base_rendered"
grep -q 'INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE:' "$base_rendered"
grep -q 'INFERCRANE_STRIPE_WEBHOOK_SECRET:' "$base_rendered"
grep -q 'sslmode=require' "$base_rendered"
grep -q 'ssl=on' "$base_rendered"
grep -q 'no-new-privileges:true' "$base_rendered"
grep -q -- '- ALL' "$base_rendered"
grep -q 'sslmode=require.*SELECT 1' "$base_rendered"
if grep -Eqi 'runpod|skypilot|RUNPOD_KEY_FILE' "$base_rendered"; then
  echo 'base production Compose is coupled to RunPod or SkyPilot' >&2
  exit 1
fi

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
  docker compose -p "$production_project" -f "$compose_file" up -d --wait --wait-timeout 120 postgres
tls_active=$(
  INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
  INFERCRANE_URL=https://infercrane.invalid \
  INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
  INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
    docker compose -p "$production_project" -f "$compose_file" exec -T postgres \
      sh -eu -c 'PGPASSWORD="$POSTGRES_PASSWORD" psql "host=127.0.0.1 user=infercrane dbname=infercrane sslmode=require" -Atc "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()"'
)
[ "$tls_active" = t ] || { echo 'production PostgreSQL connection did not negotiate TLS' >&2; exit 1; }
stop_production_stack
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
INFERCRANE_AWS_ARTIFACT_CACHE_POLICY=required \
INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON='{"model@commit":"snap-0123456789abcdef0"}' \
INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS=200 \
  docker compose -f "$compose_file" -f "$aws_compose_file" config >"$aws_rendered"
grep -q 'INFERCRANE_AWS_ROLE_ARN: arn:aws:iam::123456789012:role/test' "$aws_rendered"
grep -q 'INFERCRANE_AWS_ARTIFACT_CACHE_POLICY: required' "$aws_rendered"
grep -q 'INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS: "200"' "$aws_rendered"
grep -q 'target: /home/app/.aws' "$aws_rendered"
if grep -Eqi 'runpod|skypilot' "$aws_rendered"; then
  echo 'AWS production overlay is coupled to RunPod or SkyPilot' >&2
  exit 1
fi

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
GCLOUD_CONFIG_DIR="$fixture_gcloud" \
INFERCRANE_GCP_PROJECT=acme-test \
INFERCRANE_GCP_ZONE=europe-west4-a \
INFERCRANE_GCP_SUBNET=private-test \
INFERCRANE_GCP_MACHINE_TYPE=g2-standard-4 \
INFERCRANE_GCP_GPU=nvidia-l4 \
INFERCRANE_GCP_SERVICE_ACCOUNT=runtime@acme-test.iam.gserviceaccount.com \
INFERCRANE_GCP_VM_IMAGE=projects/cos-cloud/global/images/cos-stable-test \
INFERCRANE_GCP_WORKER_SECRET=infercrane-worker-key \
INFERCRANE_GCP_BOOT_DISK_GIB=200 \
INFERCRANE_GCP_ARTIFACT_CACHE_POLICY=required \
INFERCRANE_GCP_ARTIFACT_DISKS_JSON='{"Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567":"qwen3-8b-cache"}' \
INFERCRANE_GCP_CONTAINER_IMAGE=example.invalid/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  docker compose -f "$compose_file" -f "$gcp_compose_file" config >"$gcp_rendered"
grep -q 'INFERCRANE_GCP_PROJECT: acme-test' "$gcp_rendered"
grep -q 'INFERCRANE_GCP_BOOT_DISK_GIB: "200"' "$gcp_rendered"
grep -q 'INFERCRANE_GCP_ARTIFACT_CACHE_POLICY: required' "$gcp_rendered"
grep -q 'qwen3-8b-cache' "$gcp_rendered"
grep -q 'CLOUDSDK_CONFIG: /tmp/infercrane-gcloud' "$gcp_rendered"
grep -q 'INFERCRANE_GCLOUD_CONFIG_SOURCE: /run/infercrane/gcloud-bootstrap' "$gcp_rendered"
grep -q 'target: /run/infercrane/gcloud-bootstrap' "$gcp_rendered"
grep -q 'read_only: true' "$gcp_rendered"
if grep -Eqi 'runpod|skypilot|INFERCRANE_AWS_' "$gcp_rendered"; then
  echo 'GCP production overlay is coupled to another provider' >&2
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

echo 'provider-neutral, RunPod, AWS, GCP, and Kubernetes production Compose configurations passed'
