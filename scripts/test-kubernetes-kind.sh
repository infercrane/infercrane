#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
kind_version=v0.32.0
node_image='kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5'
tool_dir="$repo_dir/.infercrane/tools/kind-$kind_version"
kind_bin=${KIND_BIN:-}

if [[ -z "$kind_bin" ]]; then
  if command -v kind >/dev/null 2>&1; then
    kind_bin=$(command -v kind)
  else
    kind_bin="$tool_dir/kind"
    if [[ ! -x "$kind_bin" ]]; then
      mkdir -p "$tool_dir"
      GOBIN="$tool_dir" go install "sigs.k8s.io/kind@$kind_version"
    fi
  fi
fi

cluster_name="infercrane-${INFERCRANE_KIND_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
context_name="kind-$cluster_name"
cleanup() {
  "$kind_bin" delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 180s
kubectl --context "$context_name" apply -f "$repo_dir/deploy/kubernetes/base/namespace.yaml"
kubectl --context "$context_name" apply -f "$repo_dir/deploy/kubernetes/base/service-accounts.yaml"
kubectl --context "$context_name" --namespace infercrane-system create secret generic infercrane-worker \
  --from-literal=api-key=kind-test-worker-key

cd "$repo_dir"
INFERCRANE_KIND_CONTEXT="$context_name" go test -count=1 -run '^TestKubernetesKindLifecycle$' ./internal/provision
