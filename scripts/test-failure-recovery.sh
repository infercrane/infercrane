#!/usr/bin/env sh
set -eu

base_url=${INFERCRANE_SMOKE_URL:-http://127.0.0.1:18000}
api_key=${INFERCRANE_SMOKE_API_KEY:-infercrane}
model=${INFERCRANE_SMOKE_MODEL:-qwen-prod}

request() {
  curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}]}" \
    "$base_url/v1/chat/completions" >/dev/null
}

wait_ready() {
  elapsed=0
  until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
    elapsed=$((elapsed + 1))
    [ "$elapsed" -lt 30 ] || { echo "control plane did not become ready" >&2; return 1; }
    sleep 1
  done
}

wait_stable_requests() {
  message=$1
  started=$(date +%s)
  elapsed=0
  consecutive=0
  while [ "$elapsed" -lt 45 ]; do
    if request "$message" 2>/dev/null; then
      consecutive=$((consecutive + 1))
      if [ "$consecutive" -ge 3 ]; then
        finished=$(date +%s)
        echo "$message convergence: $((finished - started))s"
        return 0
      fi
    else
      consecutive=0
    fi
    elapsed=$((elapsed + 1))
    sleep 1
  done
  echo "routing did not stabilize after worker loss" >&2
  return 1
}

docker compose stop worker-a
wait_stable_requests "worker loss"

docker compose start worker-a
docker compose restart infercrane
wait_ready
wait_stable_requests "control plane restart"

echo "worker-loss and control-plane restart recovery passed"
