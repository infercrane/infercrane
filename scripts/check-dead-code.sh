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
  go run golang.org/x/tools/cmd/deadcode@v0.48.0 -test ./... github.com/infercrane/infercrane/internal/controlclient
)
if [ -n "$report" ]; then
  echo "Unreachable Terraform integration functions detected:" >&2
  echo "$report" >&2
  exit 1
fi
