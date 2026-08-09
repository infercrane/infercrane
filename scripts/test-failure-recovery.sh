#!/usr/bin/env sh
set -eu

docker compose stop worker-a
sleep 12
curl -fsS -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
  -d '{"model":"qwen-prod","messages":[{"role":"user","content":"worker loss"}]}' \
  http://127.0.0.1:18000/v1/chat/completions >/dev/null

docker compose start worker-a
docker compose restart infercrane
sleep 3
curl -fsS -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
  -d '{"model":"qwen-prod","messages":[{"role":"user","content":"control plane restart"}]}' \
  http://127.0.0.1:18000/v1/chat/completions >/dev/null

echo "worker-loss and control-plane restart recovery passed"
