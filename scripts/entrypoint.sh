#!/bin/sh
set -eu

runpod_key=${RUNPOD_API_KEY:-}
if [ -z "$runpod_key" ] && [ -n "${RUNPOD_API_KEY_FILE:-}" ]; then
  runpod_key=$(cat "$RUNPOD_API_KEY_FILE")
fi
if [ -n "$runpod_key" ]; then
  umask 077
  runpod config "$runpod_key" >/dev/null
fi
unset runpod_key

exec "$@"
