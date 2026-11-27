#!/usr/bin/env bash
# Phase 9. Deletes everything the test created in AWS and then checks that
# nothing billable is left. It needs nothing from the other phases and may be
# run alone at any time, also while the cluster is still being created, and
# as often as needed.
#
# Without --yes it prints what it would delete and deletes nothing.
# Exit code 0 only when 91-verify-clean.sh finds nothing billable; 6 when
# another run of this phase (95-watchdog.sh's, or a person's) is under way.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source=91-verify-clean.sh
. "$TOOL_DIR/91-verify-clean.sh"

YES=no
[ "${1:-}" = "--yes" ] && YES=yes

# One deletion at a time: the watchdog and a person may start one together.
if [ "$YES" = yes ]; then
  DELETE_PID_FILE="$STATE_DIR/delete.pid"
  if [ -f "$DELETE_PID_FILE" ] && kill -0 "$(cat "$DELETE_PID_FILE")" 2>/dev/null; then
    say "90-delete.sh is running already (pid $(cat "$DELETE_PID_FILE")); this run stops and deletes nothing."
    exit 6
  fi
  printf '%s\n' "$$" >"$DELETE_PID_FILE"
  trap 'rm -f -- "$DELETE_PID_FILE"' EXIT
fi

stacks() {
  aws cloudformation describe-stacks --query "Stacks[?starts_with(StackName, 'eksctl-$CLUSTER-')].StackName" --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$' || true
}

wait_nodegroups_settled() {
  local give_up=$((SECONDS + 2400)) group creating
  while :; do
    creating=""
    for group in "$GPU_NODEGROUP" "$SYSTEM_NODEGROUP"; do
      if [ "$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$group" --query nodegroup.status --output text 2>/dev/null || true)" = CREATING ]; then
        creating="$creating $group"
      fi
    done
    [ -n "$creating" ] || return 0
    if [ "$SECONDS" -ge "$give_up" ]; then
      say "  still being created after 40 minutes:$creating"
      return 0
    fi
    say "  EKS is still creating:$creating; checking again in a minute"
    sleep 60
  done
}

delete_phase() {
  need_tools aws eksctl kubectl

  local status
  status=$(cluster_status)
  step "What this phase deletes in AWS ($REGION)"
  say "  Capacity reservations tagged project=$PROJECT_TAG (first, so an unused one stops billing at once)"
  say "  EKS cluster $CLUSTER (now: $status), its node groups, instances, disks and network interfaces"
  say "  CloudFormation stacks eksctl-$CLUSTER-*: the VPC, subnets, security groups, launch templates and IAM roles"
  say "  ECR repository $ECR_REPOSITORY and the image in it"
  say "  On this machine: the cluster's kubeconfig and the test's keys in $STATE_DIR"
  say "  Kept: the evidence and run folders in $STATE_DIR, and AWS's service-linked roles (free)"
  if [ "$YES" != yes ]; then
    say ""
    say "Nothing was deleted. Run again with --yes to delete."
    return 0
  fi

  # A reservation bills whether or not an instance uses it. Cancelling it does
  # not stop the GPU node; the cluster's deletion below does.
  step "Cancelling capacity reservations"
  local reservations reservation
  if reservations=$(aws ec2 describe-capacity-reservations --filters "Name=tag:project,Values=$PROJECT_TAG" "Name=state,Values=active,pending" \
    --query 'CapacityReservations[].CapacityReservationId' --output json 2>&1); then
    reservations=$(printf '%s' "$reservations" | python3 -c 'import json, sys; print(" ".join(json.load(sys.stdin)))')
    for reservation in $reservations; do
      aws ec2 cancel-capacity-reservation --capacity-reservation-id "$reservation" --query Return --output text >/dev/null &&
        say "  cancelled $reservation" || say "  $reservation could not be cancelled; the clean check below will name it"
    done
    [ -n "$reservations" ] || say "  none"
  else
    say "  could not be read, so nothing was cancelled: $(printf '%s' "$reservations" | head -c 200)"
  fi
  if [ "$status" != ABSENT ]; then
    # Kubernetes can create AWS load balancers and volumes of its own. The
    # test makes none; if someone added one by hand it is removed first, so
    # it cannot outlive the cluster or block the VPC's deletion.
    if [ "$status" = ACTIVE ] && aws eks update-kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1; then
      step "Kubernetes objects that own AWS resources"
      local owned
      owned=$(kc get services --all-namespaces -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
      if [ -n "$owned" ]; then
        say "  deleting LoadBalancer services: $owned"
        kc delete services --all-namespaces --field-selector spec.type=LoadBalancer --wait=true --timeout=180s || true
      fi
      if [ -n "$(kc get persistentvolumeclaims --all-namespaces -o name 2>/dev/null || true)" ]; then
        say "  deleting persistent volume claims"
        kc delete persistentvolumeclaims --all --all-namespaces --wait=true --timeout=180s || true
      fi
      [ -n "$owned" ] || say "  none"
    fi

    step "Deleting the cluster (about 10 to 15 minutes)"
    eksctl delete cluster --name "$CLUSTER" --region "$REGION" --disable-nodegroup-eviction --force --wait --timeout 30m ||
      say "  eksctl reported an error; the stacks are deleted directly below"
  fi

  # Everything below reads AWS, and the deletion above can take a quarter of an
  # hour. If the sign-in has ended meanwhile, stop here with code 3: a refused
  # call must never read as "nothing there" (second cluster, 2026-11-23).
  aws_account

  # After a create that was stopped early a node group can still be in EKS's
  # hands. EKS does not let go of one it is still creating before its own
  # attempt ends, about 33 minutes after it started (third cluster, 24 Nov
  # 2026), so wait until it has, and let the stack deletion below remove what
  # is left.
  if [ "$status" != ABSENT ]; then
    wait_nodegroups_settled
  fi

  # Whatever eksctl left (a failed or half-made stack): node groups first,
  # then the cluster stack, which owns the VPC.
  local left stack
  left=$(stacks)
  if [ -n "$left" ]; then
    step "Deleting the remaining CloudFormation stacks"
    for stack in $(printf '%s\n' "$left" | grep -- '-nodegroup-' || true); do
      say "  $stack"
      aws cloudformation delete-stack --stack-name "$stack"
    done
    for stack in $(printf '%s\n' "$left" | grep -- '-nodegroup-' || true); do
      aws cloudformation wait stack-delete-complete --stack-name "$stack" || say "  $stack did not finish deleting"
    done
    for stack in $(printf '%s\n' "$left" | grep -v -- '-nodegroup-' || true); do
      say "  $stack"
      aws cloudformation delete-stack --stack-name "$stack"
      aws cloudformation wait stack-delete-complete --stack-name "$stack" || say "  $stack did not finish deleting"
    done
  fi

  step "Deleting the ECR repository"
  local repository
  if repository=$(aws ecr describe-repositories --repository-names "$ECR_REPOSITORY" --query 'repositories[0].repositoryName' --output text 2>&1); then
    aws ecr delete-repository --repository-name "$ECR_REPOSITORY" --force --query 'repository.repositoryName' --output text
  elif printf '%s' "$repository" | grep -q RepositoryNotFoundException; then
    say "  none"
  else
    say "  could not be read, so nothing was deleted: $repository"
  fi

  # Network interfaces and volumes can take a minute to disappear after their
  # instance; check until clean, for at most ten minutes.
  local attempt result=5
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    LEFT=0
    if verify_clean >"$STATE_DIR/verify-clean.txt" 2>&1; then
      result=0
      break
    fi
    say "  not clean yet (check $attempt of 10); waiting a minute"
    sleep 60
  done
  cat "$STATE_DIR/verify-clean.txt"

  if [ "$result" -ne 0 ]; then
    cat <<EOF

SOMETHING IS STILL BILLED. The lines marked LEFT above name it. Run this script
again; if the same lines stay, delete those resources by their IDs in the EC2
and CloudFormation consoles of $REGION, then run 91-verify-clean.sh.
EOF
    return 5
  fi

  # Close this cluster's record: its whole lifetime counts against the budget.
  # If the cluster was already gone when this run began, an earlier run was
  # cut off after it had asked AWS to delete it; the lifetime then ends when
  # AWS finished that, not whenever someone could sign in and run this again.
  local created now ended gone
  created=$(state_get CREATED_AT)
  now=$(date +%s)
  ended=$now
  if [ -n "$created" ]; then
    if [ "$status" = ABSENT ]; then
      gone=$(cluster_gone_at "$created" "$now")
      [ -z "$gone" ] || ended=$gone
    fi
    printf '%s\t%s\t%s\n' "$created" "$ended" "$(money "($ended - $created) / 3600 * $(rate_guard)")" >>"$SPEND_FILE"
    say ""
    if [ "$ended" != "$now" ]; then
      say "AWS finished deleting the cluster at $(iso "$ended"); its lifetime is counted until then."
    fi
    say "This cluster lived $(money "($ended - $created) / 3600") hours: at most \$$(money "($ended - $created) / 3600 * $(rate_list)") at list price (every hour counted with the dearest GPU type allowed)."
  fi
  for key in CREATED_AT DELETE_BY CLUSTER_REGION CLUSTER_GPU_INSTANCE IMAGE_DIGEST IMAGE_COMMIT AGENT_ID AGENT_NAME GPU_PRODUCT CANDIDATE_RUN_ID RUN_STARTED_AT RUN_FINISHED_AT; do
    state_unset "$key"
  done
  rm -f "$KUBECONFIG" "$STATE_DIR/api-key" "$STATE_DIR/auth-header" "$STATE_DIR/app-auth-header" "$STATE_DIR/postgres-password" \
    "$STATE_DIR/server-token" "$STATE_DIR/tls.key" "$STATE_DIR/tls.crt" "$STATE_DIR"/port-forward-*
  say "Total counted for this test so far: \$$(spent_closed) of \$$BUDGET_USD."
  say "Deleted, and checked: nothing billable of this test is left."
}

run_masked delete_phase
