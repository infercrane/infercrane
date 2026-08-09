#!/usr/bin/env sh
set -eu

report=$(go run golang.org/x/tools/cmd/deadcode@v0.48.0 -test ./...)
if [ -n "$report" ]; then
  echo "Unreachable Go functions detected:" >&2
  echo "$report" >&2
  exit 1
fi
