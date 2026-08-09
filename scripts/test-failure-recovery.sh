#!/usr/bin/env sh
set -eu

docker compose stop worker-a
sleep 12
docker compose exec -T infercrane infercrane benchmark --endpoint http://127.0.0.1:8080 \
  --model qwen-prod --api-key infercrane --requests 10 --concurrency 2

docker compose start worker-a
docker compose restart infercrane
sleep 3
docker compose exec -T infercrane infercrane benchmark --endpoint http://127.0.0.1:8080 \
  --model qwen-prod --api-key infercrane --requests 10 --concurrency 2

echo "worker-loss and control-plane restart recovery passed"
