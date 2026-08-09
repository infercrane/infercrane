#!/bin/sh
set -eu

infercrane serve &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true' INT TERM EXIT

until python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8080/readyz", timeout=2)' >/dev/null 2>&1; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    wait "$server_pid"
  fi
  sleep 1
done

INFERCRANE_URL=http://127.0.0.1:8080 infercrane target add gpu-a --url http://worker-a:8101 --runtime vllm
INFERCRANE_URL=http://127.0.0.1:8080 infercrane target add gpu-b --url http://worker-b:8102 --runtime vllm
INFERCRANE_URL=http://127.0.0.1:8080 infercrane deploy Qwen/Qwen3-8B \
  --name qwen-prod --targets gpu-a,gpu-b --idempotency-key compose-bootstrap --wait

trap - INT TERM EXIT
wait "$server_pid"
