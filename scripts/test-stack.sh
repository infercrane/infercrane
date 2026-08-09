#!/usr/bin/env sh
set -eu

base_url=${INFERCRANE_SMOKE_URL:-http://127.0.0.1:18000}
api_key=${INFERCRANE_SMOKE_API_KEY:-infercrane}
model=${INFERCRANE_SMOKE_MODEL:-qwen-prod}

docker compose up --build -d
docker compose ps

curl -fsS "$base_url/livez" >/dev/null
curl -fsS "$base_url/readyz" >/dev/null
curl -fsS -H "Authorization: Bearer $api_key" "$base_url/v1/models" >/dev/null
curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke test\"}]}" \
  "$base_url/v1/chat/completions" >/dev/null
curl -fsS "$base_url/metrics" | grep -q 'infercrane_gateway_request_duration_seconds_bucket'

docker compose exec -T infercrane infercrane plan Qwen/Qwen3-8B --targets gpu-a,gpu-b --output json >/dev/null
docker compose exec -T infercrane infercrane doctor
docker compose exec -T infercrane infercrane benchmark --endpoint http://127.0.0.1:8080 --model "$model" --api-key "$api_key" --requests 20 --concurrency 4

echo "InferCrane stack smoke test passed"
