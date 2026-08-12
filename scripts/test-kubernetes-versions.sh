#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
images=(
  'kindest/node:v1.34.8@sha256:02722c2dedddcfc00febf5d27fbeb9b7b2c14294c82109ff4a85d89ac9ba3256'
  'kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95'
  'kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5'
)

for image in "${images[@]}"; do
  version=${image#kindest/node:}
  version=${version%%@*}
  echo "==> Kubernetes $version"
  INFERCRANE_KIND_NODE_IMAGE="$image" INFERCRANE_KIND_RUN_ID="matrix-${version#v}-$$" \
    "$root/scripts/test-kubernetes-kind.sh"
done
echo "InferCrane Kubernetes version matrix passed"
