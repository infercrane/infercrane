#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
state_root=${INFERCRANE_V2_QUALIFICATION_STATE_DIR:-"$root/.infercrane/v2-manual"}
run_id=${INFERCRANE_V2_QUALIFICATION_RUN_ID:-}
approval=false

usage() {
  printf '%s\n' 'Usage: scripts/qualify-v2-manual.sh run|status|cleanup --approve-paid-resources'
  printf '%s\n' 'A fixed INFERCRANE_V2_QUALIFICATION_RUN_ID makes the workflow resumable.'
}

command_name=${1:-}; shift || true
while [ "$#" -gt 0 ]; do case "$1" in --approve-paid-resources) approval=true;; *) usage >&2; exit 2;; esac; shift; done
[ -n "$run_id" ] || { printf '%s\n' 'INFERCRANE_V2_QUALIFICATION_RUN_ID is required' >&2; exit 2; }
case "$run_id" in *[!A-Za-z0-9_.-]*) printf '%s\n' 'invalid qualification run ID' >&2; exit 2;; esac
run_dir="$state_root/$run_id"; stage_file="$run_dir/stage"; mkdir -p "$run_dir"; chmod 700 "$state_root" "$run_dir"
[ -f "$stage_file" ] || printf '%s\n' qualify >"$stage_file"

suite() {
  stage=$1; command=$2; child="$run_id-$stage"
  INFERCRANE_ACCEPTANCE_RUN_ID="$child" "$root/scripts/release-acceptance.sh" "$command" --approve-paid-resources || {
    INFERCRANE_ACCEPTANCE_RUN_ID="$child" "$root/scripts/release-acceptance.sh" cleanup || true
    INFERCRANE_ACCEPTANCE_RUN_ID="$child" "$root/scripts/release-acceptance.sh" report || true
    return 1
  }
  INFERCRANE_ACCEPTANCE_RUN_ID="$child" "$root/scripts/release-acceptance.sh" cleanup
  INFERCRANE_ACCEPTANCE_RUN_ID="$child" "$root/scripts/release-acceptance.sh" report
}

case "$command_name" in
  status) printf 'run=%s stage=%s\n' "$run_id" "$(cat "$stage_file")"; exit 0;;
  cleanup)
    for stage in qualify elastic-faults serverless-faults; do INFERCRANE_ACCEPTANCE_RUN_ID="$run_id-$stage" "$root/scripts/release-acceptance.sh" cleanup || true; done
    exit 0;;
  run) [ "$approval" = true ] || { printf '%s\n' 'refusing paid qualification without --approve-paid-resources' >&2; exit 2; };;
  *) usage >&2; exit 2;;
esac

while :; do
  stage=$(cat "$stage_file")
  case "$stage" in
    qualify) suite qualify qualify; printf '%s\n' elastic-faults >"$stage_file";;
    elastic-faults) suite elastic-faults elastic-faults; printf '%s\n' serverless-faults >"$stage_file";;
    serverless-faults) suite serverless-faults serverless-faults; printf '%s\n' complete >"$stage_file";;
    complete) printf 'InferCrane v2 manual qualification complete: %s\n' "$run_id"; exit 0;;
    *) printf 'invalid persisted stage: %s\n' "$stage" >&2; exit 1;;
  esac
done
