#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mode=${1:-quick}
case "$mode" in quick|full) ;; *) echo "usage: $0 [quick|full]" >&2; exit 2;; esac

cd "$root"
package_dir=$(mktemp -d)
trap 'rm -rf "$package_dir"' EXIT HUP INT TERM

release_version=$(go run ./cmd/infercrane version)
case "$release_version" in
  *-rc.*) python_version=${release_version%-rc.*}rc${release_version##*.} ;;
  *) python_version=$release_version ;;
esac
grep -Fq "version = \"$python_version\"" "$root/sdk/python/pyproject.toml"
grep -Fq "infercrane-python/$python_version" "$root/sdk/python/src/infercrane/client.py"
node -e 'const p=require(process.argv[1]); if(p.version!==process.argv[2]) process.exit(1)' "$root/sdk/typescript/package.json" "$release_version"
grep -Fq "infercrane-typescript/$release_version" "$root/sdk/typescript/src/client.ts"
grep -Fq "default: v$release_version" "$root/actions/infercrane/action.yml"
grep -Fq "infercrane:v$release_version" "$root/compose.production.yaml"

go run ./tools/openapi-codegen -check
PYTHONPATH="$root/sdk/python/src" python3 -m unittest discover -s "$root/sdk/python/tests"
python3 -m pip wheel --disable-pip-version-check --no-deps --wheel-dir "$package_dir" "$root/sdk/python"
npm --prefix "$root/sdk/typescript" ci --no-audit --no-fund
npm --prefix "$root/sdk/typescript" test
(cd "$root/sdk/typescript" && npm pack --dry-run >/dev/null)
node --test "$root"/actions/infercrane/test/*.test.js

(
  cd "$root/integrations/terraform"
  go test -count=1 ./...
  go build -o "$package_dir/terraform-provider-infercrane" .
)

if [ "$mode" = full ]; then
  terraform_bin=$(command -v terraform 2>/dev/null || true)
  if [ -z "$terraform_bin" ] && [ -x "$root/.infercrane/tools/terraform" ]; then
    terraform_bin="$root/.infercrane/tools/terraform"
  fi
  if [ -z "$terraform_bin" ]; then
    echo "Terraform CLI is required for full automation qualification." >&2
    echo "Install the pinned version or place it at .infercrane/tools/terraform." >&2
    exit 1
  fi
  (
    cd "$root/integrations/terraform"
    PATH="$(dirname "$terraform_bin"):$PATH" TF_ACC=1 go test -count=1 -run TestAcc -v ./internal/provider
  )
fi
