#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

# controlclient is an intentional cross-module boundary used by the isolated
# Terraform provider module. Analyze it with that module's roots below instead
# of treating the root module alone as the whole program.
report=$(go run golang.org/x/tools/cmd/deadcode@v0.48.0 -test ./... | grep -v '^internal/controlclient/' || true)
if [ -n "$report" ]; then
  echo "Unreachable Go functions detected:" >&2
  echo "$report" >&2
  exit 1
fi

report=$(
  cd "$root/integrations/terraform"
  # The Terraform module imports persisted domain DTOs through controlclient.
  # Runtime workload and serving-topology validation methods are exercised by
  # the root control plane, but are intentionally not called by this API-only
  # integration. They are reachable in the root analysis above; exclude only
  # the duplicate cross-module report.
  go run golang.org/x/tools/cmd/deadcode@v0.48.0 -test ./... github.com/infercrane/infercrane/internal/controlclient |
    grep -v -e '/internal/runtimecontract/' -e '/internal/servingcontract/' || true
)
if [ -n "$report" ]; then
  echo "Unreachable Terraform integration functions detected:" >&2
  echo "$report" >&2
  exit 1
fi
