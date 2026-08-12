#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
count=${INFERCRANE_SOAK_COUNT:-10}
case "$count" in ''|*[!0-9]*) echo "INFERCRANE_SOAK_COUNT must be a positive integer" >&2; exit 2;; esac
[[ "$count" -gt 0 ]] || { echo "INFERCRANE_SOAK_COUNT must be positive" >&2; exit 2; }

packages=(
  ./internal/admission
  ./internal/asyncinference
  ./internal/autoscale
  ./internal/gateway
  ./internal/operations
  ./internal/provision
  ./internal/reconcile
  ./internal/requestquota
  ./internal/routes
  ./internal/workflows
)

echo "==> shuffled race soak ($count repetitions)"
(cd "$root" && go test -race -shuffle=on -count="$count" "${packages[@]}")

if [[ -n "${INFERCRANE_TEST_DATABASE_URL:-}" ]]; then
  echo "==> PostgreSQL fencing/contention soak ($count repetitions)"
  (cd "$root" && go test -race -shuffle=on -count="$count" ./internal/store)
else
  echo ":: PostgreSQL soak skipped (INFERCRANE_TEST_DATABASE_URL is unset)"
fi

echo "InferCrane reliability soak passed"
