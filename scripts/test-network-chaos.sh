#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
run_id=${INFERCRANE_NETWORK_CHAOS_RUN_ID:-"$(date -u +%Y%m%d%H%M%S)-$$"}
project="infercrane-chaos-$run_id"
compose=(docker compose -p "$project" -f "$root/compose.yaml" -f "$root/compose.network-chaos.yaml")

free_port() {
  python3 - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
}

export INFERCRANE_DEV_PORT=${INFERCRANE_DEV_PORT:-$(free_port)}
export INFERCRANE_TOXIPROXY_PORT=${INFERCRANE_TOXIPROXY_PORT:-$(free_port)}
base_url="http://127.0.0.1:$INFERCRANE_DEV_PORT"
proxy_api="http://127.0.0.1:$INFERCRANE_TOXIPROXY_PORT"

cleanup() {
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_http() {
  local url=$1 expected=$2 limit=${3:-60} code
  for ((attempt=1; attempt<=limit; attempt++)); do
    code=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
    [[ "$code" == "$expected" ]] && return 0
    sleep 1
  done
  echo "timed out waiting for $url to return HTTP $expected" >&2
  return 1
}

inference() {
  curl -fsS --max-time 8 \
    -H 'Authorization: Bearer infercrane' \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen-prod","messages":[{"role":"user","content":"database partition"}]}' \
    "$base_url/v1/chat/completions" | jq -e '.choices[0].message.content != ""' >/dev/null
}

wait_inference() {
  local limit=${1:-45}
  for ((attempt=1; attempt<=limit; attempt++)); do
    inference 2>/dev/null && return 0
    sleep 1
  done
  echo "timed out waiting for the published inference route" >&2
  return 1
}

"${compose[@]}" build infercrane worker-a worker-b
"${compose[@]}" up -d postgres toxiproxy worker-a worker-b
wait_http "$proxy_api/version" 200
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"name":"postgres","listen":"0.0.0.0:15432","upstream":"postgres:5432","enabled":true}' \
  "$proxy_api/proxies" >/dev/null
"${compose[@]}" up -d --no-deps infercrane

wait_http "$base_url/readyz" 200 90
wait_inference

# Disable the database proxy, closing existing connections and refusing new
# ones. Readiness must fail closed, but an already-published inference route
# must remain useful without a PostgreSQL read in the data path.
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"name":"postgres","listen":"0.0.0.0:15432","upstream":"postgres:5432","enabled":false}' \
  "$proxy_api/proxies/postgres" >/dev/null
wait_http "$base_url/readyz" 503 15
inference

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"name":"postgres","listen":"0.0.0.0:15432","upstream":"postgres:5432","enabled":true}' \
  "$proxy_api/proxies/postgres" >/dev/null
wait_http "$base_url/readyz" 200 30
inference

echo "InferCrane PostgreSQL network-partition recovery passed"
