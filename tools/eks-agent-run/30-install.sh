#!/usr/bin/env bash
# Phase 3. The cluster operator's part, inside the cluster. It creates
# Kubernetes objects only: nothing new in AWS, no load balancer, no volume,
# so the hourly cost does not change.
#
#   1. NVIDIA's device plugin with GPU feature discovery, the L4 unshared
#   2. the InferCrane control plane in infercrane-system, bootstrapped with
#      deploy/kubernetes/base as docs/integrations/kubernetes.mdx says, with
#      the Kubernetes provider configured
#
# The customer's own model server and the agent come in 40-run.sh, once the
# L4 is shared; Qwen3-8B first gets the whole GPU in 35-deploy.sh.
#
# Safe to run again: every step applies the same objects or skips what exists.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

install_phase() {
  need_tools aws helm kubectl openssl python3 curl shasum
  need_cluster
  guard_phase install 40
  trap stop_port_forwards EXIT

  step "1. NVIDIA device plugin $NVIDIA_CHART_VERSION with GPU feature discovery"
  local chart="$STATE_DIR/nvidia-device-plugin-$NVIDIA_CHART_VERSION.tgz" sum
  if [ ! -s "$chart" ]; then
    helm pull nvidia-device-plugin --repo "$NVIDIA_CHART_REPO" --version "$NVIDIA_CHART_VERSION" --destination "$STATE_DIR" >/dev/null
  fi
  sum=$(shasum -a 256 "$chart" | cut -d' ' -f1)
  [ "$sum" = "$NVIDIA_CHART_SHA256" ] || die "the NVIDIA chart's sha256 is $sum, not the pinned $NVIDIA_CHART_SHA256"
  # 20-create.sh tells eksctl not to install its own copy of the device plugin.
  # If one is there anyway, two plugins would both register the GPU.
  kc --namespace kube-system delete daemonset nvidia-device-plugin-daemonset --ignore-not-found >/dev/null
  helm upgrade --install nvdp "$chart" --namespace nvidia-device-plugin --create-namespace \
    --values "$TOOL_DIR/values/nvidia-device-plugin.yaml" --wait --timeout 10m >/dev/null
  local gpu_node product="" allocatable="" until=$((SECONDS + 600))
  gpu_node=$(kc get nodes -l infercrane.dev/pool=gpu -o jsonpath='{.items[0].metadata.name}')
  [ -n "$gpu_node" ] || die "the cluster has no GPU node"
  while [ "$SECONDS" -lt "$until" ]; do
    product=$(kc get node "$gpu_node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')
    allocatable=$(kc get node "$gpu_node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')
    [ -n "$product" ] && [ "$allocatable" = 1 ] && break
    sleep 10
  done
  if [ -z "$product" ] || [ "$allocatable" != 1 ]; then
    kc --namespace nvidia-device-plugin get pods -o wide
    die "the GPU node shows product \"$product\" and $allocatable nvidia.com/gpu; expected a product label and 1 (unshared)"
  fi
  state_set GPU_NODE "$gpu_node"
  state_set GPU_PRODUCT_WHOLE "$product"
  say "  node $gpu_node: nvidia.com/gpu.product=$product, nvidia.com/gpu=$allocatable"
  say "  memory $(kc get node "$gpu_node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.memory}') MiB, driver $(kc get node "$gpu_node" -o jsonpath='{.metadata.labels.nvidia\.com/cuda\.driver-version\.full}'), CUDA $(kc get node "$gpu_node" -o jsonpath='{.metadata.labels.nvidia\.com/cuda\.runtime-version\.full}')"
  kc get node "$gpu_node" -o json | python3 -c 'import json, sys
labels = json.load(sys.stdin)["metadata"]["labels"]
for key in sorted(labels):
    if key.startswith(("nvidia.com/", "node.kubernetes.io/instance-type", "topology.kubernetes.io/zone", "eks.amazonaws.com/nodegroup-image")):
        print("%s=%s" % (key, labels[key]))' >"$STATE_DIR/gpu-node-labels.txt"

  step "2. The InferCrane control plane (namespace $NAMESPACE_HOSTED)"
  say "  bootstrap, as docs/integrations/kubernetes.mdx says:"
  local base
  for base in namespace service-accounts provider-rbac; do
    kc apply --dry-run=server -f "$REPO_DIR/deploy/kubernetes/base/$base.yaml" >/dev/null
    kc apply -f "$REPO_DIR/deploy/kubernetes/base/$base.yaml" | sed 's/^/    /'
  done
  secret_file "$STATE_DIR/api-key" sh -c 'printf "ic_eks_%s" "$(openssl rand -hex 24)"'
  secret_file "$STATE_DIR/auth-header" sh -c 'printf "Authorization: Bearer %s" "$(cat "$1")"' sh "$STATE_DIR/api-key"
  # No trailing newline: PostgreSQL's image sets the password from its first
  # line (initdb --pwfile), while the control plane's connection string takes
  # the whole value, so a newline made them differ (27 Nov, "password
  # authentication failed" on the first start).
  secret_file "$STATE_DIR/postgres-password" sh -c 'openssl rand -hex 24 | tr -d "\n"'
  if [ ! -s "$STATE_DIR/tls.crt" ]; then
    (umask 077 && openssl req -x509 -newkey rsa:2048 -nodes -days 3 -keyout "$STATE_DIR/tls.key" -out "$STATE_DIR/tls.crt" \
      -subj "/CN=$CONTROL_HOST" -addext "subjectAltName=DNS:$CONTROL_HOST,DNS:$CONTROL_HOST.cluster.local" >/dev/null 2>&1)
  fi
  apply_secret "$NAMESPACE_HOSTED" control-plane "api-key=$STATE_DIR/api-key" "postgres-password=$STATE_DIR/postgres-password"
  # The key every engine pod requires; the control plane presents its API
  # key to them (docs: "Create the worker credential").
  apply_secret "$NAMESPACE_HOSTED" infercrane-worker "api-key=$STATE_DIR/api-key"
  kc --namespace "$NAMESPACE_HOSTED" create secret tls control-plane-tls --cert "$STATE_DIR/tls.crt" --key "$STATE_DIR/tls.key" --dry-run=client -o yaml | kc apply -f - >/dev/null
  say "  secret $NAMESPACE_HOSTED/control-plane-tls"
  render "$TOOL_DIR/manifests/hosted.yaml" | kc apply --dry-run=server -f - >/dev/null
  render "$TOOL_DIR/manifests/hosted.yaml" | kc apply -f -
  wait_rollout "$NAMESPACE_HOSTED" deployment/postgres 300s
  wait_rollout "$NAMESPACE_HOSTED" deployment/infercrane 300s
  open_control
  local status
  status=$(api GET /api/v1/health "$STATE_DIR/control-health.json" || true)
  say "  control plane answers: HTTP $status"
  step "3. infercrane doctor --kubernetes, run by the control plane itself"
  if icli doctor --kubernetes >"$STATE_DIR/doctor-kubernetes.txt" 2>&1; then
    say "  passed"
  else
    say "  FAILED (the run goes on; the output is kept):"
  fi
  sed 's/^/    /' "$STATE_DIR/doctor-kubernetes.txt" | tail -30

  step "Installed"
  kc get pods --all-namespaces -o wide
  echo
  say "Next: tools/eks-agent-run/35-deploy.sh ($(minutes_left) minutes left before the cluster must be gone)"
  stop_port_forwards
}

run_masked install_phase
