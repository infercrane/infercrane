#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
compose_file="$root/compose.runpod-acceptance.yaml"
project=${INFERCRANE_ACCEPTANCE_PROJECT:-infercrane-runpod}
state_root=${INFERCRANE_ACCEPTANCE_STATE_DIR:-"$root/.infercrane/acceptance"}
requested_run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:-}
requested_model=${INFERCRANE_ACCEPTANCE_MODEL:-}
requested_gpu=${INFERCRANE_ACCEPTANCE_GPU:-}
requested_gpu_count=${INFERCRANE_ACCEPTANCE_GPU_COUNT:-}
requested_spec=${INFERCRANE_ACCEPTANCE_SPEC_FILE:-}
requested_name=${INFERCRANE_ACCEPTANCE_NAME:-}
run_id=${requested_run_id:-$(date -u +%Y%m%dT%H%M%SZ)}
current_file="$state_root/current"
paid_lock="$state_root/.paid.lock"
approval=false

usage() {
  cat <<'EOF'
Usage: scripts/release-acceptance.sh COMMAND [--approve-paid-resources]

Commands:
  local       Run non-provider repository and container acceptance
  preflight   Validate credentials, dependencies, template, and inventories (read-only)
  elastic     Run the resumable paid elastic smoke workflow
  elastic-evidence Run a fixed-capacity DeploymentSpec evidence suite
  elastic-qualify Run elastic benchmark, autoscaling, and Release Guard qualification
  serverless  Run the resumable paid serverless smoke workflow
  elastic-faults    Run disconnect, restart, promotion/drain, and delete-restart gates
  serverless-faults Run lost-create-response and stream-cancellation gates
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
  gpu_count=${INFERCRANE_ACCEPTANCE_GPU_COUNT:-1}
	deployment_name=${INFERCRANE_ACCEPTANCE_NAME:-qwen-elastic-$run_id}
  case "$model" in *[!A-Za-z0-9_./-]*) echo "invalid acceptance model" >&2; exit 1;; esac
  case "$gpu" in *[!A-Za-z0-9_.-]*) echo "invalid acceptance GPU" >&2; exit 1;; esac
	case "$gpu_count" in ''|*[!0-9]*) echo "invalid acceptance GPU count" >&2; exit 1;; esac
	[ "$gpu_count" -ge 1 ] && [ "$gpu_count" -le 8 ] || { echo "acceptance GPU count must be between 1 and 8" >&2; exit 1; }
	case "$deployment_name" in *[!a-z0-9-]*|'') echo "invalid acceptance deployment name" >&2; exit 1;; esac
  mkdir -p "$state_root"
  run_dir="$state_root/$run_id"
  mkdir -p "$run_dir/evidence" "$run_dir/bin"
  chmod 700 "$state_root" "$run_dir" "$run_dir/evidence"
  candidate_commit=$(git -C "$root" rev-parse HEAD)
  printf '%s\n' "$run_id" >"$current_file"
  cat >"$run_dir/state.env" <<EOF
RUN_ID='$run_id'
CANDIDATE_COMMIT='$candidate_commit'
ELASTIC_NAME='$deployment_name'
SERVERLESS_NAME='qwen-serverless-$run_id'
MODEL='$model'
GPU='$gpu'
GPU_COUNT='$gpu_count'
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
	GPU_COUNT=${GPU_COUNT:-1}
  if [ -n "$requested_model" ] && [ "$requested_model" != "$MODEL" ]; then
    echo "acceptance run $run_id was created for model $MODEL, not requested model $requested_model; use a new run ID" >&2
    exit 1
  fi
  if [ -n "$requested_gpu" ] && [ "$requested_gpu" != "$GPU" ]; then
    echo "acceptance run $run_id was created for GPU $GPU, not requested GPU $requested_gpu; use a new run ID" >&2
    exit 1
  fi
	if [ -n "$requested_gpu_count" ] && [ "$requested_gpu_count" != "$GPU_COUNT" ]; then
		echo "acceptance run $run_id was created for $GPU_COUNT GPUs, not requested $requested_gpu_count; use a new run ID" >&2
		exit 1
	fi
	if [ -n "$requested_name" ] && [ "$requested_name" != "$ELASTIC_NAME" ]; then
		echo "acceptance run $run_id was created for deployment $ELASTIC_NAME, not requested $requested_name; use a new run ID" >&2
		exit 1
	fi
  evidence="$run_dir/evidence"
  cli="$run_dir/bin/infercrane"
  config_file="$run_dir/client.json"
}

record() {
  name=$1
  shift
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  started_epoch=$(date +%s)
  output_stream="$evidence/$name-output.stream"
  progress_stream="$evidence/$name-progress.stream"
  rm -f "$output_stream" "$progress_stream"
  mkfifo "$output_stream" "$progress_stream"
  echo "==> $name (started $started_at)"
  tee "$evidence/$name.log" <"$output_stream" >/dev/null &
  output_tee_pid=$!
  tee "$evidence/$name-progress.log" <"$progress_stream" >&2 &
  progress_tee_pid=$!
  if "$@" >"$output_stream" 2>"$progress_stream"; then
    status=0
  else
    status=$?
  fi
  wait "$output_tee_pid"
  wait "$progress_tee_pid"
  rm -f "$output_stream" "$progress_stream"
  elapsed=$(( $(date +%s) - started_epoch ))
	ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	jq -n --arg operation "$name" --arg started_at "$started_at" --arg ended_at "$ended_at" \
		--argjson elapsed_seconds "$elapsed" --argjson exit_code "$status" \
		'{schema_version:1,operation:$operation,started_at:$started_at,ended_at:$ended_at,elapsed_seconds:$elapsed_seconds,exit_code:$exit_code}' \
		>"$evidence/$name-timing.json"
  echo "<== $name (exit $status, ${elapsed}s)"
  if [ "$status" -ne 0 ]; then
    echo "Last output (full log: $evidence/$name.log):" >&2
    tail -n 20 "$evidence/$name.log" >&2
  fi
  return "$status"
}

build_cli() {
  # Go's build cache makes this cheap, while timestamping only main.go can run
  # stale acceptance binaries after changes elsewhere in the command package.
  (cd "$root" && go build -o "$cli" ./cmd/infercrane)
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

acquire_paid_lock() {
  mkdir -p "$state_root"
  if ! mkdir "$paid_lock" 2>/dev/null; then
    owner=$(cat "$paid_lock/pid" 2>/dev/null || true)
    owner_run=$(cat "$paid_lock/run_id" 2>/dev/null || true)
    if [ -n "$owner" ] && kill -0 "$owner" 2>/dev/null; then
      echo "another paid acceptance run is active (pid $owner, run ${owner_run:-unknown})" >&2
      return 1
    fi
    rm -rf "$paid_lock"
    mkdir "$paid_lock"
  fi
  printf '%s\n' "$$" >"$paid_lock/pid"
  printf '%s\n' "$run_id" >"$paid_lock/run_id"
}

release_paid_lock() {
  owner=$(cat "$paid_lock/pid" 2>/dev/null || true)
  if [ "$owner" = "$$" ]; then
    rm -rf "$paid_lock"
  fi
}

start_paid_watchdog() {
	limit=$1
	case "$limit" in ''|*[!0-9]*) echo "invalid paid-resource time limit" >&2; return 1;; esac
	[ "$limit" -ge 300 ] && [ "$limit" -le 10800 ] || { echo "paid-resource time limit must be between 300 and 10800 seconds" >&2; return 1; }
	watchdog_parent=$$
	(
		sleep "$limit"
		echo "paid-resource watchdog reached ${limit}s; terminating suite for guarded cleanup" >&2
		kill -TERM "$watchdog_parent"
	) &
	watchdog_pid=$!
	jq -n --argjson max_paid_seconds "$limit" --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		'{schema_version:1,max_paid_seconds:$max_paid_seconds,started_at:$started_at}' >"$run_dir/paid-watchdog.json"
}

stop_paid_watchdog() {
	if [ -n "${watchdog_pid:-}" ]; then
		kill "$watchdog_pid" 2>/dev/null || true
		watchdog_pid=""
	fi
}

write_suite_result() {
  suite_command=$1
  outcome=$2
  exit_code=$3
  case "$suite_command" in elastic|elastic-evidence|elastic-qualify|serverless|elastic-faults|serverless-faults|qualify) ;;
    *) echo "invalid acceptance suite command: $suite_command" >&2; return 1;;
  esac
  case "$outcome" in running|passed|failed) ;;
    *) echo "invalid acceptance suite outcome: $outcome" >&2; return 1;;
  esac
  case "$exit_code" in ''|*[!0-9]*) echo "invalid acceptance suite exit code: $exit_code" >&2; return 1;; esac
  jq -n --arg command "$suite_command" --arg outcome "$outcome" --argjson exit_code "$exit_code" \
    '{schema_version:1,command:$command,outcome:$outcome,exit_code:$exit_code,recorded_at:(now|todateiso8601)}' \
    >"$run_dir/suite-result.json"
}

start_stack() {
  require_key
  started_epoch=$(date +%s)
  bootstrap_log="$evidence/local-control-plane.log"
  echo "==> local-control-plane (preparing Docker services)"
  if ! compose up --build -d >"$bootstrap_log" 2>&1; then
    echo "local control plane failed; last output (full log: $bootstrap_log):" >&2
    tail -n 40 "$bootstrap_log" >&2
    return 1
  fi
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
  echo "<== local-control-plane (ready, $(( $(date +%s) - started_epoch ))s)"
}

ensure_stack() {
  if curl -fsS "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/readyz" >/dev/null 2>&1; then
    build_cli
    if [ ! -f "$config_file" ]; then
      INFERCRANE_CONFIG="$config_file" "$cli" init --context acceptance \
        --url "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}" \
        --api-key infercrane-runpod-acceptance-key >/dev/null
    fi
    return
  fi
  start_stack
}

capture_inventory() {
  record "$1-infercrane-orphans" ic orphans --output json
  record "$1-deployments" ic deployments --output json
}

capture_provider_inventory() {
  label=$1
  api_key=$(tr -d '\r\n' <"$RUNPOD_KEY_FILE")
  pods=$(curl -fsS -H "Authorization: Bearer $api_key" 'https://rest.runpod.io/v1/pods?includeMachine=true' | \
		jq '[.[] | select((.name // "") | startswith("infercrane-")) |
			{id,name,desiredStatus,machineId,image:(.imageName // .image),gpuCount,
			 gpu_type:(.gpuTypeId // .machine.gpuTypeId // .gpu.id),containerDiskInGb,
			 dockerEntrypoint,dockerStartCmd,ports,env_keys:((.env // {}) | keys),
			 lastStartedAt,lastStatusChange,uptimeSeconds,costPerHr,adjustedCostPerHr}]')
  endpoints=$(curl -fsS -H "Authorization: Bearer $api_key" \
    'https://rest.runpod.io/v1/endpoints?includeWorkers=true' | \
    jq '[.[] | select((.name // "") | startswith("infercrane-")) |
      {id,name,workersMin,workersMax,
       active_workers:([.workers[]? | select(.desiredStatus != "EXITED")] | length),
       exited_workers:([.workers[]? | select(.desiredStatus == "EXITED")] | length),
       last_worker:([.workers[]?] | sort_by(.createdAt // "") | last |
         if . == null then null else
           {id,desiredStatus,createdAt,lastStartedAt,lastStatusChange,machineId,imageName,gpuCount}
         end)}]')
  jq -n --argjson pods "$pods" --argjson endpoints "$endpoints" \
    '{pods:$pods,endpoints:$endpoints,verified_at:(now|todateiso8601)}' | \
    tee "$evidence/provider-direct-$label.json" >/dev/null
}

verify_provider_inventory_absent() {
  capture_provider_inventory after-cleanup
  pods=$(jq '.pods' "$evidence/provider-direct-after-cleanup.json")
  endpoints=$(jq '.endpoints' "$evidence/provider-direct-after-cleanup.json")
  [ "$(printf '%s' "$pods" | jq 'length')" -eq 0 ] || { echo "RunPod still has InferCrane pods" >&2; return 1; }
  [ "$(printf '%s' "$endpoints" | jq 'length')" -eq 0 ] || { echo "RunPod still has InferCrane endpoints" >&2; return 1; }
  echo "Provider inventory verified clean · pods 0 · endpoints 0"
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

operation_json() {
  ic operation "$1" --output json
}

wait_operation_status() {
  operation_id=$1
  expected=$2
  limit=${3:-1800}
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    operation=$(operation_json "$operation_id" 2>/dev/null || true)
    status=$(printf '%s' "$operation" | jq -r '.status // empty')
    if [ "$status" = "$expected" ]; then
      printf '%s\n' "$operation" >"$evidence/operation-$operation_id-$expected.json"
      return 0
    fi
    case "$status" in failed|cancelled) printf '%s\n' "$operation" >&2; return 1;; esac
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo "operation $operation_id did not reach $expected within ${limit}s" >&2
  return 1
}

wait_operation_error() {
  operation_id=$1
  expected=$2
  limit=${3:-120}
  elapsed=0
  while [ "$elapsed" -lt "$limit" ]; do
    operation=$(operation_json "$operation_id" 2>/dev/null || true)
    if printf '%s' "$operation" | jq -e --arg code "$expected" '.error_code == $code' >/dev/null 2>&1; then
      printf '%s\n' "$operation" >"$evidence/operation-$operation_id-$expected.json"
      return 0
    fi
    status=$(printf '%s' "$operation" | jq -r '.status // empty')
    case "$status" in succeeded|failed|cancelled) printf '%s\n' "$operation" >&2; return 1;; esac
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "operation $operation_id did not expose $expected within ${limit}s" >&2
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

verify_openai_features() {
  deployment=$1
  prefix=$2
  base_url="http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}"
  # Named function choice validates the portable default vLLM contract. Auto
  # selection is opt-in and requires a model-specific parser at server launch.
  tool_file="$evidence/$prefix-tool-response.json"
  tool_status=$(curl -sS -o "$tool_file" -w '%{http_code}' -H 'Authorization: Bearer infercrane-runpod-acceptance-key' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"What is the weather in Berlin? Use the weather tool.\"}],\"tool_choice\":{\"type\":\"function\",\"function\":{\"name\":\"weather\"}},\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"weather\",\"description\":\"Get weather\",\"strict\":true,\"parameters\":{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}},\"required\":[\"city\"],\"additionalProperties\":false}}}]}" \
    "$base_url/v1/chat/completions")
  case "$tool_status" in 2??) ;; *) echo "named tool-call probe failed with HTTP $tool_status; response: $tool_file" >&2; return 1 ;; esac
  tool_response=$(cat "$tool_file")
  printf '%s' "$tool_response" | jq -e '.choices[0].message.tool_calls[0].function.name == "weather" and (.choices[0].finish_reason == "tool_calls" or .choices[0].finish_reason == "stop")' >/dev/null

  structured_file="$evidence/$prefix-structured-response.json"
  structured_status=$(curl -sS -o "$structured_file" -w '%{http_code}' -H 'Authorization: Bearer infercrane-runpod-acceptance-key' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"Return a short acceptance result.\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"acceptance\",\"strict\":true,\"schema\":{\"type\":\"object\",\"properties\":{\"result\":{\"type\":\"string\"}},\"required\":[\"result\"],\"additionalProperties\":false}}}}" \
    "$base_url/v1/chat/completions")
  case "$structured_status" in 2??) ;; *) echo "structured-output probe failed with HTTP $structured_status; response: $structured_file" >&2; return 1 ;; esac
  structured_response=$(cat "$structured_file")
  printf '%s' "$structured_response" | jq -e '.choices[0].message.content | fromjson | .result | type == "string"' >/dev/null
  printf 'tool call and structured JSON response passed\n' >"$evidence/$prefix-openai-features.log"
}

delete_if_present() {
  deployment=$1
  key=$2
  status=$(ic status "$deployment" --output json 2>/dev/null || true)
  if [ -n "$status" ]; then
    active_operation=$(printf '%s' "$status" | jq -r '.active_operation.id // empty')
    if [ -n "$active_operation" ]; then
      record "cancel-$deployment" ic operation cancel "$active_operation"
    fi
    wait_lifecycle_idle "$deployment" 600
  fi
  # Cancellation cleanup is allowed to remove a never-activated deployment.
  # Recheck after the durable operation reaches a terminal state so cleanup
  # does not race a correct 404 from the control plane.
  if ic status "$deployment" --output json >/dev/null 2>&1; then
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
  capture_provider_inventory before
	if [ -n "$requested_spec" ]; then
		[ -f "$requested_spec" ] && [ -r "$requested_spec" ] || { echo "INFERCRANE_ACCEPTANCE_SPEC_FILE is not readable" >&2; return 1; }
		record elastic-plan ic plan "$requested_spec" --output json
	else
		record elastic-plan ic plan "$MODEL" --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" --gpu-count "$GPU_COUNT" --min 1 --max 2 --output json
	fi
}

submit_elastic_deploy() {
	wait_timeout=$1
	idempotency_key=$2
	if [ -n "$requested_spec" ]; then
		exec env INFERCRANE_CONFIG="$config_file" "$cli" --context acceptance deploy "$requested_spec" --wait --wait-timeout "$wait_timeout" \
			--idempotency-key "$idempotency_key" --output json
	else
		exec env INFERCRANE_CONFIG="$config_file" "$cli" --context acceptance deploy "$MODEL" --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" --gpu-count "$GPU_COUNT" \
			--min 1 --max 1 --wait --wait-timeout "$wait_timeout" \
			--idempotency-key "$idempotency_key" --output json
	fi
}

run_elastic() {
  require_paid_approval
  run_preflight
  ready_timeout=${INFERCRANE_ACCEPTANCE_READY_TIMEOUT_SECONDS:-2700}
  record elastic-deploy ic deploy "$MODEL" --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" \
    --min 1 --max 2 --wait --wait-timeout "${ready_timeout}s" \
    --idempotency-key "$ELASTIC_NAME-create" --output json
  wait_ready "$ELASTIC_NAME"
  record elastic-buffered-request ic request "$ELASTIC_NAME" --message "acceptance probe" --output json
  record elastic-streaming-request ic request "$ELASTIC_NAME" --message "stream acceptance probe" --stream
  verify_openai_features "$ELASTIC_NAME" elastic
  printf 'buffered and streaming requests passed\n' >"$evidence/elastic-requests.log"
  record elastic-benchmark ic benchmark "$ELASTIC_NAME" --revision active --requests 100 --concurrency 10 --random-seed 17 --output json
  jq -e '.runtime_version != "" and .provider != "" and .region != "" and .model_identity != "" and .gpu_count == 1 and .request_count == 100 and .failed == 0 and (.reproduction_command | contains("${INFERCRANE_API_KEY}"))' \
    "$evidence/elastic-benchmark.log" >/dev/null || {
      echo "benchmark evidence is incomplete or contains failures" >&2
      return 1
    }
  record elastic-inspect ic inspect "$ELASTIC_NAME" --output json
  record elastic-events ic events "$ELASTIC_NAME" --output json
  record elastic-explain-scaling ic explain scaling "$ELASTIC_NAME" --output json
  record elastic-explain-cold-start ic explain cold-start "$ELASTIC_NAME" --output json
  echo "Elastic smoke completed. Full RC qualification still requires the controlled disconnect, restart, drain, and bad-candidate checkpoints in the release acceptance procedure."
}

run_elastic_evidence() {
	require_paid_approval
	[ -n "$requested_spec" ] || { echo "elastic-evidence requires INFERCRANE_ACCEPTANCE_SPEC_FILE" >&2; return 1; }
	start_paid_watchdog "${INFERCRANE_ACCEPTANCE_MAX_PAID_SECONDS:-5400}"
	run_preflight
	ready_timeout=${INFERCRANE_ACCEPTANCE_READY_TIMEOUT_SECONDS:-5400}

	echo "==> durable-deploy-cli-disconnect"
	deploy_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	deploy_started_epoch=$(date +%s)
	submit_elastic_deploy "${ready_timeout}s" "$ELASTIC_NAME-$run_id-create" >"$evidence/durable-deploy-cli.log" 2>&1 &
	client_pid=$!
	operation_id=""
	resource_id=""
	elapsed=0
	while [ "$elapsed" -lt 180 ]; do
		status=$(ic status "$ELASTIC_NAME" --output json 2>/dev/null || true)
		operation_id=$(printf '%s' "$status" | jq -r '.active_operation.id // empty')
		resource_id=$(printf '%s' "$status" | jq -r '.replicas[0].provider_resource_id // empty')
		if [ -n "$operation_id" ] && [ -n "$resource_id" ]; then break; fi
		sleep 1
		elapsed=$((elapsed + 1))
	done
	[ -n "$operation_id" ] && [ -n "$resource_id" ] || { echo "durable provider identity was not persisted" >&2; return 1; }
	kill -INT "$client_pid" 2>/dev/null || true
	set +e
	wait "$client_pid"
	client_status=$?
	set -e
	[ "$client_status" -ne 0 ] || { echo "waiting CLI did not disconnect" >&2; return 1; }
	tail -n 1 "$evidence/durable-deploy-cli.log" | jq -e --arg operation_id "$operation_id" \
		'.error.code == "operation_watch_interrupted" and (.error.message | contains("continues safely in the control plane")) and (.error.remediation | contains($operation_id))' \
		>/dev/null
	jq -n --arg operation_id "$operation_id" --arg provider_resource_id "$resource_id" \
		--arg started_at "$deploy_started_at" --argjson client_exit "$client_status" \
		'{schema_version:1,operation_id:$operation_id,provider_resource_id:$provider_resource_id,started_at:$started_at,client_exit:$client_exit,control_plane_continues:true}' \
		>"$evidence/durable-deploy-cli-disconnect.json"

	compose restart infercrane >/dev/null
	restart_wait=0
	until curl -fsS "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/readyz" >/dev/null 2>&1; do
		restart_wait=$((restart_wait + 1)); [ "$restart_wait" -lt 60 ] || return 1; sleep 1
	done
	wait_operation_status "$operation_id" succeeded "$ready_timeout"
	wait_ready "$ELASTIC_NAME"
	deploy_elapsed=$(( $(date +%s) - deploy_started_epoch ))
	deploy_ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	jq -n --arg started_at "$deploy_started_at" --arg ended_at "$deploy_ended_at" \
		--arg operation_id "$operation_id" --arg provider_resource_id "$resource_id" --argjson elapsed_seconds "$deploy_elapsed" \
		'{schema_version:1,operation:"deploy_to_ready",started_at:$started_at,ended_at:$ended_at,elapsed_seconds:$elapsed_seconds,operation_id:$operation_id,provider_resource_id:$provider_resource_id}' \
		>"$evidence/deploy-to-ready-timing.json"
	capture_provider_inventory ready

	record elastic-inspect ic inspect "$ELASTIC_NAME" --output json
	record elastic-events ic events "$ELASTIC_NAME" --output json
	record elastic-explain-cold-start ic explain cold-start "$ELASTIC_NAME" --output json
	record elastic-buffered-request ic request "$ELASTIC_NAME" --message "InferCrane qualification: reply with exactly READY" --output json
	base_url="http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}"
	record elastic-first-token curl -sS -N -o "$evidence/elastic-first-token.stream" \
		-w '{"http_code":%{http_code},"time_starttransfer_seconds":%{time_starttransfer},"time_total_seconds":%{time_total}}\n' \
		-H 'Authorization: Bearer infercrane-runpod-acceptance-key' -H 'Content-Type: application/json' \
		-d "{\"model\":\"$ELASTIC_NAME\",\"messages\":[{\"role\":\"user\",\"content\":\"Count from one to five.\"}],\"stream\":true,\"max_tokens\":64}" \
		"$base_url/v1/chat/completions"
	jq -e '.http_code >= 200 and .http_code < 300 and .time_starttransfer_seconds > 0 and .time_total_seconds >= .time_starttransfer_seconds' \
		"$evidence/elastic-first-token.log" >/dev/null
	grep -q 'data: \[DONE\]' "$evidence/elastic-first-token.stream"

	benchmark_requests=${INFERCRANE_ACCEPTANCE_BENCHMARK_REQUESTS:-64}
	benchmark_tokens=${INFERCRANE_ACCEPTANCE_BENCHMARK_OUTPUT_TOKENS:-64}
	for mode in streaming buffered; do
		case "$mode" in
			streaming) streaming=true ;;
			buffered) streaming=false ;;
		esac
		for concurrency in 1 8 32; do
			record "elastic-benchmark-$mode-c$concurrency" ic benchmark "$ELASTIC_NAME" --revision active \
				--requests "$benchmark_requests" --concurrency "$concurrency" --output-tokens "$benchmark_tokens" \
				--streaming="$streaming" --random-seed 53 --output json
			jq -e --argjson gpu_count "$GPU_COUNT" --argjson concurrency "$concurrency" --argjson streaming "$streaming" \
				'.gpu_count == $gpu_count and .failed == 0 and .succeeded == .request_count and .request_count > 0 and .workload.concurrency == $concurrency and .workload.streaming == $streaming and .latency_p95_ms != null' \
				"$evidence/elastic-benchmark-$mode-c$concurrency.log" >/dev/null
			if [ "$streaming" = true ]; then
				jq -e '.ttft_p95_ms != null and .tpot_p95_ms != null' \
					"$evidence/elastic-benchmark-$mode-c$concurrency.log" >/dev/null
			fi
		done
	done

	active_before=$(ic status "$ELASTIC_NAME" --output json | tee "$evidence/guard-active-before.json" | jq -r '.deployment.active_revision_id')
	record guard-candidate-create ic rollout create "$ELASTIC_NAME" --file "$requested_spec" --wait \
		--idempotency-key "$ELASTIC_NAME-$run_id-guard-candidate" --output json
	candidate=$(ic status "$ELASTIC_NAME" --output json | jq -r '.deployment.candidate_revision_id // empty')
	[ -n "$candidate" ] || { echo "guard candidate was not persisted" >&2; return 1; }
	record guard-evaluate ic rollout evaluate "$ELASTIC_NAME" --wait \
		--idempotency-key "$ELASTIC_NAME-$run_id-guard-evaluate" --output json
	record guard-inspect ic rollout inspect "$ELASTIC_NAME" --output json
	jq -e '.release_guard_evaluations[0].decision == "REJECT" and ([.release_guard_evaluations[0].reasons[]? | select(.code == "candidate_not_ready")] | length) == 1' \
		"$evidence/guard-inspect.log" >/dev/null
	record guard-active-still-serving ic request "$ELASTIC_NAME" --message "After rejection reply with exactly ACTIVE" --output json
	active_after=$(ic status "$ELASTIC_NAME" --output json | tee "$evidence/guard-active-after.json" | jq -r '.deployment.active_revision_id')
	[ "$active_before" = "$active_after" ] || { echo "active revision changed after Release Guard rejection" >&2; return 1; }
	jq -n --arg decision "REJECT" --arg candidate "$candidate" --arg active_before "$active_before" --arg active_after "$active_after" \
		'{schema_version:1,decision:$decision,candidate_revision_id:$candidate,active_revision_before:$active_before,active_revision_after:$active_after,active_revision_preserved:($active_before == $active_after),post_rejection_request_succeeded:true}' \
		>"$evidence/release-guard-proof.json"
	record guard-reject ic rollout reject "$ELASTIC_NAME" "$candidate" \
		--reason "qualification candidate intentionally has no capacity" --wait \
		--idempotency-key "$ELASTIC_NAME-$run_id-guard-reject" --output json

	echo "==> durable-delete-control-plane-restart"
	deletion=$(ic delete "$ELASTIC_NAME" --yes --idempotency-key "$ELASTIC_NAME-$run_id-delete-restart" --output json)
	printf '%s\n' "$deletion" >"$evidence/delete-restart-submit.json"
	delete_id=$(printf '%s' "$deletion" | jq -r '.operation.id')
	printf '%s' "$deletion" | jq -e '.operation.status == "pending"' >/dev/null
	compose restart infercrane >/dev/null
	restart_wait=0
	until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
		restart_wait=$((restart_wait + 1)); [ "$restart_wait" -lt 60 ] || return 1; sleep 1
	done
	wait_operation_status "$delete_id" succeeded 900
	if ic status "$ELASTIC_NAME" --output json >/dev/null 2>&1; then echo "deployment remained after restarted delete" >&2; return 1; fi
	capture_inventory elastic-evidence-after
	verify_provider_inventory_absent
	echo "Fixed-capacity custom OCI evidence suite completed with zero provider inventory."
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
  record serverless-cold-request ic request "$SERVERLESS_NAME" --message "cold acceptance probe" --output json
  record serverless-warm-request ic request "$SERVERLESS_NAME" --message "warm acceptance probe" --output json
  record serverless-streaming-request ic request "$SERVERLESS_NAME" --message "stream acceptance probe" --stream
  verify_openai_features "$SERVERLESS_NAME" serverless
  printf 'cold, warm, and streaming requests passed\n' >"$evidence/serverless-requests.log"
  record serverless-inspect ic inspect "$SERVERLESS_NAME" --output json
  record serverless-events ic events "$SERVERLESS_NAME" --output json
  record serverless-explain-cold-start ic explain cold-start "$SERVERLESS_NAME" --output json
  wait_serverless_zero "$SERVERLESS_NAME" 900
  record serverless-second-cold-request ic request "$SERVERLESS_NAME" --message "second cold acceptance probe" --output json
  printf 'provider scaled to zero and the second cold request passed\n' >"$evidence/serverless-second-cold.log"
  record serverless-events-after-second-cold ic events "$SERVERLESS_NAME" --output json
  record serverless-explain-second-cold ic explain cold-start "$SERVERLESS_NAME" --output json
  echo "Serverless cold, warm, scale-to-zero, second-cold, and streaming smoke completed."
}

run_elastic_faults() {
  require_paid_approval
  run_preflight

  echo "==> controlled-cli-disconnect"
  INFERCRANE_CONFIG="$config_file" "$cli" --context acceptance deploy "$MODEL" \
    --name "$ELASTIC_NAME" --cloud runpod --gpu "$GPU" --min 1 --max 1 --wait \
    --idempotency-key "$ELASTIC_NAME-create" --output json >"$evidence/disconnected-cli.log" 2>&1 &
  client_pid=$!
  operation_id=""
  elapsed=0
  while [ "$elapsed" -lt 120 ]; do
    status=$(ic status "$ELASTIC_NAME" --output json 2>/dev/null || true)
    operation_id=$(printf '%s' "$status" | jq -r '.active_operation.id // empty')
    resource_id=$(printf '%s' "$status" | jq -r '.replicas[0].provider_resource_id // empty')
    if [ -n "$operation_id" ] && [ -n "$resource_id" ]; then break; fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  [ -n "$operation_id" ] && [ -n "$resource_id" ] || { echo "provisioning identity was not persisted" >&2; return 1; }
  kill -INT "$client_pid" 2>/dev/null || true
  set +e
  wait "$client_pid"
  disconnected_status=$?
  set -e
  [ "$disconnected_status" -ne 0 ] || { echo "waiting CLI did not disconnect" >&2; return 1; }
  grep -F "operation $operation_id continues safely in the control plane" "$evidence/disconnected-cli.log" >/dev/null
  printf '{"operation_id":"%s","provider_resource_id":"%s","client_exit":%s}\n' "$operation_id" "$resource_id" "$disconnected_status" >"$evidence/cli-disconnect.json"

  echo "==> control-plane-restart-during-provisioning"
  before=$(operation_json "$operation_id")
  before_attempt=$(printf '%s' "$before" | jq -r '.attempt')
  compose restart infercrane >/dev/null
  start_wait=0
  until curl -fsS "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/readyz" >/dev/null 2>&1; do
    start_wait=$((start_wait + 1)); [ "$start_wait" -lt 60 ] || return 1; sleep 1
  done
  after=$(operation_json "$operation_id")
  printf '%s\n' "$after" | jq -e --arg id "$operation_id" '.id == $id and (.status != "failed" and .status != "cancelled")' >/dev/null
  current_resource=$(ic status "$ELASTIC_NAME" --output json | jq -r '.replicas[0].provider_resource_id')
  [ "$current_resource" = "$resource_id" ] || { echo "provider identity changed across restart" >&2; return 1; }
  printf '{"operation_id":"%s","attempt_before":%s,"provider_resource_id":"%s"}\n' "$operation_id" "$before_attempt" "$resource_id" >"$evidence/provision-restart.json"
  wait_operation_status "$operation_id" succeeded 1800
  wait_ready "$ELASTIC_NAME"
  status=$(ic status "$ELASTIC_NAME" --output json)
  printf '%s\n' "$status" | jq -e --arg resource "$resource_id" '([.replicas[] | select(.provider_resource_id == $resource and .health == "healthy")] | length) == 1' >/dev/null
  guard_requests=${INFERCRANE_ACCEPTANCE_GUARD_REQUESTS:-100}
  guard_concurrency=${INFERCRANE_ACCEPTANCE_GUARD_CONCURRENCY:-4}
  record fault-active-benchmark ic benchmark "$ELASTIC_NAME" --revision active \
    --requests "$guard_requests" --concurrency "$guard_concurrency" --random-seed 41 --output json

  record healthy-candidate-create ic rollout create "$ELASTIC_NAME" --model "$MODEL" --cloud runpod --gpu "$GPU" --min 1 --max 1 --wait --idempotency-key "$ELASTIC_NAME-healthy-create" --output json
  candidate=$(ic status "$ELASTIC_NAME" --output json | jq -r '.deployment.candidate_revision_id // empty')
  [ -n "$candidate" ] || { echo "healthy candidate was not created" >&2; return 1; }
  record healthy-candidate-provision ic rollout provision "$ELASTIC_NAME" "$candidate" --wait --wait-timeout "${INFERCRANE_ACCEPTANCE_PROVISION_TIMEOUT:-20m}" --idempotency-key "$ELASTIC_NAME-healthy-provision" --output json
  record fault-candidate-benchmark ic benchmark "$ELASTIC_NAME" --revision "$candidate" \
    --requests "$guard_requests" --concurrency "$guard_concurrency" --random-seed 41 --output json
  record healthy-candidate-guard ic rollout evaluate "$ELASTIC_NAME" --wait --idempotency-key "$ELASTIC_NAME-healthy-guard" --output json
  guard=$(ic rollout inspect "$ELASTIC_NAME" --output json)
  printf '%s\n' "$guard" >"$evidence/healthy-candidate-guard-inspect.json"
  printf '%s\n' "$guard" | jq -e '.release_guard_evaluations[0].decision == "ACCEPT"' >/dev/null

  echo "==> generation-safe-active-stream"
  stream_file="$evidence/active-generation-stream.log"
  curl -fsSN --max-time 300 -H 'Authorization: Bearer infercrane-runpod-acceptance-key' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$ELASTIC_NAME\",\"messages\":[{\"role\":\"user\",\"content\":\"Count upward continuously. Output only numbers separated by spaces.\"}],\"stream\":true,\"max_tokens\":2048,\"ignore_eos\":true}" \
    "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/v1/chat/completions" >"$stream_file" 2>&1 &
  stream_pid=$!
  elapsed=0
  until grep -q '^data:' "$stream_file" 2>/dev/null; do
    kill -0 "$stream_pid" 2>/dev/null || { echo "long stream exited before promotion" >&2; return 1; }
    elapsed=$((elapsed + 1)); [ "$elapsed" -lt 60 ] || { kill "$stream_pid" 2>/dev/null || true; return 1; }; sleep 1
  done
  promote=$(ic rollout promote "$ELASTIC_NAME" "$candidate" --idempotency-key "$ELASTIC_NAME-healthy-promote" --output json)
  printf '%s\n' "$promote" >"$evidence/healthy-candidate-promote-submit.json"
  promote_id=$(printf '%s' "$promote" | jq -r '.operation.id')
  wait_operation_error "$promote_id" active_requests_draining 120
  kill -0 "$stream_pid" 2>/dev/null || { echo "stream ended before drain fence was observed" >&2; return 1; }
  old_present=$(ic status "$ELASTIC_NAME" --output json | jq --arg old "$resource_id" '[.replicas[] | select(.provider_resource_id == $old and .lifecycle_state != "deleted")] | length')
  [ "$old_present" -eq 1 ] || { echo "old capacity was deleted during active stream" >&2; return 1; }
  wait "$stream_pid"
  grep -q 'data: \[DONE\]' "$stream_file"
  wait_operation_status "$promote_id" succeeded 900
  promoted=$(ic status "$ELASTIC_NAME" --output json)
  printf '%s\n' "$promoted" >"$evidence/healthy-promotion-final.json"
  printf '%s\n' "$promoted" | jq -e --arg candidate "$candidate" --arg old "$resource_id" '.deployment.active_revision_id == $candidate and ([.replicas[] | select(.provider_resource_id == $old and .lifecycle_state != "deleted")] | length) == 0' >/dev/null

  echo "==> restart-at-provider-delete-boundary"
  deletion=$(ic delete "$ELASTIC_NAME" --yes --idempotency-key "$ELASTIC_NAME-delete-restart" --output json)
  printf '%s\n' "$deletion" >"$evidence/delete-restart-submit.json"
  delete_id=$(printf '%s' "$deletion" | jq -r '.operation.id')
  # Restart immediately after the durable API acknowledgement. Waiting for a
  # transient retry code is racy: on a fast router/provider the entire delete
  # can succeed before the observer samples that intermediate state, causing a
  # false failure despite correct recovery and zero inventory. The submitted
  # operation is still PENDING in the captured response, so this deterministically
  # cuts the client/control-plane process boundary without depending on timing.
  printf '%s\n' "$deletion" | jq -e '.operation.status == "pending"' >/dev/null
  compose restart infercrane >/dev/null
  start_wait=0
  until curl -fsS "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/readyz" >/dev/null 2>&1; do
    start_wait=$((start_wait + 1)); [ "$start_wait" -lt 60 ] || return 1; sleep 1
  done
  wait_operation_status "$delete_id" succeeded 900
  if ic status "$ELASTIC_NAME" --output json >/dev/null 2>&1; then echo "deployment remained after restarted delete" >&2; return 1; fi
  capture_inventory elastic-faults-after
}

run_serverless_faults() {
  require_paid_approval
  [ -n "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ] || { echo "INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID is required" >&2; return 1; }
  export INFERCRANE_ACCEPTANCE_DROP_SERVERLESS_CREATE_RESPONSE=1
  run_preflight
  record serverless-fault-deploy ic deploy "$MODEL" --name "$SERVERLESS_NAME" --compute serverless --cloud runpod --gpu "$GPU" --max 1 --wait --idempotency-key "$SERVERLESS_NAME-lost-create" --output json
  deploy_attempt=$(jq -r '.operation.attempt' "$evidence/serverless-fault-deploy.log")
  [ "$deploy_attempt" -ge 2 ] || { echo "lost create response was not retried" >&2; return 1; }
  docker compose -p "$project" -f "$compose_file" logs runpod-fault-proxy >"$evidence/runpod-fault-proxy.log" 2>&1
  grep -F 'provider create response intentionally lost' "$evidence/runpod-fault-proxy.log" >/dev/null
  wait_ready "$SERVERLESS_NAME"
  endpoint=$(ic status "$SERVERLESS_NAME" --output json | jq -r '.replicas[0].provider_resource_id')
  api_key=$(tr -d '\r\n' <"$RUNPOD_KEY_FILE")
  endpoint_count=$(curl -fsS -H "Authorization: Bearer $api_key" 'https://rest.runpod.io/v1/endpoints?includeWorkers=true' | jq --arg id "$endpoint" '[.[] | select(.id == $id)] | length')
  [ "$endpoint_count" -eq 1 ] || { echo "lost response adoption did not produce exactly one endpoint" >&2; return 1; }
  printf '{"endpoint_id":"%s","operation_attempt":%s,"endpoint_count":%s}\n' "$endpoint" "$deploy_attempt" "$endpoint_count" >"$evidence/lost-create-response-adoption.json"

  echo "==> explicit-serverless-stream-cancellation"
  set +e
  curl -fsSN --max-time 2 -H 'Authorization: Bearer infercrane-runpod-acceptance-key' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$SERVERLESS_NAME\",\"messages\":[{\"role\":\"user\",\"content\":\"Write continuously.\"}],\"stream\":true,\"max_tokens\":2048,\"ignore_eos\":true}" \
    "http://127.0.0.1:${INFERCRANE_ACCEPTANCE_PORT:-18001}/v1/chat/completions" >"$evidence/serverless-cancelled-stream.log" 2>&1
  cancel_status=$?
  set -e
  [ "$cancel_status" -ne 0 ] || { echo "serverless stream completed before cancellation" >&2; return 1; }
  elapsed=0
  cancelled=0
  while [ "$elapsed" -lt 30 ]; do
    cancelled=$(compose exec -T postgres psql -U infercrane -d infercrane_acceptance -Atc "SELECT COUNT(*) FROM request_records WHERE deployment_id=(SELECT id FROM deployments WHERE name='$SERVERLESS_NAME') AND error_type='client_cancelled'" | tr -d '[:space:]')
    [ "${cancelled:-0}" -gt 0 ] && break
    sleep 1; elapsed=$((elapsed + 1))
  done
  [ "${cancelled:-0}" -gt 0 ] || { echo "client cancellation was not durably recorded" >&2; return 1; }
  printf '{"curl_exit":%s,"persisted_client_cancelled":%s}\n' "$cancel_status" "$cancelled" >"$evidence/serverless-stream-cancellation.json"
  delete_if_present "$SERVERLESS_NAME" "$SERVERLESS_NAME-delete"
  capture_inventory serverless-faults-after
}

run_elastic_qualify() {
  require_paid_approval
  run_elastic

  scale_up_timeout=${INFERCRANE_ACCEPTANCE_SCALE_UP_TIMEOUT_SECONDS:-2700}
  scale_down_timeout=${INFERCRANE_ACCEPTANCE_SCALE_DOWN_TIMEOUT_SECONDS:-900}

  # The benchmark supplies bounded real queue pressure. Autoscaling must prove
  # both provider convergence to two replicas and the idle return to one.
  # Observe scaling while pressure is active. A short fixed-token benchmark can
  # finish before two autoscaler intervals on faster hardware, producing no
  # queue signal even though clients time out. Long bounded generations keep
  # the workload present without relying on a particular GPU's throughput.
  autoscale_requests=${INFERCRANE_ACCEPTANCE_AUTOSCALE_REQUESTS:-4000}
  autoscale_concurrency=${INFERCRANE_ACCEPTANCE_AUTOSCALE_CONCURRENCY:-800}
  autoscale_output_tokens=${INFERCRANE_ACCEPTANCE_AUTOSCALE_OUTPUT_TOKENS:-1024}
  record elastic-autoscale-queue-load ic benchmark "$ELASTIC_NAME" --revision active \
    --requests "$autoscale_requests" --concurrency "$autoscale_concurrency" \
    --output-tokens "$autoscale_output_tokens" --random-seed 29 --output json &
  load_pid=$!
  if ! wait_replica_count "$ELASTIC_NAME" 2 "$scale_up_timeout"; then
    kill "$load_pid" 2>/dev/null || true
    wait "$load_pid" 2>/dev/null || true
    return 1
  fi
  if ! wait "$load_pid"; then
    echo "autoscaling load generator failed" >&2
    return 1
  fi
  jq -e '.request_count > 0 and .failed < .request_count' "$evidence/elastic-autoscale-queue-load.log" >/dev/null || {
    echo "autoscaling load produced no successful inference requests" >&2
    return 1
  }
  record elastic-explain-scale-up ic explain scaling "$ELASTIC_NAME" --output json
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
  echo "Elastic benchmark, autoscaling, Release Guard, and cleanup qualification completed."
}

run_qualify() {
  run_elastic_qualify
  run_serverless
  echo "Qualification smoke completed; guarded cleanup will now delete the serverless endpoint and verify InferCrane inventory."
}

run_cleanup() {
  load_run
  need docker; need go; need curl; need jq
  ensure_stack
  delete_if_present "$ELASTIC_NAME" "$ELASTIC_NAME-$run_id-delete"
  delete_if_present "$SERVERLESS_NAME" "$SERVERLESS_NAME-$run_id-delete"
  capture_inventory after-cleanup
  verify_provider_inventory_absent
  echo "==> local-control-plane (stopping Docker services)"
  if ! compose down >>"$evidence/local-control-plane.log" 2>&1; then
    echo "local control-plane shutdown failed; inspect $evidence/local-control-plane.log" >&2
    return 1
  fi
  echo "<== local-control-plane (stopped)"
  echo "InferCrane cleanup completed and direct RunPod inventory is empty."
}

run_report() {
  load_run
  commit=${INFERCRANE_ACCEPTANCE_EVIDENCE_COMMIT:-${CANDIDATE_COMMIT:-$(git -C "$root" rev-parse HEAD)}}
  case "$commit" in *[!0-9a-f]*) echo "invalid acceptance evidence commit" >&2; exit 1;; esac
  {
    echo "# InferCrane acceptance run $RUN_ID"
    echo
    echo "- Commit: \`$commit\`"
    echo "- Model: \`$MODEL\`"
    echo "- GPU: \`$GPU\`"
    echo "- Elastic deployment: \`$ELASTIC_NAME\`"
    echo "- Serverless deployment: \`$SERVERLESS_NAME\`"
    echo
    if [ -f "$run_dir/suite-result.json" ] && jq -e \
      '.schema_version == 1 and (.command | type == "string") and (.outcome == "running" or .outcome == "passed" or .outcome == "failed") and (.exit_code | type == "number")' \
      "$run_dir/suite-result.json" >/dev/null 2>&1; then
      suite_command=$(jq -r '.command' "$run_dir/suite-result.json")
      suite_outcome=$(jq -r '.outcome | ascii_upcase' "$run_dir/suite-result.json")
      suite_exit=$(jq -r '.exit_code' "$run_dir/suite-result.json")
      echo "Suite outcome: **$suite_outcome** — \`$suite_command\` (exit $suite_exit)"
    else
      echo "Suite outcome: **INCOMPLETE** — no terminal suite result was recorded"
    fi
    echo
    echo "## Evidence files"
    find "$evidence" -maxdepth 1 -type f -print | sort | sed 's#^.*/#- #'
    echo
    if [ -f "$evidence/provider-direct-after-cleanup.json" ] && \
      jq -e '.pods == [] and .endpoints == []' "$evidence/provider-direct-after-cleanup.json" >/dev/null 2>&1; then
      echo "Provider inventory confirmation: VERIFIED — zero InferCrane pods and endpoints"
    else
      echo "Provider inventory confirmation: PENDING OPERATOR CHECK"
    fi
  } >"$run_dir/report.md"
  echo "$run_dir/report.md"
}

case "$command_name" in
  local) run_local ;;
  preflight) run_preflight ;;
  elastic|elastic-evidence|elastic-qualify|serverless|elastic-faults|serverless-faults)
    require_paid_approval
    # Suite bookkeeping and the EXIT recovery trap both require the durable
    # run paths. Initialize them before either can execute. Individual suites
    # load the same state again during preflight, which is intentionally
    # idempotent and validates any requested model/GPU overrides.
    load_run
    acquire_paid_lock
    write_suite_result "$command_name" running 0
		trap 'exit 124' HUP INT TERM
    trap 'result=$?; trap - EXIT; stop_paid_watchdog; if [ "$result" -ne 0 ]; then write_suite_result "$command_name" failed "$result" || true; echo "acceptance failed; preserving provider inventory before guarded cleanup" >&2; capture_provider_inventory failure || true; run_cleanup || true; else write_suite_result "$command_name" passed 0 || result=$?; fi; release_paid_lock; exit "$result"' EXIT
    case "$command_name" in
      elastic) run_elastic ;;
			elastic-evidence) run_elastic_evidence ;;
      elastic-qualify) run_elastic_qualify ;;
      serverless) run_serverless ;;
      elastic-faults) run_elastic_faults ;;
      serverless-faults) run_serverless_faults ;;
    esac
    write_suite_result "$command_name" passed 0
		stop_paid_watchdog
    trap - EXIT
		trap - HUP INT TERM
    release_paid_lock
    ;;
  qualify)
    # Refuse before installing the cleanup wrapper so a missing approval cannot
    # start even the local acceptance stack.
    require_paid_approval
    load_run
    acquire_paid_lock
    write_suite_result "$command_name" running 0
    trap 'result=$?; trap - EXIT; write_suite_result "$command_name" failed "$result" || true; echo "qualification failed; preserving provider inventory before guarded cleanup" >&2; capture_provider_inventory failure || true; run_cleanup || true; release_paid_lock; exit "$result"' EXIT
    run_qualify
    trap - EXIT
    run_cleanup
    write_suite_result "$command_name" passed 0
    release_paid_lock
    ;;
  cleanup) run_cleanup ;;
  report) run_report ;;
  *) usage >&2; exit 2 ;;
esac
