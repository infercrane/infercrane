#!/usr/bin/env bash
# Phase 2. Reserves the GPU, then creates the EKS cluster and its two nodes.
# This is the phase that starts the bill: everything it creates is listed,
# with its hourly price, before anything happens.
#
# The GPU comes first. An on-demand capacity reservation is answered at once:
# a refusal costs nothing and ends the phase before anything exists, where a
# node group needs about 33 minutes to give up (23 and 24 Nov). Once granted,
# the reservation is billed like the instance, ends by itself at the
# deadline, and the GPU node (in the same zone) uses it without naming it.
#
# Without --yes it prints the plan and creates nothing.
# Safe to run again: an existing cluster is kept, a missing node group is
# added, and the time limit set by the first run is never extended.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

YES=no
[ "${1:-}" = "--yes" ] && YES=yes

nodegroup_status() {
  local status
  if status=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$1" --query nodegroup.status --output text 2>&1); then
    printf '%s' "$status"
  else
    printf 'ABSENT'
  fi
}

print_plan() {
  local lifetime_hours=$1
  step "What this phase creates in AWS ($REGION)"
  cat <<EOF
  First, and alone: an on-demand capacity reservation for 1 x $GPU_INSTANCES (Linux),
    "open", in the first of $NODE_ZONES that has one, ending by itself at the deadline.
    If no zone has one, the phase stops here and nothing exists.
  CloudFormation stack eksctl-$CLUSTER-cluster
    EKS cluster $CLUSTER, Kubernetes 1.36, public endpoint, no logging
    VPC 192.168.0.0/16, internet gateway, one public and one private subnet in
      each of three zones: the reserved one and two more of $NODE_ZONES,
      route tables; no NAT gateway, no elastic IP
    2 security groups (control plane, nodes); EKS adds one of its own
    IAM role eksctl-$CLUSTER-cluster-ServiceRole-<suffix> (the cluster's role)
  CloudFormation stack eksctl-$CLUSTER-nodegroup-$GPU_NODEGROUP
    IAM role $PROJECT_TAG-$GPU_NODEGROUP-node, a launch template
    managed node group "$GPU_NODEGROUP": 1 x $GPU_INSTANCES in the reserved zone, $GPU_DISK_GB GB gp3,
      EKS AL2023 NVIDIA AMI, one public IPv4 address
  CloudFormation stack eksctl-$CLUSTER-nodegroup-$SYSTEM_NODEGROUP
    IAM role $PROJECT_TAG-$SYSTEM_NODEGROUP-node, a launch template
    managed node group "$SYSTEM_NODEGROUP": 1 x $SYSTEM_INSTANCE in any zone, $SYSTEM_DISK_GB GB gp3,
      EKS AL2023 arm64 AMI, one public IPv4 address
  Made by EKS for the node groups: 2 Auto Scaling groups, 2 launch templates,
    2 instances with their disks and network interfaces
  EKS add-ons eksctl installs by default: vpc-cni, kube-proxy, coredns, and
    in current eksctl versions metrics-server
  An access entry that makes the signed-in identity the cluster's admin
  One scheduled action on each Auto Scaling group: scale to zero at the deadline
  Created once per account by AWS and kept afterwards (free): the service-linked
    roles AWSServiceRoleForAmazonEKS, AWSServiceRoleForAmazonEKSNodegroup and
    AWSServiceRoleForAutoScaling
  Every resource is tagged project=$PROJECT_TAG.
  Not created: NAT gateway, load balancer, elastic IP, OIDC provider, KMS key,
    log group, persistent volume, IAM user, access key.

EOF
  cost_table
  cat <<EOF

  Time limit: this cluster may live $lifetime_hours hours, at most \$$(money "$lifetime_hours * $(rate_guard)").
  Every later phase refuses to start if it could run into the last $DELETE_MINUTES minutes.
  At the deadline AWS itself scales both node groups to zero; only the control
  plane (\$$PRICE_EKS_CONTROL_PLANE an hour) is then left until 90-delete.sh runs.
EOF
}

# deadline_actions makes AWS itself scale every node group that has started
# to zero at the deadline ($1). It runs after a failed create: a system node
# that did start must not need this machine, or a valid sign-in, to stop. It
# never fails the phase.
deadline_actions() {
  local end=$1 group asg found=no
  for group in "$SYSTEM_NODEGROUP" "$GPU_NODEGROUP"; do
    asg=$(nodegroup_asg "$group")
    [ -n "$asg" ] || continue
    found=yes
    if [ "$end" -gt $(($(date +%s) + 120)) ]; then
      aws autoscaling put-scheduled-update-group-action --auto-scaling-group-name "$asg" \
        --scheduled-action-name "$PROJECT_TAG-deadline" --start-time "$(iso "$end")" --desired-capacity 0 ||
        say "  $group: the scale-to-zero action could not be set"
    fi
    say "  $group: scale to zero at $(aws autoscaling describe-scheduled-actions --auto-scaling-group-name "$asg" --scheduled-action-names "$PROJECT_TAG-deadline" --query 'ScheduledUpdateGroupActions[0].StartTime' --output text 2>/dev/null || echo unknown)"
  done
  [ "$found" = yes ] || say "  none has an Auto Scaling group"
}

# reserve_gpu asks for one GPU instance in each zone in turn, until AWS grants
# one, and records the reservation and its zone. It ends at $1 (epoch
# seconds). It reuses a reservation an earlier run of this phase recorded.
# Return code 7: no zone has the instance; nothing was created.
reserve_gpu() {
  local end=$1 zone answer id state recorded
  recorded=$(state_get GPU_RESERVATION)
  if [ -n "$recorded" ]; then
    state=$(aws ec2 describe-capacity-reservations --capacity-reservation-ids "$recorded" \
      --query 'CapacityReservations[0].State' --output text 2>/dev/null || true)
    if [ "$state" = active ]; then
      say "  reusing reservation $recorded in $(state_get GPU_ZONE)"
      return 0
    fi
    state_unset GPU_RESERVATION
    state_unset GPU_ZONE
  fi
  for zone in $NODE_ZONES; do
    if answer=$(aws ec2 create-capacity-reservation --instance-type "$GPU_INSTANCES" --instance-platform Linux/UNIX \
      --availability-zone "$zone" --instance-count 1 --instance-match-criteria open --tenancy default \
      --end-date-type limited --end-date "$(iso "$end")" \
      --tag-specifications "ResourceType=capacity-reservation,Tags=[{Key=project,Value=$PROJECT_TAG}]" \
      --query 'CapacityReservation.[CapacityReservationId,State]' --output text 2>&1); then
      id=$(printf '%s' "$answer" | awk '{print $1}')
      state=$(printf '%s' "$answer" | awk '{print $2}')
      printf '%s\t%s\tgranted\t%s\n' "$(date -u +%FT%TZ)" "$zone" "$id $state" >>"$STATE_DIR/reservation-attempts.txt"
      state_set GPU_RESERVATION "$id"
      state_set GPU_ZONE "$zone"
      local waited=0
      while [ "$state" = pending ] && [ "$waited" -lt 180 ]; do
        sleep 10
        waited=$((waited + 10))
        state=$(aws ec2 describe-capacity-reservations --capacity-reservation-ids "$id" \
          --query 'CapacityReservations[0].State' --output text 2>/dev/null || echo pending)
      done
      if [ "$state" = active ]; then
        say "  $zone: reservation $id active"
        return 0
      fi
      say "  $zone: reservation $id is $state; cancelling it and trying the next zone"
      aws ec2 cancel-capacity-reservation --capacity-reservation-id "$id" >/dev/null 2>&1 || true
      state_unset GPU_RESERVATION
      state_unset GPU_ZONE
      continue
    fi
    printf '%s\t%s\trefused\t%s\n' "$(date -u +%FT%TZ)" "$zone" "$(printf '%s' "$answer" | tr '\n' ' ')" >>"$STATE_DIR/reservation-attempts.txt"
    say "  $zone: $(printf '%s' "$answer" | tr '\n' ' ' | cut -c1-200)"
    case "$answer" in
      *InsufficientInstanceCapacity* | *InsufficientCapacity* | *Unsupported*) ;;
      *) die "AWS answered something other than \"no capacity\"; stopping before anything else is asked for" ;;
    esac
  done
  return 7
}

# cluster_config writes the eksctl config with the reserved zone filled in and
# prints its path. eksctl never reads the template itself.
cluster_config() {
  local zone path="$STATE_DIR/cluster.yaml"
  zone=$(state_get GPU_ZONE)
  [ -n "$zone" ] || die "no GPU zone is recorded; the reservation step has not run"
  render_cluster_config "$zone" >"$path"
  printf '%s' "$path"
}

create_phase() {
  need_tools aws eksctl kubectl python3

  local status spent affordable lifetime_hours now created end create_code=0
  case "$MAX_LIFETIME_HOURS" in
    '' | *[!0-9.]* | *.*.*) die "EKS_PROOF_MAX_LIFETIME_HOURS must be a number of hours, for example 2.5" ;;
  esac
  if awk "BEGIN { exit !($MAX_LIFETIME_HOURS > $MAX_LIFETIME_LIMIT || $MAX_LIFETIME_HOURS < $MIN_USEFUL_HOURS) }"; then
    die "EKS_PROOF_MAX_LIFETIME_HOURS is $MAX_LIFETIME_HOURS; it must be between $MIN_USEFUL_HOURS and $MAX_LIFETIME_LIMIT"
  fi
  status=$(cluster_status)
  spent=$(spent_closed)
  affordable=$(calc "($BUDGET_USD - $spent) / $(rate_guard)")
  lifetime_hours=$(awk "BEGIN { h = ($affordable < $MAX_LIFETIME_HOURS) ? $affordable : $MAX_LIFETIME_HOURS; printf \"%.2f\", h }")

  case "$status" in
    ABSENT)
      print_plan "$lifetime_hours"
      say ""
      say "  Budget \$$BUDGET_USD; spent by earlier clusters of this test: \$$spent."
      if awk "BEGIN { exit !($lifetime_hours < $MIN_USEFUL_HOURS) }"; then
        cat >&2 <<EOF
REFUSED: \$$spent of \$$BUDGET_USD is spent. What is left pays for $lifetime_hours hours,
less than the $MIN_USEFUL_HOURS hours a run needs. No cluster is created.
EOF
        exit 4
      fi
      if [ -n "$(state_get CREATED_AT)" ]; then
        die "the local state still records a cluster created at $(iso "$(state_get CREATED_AT)"), but none exists. Run 90-delete.sh --yes once: it checks that nothing is left and closes that record"
      fi
      if [ "$YES" != yes ]; then
        say ""
        say "Nothing was created. Run again with --yes to create it."
        return 0
      fi
      # launch.sh builds the image while the cluster is created, once the GPU
      # is reserved, and says so with EKS_PROOF_IMAGE_PENDING=1.
      [ -n "${EKS_PROOF_IMAGE:-}" ] || [ -n "$(state_get IMAGE_DIGEST)" ] || [ "${EKS_PROOF_IMAGE_PENDING:-}" = 1 ] ||
        die "no image is ready; run launch.sh, or 10-image.sh --yes first, so no cluster time is spent waiting for a build"
      now=$(date +%s)
      end=$(awk "BEGIN { printf \"%d\", $now + $lifetime_hours * 3600 }")
      local auth needed
      auth=$(auth_until)
      if [ -n "$auth" ]; then
        [ "$auth" -ge "$end" ] || end=$auth
        needed=$(awk "BEGIN { printf \"%d\", $FULL_RUN_HOURS * 3600 + $DELETE_MINUTES * 60 }")
        if [ $((end - now)) -lt "$needed" ]; then
          say ""
          say "REFUSED: the AWS sign-in ends at $(iso "$auth"), $(((auth - now) / 60)) minutes from now; a run needs"
          say "$((needed / 60)) minutes ($FULL_RUN_HOURS hours and $DELETE_MINUTES minutes to delete). Nothing was asked for or created."
          exit 8
        fi
        say "  The AWS sign-in ends at $(iso "$auth"); the deadline is $(iso "$end")."
      fi
      state_set CREATED_AT "$now"
      state_set DELETE_BY "$end"
      state_set CLUSTER_REGION "$REGION"
      state_set CLUSTER_GPU_INSTANCE "$GPU_INSTANCES"
      step "Reserving one $GPU_INSTANCES (billed from the moment AWS grants it)"
      local reserve_code=0
      reserve_gpu "$end" || reserve_code=$?
      if [ "$reserve_code" -ne 0 ]; then
        # Nothing was created: the record of this attempt is closed at zero.
        state_unset CREATED_AT
        state_unset DELETE_BY
        state_unset CLUSTER_REGION
        state_unset CLUSTER_GPU_INSTANCE
        say ""
        say "No zone of $REGION has a $GPU_INSTANCES for this account now. Nothing was created; nothing is billed."
        say "Every answer: $STATE_DIR/reservation-attempts.txt. Trying again later costs nothing."
        exit "$reserve_code"
      fi
      step "Creating the cluster (about 15 to 20 minutes). Deadline: $(iso "$end")"
      eksctl create cluster --config-file "$(cluster_config)" --install-nvidia-plugin=false \
        --kubeconfig "$KUBECONFIG" --timeout 35m &
      watch_launches $! || create_code=$?
      if [ "$create_code" -ne 0 ]; then
        step "What AWS recorded about the node groups"
        create_failures | sed 's/^/  /'
        launch_refusals | sed -n '1,6p' | sed 's/^/  /'
        step "Node groups that did start get the deadline too"
        deadline_actions "$end"
        say ""
        say "The cluster was not created (code $create_code; 7 means AWS refused to start a node)."
        say "Next: 50-collect.sh, then 90-delete.sh --yes. README.md, \"When a phase fails\", says what to change."
        exit "$create_code"
      fi
      ;;
    ACTIVE)
      say "Cluster $CLUSTER exists already; nothing new is billed by this run."
      [ "$YES" = yes ] || {
        say "Run again with --yes to repair node groups, the deadline tag and the scale-to-zero actions."
        return 0
      }
      ;;
    *)
      die "cluster $CLUSTER is $status. Wait until it is ACTIVE, or run 90-delete.sh --yes"
      ;;
  esac

  need_cluster
  created=$(state_get CREATED_AT)
  end=$(deadline)
  [ -n "$end" ] || die "the cluster exists but no deadline is recorded here or on the cluster. Run 90-delete.sh --yes"

  step "Recording the deadline on the cluster"
  aws eks tag-resource --resource-arn "$(aws eks describe-cluster --name "$CLUSTER" --query cluster.arn --output text)" \
    --tags "infercrane-created-at=${created:-unknown},infercrane-delete-by=$end"
  state_set DELETE_BY "$end"
  say "  infercrane-delete-by=$end ($(iso "$end"))"

  step "Node groups"
  local group group_status asg
  for group in "$SYSTEM_NODEGROUP" "$GPU_NODEGROUP"; do
    group_status=$(nodegroup_status "$group")
    if [ "$group_status" = ABSENT ]; then
      guard_phase "add node group $group" 15
      eksctl create nodegroup --config-file "$(cluster_config)" --include "$group" --install-nvidia-plugin=false
      group_status=$(nodegroup_status "$group")
    fi
    say "  $group: $group_status"
    [ "$group_status" = ACTIVE ] || die "node group $group is $group_status. See README.md, \"When a phase fails\""
    asg=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$group" --query 'nodegroup.resources.autoScalingGroups[0].name' --output text)
    if [ "$end" -gt $(($(date +%s) + 120)) ]; then
      aws autoscaling put-scheduled-update-group-action --auto-scaling-group-name "$asg" \
        --scheduled-action-name "$PROJECT_TAG-deadline" --start-time "$(iso "$end")" --desired-capacity 0
    fi
    say "    scale to zero at: $(aws autoscaling describe-scheduled-actions --auto-scaling-group-name "$asg" --scheduled-action-names "$PROJECT_TAG-deadline" --query 'ScheduledUpdateGroupActions[0].StartTime' --output text)"
  done

  step "Nodes"
  kc wait --for=condition=Ready nodes --all --timeout=600s
  kc get nodes -L node.kubernetes.io/instance-type,infercrane.dev/pool,topology.kubernetes.io/zone

  step "The GPU node and its reservation"
  local reservation
  reservation=$(state_get GPU_RESERVATION)
  aws ec2 describe-capacity-reservations --capacity-reservation-ids "$reservation" \
    --query 'CapacityReservations[0].[CapacityReservationId,AvailabilityZone,State,TotalInstanceCount,AvailableInstanceCount,EndDate]' \
    --output text | sed 's/^/  /' | tee "$STATE_DIR/reservation.txt"
  if [ "$(aws ec2 describe-capacity-reservations --capacity-reservation-ids "$reservation" --query 'CapacityReservations[0].AvailableInstanceCount' --output text)" != 0 ]; then
    say "  WARNING: the GPU node does not use the reservation, so both are billed. 90-delete.sh cancels it; the run goes on."
  fi

  step "What CloudFormation created"
  local stack
  for stack in "eksctl-$CLUSTER-cluster" "eksctl-$CLUSTER-nodegroup-$GPU_NODEGROUP" "eksctl-$CLUSTER-nodegroup-$SYSTEM_NODEGROUP"; do
    say "  $stack"
    aws cloudformation describe-stack-resources --stack-name "$stack" \
      --query 'StackResources[].[ResourceType,LogicalResourceId,PhysicalResourceId]' --output text | sed 's/^/    /'
  done | tee "$STATE_DIR/created-resources.txt"

  echo
  say "Cluster ready. It must be gone by $(iso "$end"): $(minutes_left) minutes from now."
  say "Next: tools/eks-agent-run/30-install.sh"
}

run_masked create_phase
