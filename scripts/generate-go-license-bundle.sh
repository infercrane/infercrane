#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=${1:-"$root/.release/generated-licenses/go"}
tool=github.com/google/go-licenses/v2@v2.0.1

case "$output" in
  /*) ;;
  *) output="$root/$output" ;;
esac

# go-licenses requires a destination that does not already exist. Only remove
# exact generated-output locations; never accept a broad or unresolved target.
case "$output" in
  "$root/.release/generated-licenses/go"|"$root/dist/"*|/out/licenses/go|/tmp/infercrane-*|/private/var/folders/*/infercrane-*) ;;
  *)
    echo "refusing unsafe generated-license output: $output" >&2
    exit 2
    ;;
esac
if test -e "$output"; then
  rm -rf -- "$output"
fi
mkdir -p "$(dirname "$output")"

cd "$root"
go run "$tool" save \
  --ignore github.com/infercrane/infercrane \
  --save_path="$output" \
  ./cmd/infercrane
go run "$tool" report \
  --ignore github.com/infercrane/infercrane \
  ./cmd/infercrane >"$output/manifest.csv"

license_count=$(find "$output" -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) | wc -l | tr -d ' ')
test "$license_count" -ge 35 || {
  echo "generated Go license bundle is unexpectedly small: $license_count files" >&2
  exit 1
}
test -f "$output/gopkg.in/yaml.v3/NOTICE"
test -f "$output/github.com/spf13/cobra/LICENSE.txt"
test -f "$output/github.com/go-jose/go-jose/v3/LICENSE"
test -f "$output/github.com/modelcontextprotocol/go-sdk/LICENSE"
if awk -F, '$3 == "Unknown" { found=1 } END { exit found ? 0 : 1 }' "$output/manifest.csv"; then
  echo "generated Go license manifest contains an unknown license" >&2
  exit 1
fi

echo "Generated $license_count dependency license files in $output."
