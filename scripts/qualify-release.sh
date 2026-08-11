#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mode=${1:-local}
approval=false
shift || true
while [ "$#" -gt 0 ]; do
  case "$1" in
    --approve-paid-resources) approval=true ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
case "$mode" in local|rc|report) ;; *) echo "usage: $0 local|rc|report [--approve-paid-resources]" >&2; exit 2;; esac

commit=$(git -C "$root" rev-parse HEAD)
short=$(git -C "$root" rev-parse --short HEAD)
worktree_clean=true
[ -z "$(git -C "$root" status --porcelain)" ] || worktree_clean=false
default_qualification_root="$root/.infercrane/qualification/$commit"
if [ "$worktree_clean" = false ]; then
  default_qualification_root="$default_qualification_root-dirty-$(date -u +%Y%m%dT%H%M%SZ)-$$"
fi
qualification_root=${INFERCRANE_QUALIFICATION_STATE_DIR:-"$default_qualification_root"}
mkdir -p "$qualification_root/stages"

step() {
  name=$1
  shift
  marker="$qualification_root/stages/$name.passed"
  log="$qualification_root/stages/$name.log"
  if [ "$worktree_clean" = true ] && [ -f "$marker" ] && [ "$(cat "$marker")" = "$commit" ]; then
    echo "==> $name (already passed for $short)"
    return 0
  fi
  started=$(date +%s)
  echo "==> $name"
  if "$@" >"$log" 2>&1; then
    printf '%s\n' "$commit" >"$marker"
    echo "<== $name (passed, $(( $(date +%s) - started ))s)"
    return 0
  else
    status=$?
    echo "<== $name (failed, $(( $(date +%s) - started ))s; log: $log)" >&2
    tail -n 80 "$log" >&2
    return "$status"
  fi
}

write_manifest() {
  stages=$(find "$qualification_root/stages" -name '*.passed' -type f -exec basename {} .passed \; | sort | jq -Rsc 'split("\n") | map(select(length > 0))')
  jq -n --arg commit "$commit" --arg mode "$mode" --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson worktree_clean "$worktree_clean" \
    --argjson stages "$stages" \
    '{schema_version:1,commit:$commit,worktree_clean:$worktree_clean,mode:$mode,generated_at:$generated_at,passed_stages:$stages}' \
    >"$qualification_root/qualification.json"
  echo "$qualification_root/qualification.json"
}

run_local_gates() {
  step developer-environment make -C "$root" dev-check-full
  step dead-code make -C "$root" deadcode
  step vulnerability-audit make -C "$root" audit
  step release-package make -C "$root" snapshot
}

run_provider_stage() {
  stage=$1
  command=$2
  run_file="$qualification_root/stages/$stage.run_id"
  if [ -s "$run_file" ]; then
    run_id=$(cat "$run_file")
  else
    run_id="$(date -u +%Y%m%dT%H%M%SZ)-$short-$stage"
    printf '%s\n' "$run_id" >"$run_file"
  fi
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" "$root/scripts/release-acceptance.sh" "$command" --approve-paid-resources
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" "$root/scripts/release-acceptance.sh" cleanup
  INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" "$root/scripts/release-acceptance.sh" report
}

if [ "$mode" = report ]; then
  write_manifest
  exit 0
fi

run_local_gates

if [ "$mode" = rc ]; then
  [ "$approval" = true ] || { echo "rc qualification requires --approve-paid-resources" >&2; exit 1; }
  [ "$worktree_clean" = true ] || { echo "rc qualification requires a clean worktree" >&2; exit 1; }
  step elastic-qualification run_provider_stage elastic-qualification elastic-qualify
  step serverless-qualification run_provider_stage serverless-qualification serverless
  step elastic-faults run_provider_stage elastic-faults elastic-faults
  step serverless-faults run_provider_stage serverless-faults serverless-faults
fi

write_manifest
