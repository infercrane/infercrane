#!/usr/bin/env sh
set -eu

base_url=${INFERCRANE_SMOKE_URL:-http://127.0.0.1:18000}
api_key=${INFERCRANE_SMOKE_API_KEY:-infercrane}
model=${INFERCRANE_SMOKE_MODEL:-qwen-prod}

docker compose up --build -d
docker compose ps

attempt=0
until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    echo "InferCrane did not become ready within 60 seconds" >&2
    docker compose logs --tail 100 infercrane >&2
    exit 1
  fi
  sleep 1
done

curl -fsS "$base_url/livez" >/dev/null
curl -fsS "$base_url/readyz" >/dev/null
curl -fsS -H "Authorization: Bearer $api_key" "$base_url/v1/models" >/dev/null
attempt=0
until curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke test\"}]}" \
  "$base_url/v1/chat/completions" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "Smoke deployment did not become routable within 30 seconds" >&2
    docker compose logs --tail 100 infercrane >&2
    exit 1
  fi
  sleep 1
done
curl -fsS "$base_url/metrics" | grep -q 'infercrane_gateway_request_duration_seconds_bucket'

stream_body=$(mktemp)
stream_headers=$(mktemp)
dashboard_body=$(mktemp)
dashboard_headers=$(mktemp)
trap 'rm -f "$stream_body" "$stream_headers" "$dashboard_body" "$dashboard_headers"' EXIT HUP INT TERM
curl -fsS -D "$dashboard_headers" -o "$dashboard_body" "$base_url/dashboard/"
grep -q '<title>InferCrane · Operations</title>' "$dashboard_body"
grep -qi '^Content-Security-Policy:' "$dashboard_headers"
grep -qi '^Cache-Control: no-store' "$dashboard_headers"
curl -fsS "$base_url/dashboard/app.mjs" | grep -q "sessionStorage.getItem"
test "$(curl -sS -o /dev/null -w '%{http_code}' "$base_url/api/v1/deployments")" = 401
curl -fsS -H "Authorization: Bearer $api_key" "$base_url/api/v1/deployments" | jq -e '.data | type == "array"' >/dev/null
curl -fsS -N -D "$stream_headers" -o "$stream_body" \
  -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"stream smoke\"}]}" \
  "$base_url/v1/chat/completions"
grep -qi '^Content-Type: text/event-stream' "$stream_headers"
grep -q '^data: {' "$stream_body"
grep -q '^data: \[DONE\]' "$stream_body"

INFERCRANE_API_KEY="$api_key" INFERCRANE_CONTROL_URL="$base_url" INFERCRANE_DEPLOYMENT="$model" \
  PYTHONPATH="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)/sdk/python/src" \
  python3 "$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)/examples/sdk/python_stream.py"
INFERCRANE_API_KEY="$api_key" INFERCRANE_CONTROL_URL="$base_url" INFERCRANE_DEPLOYMENT="$model" \
  node "$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)/examples/sdk/typescript_stream.mjs"

tool_response=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"weather\"}],\"tool_choice\":\"auto\",\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"weather\",\"parameters\":{\"type\":\"object\"}}}]}" \
  "$base_url/v1/chat/completions")
printf '%s' "$tool_response" | jq -e '.choices[0].message.tool_calls[0].function.name == "weather" and .choices[0].finish_reason == "tool_calls"' >/dev/null

structured_response=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"answer\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"answer\",\"schema\":{\"type\":\"object\",\"properties\":{\"answer\":{\"type\":\"string\"}},\"required\":[\"answer\"]}}}}" \
  "$base_url/v1/chat/completions")
printf '%s' "$structured_response" | jq -e '.choices[0].message.content | fromjson | .answer == "ok"' >/dev/null

# Prove incremental adoption against existing workloads without transferring or
# deleting their lifecycle. Run-scoped endpoint names keep repeated local runs independent.
adoption_suffix=$(date -u +%H%M%S)-$$
observe_endpoint=adopt-observe-$adoption_suffix
traffic_endpoint=adopt-traffic-$adoption_suffix
docker compose exec -T infercrane infercrane adopt endpoint "$observe_endpoint" --url http://worker-a:8101 --model adopted-smoke --upstream-model Qwen/Qwen3-8B --ownership observe-only >/dev/null
observe_status=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$observe_endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"must not route\"}]}" \
  "$base_url/v1/chat/completions")
if [ "$observe_status" != 404 ]; then
  echo "Observe-only adoption unexpectedly routed a request (HTTP $observe_status)" >&2
  exit 1
fi
docker compose exec -T infercrane infercrane adopt endpoint "$traffic_endpoint" --url http://worker-b:8102 --model adopted-smoke --upstream-model Qwen/Qwen3-8B --ownership traffic-managed >/dev/null
attempt=0
until request_headers=$(curl -fsS -D - -o /dev/null -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d "{\"model\":\"$traffic_endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"adoption smoke\"}]}" "$base_url/v1/chat/completions" 2>/dev/null); do
  attempt=$((attempt + 1))
  test "$attempt" -lt 30 || { echo "Traffic-managed adoption did not become routable" >&2; exit 1; }
  sleep 1
done
request_id=$(printf '%s\n' "$request_headers" | awk '/^X-Request-Id:/ {gsub("\r", "", $2); print $2}')
test -n "$request_id"
docker compose exec -T infercrane infercrane request inspect "$request_id" --output json | jq -e --arg endpoint "$traffic_endpoint" '.endpoint == $endpoint and .content_recorded == false and .deployment == ""' >/dev/null
docker compose exec -T infercrane infercrane doctor "$traffic_endpoint" --output json | jq -e '.[].code == "no_active_finding"' >/dev/null
docker compose exec -T infercrane infercrane adopt promote "$observe_endpoint" --ownership traffic-managed >/dev/null
attempt=0
until curl -fsS -o /dev/null -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d "{\"model\":\"$observe_endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"promoted adoption\"}]}" "$base_url/v1/chat/completions" 2>/dev/null; do
  attempt=$((attempt + 1))
  test "$attempt" -lt 30 || { echo "Promoted adoption did not become routable" >&2; exit 1; }
  sleep 1
done
docker compose exec -T infercrane infercrane endpoint delete "$observe_endpoint" --yes >/dev/null
docker compose exec -T infercrane infercrane endpoint delete "$traffic_endpoint" --yes >/dev/null
curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"shared target remains\"}]}" "$base_url/v1/chat/completions" >/dev/null

docker compose exec -T infercrane infercrane plan Qwen/Qwen3-8B --targets gpu-a,gpu-b --output json >/dev/null
docker compose exec -T infercrane infercrane doctor
docker compose exec -T infercrane aws --version
docker compose exec -T infercrane kubectl version --client=true

# Existing-target fixtures have no immutable Hugging Face artifact and are not
# presented as reproducible performance benchmarks. Exercise concurrent logical
# requests here; real deployment acceptance uses `infercrane benchmark NAME`.
sequence=1
while [ "$sequence" -le 20 ]; do
  curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke $sequence\"}]}" \
    "$base_url/v1/chat/completions" >/dev/null &
  sequence=$((sequence + 1))
done
wait

echo "InferCrane stack smoke test passed"
