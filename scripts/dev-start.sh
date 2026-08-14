#!/bin/sh
set -eu

infercrane serve &
server_pid=$!
bootstrap_pid=

stop_child() {
  child_pid=${1:-}
  [ -z "$child_pid" ] || kill -TERM "$child_pid" 2>/dev/null || true
}

wait_child() {
  child_pid=${1:-}
  [ -z "$child_pid" ] || wait "$child_pid" 2>/dev/null || true
}

shutdown_server() {
  trap - INT TERM EXIT
  stop_child "$bootstrap_pid"
  stop_child "$server_pid"
  status=0
  wait "$server_pid" || status=$?
  wait_child "$bootstrap_pid"
  exit "$status"
}

cleanup_server() {
  stop_child "$bootstrap_pid"
  stop_child "$server_pid"
  wait_child "$bootstrap_pid"
  wait_child "$server_pid"
}

# The shell is PID 1 in the development image. Waiting for the child is
# essential: exiting immediately lets the container runtime kill the Go process
# before it can withdraw HA membership and finish its own shutdown hooks.
trap shutdown_server INT TERM
trap cleanup_server EXIT

until python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8080/readyz", timeout=2)' >/dev/null 2>&1; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    wait "$server_pid"
  fi
  sleep 1
done

# Run bootstrap as one background job. POSIX shells defer traps while a
# foreground child is running; backgrounding the job keeps SIGTERM handling
# responsive and gives the Go server time to withdraw HA membership.
(
  INFERCRANE_URL=http://127.0.0.1:8080 infercrane target add gpu-a --url http://worker-a:8101 --runtime vllm
  INFERCRANE_URL=http://127.0.0.1:8080 infercrane target add gpu-b --url http://worker-b:8102 --runtime vllm
  INFERCRANE_URL=http://127.0.0.1:8080 infercrane deploy Qwen/Qwen3-8B \
    --name qwen-prod --targets gpu-a,gpu-b --idempotency-key compose-bootstrap --wait
) &
bootstrap_pid=$!
bootstrap_status=0
wait "$bootstrap_pid" || bootstrap_status=$?
bootstrap_pid=
[ "$bootstrap_status" -eq 0 ] || exit "$bootstrap_status"

# Keep the forwarding traps installed while the long-lived server is running.
# Clearing them here makes Docker stop terminate this PID 1 shell without
# notifying the Go child after a fast bootstrap.
wait "$server_pid"
