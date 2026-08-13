#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
project=${INFERCRANE_DEMO_PROJECT:-infercrane-connect-demo}
port=${INFERCRANE_DEMO_PORT:-18081}
base_url=http://127.0.0.1:$port
endpoint=${INFERCRANE_DEMO_ENDPOINT:-coder-demo}
api_key=infercrane

cd "$root"
cleanup() {
	rm -f "/tmp/infercrane-demo-response.$$" "/tmp/infercrane-demo-compose.$$"
  docker compose -p "$project" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
cleanup

echo "Starting the local InferCrane evidence stack..."
if ! INFERCRANE_DEV_PORT=$port docker compose -p "$project" up --build -d \
  >"/tmp/infercrane-demo-compose.$$" 2>&1; then
  tail -n 100 "/tmp/infercrane-demo-compose.$$" >&2
  exit 1
fi

attempt=0
until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 90 ]; then
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" logs --tail 100 infercrane >&2
    exit 1
  fi
  sleep 1
done

run_cli() {
  INFERCRANE_DEV_PORT=$port docker compose -p "$project" exec -T \
    -e INFERCRANE_URL=http://127.0.0.1:8080 \
    -e INFERCRANE_API_KEY=$api_key infercrane infercrane "$@"
}

echo
echo '$ infercrane connect http://worker-a:8101/v1 --as coder-demo --type vllm --manage-traffic'
run_cli connect http://worker-a:8101/v1 --as "$endpoint" --type vllm --manage-traffic

attempt=0
until response_headers=$(curl -fsS -D - -o /tmp/infercrane-demo-response.$$ \
  -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain durable inference in one sentence.\"}]}" \
  "$base_url/v1/chat/completions" 2>/dev/null); do
  attempt=$((attempt + 1))
  test "$attempt" -lt 30 || { echo "Connected endpoint did not become routable" >&2; exit 1; }
  sleep 1
done
request_id=$(printf '%s\n' "$response_headers" | awk '/^X-Request-Id:/ {gsub("\r", "", $2); print $2}')
echo
echo '$ infercrane request coder-demo "Explain durable inference in one sentence."'
jq -r '.choices[0].message.content' /tmp/infercrane-demo-response.$$
rm -f /tmp/infercrane-demo-response.$$

echo
echo "$ infercrane request inspect $request_id"
run_cli request inspect "$request_id"

echo
echo '$ infercrane doctor coder-demo'
run_cli doctor "$endpoint"

echo
echo "Demo complete. The imported worker remained externally owned; the temporary local stack will now be removed."
