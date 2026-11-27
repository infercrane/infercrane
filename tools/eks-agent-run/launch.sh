#!/usr/bin/env bash
# The billed start, as one command for the coordinator. It reserves the GPU
# and creates the cluster (20-create.sh), and builds and pushes the image
# (10-image.sh) only once AWS has granted the GPU, while the cluster is being
# created. A refused reservation therefore costs neither money nor a build.
#
#   tools/eks-agent-run/launch.sh --yes
#   tools/eks-agent-run/launch.sh --yes --retry-until 18:30 [--every 300]
#
# With --retry-until, a refused reservation (code 7) is asked again every
# --every seconds (default 300) until that local time. Each refusal is free;
# the first grant starts the run. The attempts are listed, with AWS's
# answers, in .infercrane/eks-agent-run/reservation-attempts.txt.
#
# EKS_PROOF_GPU_ORDER="g6.xlarge g5.xlarge" asks for each GPU type in turn in
# every attempt, all zones of the first before the second, and the first
# grant starts the run on that type (20-create.sh records it, and every later
# phase follows the record). Without it the one type is EKS_PROOF_GPU_INSTANCE,
# g6.xlarge by default.
#
# With EKS_PROOF_AUTH_UNTIL="YYYY-MM-DD HH:MM" (when the AWS sign-in ends), the
# deadline falls at that time when it comes first, and no reservation is asked
# for once a whole run no longer fits before it (exit code 8).
#
# Once the GPU is reserved, it starts 95-watchdog.sh, detached: it runs
# 90-delete.sh --yes when deleting must begin, if no one has by then.
#
# Exit codes: 0 both done; 7 no zone had the GPU (nothing created); 8 too late
# to start before the AWS sign-in ends (nothing created); any other code is
# the failing step's; the logs are in .infercrane/eks-agent-run/logs.
# After a failure other than 7: 50-collect.sh, then 90-delete.sh --yes.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[ "${1:-}" = "--yes" ] || die "this creates billed resources in AWS; run it with --yes"
shift
RETRY_UNTIL=""
EVERY=300
while [ "$#" -gt 0 ]; do
  case "$1" in
    --retry-until) RETRY_UNTIL=$2; shift 2 ;;
    --every) EVERY=$2; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
until_epoch=0
if [ -n "$RETRY_UNTIL" ]; then
  until_epoch=$(date -j -f '%Y-%m-%d %H:%M:%S' "$(date +%Y-%m-%d) $RETRY_UNTIL:00" +%s 2>/dev/null) || die "--retry-until takes a local time today, like 18:30"
  [ "$until_epoch" -gt "$(date +%s)" ] || die "--retry-until $RETRY_UNTIL has passed"
  case "$EVERY" in '' | *[!0-9]*) die "--every takes seconds" ;; esac
  [ "$EVERY" -ge 60 ] || die "--every must be at least 60 seconds"
fi
LOGS="$STATE_DIR/logs"
mkdir -p "$LOGS"
# The exit code is also written to a file: a pipe after this command hid a
# failure on 26 Nov (13:27, "kubectl is not installed" read as exit 0).
rm -f "$LOGS/launch.exit"
trap 'printf "%s\n" "$?" >"$LOGS/launch.exit"' EXIT
say "launch at $(date '+%Y-%m-%d %H:%M:%S %Z'); logs in $LOGS${RETRY_UNTIL:+; asking again every $EVERY s until $RETRY_UNTIL}"

GPU_ORDER=${EKS_PROOF_GPU_ORDER:-${EKS_PROOF_GPU_INSTANCE:-g6.xlarge}}
for gpu in $GPU_ORDER; do
  case "$gpu" in g6.xlarge | g5.xlarge) ;; *) die "EKS_PROOF_GPU_ORDER names $gpu; env.sh knows g6.xlarge and g5.xlarge" ;; esac
done
[ -z "${EKS_PROOF_GPU_ORDER:-}" ] || [ -z "${EKS_PROOF_GPU_INSTANCE:-}" ] || die "set EKS_PROOF_GPU_ORDER or EKS_PROOF_GPU_INSTANCE, not both"
say "GPU types asked for, in this order: $GPU_ORDER"

attempt=0
while :; do
  attempt=$((attempt + 1))
  code=""
  for gpu in $GPU_ORDER; do
    printf '# attempt %s at %s: %s\n' "$attempt" "$(date '+%Y-%m-%d %H:%M:%S %Z')" "$gpu" >>"$STATE_DIR/reservation-attempts.txt"
    EKS_PROOF_GPU_INSTANCE=$gpu EKS_PROOF_IMAGE_PENDING=1 caffeinate -i "$TOOL_DIR/20-create.sh" --yes >"$LOGS/20-create.log" 2>&1 &
    create=$!
    # Wait for the reservation's answer, or for the create to end without one.
    code=""
    until grep -q -E 'reservation [^ ]+ active|reusing reservation' "$LOGS/20-create.log" 2>/dev/null; do
      if ! kill -0 "$create" 2>/dev/null; then
        code=0
        wait "$create" || code=$?
        break
      fi
      sleep 5
    done
    # Granted (no code), or a failure other than "no capacity": no next type.
    [ -n "$code" ] && [ "$code" = 7 ] || break
    say "$(date +%H:%M:%S) attempt $attempt: no zone has a $gpu"
  done
  [ -z "$code" ] && break
  if [ "$code" = 7 ] && [ "$until_epoch" -gt $(($(date +%s) + EVERY)) ]; then
    say "$(date +%H:%M:%S) attempt $attempt: asking again in $EVERY s"
    sleep "$EVERY"
    continue
  fi
  say "20-create.sh ended with code $code before a GPU was reserved (attempt $attempt):"
  tail -15 "$LOGS/20-create.log"
  exit "$code"
done
say "$(date +%H:%M:%S) GPU reserved ($gpu): $(grep -E 'reservation [^ ]+ active|reusing reservation' "$LOGS/20-create.log" | tail -1 | sed 's/^ *//')"
# Detached, so it outlives this script and whoever started it.
nohup caffeinate -i "$TOOL_DIR/95-watchdog.sh" >>"$LOGS/95-watchdog.log" 2>&1 &
say "$(date +%H:%M:%S) watchdog started (pid $!): 90-delete.sh --yes at $(iso "$(($(state_get DELETE_BY) - DELETE_MINUTES * 60))") unless the cluster is gone; $LOGS/95-watchdog.log"
say "$(date +%H:%M:%S) pushing the image while the cluster is created (10-image.sh builds only when no image of this code is on this laptop)"

image_code=0
caffeinate -i "$TOOL_DIR/10-image.sh" --yes >"$LOGS/10-image.log" 2>&1 || image_code=$?
say "$(date +%H:%M:%S) 10-image.sh ended with code $image_code"
[ "$image_code" -eq 0 ] || tail -15 "$LOGS/10-image.log"

create_code=0
wait "$create" || create_code=$?
say "$(date +%H:%M:%S) 20-create.sh ended with code $create_code"
tail -12 "$LOGS/20-create.log"
[ "$create_code" -eq 0 ] || exit "$create_code"
exit "$image_code"
