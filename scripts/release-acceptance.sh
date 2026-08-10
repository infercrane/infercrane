#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
compose_file="$root/compose.runpod-acceptance.yaml"
project=${INFERCRANE_ACCEPTANCE_PROJECT:-infercrane-runpod}
state_root=${INFERCRANE_ACCEPTANCE_STATE_DIR:-"$root/.infercrane/acceptance"}
requested_run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:-}
run_id=${requested_run_id:-$(date -u +%Y%m%dT%H%M%SZ)}
current_file="$state_root/current"
approval=false

usage() {
  cat <<'EOF'
Usage: scripts/release-acceptance.sh COMMAND [--approve-paid-resources]

Commands:
  local       Run non-provider repository and container acceptance
  preflight   Validate credentials, dependencies, template, and inventories (read-only)
  elastic     Run the resumable paid elastic smoke workflow
  serverless  Run the resumable paid serverless smoke workflow
  qualify     Run elastic benchmark/autoscaling/guard, then serverless cold-start qualification
  cleanup     Delete run-owned deployments and stop the acceptance stack
  report      Refresh the sanitized evidence summary

Paid commands refuse to run without --approve-paid-resources. Set RUNPOD_KEY_FILE to a readable
key file. Serverless additionally requires INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID.
EOF
}

command_name=${1:-}
[ "$command_name" != "-h" ] && [ "$command_name" != "--help" ] || { usage; exit 0; }
[ -n "$command_name" ] || { usage; exit 2; }
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --approve-paid-resources) approval=true ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "required command not found: $1" >&2; exit 1; }; }
compose() { docker compose -p "$project" -f "$compose_file" "$@"; }

new_run() {
  case "$run_id" in *[!A-Za-z0-9_.-]*) echo "invalid acceptance run ID" >&2; exit 1;; esac
  model=${INFERCRANE_ACCEPTANCE_MODEL:-Qwen/Qwen3-8B}
  gpu=${INFERCRANE_ACCEPTANCE_GPU:-L40S}
  case "$model" in *[!A-Za-z0-9_./-]*) echo "invalid acceptance model" >&2; exit 1;; esac
  case "$gpu" in *[!A-Za-z0-9_.-]*) echo "invalid acceptance GPU" >&2; exit 1;; esac
  mkdir -p "$state_root"
  run_dir="$state_root/$run_id"
  mkdir -p "$run_dir/evidence" "$run_dir/bin"
  chmod 700 "$state_root" "$run_dir" "$run_dir/evidence"
  printf '%s\n' "$run_id" >"$current_file"
  cat >"$run_dir/state.env" <<EOF
RUN_ID='$run_id'
ELASTIC_NAME='qwen-elastic-$run_id'
SERVERLESS_NAME='qwen-serverless-$run_id'
MODEL='$model'
GPU='$gpu'
EOF
}

load_run() {
  if [ -n "$requested_run_id" ]; then
    run_id=$requested_run_id
    if [ ! -f "$state_root/$run_id/state.env" ]; then new_run; fi
    printf '%s\n' "$run_id" >"$current_file"
  else
    if [ ! -s "$current_file" ]; then new_run; fi
    run_id=$(cat "$current_file")
  fi
  run_dir="$state_root/$run_id"
  [ -f "$run_dir/state.env" ] || { echo "acceptance state is missing: $run_dir/state.env" >&2; exit 1; }
  # The state file is generated above from bounded identifiers and contains no credentials.
  . "$run_dir/state.env"
  evidence="$run_dir/evidence"
  cli="$run_dir/bin/infercrane"
  config_file="$run_dir/client.json"
}

record() {
  name=$1
  shift
  echo "==> $name"
  temporary="$evidence/$name.tmp"
  if "$@" >"$temporary" 2>&1; then
    tee "$evidence/$name.log" <"$temporary"
    rm -f "$temporary"
  else
    status=$?
    tee "$evidence/$name.log" <"$temporary" >&2
    rm -f "$temporary"
    return "$status"
  fi
}

build_cli() {
  if [ ! -x "$cli" ] || [ "$root/cmd/infercrane/main.go" -nt "$cli" ]; then
    (cd "$root" && go build -o "$cli" ./cmd/infercrane)
  fi
}

ic() {
  INFERCRANE_CONFIG="$config_file" "$cli" --context acceptance "$@"
}

require_key() {
  [ -n "${RUNPOD_KEY_FILE:-}" ] || { echo "RUNPOD_KEY_FILE is required" >&2; exit 1; }
  [ -f "$RUNPOD_KEY_FILE" ] && [ -r "$RUNPOD_KEY_FILE" ] || { echo "RUNPOD_KEY_FILE is not a readable file" >&2; exit 1; }
}

require_paid_approval() {
  [ "$approval" = true ] || {
    echo "refusing paid provider mutation without --approve-paid-resources" >&2
    exit 1
  }
}

start_stack() {
  require_key
  compose up --build -d
  attempt=0
  until curl -fsS "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/readyz" >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 90 ] || { compose logs --tail 100 infercrane >&2; exit 1; }
    sleep 1
  done
  build_cli
  INFERCRANE_CONFIG="$config_file" "$cli" init --context acceptance \
    --url "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}" \
    --api-key infercrane-runpod-acceptance-key >/dev/null
}

capture_inventory() {
  record "$1-infercrane-orphans" ic orphans --output json
  record "$1-deployments" ic deployments --output json
}

wait_ready() {
  deployment=$1
  limit=${INFERCRANE_ACCEPTANCE_READY_TIMEOUT_SECONDS:-2700}
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    status=$(ic status "$deployment" --output json 2>/dev/null || true)
    if [ -n "$status" ] && printf '%s' "$status" | jq -e \
      '.deployment.observed_state == "healthy" and ([.replicas[] | select(.health == "healthy" and (.lifecycle_state == "active" or .lifecycle_state == "ready"))] | length) > 0' >/dev/null 2>&1; then
      printf '%s\n' "$status" >"$evidence/$deployment-ready.json"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done
  echo "deployment did not become ready within ${limit}s: $deployment" >&2
  return 1
}

wait_replica_count() {
  deployment=$1
  expected=$2
  limit=${3:-900}
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    status=$(ic status "$deployment" --output json 2>/dev/null || true)
    count=$(printf '%s' "$status" | jq '[.replicas[]? | select(.health == "healthy" and (.lifecycle_state == "active" or .lifecycle_state == "ready"))] | length' 2>/dev/null || echo 0)
    if [ "$count" -eq "$expected" ]; then
      printf '%s\n' "$status" >"$evidence/$deployment-replicas-$expected.json"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done
  echo "deployment did not reach $expected healthy replicas within ${limit}s: $deployment" >&2
  return 1
}

wait_lifecycle_idle() {
  deployment=$1
  limit=${2:-600}
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    status=$(ic status "$deployment" --output json 2>/dev/null || true)
    if [ -z "$status" ]; then
      # Cancellation cleanup may remove the deployment while this waiter is
      # active. Absence is already terminal for cleanup serialization.
      return 0
    fi
    if [ -n "$status" ] && printf '%s' "$status" | jq -e '.active_operation == null' >/dev/null 2>&1; then
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done
  echo "deployment lifecycle did not become idle within ${limit}s: $deployment" >&2
  return 1
}

wait_serverless_zero() {
  deployment=$1
  limit=${2:-900}
  endpoint=$(ic status "$deployment" --output json | jq -r '.replicas[0].provider_resource_id')
  [ -n "$endpoint" ] && [ "$endpoint" != null ] || { echo "serverless endpoint ID is missing" >&2; return 1; }
  api_key=$(tr -d '\r\n' <"$RUNPOD_KEY_FILE")
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    endpoint_json=$(curl -fsS -H "Authorization: Bearer $api_key" \
      'https://rest.runpod.io/v1/endpoints?includeWorkers=true' | \
      jq -c --arg id "$endpoint" '.[] | select(.id == $id) | {id,name,workersMin,workersMax,workers_observed:([.workers[]? | select(.desiredStatus != "EXITED")] | length)}')
    workers=$(printf '%s' "$endpoint_json" | jq -r '.workers_observed // -1')
    if [ "$workers" -eq 0 ]; then
      printf '%s\n' "$endpoint_json" >"$evidence/$deployment-scale-to-zero.json"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done
  echo "serverless endpoint did not scale to zero within ${limit}s: $endpoint" >&2
  return 1
}

delete_if_present() {
  deployment=$1
  key=$2
  if ic status "$deployment" --output json >/dev/null 2>&1; then
    wait_lifecycle_idle "$deployment" 600
    record "delete-plan-$deployment" ic delete "$deployment" --plan --output json
    record "delete-$deployment" ic delete "$deployment" --yes --wait \
      --idempotency-key "$key" --output json
  fi
}

run_local() {
  load_run
  need docker; need go; need npm
  record local-tests sh -c 'cd "$1" && make verify && make deadcode && make audit && make docs-check && make test-container && make test-stack && make test-failure' sh "$root"
  (cd "$root" && docker compose --profile test down)
}

run_preflight() {
  load_run
  need docker; need go; need curl; need jq
  require_key
  start_stack
  record preflight-doctor ic doctor --cloud --output json
  if [ -n "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ]; then
    record preflight-serverless ic doctor --serverless --output json
  else
    echo "serverless template is not configured; elastic preflight only" | tee "$evidence/preflight-serverless-skipped.log"
  fi
  capture_inventory before
  record elastic-plan ic plan "$MODEL" --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" --min 1 --max 2 --output json
}

run_elastic() {
  require_paid_approval
  run_preflight
  record elastic-deploy ic deploy "$MODEL" --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" \
    --min 1 --max 2 --wait --idempotency-key "$ELASTIC_NAME-create" --output json
  wait_ready "$ELASTIC_NAME"
  ic request "$ELASTIC_NAME" --message "acceptance probe" --output json >/dev/null
  ic request "$ELASTIC_NAME" --message "stream acceptance probe" --stream >/dev/null
  printf 'buffered and streaming requests passed\n' >"$evidence/elastic-requests.log"
  record elastic-benchmark ic benchmark "$ELASTIC_NAME" --revision active --requests 100 --concurrency 10 --random-seed 17 --output json
  record elastic-inspect ic inspect "$ELASTIC_NAME" --output json
  record elastic-events ic events "$ELASTIC_NAME" --output json
  record elastic-explain-scaling ic explain scaling "$ELASTIC_NAME" --output json
  record elastic-explain-cold-start ic explain cold-start "$ELASTIC_NAME" --output json
  echo "Elastic smoke completed. Full RC qualification still requires the controlled disconnect, restart, drain, and bad-candidate checkpoints in docs/release-acceptance.md."
}

run_serverless() {
  require_paid_approval
  [ -n "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ] || { echo "INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID is required" >&2; exit 1; }
  run_preflight
  record serverless-deploy ic deploy "$MODEL" --name "$SERVERLESS_NAME" --compute serverless \
    --cloud runpod --gpu "$GPU" --max 2 --wait --idempotency-key "$SERVERLESS_NAME-create" --output json
  # Provider endpoint persistence completes before the reconciler publishes the
  # logical alias. Do not race the first request against route publication.
  wait_ready "$SERVERLESS_NAME"
  ic request "$SERVERLESS_NAME" --message "cold acceptance probe" --output json >/dev/null
  ic request "$SERVERLESS_NAME" --message "warm acceptance probe" --output json >/dev/null
  ic request "$SERVERLESS_NAME" --message "stream acceptance probe" --stream >/dev/null
  printf 'cold, warm, and streaming requests passed\n' >"$evidence/serverless-requests.log"
  record serverless-inspect ic inspect "$SERVERLESS_NAME" --output json
  record serverless-events ic events "$SERVERLESS_NAME" --output json
  record serverless-explain-cold-start ic explain cold-start "$SERVERLESS_NAME" --output json
  wait_serverless_zero "$SERVERLESS_NAME" 900
  ic request "$SERVERLESS_NAME" --message "second cold acceptance probe" --output json >/dev/null
  printf 'provider scaled to zero and the second cold request passed\n' >"$evidence/serverless-second-cold.log"
  record serverless-events-after-second-cold ic events "$SERVERLESS_NAME" --output json
  record serverless-explain-second-cold ic explain cold-start "$SERVERLESS_NAME" --output json
  echo "Serverless cold, warm, scale-to-zero, second-cold, and streaming smoke completed."
}

run_qualify() {
  require_paid_approval
  run_elastic

  scale_up_timeout=${INFERCRANE_ACCEPTANCE_SCALE_UP_TIMEOUT_SECONDS:-2700}
  scale_down_timeout=${INFERCRANE_ACCEPTANCE_SCALE_DOWN_TIMEOUT_SECONDS:-900}

  # The benchmark supplies bounded real queue pressure. Autoscaling must prove
  # both provider convergence to two replicas and the idle return to one.
  record elastic-autoscale-queue-load ic benchmark "$ELASTIC_NAME" --revision active \
    --requests 2000 --concurrency 400 --random-seed 29 --output json
  record elastic-explain-scale-up ic explain scaling "$ELASTIC_NAME" --output json
  wait_replica_count "$ELASTIC_NAME" 2 "$scale_up_timeout"
  wait_replica_count "$ELASTIC_NAME" 1 "$scale_down_timeout"
  wait_lifecycle_idle "$ELASTIC_NAME" 600
  record elastic-explain-scale-down ic explain scaling "$ELASTIC_NAME" --output json

  # A candidate with no provisioned capacity is a deterministic bad update:
  # Release Guard must reject candidate_not_ready without creating another pod.
  record guard-candidate-create ic rollout create "$ELASTIC_NAME" --model "$MODEL" \
    --cloud runpod --gpu "$GPU" --min 1 --max 1 --wait \
    --idempotency-key "$ELASTIC_NAME-guard-candidate" --output json
  candidate=$(ic status "$ELASTIC_NAME" --output json | jq -r '.deployment.candidate_revision_id')
  [ -n "$candidate" ] && [ "$candidate" != null ] || { echo "candidate revision was not persisted" >&2; return 1; }
  record guard-evaluate ic rollout evaluate "$ELASTIC_NAME" --wait \
    --idempotency-key "$ELASTIC_NAME-guard-evaluate" --output json
  record guard-explain ic explain rollout "$ELASTIC_NAME" --output json
  record guard-inspect ic rollout inspect "$ELASTIC_NAME" --output json
  record guard-reject ic rollout reject "$ELASTIC_NAME" "$candidate" \
    --reason "acceptance candidate intentionally has no ready capacity" --wait \
    --idempotency-key "$ELASTIC_NAME-guard-reject" --output json

  delete_if_present "$ELASTIC_NAME" "$ELASTIC_NAME-delete"
  run_serverless
  echo "Qualification smoke completed; guarded cleanup will now delete the serverless endpoint and verify InferCrane inventory."
}

run_cleanup() {
  load_run
  need docker; need go; need curl; need jq
  start_stack
  delete_if_present "$ELASTIC_NAME" "$ELASTIC_NAME-delete"
  delete_if_present "$SERVERLESS_NAME" "$SERVERLESS_NAME-delete"
  capture_inventory after-cleanup
  compose down
  echo "InferCrane cleanup completed. Confirm zero run-owned pods, endpoints, and workers in RunPod before removing volumes."
}

run_report() {
  load_run
  commit=$(git -C "$root" rev-parse HEAD)
  {
    echo "# InferCrane acceptance run $RUN_ID"
    echo
    echo "- Commit: \`$commit\`"
    echo "- Model: \`$MODEL\`"
    echo "- GPU: \`$GPU\`"
    echo "- Elastic deployment: \`$ELASTIC_NAME\`"
    echo "- Serverless deployment: \`$SERVERLESS_NAME\`"
    echo
    echo "## Evidence files"
    find "$evidence" -maxdepth 1 -type f -print | sort | sed 's#^.*/#- #'
    echo
    echo "Provider inventory confirmation: PENDING OPERATOR CHECK"
  } >"$run_dir/report.md"
  echo "$run_dir/report.md"
}

case "$command_name" in
  local) run_local ;;
  preflight) run_preflight ;;
  elastic|serverless)
    require_paid_approval
    trap 'result=$?; trap - EXIT; if [ "$result" -ne 0 ]; then echo "acceptance failed; running guarded cleanup" >&2; run_cleanup || true; fi; exit "$result"' EXIT
    if [ "$command_name" = elastic ]; then run_elastic; else run_serverless; fi
    trap - EXIT
    ;;
  qualify)
    # Refuse before installing the cleanup wrapper so a missing approval cannot
    # start even the local acceptance stack.
    require_paid_approval
    trap 'result=$?; trap - EXIT; echo "qualification failed; running guarded cleanup" >&2; run_cleanup || true; exit "$result"' EXIT
    run_qualify
    trap - EXIT
    run_cleanup
    ;;
  cleanup) run_cleanup ;;
  report) run_report ;;
  *) usage >&2; exit 2 ;;
esac
