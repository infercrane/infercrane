#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command_name=${1:-run}
case "$command_name" in
  run|report) ;;
  *) echo "usage: $0 run|report" >&2; exit 2 ;;
esac

commit=$(git -C "$root" rev-parse HEAD)
short=$(git -C "$root" rev-parse --short HEAD)
source_state=clean
git -C "$root" diff --quiet --ignore-submodules HEAD -- || source_state=dirty
test -z "$(git -C "$root" ls-files --others --exclude-standard)" || source_state=dirty
source_digest=$(
  {
    git -C "$root" diff --binary HEAD --
    git -C "$root" ls-files --others --exclude-standard | LC_ALL=C sort | while IFS= read -r path; do
      printf 'untracked %s %s\n' "$(git -C "$root" hash-object -- "$path")" "$path"
    done
  } | git hash-object --stdin
)
run_id=${INFERCRANE_USER_LIFECYCLE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$short-$$}
case "$run_id" in *[!A-Za-z0-9_.-]*) echo "invalid lifecycle run ID" >&2; exit 2;; esac

state_root=${INFERCRANE_USER_LIFECYCLE_STATE_DIR:-$root/.infercrane/user-lifecycle}
run_dir=$state_root/$run_id
stage_dir=$run_dir/stages
mkdir -p "$stage_dir"

stage() {
  name=$1
  shift
  log=$stage_dir/$name.log
  marker=$stage_dir/$name.passed
  rm -f "$marker"
  started=$(date +%s)
  echo "==> $name"
  # A function invoked directly as an `if` condition inherits a POSIX shell
  # context where `set -e` is ignored for every command inside that function.
  # Run the stage in a non-conditional subshell with errexit explicitly enabled
  # so a failed assertion can never be followed by a green marker.
  set +e
  (set -e; "$@") >"$log" 2>&1
  status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    printf '%s\n' "$commit" >"$marker"
    echo "<== $name (passed, $(( $(date +%s) - started ))s)"
  else
    echo "<== $name (failed, $(( $(date +%s) - started ))s; log: $log)" >&2
    tail -n 120 "$log" >&2
    return "$status"
  fi
}

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

preflight() {
  command -v docker >/dev/null
  command -v go >/dev/null
  command -v curl >/dev/null
  command -v jq >/dev/null
  docker info >/dev/null
  test -f "$root/compose.yaml"
  test -f "$root/compose.acceptance-empty.yaml"
}

offline_install() {
  temporary=$(mktemp -d)
  trap 'rm -rf "$temporary"' EXIT HUP INT TERM
  binary=$temporary/infercrane
  home=$temporary/home
  mkdir -p "$home"
  go build -o "$binary" "$root/cmd/infercrane"
  "$binary" version | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+'
  HOME=$home INFERCRANE_API_KEY=offline-only-credential \
    "$binary" init --context clean-install --url https://control.invalid --skip-check --output json \
    | jq -e '.context == "clean-install" and .credential_stored == true' >/dev/null
  HOME=$home "$binary" context show \
    | grep -q 'https://control.invalid'
  for shell in bash zsh fish powershell; do
    "$binary" completion "$shell" >"$temporary/$shell"
    test -s "$temporary/$shell"
  done
  rm -rf "$temporary"
  trap - EXIT HUP INT TERM
}

empty_to_operated() {
  project=$(printf '%s' "infercrane-lifecycle-$run_id" | tr '[:upper:]' '[:lower:]')
  port=$(free_port)
  temporary=$(mktemp -d)
  binary=$temporary/infercrane
  home=$temporary/home
  mkdir -p "$home"

  compose() {
    INFERCRANE_DEV_PORT=$port docker compose -p "$project" \
      -f "$root/compose.yaml" -f "$root/compose.acceptance-empty.yaml" "$@"
  }
  cleanup() {
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$temporary"
  }
  trap cleanup EXIT HUP INT TERM

  go build -o "$binary" "$root/cmd/infercrane"
  compose up --build -d
  base_url=http://127.0.0.1:$port
  attempt=0
  until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 90 ]; then
      compose logs --tail 150 infercrane >&2
      return 1
    fi
    sleep 1
  done

  HOME=$home INFERCRANE_API_KEY=infercrane "$binary" init \
    --context lifecycle --url "$base_url" --output json \
    | jq -e '.context == "lifecycle" and .credential_stored == true' >/dev/null
  cli() {
    HOME=$home INFERCRANE_API_KEY=infercrane "$binary" --context lifecycle "$@"
  }

  # A real empty database must remain distinguishable from measured zero traffic.
  echo "checkpoint=empty-database"
  cli deployments --output json | jq -e '.data == []' >/dev/null
  cli endpoint list --output json | jq -e '. == []' >/dev/null
  curl -fsS -H 'Authorization: Bearer infercrane' "$base_url/api/v1/operations" \
    | jq -e '.data == []' >/dev/null

  # Register existing capacity explicitly, then create one durable deployment.
  echo "checkpoint=deployment"
  cli target add gpu-a --url http://worker-a:8101 --runtime vllm \
    --upstream-model Qwen/Qwen3-8B --output json >/dev/null
  cli target add gpu-b --url http://worker-b:8102 --runtime vllm \
    --upstream-model Qwen/Qwen3-8B --output json >/dev/null
  cli deploy Qwen/Qwen3-8B --name support-production --targets gpu-a,gpu-b \
    --idempotency-key lifecycle-deploy --wait --output json >"$run_dir/deploy.json"
  cli status support-production --output json \
    | jq -e '.deployment.name == "support-production" and .deployment.active_revision_id != null' >/dev/null
  # Immediate safe retry must converge on the same logical deployment rather than duplicate it.
  cli deploy Qwen/Qwen3-8B --name support-production --targets gpu-a,gpu-b \
    --idempotency-key lifecycle-deploy --wait --output json >/dev/null
  cli deployments --output json \
    | jq -e '[.data[] | select(.name == "support-production")] | length == 1' >/dev/null

  # Build the stable application identity explicitly through public CLI contracts.
  echo "checkpoint=stable-endpoint"
  cli logical-model create support --description 'Customer support inference' --output json >/dev/null
  cli endpoint create support-api --model support --environment production --output json >/dev/null
  cli endpoint bind support-api --name primary --deployment support-production \
    --ownership lifecycle-managed --output json >/dev/null
  cli endpoint plan support-api --policy manual --bindings primary --output json \
    | jq -e '.slot == "active" and .plan.id != null' >/dev/null

  headers=$temporary/headers
  body=$temporary/body
  attempt=0
  until curl -fsS -D "$headers" -o "$body" -H 'Authorization: Bearer infercrane' \
      -H 'Content-Type: application/json' \
      -d '{"model":"support-api","messages":[{"role":"user","content":"Summarize the incident."}]}' \
      "$base_url/v1/chat/completions"; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 30 || { echo "stable endpoint did not become routable" >&2; return 1; }
    sleep 1
  done
  jq -e '.choices[0].message.content != null' "$body" >/dev/null
  request_id=$(awk 'BEGIN{IGNORECASE=1} /^X-Request-Id:/ {gsub("\r", "", $2); print $2}' "$headers")
  test -n "$request_id"
  cli request inspect "$request_id" --output json \
    | jq -e '.endpoint == "support-api" and .deployment == "support-production" and .content_recorded == false' >/dev/null
  cli doctor support-api --output json | jq -e 'type == "array" and length > 0' >/dev/null

  echo "checkpoint=streaming"
  stream=$temporary/stream
  attempt=0
  until curl -fsS -N -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
      -d '{"model":"support-api","stream":true,"messages":[{"role":"user","content":"Stream a short response."}]}' \
      "$base_url/v1/chat/completions" >"$stream"; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 10 || { echo "stable endpoint did not stream" >&2; return 1; }
    sleep 1
  done
  grep -q '^data: {' "$stream"
  grep -q '^data: \[DONE\]' "$stream"

  echo "checkpoint=monitoring-and-policy"
  monitoring_status=$(curl -sS -o "$run_dir/monitoring.json" -w '%{http_code}' \
    -H 'Authorization: Bearer infercrane' \
    "$base_url/api/v1/endpoints/support-api/monitoring?window_seconds=3600&bucket_seconds=60")
  test "$monitoring_status" = 200 || {
    echo "monitoring returned HTTP $monitoring_status" >&2
    cat "$run_dir/monitoring.json" >&2
    return 1
  }
  jq -e '.summary.requests >= 2 and .evidence.fresh == true and .evidence.content_recorded == false' \
    "$run_dir/monitoring.json" >/dev/null
  cli admission set support-api --max-concurrency 2 --max-queue 3 --queue-timeout-ms 250 \
    --max-output-tokens 128 --priorities normal,high --output json \
    | jq -e '.policy.max_concurrency == 2 and .policy.max_queue_depth == 3' >/dev/null
  cli slo set support-production --ttft-p95 500 --latency-p95 2000 --error-rate 0.05 \
    --output json >/dev/null
  cli replay support-production --window 1h --output json \
    | jq -e '.shape_digest != null and .summary.requests >= 2' >/dev/null
  cli recommend support-production --output json \
    | jq -e '.status == "unknown" and (.input_snapshot.evidence | type == "array")' >/dev/null

  # Connect without migration, prove observe-only does not route, then transfer traffic explicitly.
  echo "checkpoint=adoption"
  cli adopt endpoint adopted-api --url http://worker-a:8101/v1 --model adopted-model \
    --upstream-model Qwen/Qwen3-8B --runtime vllm --ownership observe-only --output json >/dev/null
  observe_status=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer infercrane' \
    -H 'Content-Type: application/json' \
    -d '{"model":"adopted-api","messages":[{"role":"user","content":"must not route"}]}' \
    "$base_url/v1/chat/completions")
  test "$observe_status" = 404
  cli adopt promote adopted-api --ownership traffic-managed --output json \
    | jq -e '.adoption.ownership_mode == "traffic-managed"' >/dev/null
  curl -fsS -H 'Authorization: Bearer infercrane' -H 'Content-Type: application/json' \
    -d '{"model":"adopted-api","messages":[{"role":"user","content":"now routed"}]}' \
    "$base_url/v1/chat/completions" >/dev/null

  # A deliberately unready candidate must be rejected without changing active traffic.
  echo "checkpoint=release-guard"
  active=$(cli status support-production --output json | jq -r '.deployment.active_revision_id')
  cli rollout create support-production --model Qwen/Qwen3-8B --cloud runpod --gpu L40S \
    --min 1 --max 1 --wait --idempotency-key lifecycle-bad-candidate --output json >/dev/null
  candidate=$(cli status support-production --output json | jq -r '.deployment.candidate_revision_id')
  test -n "$candidate" && test "$candidate" != null
  cli rollout evaluate support-production --wait --idempotency-key lifecycle-guard --output json >/dev/null
  cli rollout inspect support-production --output json \
    | jq -e --arg active "$active" --arg candidate "$candidate" \
      '.active_revision_id == $active and .candidate_revision_id == $candidate and .release_guard_evaluations[0].decision == "REJECT"' >/dev/null
  cli rollout reject support-production "$candidate" --reason 'qualification candidate intentionally unready' \
    --wait --idempotency-key lifecycle-reject --output json >/dev/null
  cli status support-production --output json \
    | jq -e --arg active "$active" '.deployment.active_revision_id == $active and ((.deployment.candidate_revision_id // "") == "")' >/dev/null

  # Destructive operations are plan-first and finish with no user-owned logical resources.
  echo "checkpoint=cleanup"
  cli endpoint delete adopted-api --yes --output json >/dev/null
  cli endpoint delete support-api --yes --output json >/dev/null
  # Deployment compatibility creates a same-name stable alias. Delete that
  # application identity explicitly rather than confusing deployment cleanup
  # with endpoint lifecycle ownership.
  cli endpoint delete support-production --yes --output json >/dev/null
  cli delete support-production --plan --output json | jq -e '.deployment == "support-production"' >/dev/null
  cli delete support-production --yes --wait --idempotency-key lifecycle-delete --output json >/dev/null
  cli deployments --output json >"$run_dir/final-deployments.json"
  cli endpoint list --output json >"$run_dir/final-endpoints.json"
  cli orphans --output json >"$run_dir/final-orphans.json"
  jq -e '.data == []' "$run_dir/final-deployments.json" >/dev/null
  jq -e 'all(.[]; .desired_state == "deleted")' "$run_dir/final-endpoints.json" >/dev/null
  jq -e '.data == []' "$run_dir/final-orphans.json" >/dev/null

  jq -n --arg request_id "$request_id" --arg active_revision "$active" \
    --arg commit "$commit" --arg source_state "$source_state" --arg source_digest "$source_digest" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{schema_version:1,generated_at:$generated_at,commit:$commit,source_state:$source_state,source_digest:$source_digest,request_id:$request_id,active_revision:$active_revision,proofs:["empty_database","clean_cli_context","idempotent_deploy","stable_endpoint","buffered_request","streaming_request","request_inspector","doctor","monitoring","admission","slo","replay","recommendation_fail_closed","observe_only_adoption","explicit_traffic_transfer","release_guard_rejection","plan_first_delete","zero_logical_resources","zero_orphans"]}' \
    >"$run_dir/lifecycle.json"

  cleanup
  trap - EXIT HUP INT TERM
}

write_report() {
  passed=$(find "$stage_dir" -name '*.passed' -type f -exec basename {} .passed \; | sort | jq -Rsc 'split("\n") | map(select(length > 0))')
  failed=$(find "$stage_dir" -name '*.log' -type f | while read -r log; do marker=${log%.log}.passed; test -f "$marker" || basename "$log" .log; done | sort | jq -Rsc 'split("\n") | map(select(length > 0))')
  jq -n --arg run_id "$run_id" --arg commit "$commit" \
    --arg source_state "$source_state" --arg source_digest "$source_digest" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson passed_stages "$passed" --argjson failed_stages "$failed" \
    '{schema_version:1,run_id:$run_id,commit:$commit,source_state:$source_state,source_digest:$source_digest,generated_at:$generated_at,passed_stages:$passed_stages,failed_stages:$failed_stages,verdict:(if ($failed_stages|length)==0 and ($passed_stages|index("empty-to-operated"))!=null then "LOCAL_USER_LIFECYCLE_QUALIFIED" else "INCOMPLETE" end),scope:"clean install plus empty PostgreSQL-backed local lifecycle; real provider, GPU, hosted identity, and human UX evidence excluded"}' \
    >"$run_dir/report.json"
  jq . "$run_dir/report.json"
  echo "evidence=$run_dir"
}

case "$command_name" in
  run)
    stage preflight preflight
    stage offline-install offline_install
    stage empty-to-operated empty_to_operated
    write_report
    ;;
  report) write_report ;;
esac
