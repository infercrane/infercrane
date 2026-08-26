#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

run_deadcode() {
  if command -v deadcode >/dev/null 2>&1; then
    deadcode "$@"
  elif [ -x "$(go env GOPATH)/bin/deadcode" ]; then
    "$(go env GOPATH)/bin/deadcode" "$@"
  else
    go run golang.org/x/tools/cmd/deadcode@v0.48.0 "$@"
  fi
}

# controlclient is an intentional cross-module boundary used by the isolated
# Terraform provider module. Analyze it with that module's roots below instead
# of treating the root module alone as the whole program.
if ! run_deadcode -test ./... >"$temporary/root.txt"; then
  echo "dead-code analysis failed" >&2
  cat "$temporary/root.txt" >&2
  exit 1
fi
report=$(grep -v '^internal/controlclient/' "$temporary/root.txt" || true)
if [ -n "$report" ]; then
  echo "Unreachable Go functions detected:" >&2
  echo "$report" >&2
  exit 1
fi

if ! (
  cd "$root/integrations/terraform"
  # The Terraform module imports persisted domain DTOs through controlclient.
  # Runtime workload and serving-topology validation methods are exercised by
  # the root control plane, but are intentionally not called by this API-only
  # integration. They are reachable in the root analysis above; exclude only
  # the duplicate cross-module report.
  run_deadcode -test ./... github.com/infercrane/infercrane/internal/controlclient
) >"$temporary/terraform.txt"; then
  echo "Terraform dead-code analysis failed" >&2
  cat "$temporary/terraform.txt" >&2
  exit 1
fi
report=$(grep -v -e '/internal/runtimecontract/' -e '/internal/servingcontract/' "$temporary/terraform.txt" || true)
if [ -n "$report" ]; then
  echo "Unreachable Terraform integration functions detected:" >&2
  echo "$report" >&2
  exit 1
fi
