#!/usr/bin/env bash
# Phase 0. Reads only: creates nothing and costs nothing. Run it first, and
# again right before 20-create.sh.
#
# It checks the tools, the AWS sign-in, the quotas and the zones, records what
# the region holds before the test (the baseline 91-verify-clean.sh compares
# against), and dry-runs the cluster config, the Helm charts and the
# manifests.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

quota() { aws service-quotas get-service-quota --service-code ec2 --quota-code "$1" --query 'Quota.Value' --output text | cut -d. -f1; }
count() { aws "$@" --output text; }

preflight() {
  step "Tools"
  need_tools aws eksctl helm kubectl docker openssl python3 curl perl shasum
  say "  aws      $(aws --version 2>&1 | cut -d' ' -f1)"
  say "  eksctl   $(eksctl version)"
  say "  helm     $(helm version --short)"
  say "  kubectl  $(kubectl version --client 2>/dev/null | head -1)"

  step "AWS sign-in"
  say "  caller   $(aws sts get-caller-identity --query Arn --output text)"
  say "  region   $REGION"

  step "Quotas (vCPUs)"
  local gpu_quota standard_quota
  gpu_quota=$(quota L-DB2E81BA)
  standard_quota=$(quota L-1216C47A)
  say "  on-demand G and VT instances   $gpu_quota (the test needs 4: one of $GPU_INSTANCES)"
  say "  on-demand standard instances   $standard_quota (the test needs 2: one $SYSTEM_INSTANCE)"
  [ "$gpu_quota" -ge 4 ] || die "the G quota is below 4 vCPUs"
  [ "$standard_quota" -ge 2 ] || die "the standard quota is below 2 vCPUs"

  step "Instance types offered in the node zones"
  local zone offered configured type
  # The config 20-create.sh will use, with any zone for the GPU.
  render_cluster_config "${NODE_ZONES%% *}" >"$STATE_DIR/dry-run-config.yaml"
  grep -q '@@' "$STATE_DIR/dry-run-config.yaml" && die "cluster.yaml still holds a placeholder after rendering"
  configured=$(python3 -c 'import re, sys
text = open(sys.argv[1]).read()
print(re.search(r"^  region: \"?([a-z0-9-]+)", text, re.M).group(1))' "$STATE_DIR/dry-run-config.yaml")
  [ "$configured" = "$REGION" ] || die "the rendered config names region $configured, env.sh $REGION"
  configured=$(sed -n 's/^ *instanceType: "\{0,1\}\(g[0-9][a-z0-9]*\.[a-z0-9]*\)"\{0,1\}$/\1/p' "$STATE_DIR/dry-run-config.yaml" | tr '\n' ' ' | sed 's/ $//')
  [ "$configured" = "$GPU_INSTANCES" ] || die "cluster.yaml names GPU types \"$configured\" and env.sh names \"$GPU_INSTANCES\"; they must agree, in the same order"
  local dearest=0
  for type in $GPU_INSTANCES; do
    gpu_price "$type" >/dev/null || die "env.sh has no price for $type in GPU_PRICES"
    dearest=$(awk "BEGIN { p = $(gpu_price "$type"); print (p > $dearest) ? p : $dearest }")
  done
  awk "BEGIN { exit !($PRICE_GPU_INSTANCE == $dearest) }" || die "PRICE_GPU_INSTANCE is $PRICE_GPU_INSTANCE; it must be the dearest allowed GPU type's price, $dearest"
  for zone in $NODE_ZONES; do
    offered=$(aws ec2 describe-instance-type-offerings --location-type availability-zone \
      --filters "Name=location,Values=$zone" "Name=instance-type,Values=$(printf '%s' "$GPU_INSTANCES" | tr ' ' ','),$SYSTEM_INSTANCE" \
      --query 'InstanceTypeOfferings[].InstanceType' --output text)
    say "  $zone: $offered"
    for type in $GPU_INSTANCES $SYSTEM_INSTANCE; do
      case "$offered" in *"$type"*) ;; *) die "$type is not offered in $zone" ;; esac
    done
  done
  say "  Offered does not mean available: AWS says whether it has capacity only when a node is requested."

  step "The region before the test"
  local status
  status=$(cluster_status)
  say "  cluster $CLUSTER: $status"
  if [ "$status" = ABSENT ]; then
    cat >"$STATE_DIR/baseline-$REGION.env" <<EOF
# Region-wide counts read by 00-preflight.sh at $(iso "$(date +%s)"), before the test.
BASELINE_INSTANCES=$(count ec2 describe-instances --filters 'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down' --query 'length(Reservations[].Instances[])')
BASELINE_VOLUMES=$(count ec2 describe-volumes --query 'length(Volumes)')
BASELINE_ENIS=$(count ec2 describe-network-interfaces --query 'length(NetworkInterfaces)')
BASELINE_LOAD_BALANCERS=$(($(count elbv2 describe-load-balancers --query 'length(LoadBalancers)') + $(count elb describe-load-balancers --query 'length(LoadBalancerDescriptions)')))
BASELINE_NAT_GATEWAYS=$(count ec2 describe-nat-gateways --filter 'Name=state,Values=pending,available,deleting' --query 'length(NatGateways)')
BASELINE_EIPS=$(count ec2 describe-addresses --query 'length(Addresses)')
BASELINE_VPCS=$(count ec2 describe-vpcs --query 'length(Vpcs)')
BASELINE_CAPACITY_RESERVATIONS=$(count ec2 describe-capacity-reservations --filters 'Name=state,Values=active,pending' --query 'length(CapacityReservations)')
EOF
    sed -n '2,$p' "$STATE_DIR/baseline-$REGION.env" | sed 's/^/  /'
    say "  recorded in $STATE_DIR/baseline-$REGION.env"
  else
    say "  the cluster exists, so the baseline recorded earlier is kept"
  fi
  say "  EKS clusters in the region: $(count eks list-clusters --query 'length(clusters)')"
  say "  VPCs: $(count ec2 describe-vpcs --query 'length(Vpcs)') of 5 allowed (the test adds one)"

  step "Local ports the run uses"
  local port
  for port in "$CONTROL_LOCAL_PORT" "$AGENT_LOCAL_PORT"; do
    if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      die "local port $port is in use; set EKS_PROOF_CONTROL_PORT or EKS_PROOF_AGENT_PORT"
    fi
    say "  $port is free"
  done

  step "Dry run: eksctl cluster config"
  eksctl create cluster -f "$STATE_DIR/dry-run-config.yaml" --dry-run >"$STATE_DIR/dry-run-cluster.yaml"
  say "  ok ($(grep -c . "$STATE_DIR/dry-run-cluster.yaml") lines, in $STATE_DIR/dry-run-cluster.yaml)"

  step "Dry run: Helm charts"
  local work="$STATE_DIR/dry-run"
  rm -rf "$work"
  mkdir -p "$work"
  helm lint "$REPO_DIR/deploy/helm/infercrane-agent" --values "$TOOL_DIR/values/agent.yaml" \
    --set controlPlane.url="https://$CONTROL_HOST:8443" --set-string controlPlane.caBundle=placeholder \
    --set agent.endpointName="$ENDPOINT_NAME" --set agent.model="$MODEL" \
    --set "executor.allowedAccelerators[0]=$GPU_PRODUCT_NAME-SHARED" >/dev/null
  helm template "$HELM_RELEASE" "$REPO_DIR/deploy/helm/infercrane-agent" --namespace "$NAMESPACE_MODELS" \
    --values "$TOOL_DIR/values/agent.yaml" \
    --set image.repository=registry.invalid/infercrane --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 \
    --set controlPlane.url="https://$CONTROL_HOST:8443" --set-string controlPlane.caBundle=placeholder \
    --set agent.endpointName="$ENDPOINT_NAME" --set agent.model="$MODEL" \
    --set "executor.allowedAccelerators[0]=$GPU_PRODUCT_NAME-SHARED" >"$work/agent.yaml"
  say "  agent chart: lint ok, $(grep -c '^kind:' "$work/agent.yaml") objects rendered"
  helm pull nvidia-device-plugin --repo "$NVIDIA_CHART_REPO" --version "$NVIDIA_CHART_VERSION" --destination "$work" >/dev/null
  local chart="$work/nvidia-device-plugin-$NVIDIA_CHART_VERSION.tgz" sum
  sum=$(shasum -a 256 "$chart" | cut -d' ' -f1)
  [ "$sum" = "$NVIDIA_CHART_SHA256" ] || die "the NVIDIA chart's sha256 is $sum, not the pinned $NVIDIA_CHART_SHA256"
  helm template nvdp "$chart" --namespace nvidia-device-plugin --values "$TOOL_DIR/values/nvidia-device-plugin.yaml" >"$work/nvidia.yaml"
  say "  NVIDIA device plugin $NVIDIA_CHART_VERSION: sha256 matches, $(grep -c '^kind:' "$work/nvidia.yaml") objects rendered"

  step "Dry run: manifests"
  # kubectl's own client dry run needs an API server for discovery, and the
  # only ones on this laptop belong to other sessions. So the manifests are
  # parsed offline here, and 30-install.sh runs a server-side dry run on the
  # new cluster before it applies anything.
  local name
  for name in hosted models; do
    EKS_PROOF_IMAGE=registry.invalid/infercrane:preflight GPU_PRODUCT=$GPU_PRODUCT_NAME-SHARED render "$TOOL_DIR/manifests/$name.yaml" >"$work/$name.yaml"
  done
  cat >"$work/kustomization.yaml" <<EOF
resources:
  - hosted.yaml
  - models.yaml
  - agent.yaml
EOF
  kubectl kustomize "$work" >"$work/all.yaml"
  if grep -q '@@' "$work/all.yaml"; then
    die "a manifest still holds an unfilled @@placeholder@@"
  fi
  say "  $(grep -c '^kind:' "$work/all.yaml") objects parse: $(grep '^kind:' "$work/all.yaml" | sort | uniq -c | awk '{printf "%s %s, ", $1, $3}' | sed 's/, $//')"
  EKS_PROOF_IMAGE=registry.invalid/infercrane:preflight GPU_PRODUCT=$GPU_PRODUCT_NAME-SHARED render "$TOOL_DIR/candidate-run.template.json" | python3 -m json.tool >/dev/null
  say "  the candidate-run spec is valid JSON"

  step "Cost and time limit"
  cost_table
  local spent affordable
  spent=$(money "$(spent_closed) + $(spent_open)")
  affordable=$(calc "($BUDGET_USD - $spent) / $(rate_guard)")
  say "  Budget \$$BUDGET_USD. Spent by earlier clusters of this test: \$$spent."
  say "  One cluster may live $MAX_LIFETIME_HOURS hours at most (\$$(money "$MAX_LIFETIME_HOURS * $(rate_guard)"))."
  say "  The budget left pays for $(money "$affordable") hours."
  say "  A run that goes to plan takes about $FULL_RUN_HOURS hours: \$$(money "$FULL_RUN_HOURS * $(rate_list)") at list price."
  # The create phase refuses the same case; refusing here as well keeps an
  # image from being built for a cluster that cannot be created (24 Nov 2026).
  if awk "BEGIN { exit !($affordable < $MIN_USEFUL_HOURS) }"; then
    cat >&2 <<EOF
REFUSED: \$$spent of \$$BUDGET_USD is counted. What is left pays for $(money "$affordable") hours,
less than the $MIN_USEFUL_HOURS hours a run needs. Build no image and create no cluster.
EOF
    exit 4
  fi

  echo
  say "Preflight passed. Nothing was created."
}

run_masked preflight
