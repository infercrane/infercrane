# Helpers shared by the phase scripts. Sourced, never run.
# shellcheck shell=bash
set -euo pipefail

TOOL_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(cd "$TOOL_DIR/../.." && pwd)
# shellcheck source=env.sh
. "$TOOL_DIR/env.sh"

# Everything a run leaves on this machine lives here (.infercrane/ is ignored
# by git): the cluster's own kubeconfig, the stand-in control plane's key and
# certificate, the deadline, and the outputs of each phase.
STATE_DIR=${EKS_PROOF_STATE_DIR:-$REPO_DIR/.infercrane/eks-agent-run}
mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"
STATE_FILE="$STATE_DIR/state.env"
SPEND_FILE="$STATE_DIR/spend.tsv"

# 20-create.sh records the region and GPU type of the cluster it creates, and
# 90-delete.sh forgets them once nothing is left. While they are recorded,
# every phase uses them, also when EKS_PROOF_REGION or EKS_PROOF_GPU_INSTANCE
# is not set: a teardown started without them must never check the default
# region and report "clean" while the cluster bills in another, nor count its
# hours at another GPU's price. Naming a different one then is refused.
recorded_setting() { { [ -f "$STATE_FILE" ] && sed -n "s/^$1=//p" "$STATE_FILE" | tail -1; } || true; }
CLUSTER_REGION_RECORDED=$(recorded_setting CLUSTER_REGION)
CLUSTER_GPU_RECORDED=$(recorded_setting CLUSTER_GPU_INSTANCE)
reload_env=no
if [ -n "$CLUSTER_REGION_RECORDED" ] && [ "$CLUSTER_REGION_RECORDED" != "$REGION" ]; then
  if [ -n "${EKS_PROOF_REGION:-}" ]; then
    printf 'error: a cluster of this test was created in %s (CLUSTER_REGION in %s); EKS_PROOF_REGION=%s would work on the wrong region. Unset it, or run 90-delete.sh for %s first.\n' \
      "$CLUSTER_REGION_RECORDED" "$STATE_FILE" "$EKS_PROOF_REGION" "$CLUSTER_REGION_RECORDED" >&2
    exit 1
  fi
  EKS_PROOF_REGION=$CLUSTER_REGION_RECORDED
  reload_env=yes
fi
if [ -n "$CLUSTER_GPU_RECORDED" ] && [ "$CLUSTER_GPU_RECORDED" != "$GPU_INSTANCES" ]; then
  if [ -n "${EKS_PROOF_GPU_INSTANCE:-}" ]; then
    printf 'error: the cluster of this test was created with %s (CLUSTER_GPU_INSTANCE in %s); EKS_PROOF_GPU_INSTANCE=%s names another GPU. Unset it, or run 90-delete.sh first.\n' \
      "$CLUSTER_GPU_RECORDED" "$STATE_FILE" "$EKS_PROOF_GPU_INSTANCE" >&2
    exit 1
  fi
  EKS_PROOF_GPU_INSTANCE=$CLUSTER_GPU_RECORDED
  reload_env=yes
fi
if [ "$reload_env" = yes ]; then
  # shellcheck source=env.sh
  . "$TOOL_DIR/env.sh"
fi
unset reload_env

# The cluster gets its own kubeconfig. The user's ~/.kube/config and current
# context (other sessions use kind clusters on this laptop) are never touched.
export KUBECONFIG="$STATE_DIR/kubeconfig"
export AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION" AWS_PAGER=""

CONTROL_LOCAL_PORT=${EKS_PROOF_CONTROL_PORT:-28443}
AGENT_LOCAL_PORT=${EKS_PROOF_AGENT_PORT:-28080}
CONTROL_HOST="infercrane.$NAMESPACE_HOSTED.svc"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# Docker Desktop puts kubectl in ~/.docker/bin and Homebrew in /opt/homebrew/bin;
# a shell started without them on PATH found no kubectl (26 Nov, 13:27).
for TOOL_PATH_DIR in "$HOME/.docker/bin" /opt/homebrew/bin /usr/local/bin; do
  case ":$PATH:" in
    *":$TOOL_PATH_DIR:"*) ;;
    *) [ -d "$TOOL_PATH_DIR" ] && PATH="$PATH:$TOOL_PATH_DIR" ;;
  esac
done
export PATH

need_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 ||
      die "$tool is not installed or not on PATH (also looked in ~/.docker/bin, /opt/homebrew/bin, /usr/local/bin): brew install $tool"
  done
}

# --- AWS sign-in and account masking ---------------------------------------

ACCOUNT=""

# aws_account sets ACCOUNT, or stops with exit code 3 when the sign-in is
# missing or expired. Nothing here can fix that: the user runs `aws login`.
aws_account() {
  if ! ACCOUNT=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) || [ -z "$ACCOUNT" ]; then
    printf 'AWS credentials are missing or expired. Stop here: the user runs "aws login" again.\n' >&2
    exit 3
  fi
}

# run_masked runs a phase with the account number replaced in everything it
# prints, so no log or evidence file ever holds it.
run_masked() {
  aws_account
  "$@" 2>&1 | ACCOUNT="$ACCOUNT" perl -pe 'BEGIN { $| = 1 } s/\Q$ENV{ACCOUNT}\E/<ACCOUNT>/g'
  return "${PIPESTATUS[0]}"
}

ecr_registry() { printf '%s.dkr.ecr.%s.amazonaws.com' "$ACCOUNT" "$REGION"; }

# --- Local state ------------------------------------------------------------

state_get() {
  [ -f "$STATE_FILE" ] || return 0
  sed -n "s/^$1=//p" "$STATE_FILE" | tail -1
}

# launch.sh runs 20-create.sh and 10-image.sh at the same time, and both
# write this file; a directory lock keeps one write from undoing the other.
state_lock() {
  local tries=0
  until mkdir "$STATE_FILE.lock" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 100 ] || die "the state file has been locked for 20 seconds ($STATE_FILE.lock); remove the lock if no phase is running"
    sleep 0.2
  done
}
state_unlock() { rmdir "$STATE_FILE.lock" 2>/dev/null || true; }

state_set() {
  local tmp="$STATE_FILE.tmp.$$"
  state_lock
  touch "$STATE_FILE"
  grep -v "^$1=" "$STATE_FILE" >"$tmp" || true
  printf '%s=%s\n' "$1" "$2" >>"$tmp"
  mv "$tmp" "$STATE_FILE"
  chmod 600 "$STATE_FILE"
  state_unlock
}

state_unset() {
  [ -f "$STATE_FILE" ] || return 0
  local tmp="$STATE_FILE.tmp.$$"
  state_lock
  grep -v "^$1=" "$STATE_FILE" >"$tmp" || true
  mv "$tmp" "$STATE_FILE"
  state_unlock
}

# --- Cost and time limit ----------------------------------------------------

calc() { awk "BEGIN { printf \"%.4f\", $* }"; }
money() { awk "BEGIN { printf \"%.2f\", $* }"; }
iso() { python3 -c 'import datetime, sys; print(datetime.datetime.fromtimestamp(int(sys.argv[1]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1"; }

# gpu_price prints the hourly list price of one allowed GPU instance type.
gpu_price() {
  local pair
  for pair in $GPU_PRICES; do
    if [ "${pair%%=*}" = "$1" ]; then
      printf '%s' "${pair#*=}"
      return 0
    fi
  done
  return 1
}

# rate_list is the hourly list price of everything the test keeps running,
# with the dearest GPU type allowed.
rate_list() {
  calc "$PRICE_EKS_CONTROL_PLANE + $PRICE_GPU_INSTANCE + $PRICE_SYSTEM_INSTANCE + 2 * $PRICE_PUBLIC_IPV4 \
    + ($GPU_DISK_GB + $SYSTEM_DISK_GB) * $PRICE_GP3_GB_MONTH / $HOURS_PER_MONTH \
    + $ECR_IMAGE_GB * $PRICE_ECR_GB_MONTH / $HOURS_PER_MONTH"
}

# rate_guard is what the time limit counts with.
rate_guard() { calc "$(rate_list) * $GUARD_ALLOWANCE"; }

cost_table() {
  local disks ecr
  disks=$(calc "($GPU_DISK_GB + $SYSTEM_DISK_GB) * $PRICE_GP3_GB_MONTH / $HOURS_PER_MONTH")
  ecr=$(calc "$ECR_IMAGE_GB * $PRICE_ECR_GB_MONTH / $HOURS_PER_MONTH")
  cat <<EOF
  Hourly list price, $REGION, USD before tax:
    EKS control plane (standard support)        $PRICE_EKS_CONTROL_PLANE
    1 GPU node, at the dearest type allowed      $PRICE_GPU_INSTANCE
      ($GPU_PRICES)
    1 x $SYSTEM_INSTANCE (system node), on demand   $PRICE_SYSTEM_INSTANCE
    2 public IPv4 addresses                      $(calc "2 * $PRICE_PUBLIC_IPV4")
    $((GPU_DISK_GB + SYSTEM_DISK_GB)) GB gp3 disks                             $disks
    ECR image storage (about $ECR_IMAGE_GB GB)             $ecr
    ---------------------------------------------------
    total                                        $(rate_list) an hour
    counted by the time limit (+3% allowance)    $(rate_guard) an hour
EOF
}

# spent_closed is what earlier, already deleted clusters cost, counted at the
# guard rate for their whole lifetime.
spent_closed() {
  if [ -f "$SPEND_FILE" ]; then
    awk -F'\t' '{ total += $3 } END { printf "%.2f", total + 0 }' "$SPEND_FILE"
  else
    printf '0.00'
  fi
}

# spent_open is what the current cluster has cost so far.
spent_open() {
  local created
  created=$(state_get CREATED_AT)
  if [ -z "$created" ]; then
    printf '0.00'
    return
  fi
  money "($(date +%s) - $created) / 3600 * $(rate_guard)"
}

# deadline prints the epoch second by which the cluster must be gone. The
# local state is the first source; the cluster's own tag is the second, so the
# limit survives a lost state directory.
deadline() {
  local value
  value=$(state_get DELETE_BY)
  if [ -z "$value" ]; then
    value=$(aws eks describe-cluster --name "$CLUSTER" --query 'cluster.tags."infercrane-delete-by"' --output text 2>/dev/null || true)
    case "$value" in '' | None | null) value="" ;; esac
  fi
  printf '%s' "$value"
}

# auth_until prints EKS_PROOF_AUTH_UNTIL as epoch seconds, or nothing.
auth_until() {
  [ -n "${EKS_PROOF_AUTH_UNTIL:-}" ] || return 0
  # BSD date fills the fields a format leaves out from the current time, so
  # the seconds are given: "00:15" is 00:15:00, not 00:15 and the current second.
  date -j -f '%Y-%m-%d %H:%M:%S' "$EKS_PROOF_AUTH_UNTIL:00" +%s 2>/dev/null ||
    die "EKS_PROOF_AUTH_UNTIL takes a local date and time, like \"2026-11-27 00:15\""
}

minutes_left() {
  local end
  end=$(deadline)
  [ -n "$end" ] || {
    printf 'unknown'
    return
  }
  printf '%s' $(((end - $(date +%s)) / 60))
}

# guard_phase refuses to start a phase that could run past the point where
# deleting must begin.
guard_phase() {
  local name=$1 minutes=$2 end now latest
  end=$(deadline)
  [ -n "$end" ] || die "no time limit is recorded for cluster $CLUSTER; only 90-delete.sh may run"
  now=$(date +%s)
  latest=$((end - DELETE_MINUTES * 60))
  if [ $((now + minutes * 60)) -gt "$latest" ]; then
    cat >&2 <<EOF
REFUSED: phase "$name" may take $minutes minutes, and deleting must start by
$(iso "$latest") ($(((latest - now) / 60)) minutes from now) to keep the test inside \$$BUDGET_USD.
Run tools/eks-agent-run/90-delete.sh --yes now.
EOF
    exit 4
  fi
  say "Time limit: the cluster must be gone by $(iso "$end") ($(((end - now) / 60)) minutes from now)."
  say "This phase may take up to $minutes minutes. Counted so far:\$$(money "$(spent_closed) + $(spent_open)") of \$$BUDGET_USD."
}

# --- Cluster access ---------------------------------------------------------

cluster_status() {
  local status
  if status=$(aws eks describe-cluster --name "$CLUSTER" --query cluster.status --output text 2>&1); then
    printf '%s' "$status"
  elif printf '%s' "$status" | grep -q ResourceNotFoundException; then
    printf 'ABSENT'
  else
    printf 'error: %s\n' "$status" >&2
    return 1
  fi
}

need_cluster() {
  [ "$(cluster_status)" = ACTIVE ] || die "cluster $CLUSTER is not active; run 20-create.sh first"
  [ -s "$KUBECONFIG" ] || aws eks update-kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
  chmod 600 "$KUBECONFIG"
}

# create_failures prints why CloudFormation could not create a node group, one
# line per failure. A failed node group stack rolls back, so its events are the
# only place the reason is kept, and only until 90-delete.sh removes the stack.
create_failures() {
  local group
  for group in "$GPU_NODEGROUP" "$SYSTEM_NODEGROUP"; do
    aws cloudformation describe-stack-events --stack-name "eksctl-$CLUSTER-nodegroup-$group" \
      --query "StackEvents[?ResourceStatus=='CREATE_FAILED'].[Timestamp,LogicalResourceId,ResourceStatusReason]" \
      --output text 2>/dev/null | sed "s/^/$group: /" || true
  done
}

# cluster_gone_at prints the second at which AWS finished deleting the newest
# cluster stack, when that lies between $1 and $2 (epoch seconds). It prints
# nothing when that cannot be read; the caller then counts until now.
# Both calls ask for JSON: with --output text the CLI applies a --query to
# each page of a paginated answer and prints one result per page, and a
# second page's "None" made the first version of this read nothing at all
# (attempt 2's record, 24 Nov 2026).
cluster_gone_at() {
  local stack_id finished
  stack_id=$(aws cloudformation list-stacks --stack-status-filter DELETE_COMPLETE \
    --query "reverse(sort_by(StackSummaries[?StackName=='eksctl-$CLUSTER-cluster'], &CreationTime))[0].StackId" \
    --output json 2>/dev/null) || return 0
  stack_id=$(json_string "$stack_id")
  [ -n "$stack_id" ] || return 0
  finished=$(aws cloudformation describe-stack-events --stack-name "$stack_id" \
    --query "StackEvents[?ResourceType=='AWS::CloudFormation::Stack' && ResourceStatus=='DELETE_COMPLETE'].Timestamp | [0]" \
    --output json 2>/dev/null) || return 0
  python3 -c 'import datetime, json, sys
try:
    value = json.loads(sys.argv[1])
    at = int(datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp())
    if int(sys.argv[2]) <= at <= int(sys.argv[3]):
        print(at)
except (AttributeError, TypeError, ValueError):
    pass' "$finished" "$1" "$2" || true
}

# json_string prints a JSON string value, or nothing for anything else.
json_string() {
  python3 -c 'import json, sys
try:
    value = json.loads(sys.argv[1])
except ValueError:
    value = None
print(value if isinstance(value, str) else "")' "$1"
}

# nodegroup_asg prints the name of a node group's Auto Scaling group, or
# nothing while EKS has not made one. It is found by the tags EKS puts on it:
# `aws eks describe-nodegroup` names the group only once the node group is
# ACTIVE, which is too late to see a refusal (second cluster, 2026-11-23).
nodegroup_asg() {
  local asg
  asg=$(aws autoscaling describe-auto-scaling-groups \
    --filters "Name=tag:eks:cluster-name,Values=$CLUSTER" "Name=tag:eks:nodegroup-name,Values=$1" \
    --query 'AutoScalingGroups[0].AutoScalingGroupName' --output text 2>/dev/null) || asg=""
  case "$asg" in None | null) asg="" ;; esac
  printf '%s' "$asg"
}

# launch_refusals prints every instance request AWS refused, one line each,
# for both node groups. This is where a missing-capacity answer shows first.
launch_refusals() {
  local group asg
  for group in "$GPU_NODEGROUP" "$SYSTEM_NODEGROUP"; do
    asg=$(nodegroup_asg "$group")
    [ -n "$asg" ] || continue
    aws autoscaling describe-scaling-activities --auto-scaling-group-name "$asg" \
      --query "Activities[?StatusCode=='Failed'].[StartTime,StatusMessage]" \
      --output text 2>/dev/null | sed "s/^/$group: /" || true
  done
}

# launch_refused prints one line when a node group has no instance, has asked
# for one at least LAUNCH_REFUSED_ATTEMPTS times over LAUNCH_REFUSED_SECONDS,
# and every request was refused. It prints nothing in every other case, also
# when something cannot be read: no answer never ends a healthy create.
launch_refused() {
  local asg instances activities
  asg=$(nodegroup_asg "$1")
  [ -n "$asg" ] || return 0
  instances=$(aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$asg" \
    --query 'AutoScalingGroups[0].Instances[].InstanceId' --output json 2>/dev/null) || return 0
  activities=$(aws autoscaling describe-scaling-activities --auto-scaling-group-name "$asg" \
    --query 'Activities[].{status:StatusCode,start:StartTime,message:StatusMessage}' --output json 2>/dev/null) || return 0
  INSTANCES="$instances" ACTIVITIES="$activities" python3 - "$LAUNCH_REFUSED_ATTEMPTS" "$LAUNCH_REFUSED_SECONDS" <<'PY' || true
import datetime, json, os, sys
attempts, seconds = int(sys.argv[1]), int(sys.argv[2])
try:
    instances = json.loads(os.environ.get("INSTANCES") or "null")
    activities = json.loads(os.environ.get("ACTIVITIES") or "null")
    if not isinstance(instances, list) or not isinstance(activities, list) or instances:
        sys.exit(0)
    failed = [a for a in activities if a.get("status") == "Failed"]
    if len(failed) < attempts or len(failed) != len(activities):
        sys.exit(0)
    def when(activity):
        return datetime.datetime.fromisoformat(activity["start"].replace("Z", "+00:00"))
    first, last = min(failed, key=when), max(failed, key=when)
    waited = (datetime.datetime.now(datetime.timezone.utc) - when(first)).total_seconds()
    if waited < seconds:
        sys.exit(0)
    print("%d requests in %d minutes, every one refused. AWS: %s"
          % (len(failed), waited // 60, " ".join((last.get("message") or "no message").split())))
except (AttributeError, KeyError, TypeError, ValueError):
    sys.exit(0)
PY
}

# watch_launches waits for eksctl (process $1) and ends the wait early, with
# code 7, when AWS keeps refusing to start a node. EKS itself reports that
# only after about 33 minutes (first cluster, 2026-11-23); the Auto Scaling
# group shows it at once. Stopping eksctl stops nothing in AWS: CloudFormation
# keeps working until 90-delete.sh runs.
watch_launches() {
  local pid=$1 group verdict
  while kill -0 "$pid" 2>/dev/null; do
    sleep "$LAUNCH_WATCH_SECONDS"
    kill -0 "$pid" 2>/dev/null || break
    for group in "$SYSTEM_NODEGROUP" "$GPU_NODEGROUP"; do
      verdict=$(launch_refused "$group") || verdict=""
      if [ -n "$verdict" ]; then
        step "AWS will not start the \"$group\" node; the wait ends here"
        say "  $verdict"
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
        return 7
      fi
    done
  done
  wait "$pid"
}

kc() { kubectl "$@"; }

wait_rollout() {
  local namespace=$1 target=$2 timeout=$3
  kc --namespace "$namespace" rollout status "$target" --timeout="$timeout"
}

# --- Objects in the cluster (shared by the install and run phases) ---------

secret_file() {
  # secret_file PATH COMMAND... writes COMMAND's output to PATH once, 0600.
  local path=$1
  shift
  if [ ! -s "$path" ]; then
    (umask 077 && "$@" >"$path")
  fi
}

apply_secret() {
  # apply_secret NAMESPACE NAME KEY=FILE... creates or updates a Secret from files.
  local namespace=$1 name=$2 args=() pair
  shift 2
  for pair in "$@"; do
    args+=("--from-file=$pair")
  done
  kc --namespace "$namespace" create secret generic "$name" "${args[@]}" --dry-run=client -o yaml | kc apply -f - >/dev/null
  say "  secret $namespace/$name"
}

ensure_namespace() {
  kc create namespace "$1" --dry-run=client -o yaml | kc apply -f - >/dev/null
}

# wait_model_server waits for a Deployment of a model server and stops early,
# with the reason, when its pod cannot start.
wait_model_server() {
  local namespace=$1 name=$2 minutes=$3 until=$((SECONDS + $3 * 60)) reason restarts
  while [ "$SECONDS" -lt "$until" ]; do
    if [ "$(kc --namespace "$namespace" get deployment "$name" -o jsonpath='{.status.availableReplicas}')" = "1" ]; then
      say "  $name is ready after $((SECONDS / 60)) minutes of this phase"
      return 0
    fi
    reason=$(kc --namespace "$namespace" get pods -l "app=$name" -o jsonpath='{.items[0].status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
    restarts=$(kc --namespace "$namespace" get pods -l "app=$name" -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null || true)
    case "$reason" in
      CrashLoopBackOff | ErrImagePull | ImagePullBackOff | CreateContainerConfigError | InvalidImageName)
        kc --namespace "$namespace" describe pods -l "app=$name" | tail -25
        kc --namespace "$namespace" logs -l "app=$name" --tail=60 --previous 2>/dev/null || kc --namespace "$namespace" logs -l "app=$name" --tail=60 || true
        die "$name cannot start: $reason"
        ;;
    esac
    if [ -n "$restarts" ] && [ "$restarts" != 0 ]; then
      kc --namespace "$namespace" logs -l "app=$name" --tail=60 --previous || true
      die "$name restarted $restarts time(s) while starting"
    fi
    say "  waiting for $name: $(kc --namespace "$namespace" get pods -l "app=$name" --no-headers 2>/dev/null | awk '{print $3}' | head -1)"
    sleep 20
  done
  kc --namespace "$namespace" describe pods -l "app=$name" | tail -25
  die "$name was not ready within $minutes minutes"
}

# icli runs the infercrane CLI inside the control plane's own pod, against
# its Service name with its own certificate, as an operator in the cluster
# would. The pod's INFERCRANE_API_KEY is the CLI's key; it never leaves the
# pod. Arguments are passed as they are; nothing goes through a shell.
icli() {
  kc --namespace "$NAMESPACE_HOSTED" exec -i deployment/infercrane -- env \
    INFERCRANE_URL="https://$CONTROL_HOST:8443" SSL_CERT_FILE=/tls/tls.crt infercrane "$@"
}

# check NAME pass|fail|unknown DETAIL records one criterion line in CHECKS.
CHECKS=${CHECKS:-$STATE_DIR/run/checks.tsv}
FAILED=0
check() {
  mkdir -p "$(dirname "$CHECKS")"
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$1" "$2" "$3" >>"$CHECKS"
  case "$2" in
    pass) say "  PASS     $1: $3" ;;
    unknown) say "  UNKNOWN  $1: $3" ;;
    *)
      FAILED=1
      say "  FAIL     $1: $3"
      ;;
  esac
}

PORT_FORWARDS=""

port_forward() {
  local namespace=$1 target=$2 local_port=$3 remote_port=$4 probe=$5 log="$STATE_DIR/port-forward-$3.log" until=$((SECONDS + 90))
  local pid_file="$STATE_DIR/port-forward-$3.pid" stale
  # A forward left by an interrupted phase would answer the probe below while
  # pointing at a pod that may be gone.
  if [ -s "$pid_file" ]; then
    stale=$(cat "$pid_file")
    if ps -p "$stale" -o command= 2>/dev/null | grep -q 'port-forward'; then
      kill "$stale" >/dev/null 2>&1 || true
      sleep 1
    fi
    rm -f "$pid_file"
  fi
  kc --namespace "$namespace" port-forward "$target" "$local_port:$remote_port" >"$log" 2>&1 &
  PORT_FORWARDS="$PORT_FORWARDS $!"
  printf '%s' "$!" >"$pid_file"
  until curl -sk -o /dev/null --max-time 3 "$probe"; do
    [ "$SECONDS" -lt "$until" ] || die "port-forward to $target did not answer (see $log)"
    sleep 1
  done
}

# stop_port_forwards is both the EXIT trap of a phase and its last line: bash
# 3.2 (macOS) does not run an EXIT trap when a phase simply returns.
stop_port_forwards() {
  local pid
  for pid in $PORT_FORWARDS; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  PORT_FORWARDS=""
  rm -f "$STATE_DIR"/port-forward-*.pid
}

# --- The stand-in control plane's API ---------------------------------------

open_control() {
  [ -s "$STATE_DIR/auth-header" ] && [ -s "$STATE_DIR/tls.crt" ] || die "the control plane is not installed; run 30-install.sh first"
  port_forward "$NAMESPACE_HOSTED" service/infercrane "$CONTROL_LOCAL_PORT" 8443 "https://127.0.0.1:$CONTROL_LOCAL_PORT/readyz"
}

# api METHOD PATH OUT [BODY_FILE] writes the response body to OUT and prints the
# HTTP status. The API key is read by curl from a 0600 file, so it is never an
# argument of a process or a line of a log.
api() {
  local method=$1 path=$2 out=$3 body=${4:-}
  local args=(-sS --max-time 60 --cacert "$STATE_DIR/tls.crt"
    --resolve "$CONTROL_HOST:$CONTROL_LOCAL_PORT:127.0.0.1"
    -X "$method" -H "Content-Type: application/json" -H "@$STATE_DIR/auth-header"
    -o "$out" -w '%{http_code}')
  if [ -n "$body" ]; then
    args+=(--data-binary "@$body")
  fi
  curl "${args[@]}" "https://$CONTROL_HOST:$CONTROL_LOCAL_PORT$path"
}

json_field() { python3 -c 'import json, sys
value = json.load(open(sys.argv[1]))
for key in sys.argv[2].split("."):
    value = value.get(key) if isinstance(value, dict) else None
print("" if value is None else value)' "$1" "$2"; }

# --- Image ------------------------------------------------------------------

# image_ref prints the InferCrane image the cluster runs. EKS_PROOF_IMAGE names
# an image that is already pullable (for example the published release once
# its registry package is public); otherwise it is the one 10-image.sh pushed.
image_ref() {
  if [ -n "${EKS_PROOF_IMAGE:-}" ]; then
    printf '%s' "$EKS_PROOF_IMAGE"
    return
  fi
  local digest pushed_to
  digest=$(state_get IMAGE_DIGEST)
  [ -n "$digest" ] || die "no image is recorded; run 10-image.sh first, or set EKS_PROOF_IMAGE"
  # The first image of 26 Nov was pushed before regions were a setting, to
  # eu-central-1, and recorded no region.
  pushed_to=$(state_get IMAGE_REGION)
  pushed_to=${pushed_to:-eu-central-1}
  [ "$pushed_to" = "$REGION" ] || die "the recorded image is in $pushed_to's registry, not $REGION's; run 10-image.sh --yes"
  printf '%s/%s@%s' "$(ecr_registry)" "$ECR_REPOSITORY" "$digest"
}

# helm_image_args prints the chart's image settings for image_ref, one per line.
helm_image_args() {
  local ref
  ref=$(image_ref)
  case "$ref" in
    *@sha256:*) printf -- '--set\nimage.repository=%s\n--set\nimage.digest=%s\n' "${ref%@*}" "${ref##*@}" ;;
    *:*) printf -- '--set\nimage.repository=%s\n--set\nimage.tag=%s\n' "${ref%:*}" "${ref##*:}" ;;
    *) die "image $ref has neither a tag nor a digest" ;;
  esac
}

# cluster_zones ZONE prints the three zones the cluster's VPC spans: the zone
# the GPU is reserved in, then the first two other zones of NODE_ZONES. Three,
# as in every cluster of this test so far, whatever the region offers
# (us-east-1 offers five): the VPC keeps the shape eksctl created on 23-24 Nov.
cluster_zones() {
  local zone picked=$1 n=1
  for zone in $NODE_ZONES; do
    [ "$n" -lt 3 ] || break
    [ "$zone" = "$1" ] && continue
    picked="$picked $zone"
    n=$((n + 1))
  done
  printf '%s' "$picked"
}

# render_cluster_config ZONE prints cluster.yaml for the region, its zones,
# and the zone the GPU is reserved in.
render_cluster_config() {
  sed -e "s/@@REGION@@/$REGION/" -e "s/@@NODE_ZONES@@/$(cluster_zones "$1" | sed 's/ /, /g')/" \
    -e "s/@@GPU_ZONE@@/$1/" -e "s/@@GPU_INSTANCE@@/$GPU_INSTANCES/" "$TOOL_DIR/cluster.yaml"
}

# render fills the @@NAME@@ placeholders of a manifest template.
render() {
  sed -e "s|@@IMAGE@@|$(image_ref)|g" \
    -e "s|@@POSTGRES_IMAGE@@|$POSTGRES_IMAGE|g" \
    -e "s|@@VLLM_IMAGE@@|$VLLM_IMAGE|g" \
    -e "s|@@NAMESPACE_HOSTED@@|$NAMESPACE_HOSTED|g" \
    -e "s|@@NAMESPACE_MODELS@@|$NAMESPACE_MODELS|g" \
    -e "s|@@MODEL@@|$MODEL|g" \
    -e "s|@@MODEL_REVISION@@|$MODEL_REVISION|g" \
    -e "s|@@MAX_MODEL_LEN@@|$MAX_MODEL_LEN|g" \
    -e "s|@@GPU_MEMORY_FRACTION@@|$GPU_MEMORY_FRACTION|g" \
    -e "s|@@GPU_PRODUCT@@|${GPU_PRODUCT:-UNKNOWN}|g" \
    -e "s|@@ENDPOINT_NAME@@|$ENDPOINT_NAME|g" \
    -e "s|@@LOAD_REQUESTS@@|$LOAD_REQUESTS|g" \
    -e "s|@@LOAD_CONCURRENCY@@|$LOAD_CONCURRENCY|g" \
    -e "s|@@LOAD_INPUT_TOKENS@@|$LOAD_INPUT_TOKENS|g" \
    -e "s|@@LOAD_OUTPUT_TOKENS@@|$LOAD_OUTPUT_TOKENS|g" \
    -e "s|@@CANDIDATE_TIMEOUT_SECONDS@@|$CANDIDATE_TIMEOUT_SECONDS|g" \
    -e "s|@@PRICES_JSON@@|$PRICES_JSON|g" \
    -e "s|@@FULL_MODEL@@|$FULL_MODEL|g" \
    -e "s|@@FULL_MODEL_REVISION@@|$FULL_MODEL_REVISION|g" \
    -e "s|@@FULL_MAX_MODEL_LEN@@|$FULL_MAX_MODEL_LEN|g" \
    -e "s|@@FULL_GPU_MEMORY_FRACTION@@|$FULL_GPU_MEMORY_FRACTION|g" \
    "$1"
}
