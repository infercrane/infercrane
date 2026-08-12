#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
kwok_version=v0.8.0
tool_dir="$root/.infercrane/tools/kwok-$kwok_version"
kwokctl=${KWOKCTL_BIN:-}
if [[ -z "$kwokctl" ]]; then
  kwokctl="$tool_dir/kwokctl"
  if [[ ! -x "$kwokctl" ]]; then
    mkdir -p "$tool_dir"
    GOBIN="$tool_dir" go install "sigs.k8s.io/kwok/cmd/kwokctl@$kwok_version"
  fi
fi

cluster="infercrane-${INFERCRANE_KWOK_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
kubeconfig=$(mktemp)
cleanup() {
  "$kwokctl" delete cluster --name "$cluster" >/dev/null 2>&1 || true
  rm -f "$kubeconfig"
}
trap cleanup EXIT INT TERM

"$kwokctl" create cluster --name "$cluster" --runtime docker --wait 180s
"$kwokctl" --name "$cluster" get kubeconfig >"$kubeconfig"
export KUBECONFIG="$kubeconfig"
context="kwok-$cluster"

kubectl --context "$context" apply -f "$root/deploy/kubernetes/base/namespace.yaml"
kubectl --context "$context" apply -f "$root/deploy/kubernetes/base/service-accounts.yaml"
kubectl --context "$context" --namespace infercrane-system create secret generic infercrane-worker \
  --from-literal=api-key=kwok-test-worker-key

nodes=${INFERCRANE_KWOK_NODES:-200}
workloads=${INFERCRANE_KWOK_WORKLOADS:-100}
"$kwokctl" --name "$cluster" scale node --replicas "$nodes" --serial-length 5
[[ "$(kubectl --context "$context" get nodes -o json | jq '.items | length')" -eq "$nodes" ]]

cd "$root"
INFERCRANE_KWOK_CONTEXT="$context" INFERCRANE_KWOK_WORKLOADS="$workloads" \
  go test -count=1 -timeout=10m -run '^TestKubernetesKWOKFleetLifecycle$' ./internal/provision
