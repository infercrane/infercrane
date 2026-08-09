#!/usr/bin/env sh
set -eu

if ! command -v deadcode >/dev/null 2>&1; then
  echo "deadcode is required; install golang.org/x/tools/cmd/deadcode@v0.48.0" >&2
  exit 1
fi

report=$(deadcode -test ./...)
if [ -n "$report" ]; then
  echo "Unreachable Go functions detected:" >&2
  echo "$report" >&2
  exit 1
fi
