#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

# These hermetic tests exercise the exact InferCrane adapters against hostile
# HTTP fixtures. They prove our retry, adoption, idempotency, validation,
# ownership, redaction, and cleanup logic; they deliberately do not claim that
# a fixture reproduces a cloud provider's implementation.
go test -race -count=1 -timeout=10m \
  -run '^(TestAWSEC2|TestGCPCompute|TestProviderContract|TestRunPodServerless|TestKubernetes)' \
  ./internal/provision
go test -race -count=1 -timeout=5m \
  -run '^(TestAWSEC2|TestKubernetes|TestElasticTimeout)' \
  ./internal/conformance
go test -race -count=1 -timeout=5m \
  -run '^(TestAWSBYOC|TestGCPBYOC|TestKubernetes)' \
  ./internal/config

echo "InferCrane hermetic cloud-adapter qualification passed"
