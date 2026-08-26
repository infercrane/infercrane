#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fuzz_time=${INFERCRANE_FUZZ_TIME:-30s}
parallel=${INFERCRANE_FUZZ_PARALLEL:-4}
found=0

while IFS=: read -r file _ declaration; do
  [[ -n "$file" ]] || continue
  name=${declaration#func }
  name=${name%%(*}
  package=./$(dirname "$file")
  echo "==> fuzz $package/$name ($fuzz_time)"
  (cd "$root" && go test "$package" -run '^$' -fuzz "^${name}$" -fuzztime "$fuzz_time" -parallel "$parallel")
  found=$((found + 1))
done < <(cd "$root" && git grep -n -E '^func Fuzz[A-Za-z0-9_]+\(' -- '*_test.go' | sort)

if [[ "$found" -eq 0 ]]; then
  echo "no Go fuzz targets found" >&2
  exit 1
fi
echo "InferCrane fuzz qualification passed ($found targets)"
