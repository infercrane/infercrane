#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command_name=${1:-local}
shift || true

case "$command_name" in
  local|offline|first-value|modules|adoption|reliability|release|report|list) ;;
  *)
    echo "usage: $0 local|offline|first-value|modules|adoption|reliability|release|report|list" >&2
    exit 2
    ;;
esac

commit=$(git -C "$root" rev-parse HEAD)
short=$(git -C "$root" rev-parse --short HEAD)
run_id=${INFERCRANE_PRODUCT_ACCEPTANCE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$short-$$}
case "$run_id" in *[!A-Za-z0-9_.-]*) echo "invalid product acceptance run ID" >&2; exit 2;; esac

state_root=${INFERCRANE_PRODUCT_ACCEPTANCE_STATE_DIR:-$root/.infercrane/product-acceptance}
run_dir=$state_root/$run_id
stage_dir=$run_dir/stages
mkdir -p "$stage_dir"

worktree_clean=true
[ -z "$(git -C "$root" status --porcelain)" ] || worktree_clean=false

stage() {
  name=$1
  shift
  log=$stage_dir/$name.log
  marker=$stage_dir/$name.passed
  rm -f "$marker"
  started=$(date +%s)
  echo "==> $name"
  if "$@" >"$log" 2>&1; then
    printf '%s\n' "$commit" >"$marker"
    echo "<== $name (passed, $(( $(date +%s) - started ))s)"
  else
    status=$?
    echo "<== $name (failed, $(( $(date +%s) - started ))s; log: $log)" >&2
    tail -n 100 "$log" >&2
    return "$status"
  fi
}

docker_preflight() {
  docker version >/dev/null || return
  docker info >/dev/null || return
  available_kb=$(df -Pk "$root" | awk 'NR==2 {print $4}') || return
  [ "${available_kb:-0}" -ge 15728640 ] || {
    echo "product acceptance requires at least 15 GiB free disk; 30 GiB is recommended" >&2
    return 1
  }
}

acceptance_port() {
  if [ -n "${INFERCRANE_PRODUCT_ACCEPTANCE_PORT:-}" ]; then
    printf '%s\n' "$INFERCRANE_PRODUCT_ACCEPTANCE_PORT"
    return
  fi
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

offline() {
  offline_dir=$(mktemp -d) || return
  cleanup_offline() { rm -rf "$offline_dir"; }
  trap cleanup_offline EXIT HUP INT TERM
  binary=$offline_dir/infercrane
  config_home=$offline_dir/home
  mkdir -p "$config_home" || return

  go build -o "$binary" "$root/cmd/infercrane" || return
  "$binary" version | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+' || return
  "$binary" --help | grep -q 'InferCrane operates durable' || return
  for shell in bash zsh fish powershell; do
    "$binary" completion "$shell" >"$offline_dir/completion-$shell" || return
    [ -s "$offline_dir/completion-$shell" ] || return
  done

  HOME=$config_home INFERCRANE_API_KEY=offline-acceptance-credential \
    "$binary" init --context acceptance --url https://control.invalid --skip-check --output json \
    >"$offline_dir/init.json" || return
  jq -e '.context == "acceptance" and .control_url == "https://control.invalid" and .credential_stored == true' \
    "$offline_dir/init.json" >/dev/null || return
  HOME=$config_home "$binary" context list | grep -q 'acceptance' || return
  HOME=$config_home "$binary" context show | grep -q 'https://control.invalid' || return

  key_file=$offline_dir/passport-signing-key
  "$binary" passport keygen --file "$key_file" >/dev/null || return
  [ "$(stat -f '%Lp' "$key_file" 2>/dev/null || stat -c '%a' "$key_file")" = 600 ] || return

  cleanup_offline
  trap - EXIT HUP INT TERM
}

first_value() {
  project=$(printf '%s' "infercrane-product-$run_id" | tr '[:upper:]' '[:lower:]')
  port=$(acceptance_port) || return
  cleanup() { INFERCRANE_DEV_PORT=$port docker compose -p "$project" down --volumes --remove-orphans >/dev/null 2>&1 || true; }
  trap cleanup EXIT HUP INT TERM
  COMPOSE_PROJECT_NAME=$project INFERCRANE_DEV_PORT=$port INFERCRANE_SMOKE_URL=http://127.0.0.1:$port \
    "$root/scripts/test-stack.sh" || return

  cli() {
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" exec -T \
      -e INFERCRANE_URL=http://127.0.0.1:8080 \
      -e INFERCRANE_API_KEY=infercrane infercrane infercrane "$@"
  }
  cli version || return
  cli auth status --output json | jq -e '.principal.role != null and .principal.tenant_id != null' >/dev/null || return
  cli deployments --output json | jq -e '.data | type == "array" and length > 0' >/dev/null || return
  cli status qwen-prod --output json | jq -e '.deployment.name == "qwen-prod"' >/dev/null || return
  cli events qwen-prod --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli inspect qwen-prod --output json | jq -e '.deployment.name == "qwen-prod"' >/dev/null || return
  cli integrations --output json | jq -e '.data.provider_contract != null and .data.runtime_contract != null' >/dev/null || return
  cli explain qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cli explain scaling qwen-prod --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli explain rollout qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cli explain cold-start qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cleanup
  trap - EXIT HUP INT TERM
}

modules() {
  project=$(printf '%s' "infercrane-modules-$run_id" | tr '[:upper:]' '[:lower:]')
  port=$(acceptance_port) || return
  async_key=01234567890123456789012345678901
  cleanup_modules() {
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" down --volumes --remove-orphans >/dev/null 2>&1 || true
  }
  trap cleanup_modules EXIT HUP INT TERM
  COMPOSE_PROJECT_NAME=$project INFERCRANE_DEV_PORT=$port INFERCRANE_SMOKE_URL=http://127.0.0.1:$port \
    INFERCRANE_ASYNC_ENCRYPTION_KEY=$async_key "$root/scripts/test-stack.sh" || return

  python3 - "$port" <<'PY' || return
import socket
import sys

port = int(sys.argv[1])
with socket.create_connection(("127.0.0.1", port), timeout=5) as connection:
    connection.sendall(b"GET /livez HTTP/1.1\r\nHost: localhost\r\nX-Oversized: ")
    chunk = b"a" * 65536
    for _ in range(17):
        connection.sendall(chunk)
    connection.sendall(b"\r\n\r\n")
    response = connection.recv(4096)
if not response.startswith(b"HTTP/1.1 431"):
    raise SystemExit("oversized public header was not rejected: " + repr(response[:80]))
PY
  curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null || return

  cli() {
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" exec -T \
      -e INFERCRANE_URL=http://127.0.0.1:8080 \
      -e INFERCRANE_API_KEY=infercrane infercrane infercrane "$@"
  }
  suffix=$(printf '%s' "$short-$$" | tr -c 'A-Za-z0-9-' '-' | cut -c1-32)
  logical_model="acceptance-$suffix"
  environment="staging-$suffix"
  endpoint="acceptance-$suffix"

  cli target list --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli secret list --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli orphans --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli recipes no-match --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli capacity --window 1h --output json | jq -e '.capacity | type == "array"' >/dev/null || return
  if cli capacity unexpected-subject >"$run_dir/capacity-invalid.out" 2>"$run_dir/capacity-invalid.err"; then
    echo "capacity accepted an undocumented positional subject" >&2
    return 1
  fi
  grep -q 'usage: infercrane capacity' "$run_dir/capacity-invalid.err" || return

  cli logical-model create "$logical_model" --description 'whole-product acceptance' --output json |
    jq -e --arg name "$logical_model" '.logical_model.name == $name' >/dev/null || return
  cli environment create "$environment" --policy '{"release":"guarded"}' --output json |
    jq -e --arg name "$environment" '.environment.name == $name' >/dev/null || return
  cli endpoint create "$endpoint" --model "$logical_model" --environment "$environment" --output json |
    jq -e --arg name "$endpoint" '.endpoint.name == $name' >/dev/null || return
  cli endpoint bind "$endpoint" --name active --deployment qwen-prod --ownership traffic-managed --output json |
    jq -e '.binding.name == "active" and .binding.ownership_mode == "traffic-managed"' >/dev/null || return
  plan=$(cli endpoint plan "$endpoint" --policy manual --bindings active --output json) || return
  printf '%s\n' "$plan" | jq -e '.plan.id != null and .slot == "active"' >/dev/null || return
  curl -fsS -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"module acceptance\"}]}" \
    "http://127.0.0.1:$port/v1/chat/completions" | jq -e '.choices[0].message.content != null' >/dev/null || return

  cli admission set "$endpoint" --max-concurrency 2 --max-queue 3 --queue-timeout-ms 250 \
    --max-output-tokens 64 --priorities normal,high --output json |
    jq -e '.policy.max_concurrency == 2 and .policy.max_queue_depth == 3 and .policy.request_timeout_ms == 300000' >/dev/null || return
  cli admission get "$endpoint" --output json |
    jq -e '.policy.max_output_tokens == 64 and .policy.request_timeout_ms == 300000' >/dev/null || return

  provider_connection="model-api-$suffix"
  provider=$(cli provider connect "$provider_connection" --adapter openrouter \
    --model openai/gpt-oss-20b --from-env INFERCRANE_EXTERNAL_ACCEPTANCE_KEY --output json) || return
  printf '%s\n' "$provider" |
    jq -e --arg name "$provider_connection" '.connection.name == $name and .connection.adapter == "openrouter"' >/dev/null || return
  secret_id=$(printf '%s\n' "$provider" | jq -r '.connection.secret_reference_id') || return
  cli provider list --output json |
    jq -e --arg name "$provider_connection" 'map(select(.name == $name)) | length == 1' >/dev/null || return
  external_target="provider-$provider_connection"
  cli external configure qwen-prod --target "$external_target" --secret-reference "$secret_id" \
    --request-limit 3 --cost-limit-usd 0.30 --max-request-cost-usd 0.10 \
    --acknowledge-external-data --output json |
    jq -e '.policy.enabled == false and .policy.privacy_acknowledged == true and .policy.request_limit == 3' >/dev/null || return
  cli external inspect qwen-prod --output json | jq -e '.policy.cost_limit_microusd == 300000' >/dev/null || return
  managed_binding="managed-api-$suffix"
  cli endpoint bind "$endpoint" --name "$managed_binding" --connection "$provider_connection" \
    --request-limit 3 --cost-limit-usd 0.30 --max-request-cost-usd 0.10 \
    --acknowledge-external-data --enable-external --output json |
    jq -e --arg name "$managed_binding" '.binding.name == $name and .binding.kind == "external" and .binding.ownership_mode == "traffic-managed" and .binding.config.adapter == "openrouter" and .binding.config.enabled == true' >/dev/null || return
  cli provider delete "$provider_connection" --output json |
    jq -e --arg name "$provider_connection" '.connection == $name and .deleted == true and .existing_bindings_unchanged == true' >/dev/null || return
  cli endpoint inspect "$endpoint" |
    jq -e --arg name "$managed_binding" '.bindings | map(select(.name == $name and .config.adapter == "openrouter")) | length == 1' >/dev/null || return
  cli provider list --output json |
    jq -e --arg name "$provider_connection" 'map(select(.name == $name)) | length == 0' >/dev/null || return
  cli alert configure "$endpoint" --name operations --webhook https://alerts.example.com/infercrane \
    --secret-reference "$secret_id" --minimum-severity warning --output json |
    jq -e '.policy.name == "operations" and .policy.max_attempts == 3' >/dev/null || return
  cli alert list "$endpoint" --output json | jq -e '.data | type == "array" and length == 1' >/dev/null || return

  job=$(printf '%s' "{\"model\":\"$endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"durable async acceptance\"}],\"max_tokens\":16}" |
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" exec -T \
      -e INFERCRANE_URL=http://127.0.0.1:8080 -e INFERCRANE_API_KEY=infercrane \
      infercrane infercrane async submit "$endpoint" --file /dev/stdin \
      --idempotency-key "$suffix-async" --output json) || return
  job_id=$(printf '%s\n' "$job" | jq -r '.job.id') || return
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    current=$(cli async get "$job_id" --output json) || return
    job_status=$(printf '%s\n' "$current" | jq -r '.job.status') || return
    case "$job_status" in
      succeeded) break ;;
      failed|cancelled) printf '%s\n' "$current" >&2; return 1 ;;
    esac
    attempts=$((attempts + 1))
    sleep 1
  done
  printf '%s\n' "$current" | jq -e '.job.status == "succeeded" and .result.choices[0].message.content != null and .job.content_encrypted_at_rest == true' >/dev/null || return
  if printf '%s' '{"messages":[]}' | INFERCRANE_DEV_PORT=$port docker compose -p "$project" exec -T \
      -e INFERCRANE_URL=http://127.0.0.1:8080 -e INFERCRANE_API_KEY=infercrane \
      infercrane infercrane async submit "$endpoint" --file /dev/stdin \
      --idempotency-key "$suffix-invalid-async" --output json >"$run_dir/async-invalid.out" 2>"$run_dir/async-invalid.err"; then
    echo "async submission queued a payload without endpoint model identity" >&2
    return 1
  fi
  grep -q 'invalid_request' "$run_dir/async-invalid.err" || return

  session=$(cli session create qwen-prod --ttl 1h --output json) || return
  session_id=$(printf '%s\n' "$session" | jq -r '.context_passport.id') || return
  printf '%s\n' "$session" | jq -e '.durable_kv == false and .context_passport.status == "active"' >/dev/null || return
  cli session inspect "$session_id" --output json | jq -e --arg id "$session_id" '.context_passport.id == $id' >/dev/null || return
  cli replay qwen-prod --window 1h --output json | jq -e '.shape_digest != null and .summary.requests > 0' >/dev/null || return
  cli finops qwen-prod --window 1h --output json |
    jq -e '(.status == "unavailable") and (.summary.missing | index("sourced_current_cost") != null)' >/dev/null || return
  cli slo set qwen-prod --ttft-p95 500 --latency-p95 2000 --error-rate 0.05 --output json |
    jq -e '.max_ttft_p95_ms == 500 and .max_error_rate == 0.05' >/dev/null || return
  cli slo get qwen-prod --output json | jq -e '.max_latency_p95_ms == 2000' >/dev/null || return
  recommendation=$(cli recommend qwen-prod --output json) || return
  printf '%s\n' "$recommendation" |
    jq -e '(.candidates | type == "array") and (.input_snapshot.evidence | type == "array") and .status == "unknown"' >/dev/null || return
  if cli autopilot plan qwen-prod --output json >"$run_dir/autopilot-unavailable.out" 2>"$run_dir/autopilot-unavailable.err"; then
    echo "autopilot created a plan without an eligible measured recommendation" >&2
    return 1
  fi
  grep -q 'recommendation_required' "$run_dir/autopilot-unavailable.err" || return
  cli burst qwen-prod --queue-depth 9 --breaches 3 --incremental-cost-microusd-hour 5 \
    --max-incremental-cost-microusd-hour 10 --external-healthy --output json |
    jq -e '.decision.action != null and .decision.reason != null' >/dev/null || return
  cli lab nonexistent-model@immutable --output json | jq -e '.evaluation.results | type == "array" and length == 0' >/dev/null || return
  cli doctor "$endpoint" --output json | jq -e 'type == "array" and length > 0' >/dev/null || return

  # Optimization starts as a free, immutable proposal. Persisting and approving
  # a campaign must remain separate from provider mutation and promotion.
  optimization=$(cli optimize create llama-3.1-8b-instruct \
    --provider aws --region eu-central-1 --gpu L40S --source catalog \
    --objective interactive --max-candidates 2 --output json) || return
  printf '%s\n' "$optimization" >"$run_dir/optimization-campaign.json" || return
  campaign_id=$(printf '%s\n' "$optimization" | jq -er \
    '.campaign.id | select(type == "string" and length == 32)') || return
  printf '%s\n' "$optimization" |
    jq -e '.created == true and .campaign.state == "awaiting_approval" and (.campaign.candidates | length) == 2' >/dev/null || return
  cli optimize inspect "$campaign_id" --output json |
    jq -e --arg id "$campaign_id" '.campaign.id == $id and .campaign.state == "awaiting_approval"' >/dev/null || return
  cli optimize approve "$campaign_id" --max-cost-usd 1 --expires-in 10m --output json |
    jq -e '.campaign.state == "approved" and .campaign.approved_max_cost_usd == 1' >/dev/null || return
  cli optimize cancel "$campaign_id" --output json |
    jq -e '.campaign.state == "cancelled" and all(.campaign.candidates[]; .state == "cancelled" and .evidence_state == "stale")' >/dev/null || return
  cli orphans --output json | jq -e '.data | length == 0' >/dev/null || return

  sandbox=$(cli sandbox connect --provider e2b --external-id "acceptance-$suffix" \
    --external-revision template-v1 --endpoint "$endpoint" --ttl 10m --output json) || return
  sandbox_id=$(printf '%s\n' "$sandbox" | jq -r '.reference.id') || return
  sandbox_token=$(printf '%s\n' "$sandbox" | jq -r '.credential') || return
  printf '%s\n' "$sandbox" |
    jq -e --arg endpoint "$endpoint" '.credential_once == true and .credential_cache_synchronized == true and .external_resource_mutated == false and .reference.endpoint == $endpoint' >/dev/null || return
  curl -fsS -H "Authorization: Bearer $sandbox_token" "http://127.0.0.1:$port/v1/models" |
    jq -e --arg endpoint "$endpoint" '.data | length == 1 and .[0].id == $endpoint' >/dev/null || return
  curl -fsS -H "Authorization: Bearer $sandbox_token" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$endpoint\",\"messages\":[{\"role\":\"user\",\"content\":\"sandbox-scoped acceptance\"}]}" \
    "http://127.0.0.1:$port/v1/chat/completions" | jq -e '.choices[0].message.content != null' >/dev/null || return
  denied=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $sandbox_token" -H 'Content-Type: application/json' \
    -d '{"model":"qwen-prod","messages":[{"role":"user","content":"must be denied"}]}' \
    "http://127.0.0.1:$port/v1/chat/completions") || return
  [ "$denied" = 403 ] || { echo "sandbox credential invoked an unscoped endpoint: HTTP $denied" >&2; return 1; }
  rotated=$(cli sandbox rotate "$sandbox_id" --output json) || return
  rotated_token=$(printf '%s\n' "$rotated" | jq -r '.credential') || return
  printf '%s\n' "$rotated" | jq -e '.credential_cache_synchronized == true' >/dev/null || return
  [ "$rotated_token" != "$sandbox_token" ] || return
  expired=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $sandbox_token" "http://127.0.0.1:$port/v1/models") || return
  [ "$expired" = 401 ] || { echo "rotated sandbox credential remained valid: HTTP $expired" >&2; return 1; }
  cli sandbox revoke "$sandbox_id" --yes --output json |
    jq -e '.status == "stopped" and .credential_cache_synchronized == true and .external_resource_mutated == false' >/dev/null || return
  revoked=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $rotated_token" "http://127.0.0.1:$port/v1/models") || return
  [ "$revoked" = 401 ] || { echo "revoked sandbox credential remained valid: HTTP $revoked" >&2; return 1; }

  if cli benchmark qwen-prod --requests 1 --concurrency 1 --output json >"$run_dir/benchmark-unavailable.out" 2>"$run_dir/benchmark-unavailable.err"; then
    echo "benchmark fabricated immutable artifact evidence for an adopted target" >&2
    return 1
  fi
  grep -q 'artifact_unresolved' "$run_dir/benchmark-unavailable.err" || return
  if cli recipe create qwen-prod --name acceptance --version 1.0.0 --output json >"$run_dir/recipe-unavailable.out" 2>"$run_dir/recipe-unavailable.err"; then
    echo "recipe was captured without immutable artifact and measured benchmark evidence" >&2
    return 1
  fi
  grep -q 'immutable_artifact_required' "$run_dir/recipe-unavailable.err" || return

  training_revision=$(cli inspect qwen-prod --output json | jq -r '.deployment.active_revision_id') || return
  training_key="/tmp/infercrane-training-$suffix.key"
  training_file="/tmp/infercrane-training-$suffix.json"
  cli training keygen --file "$training_key" --output json | jq -e '.mode == "0600" and .private_key_exposed == false' >/dev/null || return
  cli training sign qwen-prod "$training_revision" --provider mlflow --run "run-$suffix" \
    --repository "mlflow://registry/qwen-prod/$suffix" --immutable-revision "$suffix" \
    --digest sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    --framework transformers --framework-version 5.0.0 --method lora \
    --key "$training_key" --file "$training_file" --output json |
    jq -e '.content_recorded == false and .evidence_digest != null' >/dev/null || return
  cli training verify "$training_file" --output json |
    jq -e --arg revision "$training_revision" '.verified == true and .revision_id == $revision and .content_recorded == false' >/dev/null || return
  cli training attach qwen-prod "$training_file" --output json |
    jq -e --arg revision "$training_revision" '.handoff.revision_id == $revision and .training_executed_by_infercrane == false' >/dev/null || return
  cli training list qwen-prod --output json |
    jq -e --arg revision "$training_revision" '.data | length == 1 and .[0].revision_id == $revision' >/dev/null || return

  cli endpoint delete "$endpoint" --yes --output json | jq -e '.state == "deleted"' >/dev/null || return
  curl -fsS -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
    -d '{"model":"qwen-prod","messages":[{"role":"user","content":"external lifecycle remains"}]}' \
    "http://127.0.0.1:$port/v1/chat/completions" >/dev/null || return
  if cli secret delete "$secret_id" --yes >"$run_dir/secret-in-use.out" 2>"$run_dir/secret-in-use.err"; then
    echo "secret deletion ignored a live external-policy reference" >&2
    return 1
  fi
  grep -q 'conflict' "$run_dir/secret-in-use.err" || return

  cleanup_modules
  trap - EXIT HUP INT TERM
}

adoption() {
  "$root/scripts/demo-connect.sh"
}

reliability() {
  project=$(printf '%s' "infercrane-recovery-$run_id" | tr '[:upper:]' '[:lower:]')
  port=$(acceptance_port) || return
  cleanup_reliability() {
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" down --volumes --remove-orphans >/dev/null 2>&1 || true
  }
  trap cleanup_reliability EXIT HUP INT TERM

  COMPOSE_PROJECT_NAME=$project INFERCRANE_DEV_PORT=$port INFERCRANE_SMOKE_URL=http://127.0.0.1:$port \
    "$root/scripts/test-stack.sh" || return
  COMPOSE_PROJECT_NAME=$project INFERCRANE_DEV_PORT=$port INFERCRANE_SMOKE_URL=http://127.0.0.1:$port \
    "$root/scripts/test-failure-recovery.sh" || return
  cleanup_reliability
  trap - EXIT HUP INT TERM

  "$root/scripts/test-ha-control-plane.sh" &&
    "$root/scripts/test-backup-restore-safety.sh" &&
    "$root/scripts/test-backup-restore-docker.sh"
}

release_check() {
  INFERCRANE_RELEASE_CANDIDATE_TAG=${INFERCRANE_RELEASE_CANDIDATE_TAG:-v2.0.0-rc.1} \
    "$root/scripts/qualify-release.sh" local
}

write_report() {
  stages=$(find "$stage_dir" -name '*.passed' -type f -exec basename {} .passed \; | sort | jq -Rsc 'split("\n") | map(select(length > 0))')
  failures=$(find "$stage_dir" -name '*.log' -type f | while read -r log; do marker=${log%.log}.passed; [ -f "$marker" ] || basename "$log" .log; done | sort | jq -Rsc 'split("\n") | map(select(length > 0))')
  jq -n \
    --arg run_id "$run_id" \
    --arg commit "$commit" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson worktree_clean "$worktree_clean" \
    --argjson passed_stages "$stages" \
    --argjson failed_stages "$failures" \
    '{schema_version:1,run_id:$run_id,commit:$commit,generated_at:$generated_at,worktree_clean:$worktree_clean,passed_stages:$passed_stages,failed_stages:$failed_stages,scope:"local black-box product acceptance; real provider semantics excluded"}' \
    >"$run_dir/report.json"
  jq . "$run_dir/report.json"
  echo "evidence=$run_dir"
}

case "$command_name" in
  list)
    printf '%s\n' offline first-value modules adoption reliability release
    ;;
  offline)
    rm -f "$stage_dir/offline.passed"
    stage offline offline
    write_report
    ;;
  first-value)
    rm -f "$stage_dir/first-value.passed"
    stage docker-preflight docker_preflight
    stage first-value first_value
    write_report
    ;;
  modules)
    rm -f "$stage_dir/modules.passed"
    stage docker-preflight docker_preflight
    stage modules modules
    write_report
    ;;
  adoption)
    rm -f "$stage_dir/adoption.passed"
    stage docker-preflight docker_preflight
    stage adoption adoption
    write_report
    ;;
  reliability)
    rm -f "$stage_dir/reliability.passed"
    stage docker-preflight docker_preflight
    stage reliability reliability
    write_report
    ;;
  release)
    rm -f "$stage_dir/release.passed"
    stage docker-preflight docker_preflight
    stage release release_check
    write_report
    ;;
  local)
    rm -f "$stage_dir/offline.passed" "$stage_dir/first-value.passed" "$stage_dir/modules.passed" "$stage_dir/adoption.passed" "$stage_dir/reliability.passed" "$stage_dir/release.passed"
    stage offline offline
    stage docker-preflight docker_preflight
    stage first-value first_value
    stage modules modules
    stage adoption adoption
    stage reliability reliability
    stage release release_check
    write_report
    ;;
  report)
    write_report
    ;;
esac
