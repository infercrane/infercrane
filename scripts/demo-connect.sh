#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mode=${1:-adoption}
case "$mode" in
  adoption|full) ;;
  *) echo "usage: $0 [adoption|full]" >&2; exit 2 ;;
esac
project=${INFERCRANE_DEMO_PROJECT:-infercrane-connect-demo-$$}
port=${INFERCRANE_DEMO_PORT:-}
if [ -z "$port" ]; then
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
fi
base_url=http://127.0.0.1:$port
endpoint=${INFERCRANE_DEMO_ENDPOINT:-coder-demo}
api_key=infercrane
temporary=$(mktemp -d)
compose_log=$temporary/compose.log
response_body=$temporary/response.json

cd "$root"
cleanup() {
  docker compose -p "$project" down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$temporary"
}
trap cleanup EXIT HUP INT TERM

echo "LOCAL FIXTURE DEMO"
echo "This proves InferCrane control-plane behavior, not GPU performance or provider reliability."
echo
echo "Starting the local InferCrane evidence stack..."
if ! INFERCRANE_DEV_PORT=$port docker compose -p "$project" up --build -d \
  >"$compose_log" 2>&1; then
  tail -n 100 "$compose_log" >&2
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
until response_headers=$(curl -fsS -D - -o "$response_body" \
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
jq -r '.choices[0].message.content' "$response_body"

echo
echo "$ infercrane request inspect $request_id"
run_cli request inspect "$request_id"

echo
echo '$ infercrane doctor coder-demo'
run_cli doctor "$endpoint"

if [ "$mode" = full ]; then
  active_before=$(run_cli status qwen-prod --output json | jq -r '.deployment.active_revision_id')
  test -n "$active_before" && test "$active_before" != null || {
    echo "fixture deployment has no active revision" >&2
    exit 1
  }

  echo
  echo '$ infercrane rollout create qwen-prod --model Qwen/Qwen3-8B --cloud runpod --gpu L40S --wait'
  echo 'Creating an intentionally unready candidate. This step persists intent only and does not provision paid capacity.'
  run_cli rollout create qwen-prod --model Qwen/Qwen3-8B --cloud runpod --gpu L40S \
    --min 1 --max 1 --wait --idempotency-key "$project-bad-candidate" >/dev/null
  candidate=$(run_cli status qwen-prod --output json | jq -r '.deployment.candidate_revision_id')
  test -n "$candidate" && test "$candidate" != null || {
    echo "candidate revision was not persisted" >&2
    exit 1
  }
  echo "Candidate persisted: $candidate"

  echo
  echo '$ infercrane rollout evaluate qwen-prod --wait'
  run_cli rollout evaluate qwen-prod --wait --idempotency-key "$project-guard" >/dev/null
  guard=$(run_cli rollout inspect qwen-prod --output json)
  printf '%s\n' "$guard" | jq -e \
    --arg active "$active_before" --arg candidate "$candidate" \
    '.active_revision_id == $active and .candidate_revision_id == $candidate and .release_guard_evaluations[0].decision == "REJECT" and (.release_guard_evaluations[0].reasons | tostring | contains("candidate_not_ready"))' \
    >/dev/null || {
      echo "Release Guard did not preserve the active revision and reject the unready candidate" >&2
      printf '%s\n' "$guard" >&2
      exit 1
    }
  run_cli rollout inspect qwen-prod

  echo
  echo '$ infercrane explain rollout qwen-prod'
  run_cli explain rollout qwen-prod

  echo
  echo '$ infercrane rollout reject qwen-prod CANDIDATE --reason "candidate intentionally unready" --wait'
  run_cli rollout reject qwen-prod "$candidate" --reason "local demo candidate intentionally has no ready capacity" \
    --wait --idempotency-key "$project-reject" >/dev/null
  final=$(run_cli status qwen-prod --output json)
  printf '%s\n' "$final" | jq -e --arg active "$active_before" \
    '.deployment.active_revision_id == $active and ((.deployment.candidate_revision_id // "") == "")' >/dev/null || {
      echo "candidate cleanup changed the active revision or left a candidate behind" >&2
      printf '%s\n' "$final" >&2
      exit 1
    }
  echo "Verified: the active revision remained $active_before and the rejected candidate was cleaned up."
fi

echo
echo "Demo complete. The imported worker remained externally owned; the temporary local stack will now be removed."
