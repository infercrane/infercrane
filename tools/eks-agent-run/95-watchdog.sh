#!/usr/bin/env bash
# The backstop for a run nobody is watching. launch.sh starts it, detached,
# once AWS has granted the GPU. It runs 90-delete.sh --yes when deleting must
# begin (DELETE_MINUTES before the deadline, which 20-create.sh puts no later
# than the end of the AWS sign-in), unless 90-delete.sh has already removed
# the cluster. A phase that hangs, or a session that stops answering, then
# still ends with nothing left.
#
# It runs on this laptop, so a laptop that sleeps or shuts down stops it. AWS
# itself still scales both node groups to zero at the deadline and ends the
# capacity reservation; only the EKS control plane ($0.10 an hour) is then
# left until 90-delete.sh runs.
#
# Creates nothing. Start it by hand with:
#   nohup caffeinate -i tools/eks-agent-run/95-watchdog.sh >>.infercrane/eks-agent-run/logs/95-watchdog.log 2>&1 &
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

watch_phase() {
  local end start code tries=0
  say "watchdog started $(date '+%Y-%m-%d %H:%M:%S %Z'), pid $$, region $REGION"
  while :; do
    if [ -z "$(state_get CREATED_AT)" ]; then
      say "$(date '+%H:%M:%S') no cluster of this test is recorded (90-delete.sh has run, or none was created); the watchdog ends"
      return 0
    fi
    end=$(state_get DELETE_BY)
    if [ -n "$end" ]; then
      start=$((end - DELETE_MINUTES * 60))
      if [ "$(date +%s)" -ge "$start" ]; then
        say "$(date '+%H:%M:%S') deleting must begin now (deadline $(iso "$end")); running 90-delete.sh --yes"
        code=0
        "$TOOL_DIR/90-delete.sh" --yes || code=$?
        # 0: done (the loop sees no cluster recorded and ends). 6: someone
        # else's deletion is under way; look again in a minute. Any other
        # code: something is left or AWS could not be asked; try again in
        # five minutes, twelve times at most.
        if [ "$code" -ne 0 ] && [ "$code" -ne 6 ]; then
          tries=$((tries + 1))
          say "$(date '+%H:%M:%S') 90-delete.sh ended with code $code (try $tries of 12)"
          [ "$tries" -lt 12 ] || return "$code"
          sleep 240
        fi
      fi
    fi
    sleep 60
  done
}

watch_phase
