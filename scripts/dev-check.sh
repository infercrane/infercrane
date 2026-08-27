#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mode=${1:-quick}
case "$mode" in
  quick|full|ci-core|ci-container|ci-stack) ;;
  *) echo "usage: $0 [quick|full|ci-core|ci-container|ci-stack]" >&2; exit 2 ;;
esac

run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
evidence="$root/.infercrane/dev-check/$run_id"
lock="$root/.infercrane/dev-check.lock"
mkdir -p "$root/.infercrane" "$evidence"

if ! mkdir "$lock" 2>/dev/null; then
  owner=$(cat "$lock/pid" 2>/dev/null || true)
  if [ -n "$owner" ] && kill -0 "$owner" 2>/dev/null; then
    echo "another InferCrane developer check is running (pid $owner)" >&2
    exit 1
  fi
  rm -rf "$lock"
  mkdir "$lock"
fi
printf '%s\n' "$$" >"$lock/pid"

project=$(printf '%s' "infercrane-devcheck-$run_id" | tr '[:upper:]' '[:lower:]')
port=${INFERCRANE_DEV_CHECK_PORT:-}
if [ -z "$port" ]; then
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
fi
export COMPOSE_PROJECT_NAME="$project"
export INFERCRANE_DEV_PORT="$port"
export INFERCRANE_SMOKE_URL="http://127.0.0.1:$port"

cleanup() {
  docker compose --profile test down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$lock"
}
trap cleanup EXIT HUP INT TERM

step() {
  name=$1
  shift
  started=$(date +%s)
  echo "==> $name"
  if "$@" >"$evidence/$name.log" 2>&1; then
    echo "<== $name (passed, $(( $(date +%s) - started ))s)"
    return 0
  else
    status=$?
    echo "<== $name (failed, $(( $(date +%s) - started ))s)" >&2
    tail -n 60 "$evidence/$name.log" >&2
    return "$status"
  fi
}

# `full` retains the complete sequential local gate. CI uses the three
# `ci-*` modes on isolated runners and joins them behind one required summary
# job, reducing wall-clock latency without omitting any full-mode stage.
case "$mode" in
quick|full|ci-core)
  # The repository verifier deliberately skips PostgreSQL integration tests
  # when no test database is configured; the full developer check runs those
  # tests later in its isolated container stage. Never let a stale
  # caller-owned URL accidentally enable them against an unavailable or
  # unrelated database.
  step repository env -u INFERCRANE_TEST_DATABASE_URL make -C "$root" verify
  step provider-contracts sh -c 'cd "$1" && go test -count=1 ./internal/integration ./internal/conformance ./internal/provision ./internal/gateway ./internal/reconcile ./internal/workflows' sh "$root"
  step acceptance-safety "$root/scripts/test-acceptance-safety.sh"
  ;;
esac

case "$mode" in
full|ci-core)
  step kubernetes-manifests make -C "$root" test-kubernetes-manifests
  step kubernetes-kind make -C "$root" test-kubernetes-kind
  step automation-clients make -C "$root" test-automation-full
  ;;
esac

case "$mode" in
full|ci-container)
  step container-tests make -C "$root" test-container
  ;;
esac

case "$mode" in
full|ci-stack)
  step stack-smoke "$root/scripts/test-stack.sh"
  step failure-recovery "$root/scripts/test-failure-recovery.sh"
  step control-plane-ha "$root/scripts/test-ha-control-plane.sh"
  step backup-restore-safety "$root/scripts/test-backup-restore-safety.sh"
  step backup-restore-docker "$root/scripts/test-backup-restore-docker.sh"
  step production-config make -C "$root" test-production-config
  ;;
esac

case "$mode" in
full|ci-core)
  step docs make -C "$root" docs-check
  ;;
esac

cat >"$evidence/summary.txt" <<EOF
InferCrane developer check passed
mode=$mode
commit=$(git -C "$root" rev-parse HEAD)
project=$project
port=$port
evidence=$evidence
EOF
cat "$evidence/summary.txt"
