#!/usr/bin/env bash
# Checks that nothing of this test is left in AWS. Reads only; costs nothing.
# Run by 90-delete.sh at its end, and safe to run alone at any time.
#
# It looks for what the test creates in three ways, because a tag can be
# missing on a resource that Kubernetes or EKS made on the test's behalf:
#   1. by name (the cluster, its CloudFormation stacks, the ECR repository,
#      the IAM roles);
#   2. by tag (project, and the tags EKS and eksctl put on what they create);
#   3. by counting every instance, volume, network interface, load balancer,
#      NAT gateway, elastic IP and VPC in the region against the numbers
#      00-preflight.sh recorded before anything was created.
#
# Exit code 0: nothing billable is left. Exit code 5: something is.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# What the region held on 2026-11-23, before this test: one default VPC, and
# one idle elastic IP that belongs to an August stack
# (infercrane-cache-qual-20260824), not to this test.
BASELINE_INSTANCES=0
BASELINE_VOLUMES=0
BASELINE_ENIS=0
BASELINE_LOAD_BALANCERS=0
BASELINE_NAT_GATEWAYS=0
BASELINE_EIPS=1
BASELINE_VPCS=1
BASELINE_CAPACITY_RESERVATIONS=0
# Preflight records the counts per region; the first run's file had no
# region in its name and was for eu-central-1.
if [ -f "$STATE_DIR/baseline-$REGION.env" ]; then
  # shellcheck disable=SC1091
  . "$STATE_DIR/baseline-$REGION.env"
elif [ "$REGION" = eu-central-1 ] && [ -f "$STATE_DIR/baseline.env" ]; then
  # shellcheck disable=SC1091
  . "$STATE_DIR/baseline.env"
fi

LEFT=0

# found LABEL BILLABLE IDS... records one finding.
found() {
  local label=$1 billable=$2
  shift 2
  local ids="$*"
  if [ -z "$ids" ] || [ "$ids" = None ]; then
    printf '  %-46s none\n' "$label"
    return
  fi
  if [ "$billable" = yes ]; then
    LEFT=1
    printf '  %-46s LEFT (billable): %s\n' "$label" "$ids"
  else
    printf '  %-46s left (free): %s\n' "$label" "$ids"
  fi
}

# over LABEL COUNT BASELINE flags a region-wide count above its baseline.
over() {
  local label=$1 count=$2 baseline=$3
  case "$count" in
    '' | *[!0-9]*)
      LEFT=1
      printf '  %-46s CHECK-FAILED: the count could not be read\n' "$label"
      return
      ;;
  esac
  if [ "$count" -gt "$baseline" ]; then
    LEFT=1
    printf '  %-46s %s, was %s before the test: LEFT (billable)\n' "$label" "$count" "$baseline"
  else
    printf '  %-46s %s (was %s before the test)\n' "$label" "$count" "$baseline"
  fi
}

# text prints an AWS answer on one line. "Not found" is an empty answer. Any
# other failure prints CHECK-FAILED, which counts as something left: a check
# that could not run never reads as clean.
text() {
  local out
  if out=$(aws "$@" --output text 2>&1); then
    printf '%s' "$out" | tr '\t\n' '  ' | sed 's/  */ /g; s/^ //; s/ $//'
  elif printf '%s' "$out" | grep -Eq 'ResourceNotFoundException|RepositoryNotFoundException|No cluster found'; then
    return 0
  else
    printf 'CHECK-FAILED(%s)' "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-120)"
  fi
}

# number prints a count, or nothing when the call failed.
number() { aws "$@" --output text 2>/dev/null || true; }

# by_tags prints the IDs an EC2 describe call returns for any of the tags the
# test, eksctl, EKS or the VPC CNI put on a resource.
by_tags() {
  local call=$1 query=$2 extra=${3:-} filter ids="" flag=--filters
  # describe-nat-gateways alone spells the option --filter.
  [ "$call" = describe-nat-gateways ] && flag=--filter
  for filter in \
    "Name=tag:project,Values=$PROJECT_TAG" \
    "Name=tag:eks:cluster-name,Values=$CLUSTER" \
    "Name=tag:alpha.eksctl.io/cluster-name,Values=$CLUSTER" \
    "Name=tag:kubernetes.io/cluster/$CLUSTER,Values=owned,shared" \
    "Name=tag:cluster.k8s.amazonaws.com/name,Values=$CLUSTER"; do
    # shellcheck disable=SC2086
    ids="$ids $(text ec2 "$call" "$flag" "$filter" $extra --query "$query")"
  done
  # A failed call must stay visible as one finding, not be split into words.
  # Its first error is kept: on 26 Nov four of these failed once and passed
  # on the next read, and the reason was not recorded.
  case "$ids" in *CHECK-FAILED*)
    printf 'CHECK-FAILED: %s could not be read: %s' "$call" "$(printf '%s' "$ids" | sed -n 's/.*\(CHECK-FAILED([^)]*)\).*/\1/p' | cut -c1-160)"
    return
    ;;
  esac
  printf '%s' "$ids" | tr ' ' '\n' | grep -v '^$' | grep -v '^None$' | sort -u | tr '\n' ' ' | sed 's/ $//'
}

# count_reservation_records keeps every ARN of its input except capacity
# reservations, which it replaces by their number. Each refused reservation
# request stays in the account as a "failed" record AWS keeps for a while (26
# Nov: 192 refusals in eu-central-1 left 175 tagged records); they cost nothing. A reservation
# that is active or pending is found by the line above and by the region count.
count_reservation_records() {
  local arns others n
  arns=$(cat)
  case "$arns" in CHECK-FAILED*) printf '%s' "$arns"; return ;; esac
  n=$(printf '%s' "$arns" | tr ' ' '\n' | grep -c ':capacity-reservation/' || true)
  others=$(printf '%s' "$arns" | tr ' ' '\n' | grep -v ':capacity-reservation/' | grep -v '^$' | tr '\n' ' ' | sed 's/ $//')
  [ "$n" -gt 0 ] && others="${others:+$others }$n capacity reservation records (refused or ended requests AWS keeps; not billed)"
  printf '%s' "$others"
}

verify_clean() {
  local live='Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down'

  step "By name"
  found "EKS cluster $CLUSTER" yes "$(text eks describe-cluster --name "$CLUSTER" --query 'cluster.[name,status]')"
  found "EKS node groups" yes "$(text eks list-nodegroups --cluster-name "$CLUSTER" --query 'nodegroups')"
  found "CloudFormation stacks eksctl-$CLUSTER-*" yes "$(text cloudformation describe-stacks --query "Stacks[?starts_with(StackName, 'eksctl-$CLUSTER-')].[StackName,StackStatus]")"
  found "ECR repository $ECR_REPOSITORY" yes "$(text ecr describe-repositories --repository-names "$ECR_REPOSITORY" --query 'repositories[].repositoryName')"
  found "CloudWatch log groups /aws/eks/$CLUSTER/" yes "$(text logs describe-log-groups --log-group-name-prefix "/aws/eks/$CLUSTER/" --query 'logGroups[].logGroupName')"
  found "IAM roles of the test" no "$(text iam list-roles --query "Roles[?starts_with(RoleName, 'eksctl-$CLUSTER-') || starts_with(RoleName, '$PROJECT_TAG-')].RoleName")"

  step "By tag"
  found "EC2 instances" yes "$(by_tags describe-instances 'Reservations[].Instances[].InstanceId' "$live")"
  found "EBS volumes" yes "$(by_tags describe-volumes 'Volumes[].VolumeId')"
  found "Network interfaces" yes "$(by_tags describe-network-interfaces 'NetworkInterfaces[].NetworkInterfaceId')"
  found "Elastic IPs" yes "$(by_tags describe-addresses 'Addresses[].AllocationId')"
  found "NAT gateways" yes "$(by_tags describe-nat-gateways 'NatGateways[?State!=`deleted`].NatGatewayId')"
  found "VPCs" no "$(by_tags describe-vpcs 'Vpcs[].VpcId')"
  found "Security groups" no "$(by_tags describe-security-groups 'SecurityGroups[].GroupId')"
  found "Launch templates" no "$(by_tags describe-launch-templates 'LaunchTemplates[].LaunchTemplateId')"
  found "Capacity reservations (active or pending)" yes "$(text ec2 describe-capacity-reservations --filters "Name=tag:project,Values=$PROJECT_TAG" "Name=state,Values=active,pending" --query 'CapacityReservations[].CapacityReservationId')"
  found "Auto Scaling groups" yes "$(text autoscaling describe-auto-scaling-groups --query "AutoScalingGroups[?Tags[?Key=='eks:cluster-name' && Value=='$CLUSTER'] || Tags[?Key=='project' && Value=='$PROJECT_TAG']].AutoScalingGroupName")"
  found "Load balancers tagged for the cluster" yes "$(text resourcegroupstaggingapi get-resources --resource-type-filters elasticloadbalancing:loadbalancer --tag-filters "Key=kubernetes.io/cluster/$CLUSTER" --query 'ResourceTagMappingList[].ResourceARN')"
  # The tagging API can list a resource for some minutes after it is gone, so
  # this line alone does not fail the check; the lines above and below do.
  found "Anything else tagged project=$PROJECT_TAG" no "$(text resourcegroupstaggingapi get-resources --tag-filters "Key=project,Values=$PROJECT_TAG" --query 'ResourceTagMappingList[].ResourceARN' | count_reservation_records)"

  step "Whole region against the count before the test"
  local load_balancers
  load_balancers="$(number elbv2 describe-load-balancers --query 'length(LoadBalancers)')+$(number elb describe-load-balancers --query 'length(LoadBalancerDescriptions)')"
  case "$load_balancers" in [0-9]*+[0-9]*) load_balancers=$((load_balancers)) ;; *) load_balancers="" ;; esac
  over "EC2 instances not terminated" "$(number ec2 describe-instances --filters "$live" --query 'length(Reservations[].Instances[])')" "$BASELINE_INSTANCES"
  over "EBS volumes" "$(number ec2 describe-volumes --query 'length(Volumes)')" "$BASELINE_VOLUMES"
  over "Network interfaces" "$(number ec2 describe-network-interfaces --query 'length(NetworkInterfaces)')" "$BASELINE_ENIS"
  over "Load balancers (all kinds)" "$load_balancers" "$BASELINE_LOAD_BALANCERS"
  over "NAT gateways" "$(number ec2 describe-nat-gateways --filter 'Name=state,Values=pending,available,deleting' --query 'length(NatGateways)')" "$BASELINE_NAT_GATEWAYS"
  over "Capacity reservations (active or pending)" "$(number ec2 describe-capacity-reservations --filters 'Name=state,Values=active,pending' --query 'length(CapacityReservations)')" "$BASELINE_CAPACITY_RESERVATIONS"
  over "Elastic IPs" "$(number ec2 describe-addresses --query 'length(Addresses)')" "$BASELINE_EIPS"
  over "VPCs" "$(number ec2 describe-vpcs --query 'length(Vpcs)')" "$BASELINE_VPCS"

  step "Left on purpose (free)"
  say "  Service-linked roles AWS creates once per account and keeps:"
  say "    $(text iam list-roles --path-prefix /aws-service-role/ --query "Roles[?contains(RoleName, 'EKS') || contains(RoleName, 'AutoScaling')].RoleName")"

  echo
  if [ "$LEFT" -eq 0 ]; then
    say "RESULT: nothing billable of this test is left in $REGION."
    return 0
  fi
  say "RESULT: something billable is LEFT. See the lines marked LEFT above."
  return 5
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  need_tools aws perl
  run_masked verify_clean
fi
