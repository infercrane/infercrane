#!/usr/bin/env bash
# The billed phases after launch.sh, in order, each under a wall limit, and
# always ending with 90-delete.sh --yes:
#
#   tools/eks-agent-run/run-phases.sh [--from 35-deploy] [--keep-on-failure]
#
#   30-install  35-deploy  40-run  45-migrate  60-uninstall  50-collect  -> 90-delete
#
# Each phase runs in its own process group. When it passes its wall limit
# (WALL_MINUTES below, or the time left before deleting must begin, whichever
# is shorter) the whole group is stopped: TERM, then KILL 30 seconds later, so
# no port-forward or watch loop is left. A phase that fails or is stopped ends
# the chain: 50-collect.sh gathers what there is, then 90-delete.sh --yes.
# --keep-on-failure skips that deletion for someone who is watching and will
# fix the phase and continue with --from; 95-watchdog.sh still deletes when
# deleting must begin.
#
# Logs: .infercrane/eks-agent-run/logs/<phase>.log, and run-phases.log for
# this script. Exit code: the first failing phase's (124 for a wall limit, 4
# for "no time left"), otherwise 90-delete.sh's (0 only when nothing billable
# is left).
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PHASES="30-install 35-deploy 40-run 45-migrate 60-uninstall 50-collect"

# The most each phase may take, in minutes: the sum of its own waits at their
# limits (device plugin 10 + GPU label 10 + control plane; deploy 30 + probes
# + delete 5; shared GPU 10 + customer's server 20 + Helm 5 + discovery 4 +
# candidate run 70; migration set-up + campaign watch 120 + canary and
# rollback). Their sum is longer than a cluster lives: the deadline is the
# limit that binds when phases run long.
wall_minutes() {
  case "$1" in
    30-install) echo 40 ;;
    35-deploy) echo 50 ;;
    40-run) echo 110 ;;
    45-migrate) echo 165 ;;
    60-uninstall) echo 25 ;;
    50-collect) echo 15 ;;
    *) die "no wall limit for phase $1" ;;
  esac
}

FROM=""
KEEP=no
while [ "$#" -gt 0 ]; do
  case "$1" in
    --from) FROM=$2; shift 2 ;;
    --keep-on-failure) KEEP=yes; shift ;;
    *) die "unknown argument $1" ;;
  esac
done
if [ -n "$FROM" ]; then
  case " $PHASES " in *" $FROM "*) ;; *) die "--from takes one of: $PHASES" ;; esac
fi

LOGS="$STATE_DIR/logs"
mkdir -p "$LOGS"
RUNNER_LOG="$LOGS/run-phases.log"
log() { printf '%s %s\n' "$(date '+%H:%M:%S')" "$*" | tee -a "$RUNNER_LOG"; }

# minutes_before_delete prints the whole minutes left until deleting must
# begin, or a large number when no deadline is recorded.
minutes_before_delete() {
  local end
  end=$(deadline)
  [ -n "$end" ] || {
    printf '9999'
    return
  }
  printf '%s' $(((end - DELETE_MINUTES * 60 - $(date +%s)) / 60))
}

check_counts() {
  if [ -f "$CHECKS" ]; then
    awk -F'\t' '{ n++ } $3 == "fail" { f++ } END { printf "%d check lines so far, %d failed", n, f }' "$CHECKS"
  else
    printf 'no check lines yet'
  fi
}

# run_walled MINUTES LOG COMMAND... runs COMMAND in its own process group and
# stops the whole group once MINUTES have passed. Returns the command's exit
# code, or 124 when the wall limit stopped it.
run_walled() {
  local minutes=$1 out=$2 pid started code=0
  shift 2
  started=$(date +%s)
  set -m
  "$@" >>"$out" 2>&1 &
  pid=$!
  set +m
  while kill -0 "$pid" 2>/dev/null; do
    if [ $(($(date +%s) - started)) -ge $((minutes * 60)) ]; then
      log "  wall limit of $minutes minutes reached; stopping process group $pid"
      kill -TERM -- "-$pid" 2>/dev/null || true
      sleep 30
      kill -KILL -- "-$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 5
  done
  wait "$pid" || code=$?
  return "$code"
}

# delete_all runs 90-delete.sh --yes. When the watchdog's deletion is under
# way (code 6) it waits for that to end, then runs it again, which checks.
delete_all() {
  local code=0 holder
  log "90-delete.sh --yes"
  caffeinate -i "$TOOL_DIR/90-delete.sh" --yes >>"$LOGS/90-delete.log" 2>&1 || code=$?
  if [ "$code" -eq 6 ]; then
    holder=$(cat "$STATE_DIR/delete.pid" 2>/dev/null || true)
    log "  another deletion (pid ${holder:-unknown}) is under way; waiting for it"
    while [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; do sleep 15; done
    code=0
    caffeinate -i "$TOOL_DIR/90-delete.sh" --yes >>"$LOGS/90-delete.log" 2>&1 || code=$?
  fi
  log "90-delete.sh ended with code $code: $(grep -E '^RESULT|nothing billable' "$LOGS/90-delete.log" | tail -1)"
  return "$code"
}

run_all() {
  need_tools aws kubectl helm caffeinate
  need_cluster
  log "run-phases at $(date '+%Y-%m-%d %H:%M:%S %Z'), region $REGION, deadline $(iso "$(deadline)"), deleting begins $DELETE_MINUTES minutes before it"
  local phase started=no failed="" code=0 wall left
  for phase in $PHASES; do
    if [ "$started" = no ]; then
      [ -z "$FROM" ] || [ "$phase" = "$FROM" ] || continue
      started=yes
    fi
    left=$(minutes_before_delete)
    wall=$(wall_minutes "$phase")
    if [ "$left" -lt 1 ]; then
      failed=$phase code=4
      log "$phase: not started, deleting must begin now"
      break
    fi
    [ "$left" -ge "$wall" ] || wall=$left
    log "$phase: started, wall limit $wall minutes ($left before deleting must begin)"
    code=0
    run_walled "$wall" "$LOGS/$phase.log" caffeinate -i "$TOOL_DIR/$phase.sh" || code=$?
    log "$phase: ended with code $code; $(check_counts)"
    if [ "$code" -ne 0 ]; then
      failed=$phase
      break
    fi
  done

  if [ -n "$failed" ]; then
    log "the chain stops at $failed (code $code)"
    if [ "$failed" != 50-collect ]; then
      left=$(minutes_before_delete)
      if [ "$left" -ge 3 ]; then
        wall=$(wall_minutes 50-collect)
        [ "$left" -ge "$wall" ] || wall=$left
        log "50-collect: started after the failure, wall limit $wall minutes"
        run_walled "$wall" "$LOGS/50-collect.log" caffeinate -i "$TOOL_DIR/50-collect.sh" || log "50-collect: ended with code $?"
      else
        log "50-collect: skipped, deleting must begin"
      fi
    fi
    if [ "$KEEP" = yes ]; then
      log "--keep-on-failure: the cluster is kept; 95-watchdog.sh deletes it at $(iso "$(($(deadline) - DELETE_MINUTES * 60))") unless 90-delete.sh runs first"
      return "$code"
    fi
    delete_all || true
    return "$code"
  fi
  delete_all
}

run_all
