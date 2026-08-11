#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
base="$repo_dir/deploy/kubernetes/base"
kserve="$repo_dir/deploy/kubernetes/kserve/provider-rbac.yaml"
route="$repo_dir/deploy/kubernetes/gateway-api/httproute.yaml"

cd "$repo_dir"
go run ./tools/kubernetes-manifest-check \
  "$base/namespace.yaml" \
  "$base/service-accounts.yaml" \
  "$base/provider-rbac.yaml" \
  "$kserve" \
  "$route"
