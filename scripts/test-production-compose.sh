#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$project_root/compose.production.yaml"
fixture_key=$(mktemp)
rendered=$(mktemp)
trap 'rm -f "$fixture_key" "$rendered"' EXIT

chmod 600 "$fixture_key"
printf '%s\n' 'test-only-runpod-key' >"$fixture_key"

INFERCRANE_IMAGE=ghcr.io/infercrane/infercrane:test \
INFERCRANE_URL=https://infercrane.invalid \
INFERCRANE_API_KEY=test-only-api-key-at-least-32-characters \
INFERCRANE_POSTGRES_PASSWORD=test-only-postgres-password \
RUNPOD_KEY_FILE="$fixture_key" \
  docker compose -f "$compose_file" config >"$rendered"

grep -q 'INFERCRANE_ENV: production' "$rendered"
grep -q 'target: /run/secrets/runpod-api-key' "$rendered"
if grep -Eq 'fake-vllm|fake-router|runpod-fault-proxy|infercrane-runpod-acceptance-key' "$rendered"; then
  echo 'production Compose includes a development or acceptance-only component' >&2
  exit 1
fi

echo 'production Compose configuration passed'
