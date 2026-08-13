#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command_name=${1:-local}
shift || true

case "$command_name" in
  local|offline|first-value|adoption|reliability|release|report|list) ;;
  *)
    echo "usage: $0 local|offline|first-value|adoption|reliability|release|report|list" >&2
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
  cli dashboard --output json | jq -e '.url | endswith("/dashboard/")' >/dev/null || return
  cli explain qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cli explain scaling qwen-prod --output json | jq -e '.data | type == "array"' >/dev/null || return
  cli explain rollout qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cli explain cold-start qwen-prod --output json | jq -e '.deployment == "qwen-prod"' >/dev/null || return
  cleanup
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
  INFERCRANE_RELEASE_CANDIDATE_TAG=${INFERCRANE_RELEASE_CANDIDATE_TAG:-v2.0.0-rc.2} \
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
    printf '%s\n' offline first-value adoption reliability release
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
    rm -f "$stage_dir/offline.passed" "$stage_dir/first-value.passed" "$stage_dir/adoption.passed" "$stage_dir/reliability.passed" "$stage_dir/release.passed"
    stage offline offline
    stage docker-preflight docker_preflight
    stage first-value first_value
    stage adoption adoption
    stage reliability reliability
    stage release release_check
    write_report
    ;;
  report)
    write_report
    ;;
esac
