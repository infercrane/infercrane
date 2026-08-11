#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:-}
shift || true
approval=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --approve-paid-resources) approval=true ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
case "$mode" in preflight|qualify|cleanup|report) ;;
  *) echo "usage: $0 preflight|qualify|cleanup|report [--approve-paid-resources]" >&2; exit 2 ;;
esac

run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:-}
if [ -z "$run_id" ]; then
  run_id="$(date -u +%Y%m%dT%H%M%SZ)-v1"
fi
case "$run_id" in *[!A-Za-z0-9._-]*) echo "acceptance run ID contains unsupported characters" >&2; exit 1;; esac
state_root=${INFERCRANE_V1_ACCEPTANCE_STATE_DIR:-"$root/.infercrane/v1-acceptance"}
state="$state_root/$run_id"
mkdir -p "$state/stages"
commit=$(git -C "$root" rev-parse HEAD)
short=$(git -C "$root" rev-parse --short HEAD)
paid_lock="$state_root/.paid.lock"
lock_owned=false

release_lock() {
  if [ "$lock_owned" = true ]; then
    rm -f "$paid_lock/pid" "$paid_lock/run_id"
    rmdir "$paid_lock" 2>/dev/null || true
  fi
}

acquire_lock() {
  if ! mkdir "$paid_lock" 2>/dev/null; then
    owner=$(cat "$paid_lock/pid" 2>/dev/null || true)
    if [ -n "$owner" ] && kill -0 "$owner" 2>/dev/null; then
      echo "another v1 paid qualification is active (pid $owner)" >&2
      return 1
    fi
    rm -f "$paid_lock/pid" "$paid_lock/run_id"
    rmdir "$paid_lock" 2>/dev/null || return 1
    mkdir "$paid_lock"
  fi
  printf '%s\n' "$$" >"$paid_lock/pid"
  printf '%s\n' "$run_id" >"$paid_lock/run_id"
  lock_owned=true
  trap release_lock EXIT HUP INT TERM
}

stage() {
  name=$1
  shift
  marker="$state/stages/$name.passed"
  log="$state/stages/$name.log"
  if [ -f "$marker" ] && [ "$(cat "$marker")" = "$commit" ]; then
    echo "==> $name (already passed for $short)"
    return 0
  fi
  echo "==> $name"
  if "$@" >"$log" 2>&1; then
    printf '%s\n' "$commit" >"$marker"
    echo "<== $name (passed)"
  else
    status=$?
    echo "<== $name (failed; $log)" >&2
    tail -n 100 "$log" >&2
    return "$status"
  fi
}

require_file() {
  variable=$1
  eval "value=\${$variable:-}"
  [ -n "$value" ] && [ -r "$value" ] || {
    echo "$variable must name a readable file" >&2
    return 1
  }
}

preflight() {
  require_file RUNPOD_KEY_FILE
  [ -n "${INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID:-}" ] || {
    echo "INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID is required" >&2
    return 1
  }
  require_file INFERCRANE_V1_AWS_ENV_FILE
  require_file INFERCRANE_V1_KUBERNETES_ENV_FILE
  require_file INFERCRANE_V1_API_KEY_FILE
  [ -d "${INFERCRANE_V1_AWS_SPEC_DIR:-}" ] || { echo "INFERCRANE_V1_AWS_SPEC_DIR is required" >&2; return 1; }
  [ -d "${INFERCRANE_V1_KUBERNETES_SPEC_DIR:-}" ] || { echo "INFERCRANE_V1_KUBERNETES_SPEC_DIR is required" >&2; return 1; }
  for directory in "$INFERCRANE_V1_AWS_SPEC_DIR" "$INFERCRANE_V1_KUBERNETES_SPEC_DIR"; do
    for spec in vllm.yaml sglang.yaml custom-oci.yaml; do
      [ -r "$directory/$spec" ] || { echo "missing qualification spec: $directory/$spec" >&2; return 1; }
    done
  done
  "$root/scripts/release-acceptance.sh" preflight
  echo "v1 provider configuration and read-only RunPod preflight passed"
}

portable() {
  cloud=$1
  case "$cloud" in
    aws)
      provider_env=$INFERCRANE_V1_AWS_ENV_FILE
      spec_dir=$INFERCRANE_V1_AWS_SPEC_DIR
      ;;
    kubernetes)
      provider_env=$INFERCRANE_V1_KUBERNETES_ENV_FILE
      spec_dir=$INFERCRANE_V1_KUBERNETES_SPEC_DIR
      ;;
  esac
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-$cloud" \
  INFERCRANE_V1_PROVIDER_ENV_FILE="$provider_env" \
  INFERCRANE_V1_SPEC_DIR="$spec_dir" \
  INFERCRANE_V1_API_KEY_FILE="$INFERCRANE_V1_API_KEY_FILE" \
    "$root/scripts/portable-provider-acceptance.sh" "$cloud" --approve-paid-resources
}

write_report() {
  passed=$(
    for marker in "$state"/stages/*.passed; do
      [ -f "$marker" ] || continue
      [ "$(cat "$marker")" = "$commit" ] || continue
      basename "$marker" .passed
    done | sort | jq -Rsc 'split("\n") | map(select(length > 0))'
  )
  jq -n --arg run_id "$run_id" --arg commit "$commit" --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson passed "$passed" \
    '{schema_version:1,run_id:$run_id,commit:$commit,generated_at:$generated_at,passed_stages:$passed,real_infrastructure:(if ($passed|index("runpod")) and ($passed|index("aws")) and ($passed|index("kubernetes")) then "passed" else "incomplete" end)}' \
    >"$state/qualification.json"
  echo "$state/qualification.json"
}

case "$mode" in
  preflight)
    stage local make -C "$root" qualify-local
    stage provider-preflight preflight
    ;;
  qualify)
    [ "$approval" = true ] || { echo "qualification requires --approve-paid-resources" >&2; exit 1; }
    acquire_lock
    [ -z "$(git -C "$root" status --porcelain)" ] || { echo "qualification requires a clean worktree" >&2; exit 1; }
    [ "$(git -C "$root" describe --tags --exact-match HEAD 2>/dev/null || true)" = v1.0.0-rc.1 ] || {
      echo "qualification requires HEAD at local tag v1.0.0-rc.1" >&2
      exit 1
    }
    stage provider-preflight preflight
    stage runpod env INFERCRANE_QUALIFICATION_STATE_DIR="$state/runpod" "$root/scripts/qualify-release.sh" rc --approve-paid-resources
    stage aws portable aws
    stage kubernetes portable kubernetes
    ;;
  cleanup)
    if [ -n "${RUNPOD_KEY_FILE:-}" ]; then
      INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" "$root/scripts/release-acceptance.sh" cleanup || true
    fi
    echo "Portable provider stages perform guarded deletion before stopping their control planes."
    echo "If a stage was interrupted, resume 'qualify' with the same INFERCRANE_ACCEPTANCE_RUN_ID before final inventory sign-off."
    ;;
  report) ;;
esac

write_report
