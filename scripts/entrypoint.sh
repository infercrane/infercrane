#!/bin/sh
set -eu

runpod_key=${RUNPOD_API_KEY:-}
if [ -z "$runpod_key" ] && [ -n "${RUNPOD_API_KEY_FILE:-}" ]; then
  [ -r "$RUNPOD_API_KEY_FILE" ] || {
    echo "RUNPOD_API_KEY_FILE is not readable: $RUNPOD_API_KEY_FILE" >&2
    exit 1
  }
  runpod_key=$(tr -d '\r\n' <"$RUNPOD_API_KEY_FILE")
fi
if [ -n "$runpod_key" ]; then
  umask 077
  rm -f "${HOME:?}/.runpod/config.toml"
  runpod config "$runpod_key" >/dev/null
  export RUNPOD_API_KEY="$runpod_key"
fi
unset runpod_key

if [ "${1:-}" != "infercrane" ] || [ "${2:-}" != "serve" ]; then
  exec "$@"
fi

skypilot_mode=${INFERCRANE_SKYPILOT_API:-auto}
case "$skypilot_mode" in
  auto) [ -n "${RUNPOD_API_KEY:-}" ] || exec "$@" ;;
  enabled) [ -n "${RUNPOD_API_KEY:-}" ] || {
    echo "INFERCRANE_SKYPILOT_API=enabled requires a RunPod API key" >&2
    exit 1
  } ;;
  disabled) exec "$@" ;;
  *)
    echo "INFERCRANE_SKYPILOT_API must be auto, enabled, or disabled" >&2
    exit 1
    ;;
esac

sky api start --host 127.0.0.1 --foreground &
sky_pid=$!

cleanup() {
  kill "${app_pid:-}" "$sky_pid" 2>/dev/null || true
  wait "${app_pid:-}" "$sky_pid" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

until python -c 'import socket; socket.create_connection(("127.0.0.1", 46580), timeout=1).close()' 2>/dev/null; do
  if ! kill -0 "$sky_pid" 2>/dev/null; then
    wait "$sky_pid"
  fi
  sleep 1
done

"$@" &
app_pid=$!

while kill -0 "$app_pid" 2>/dev/null && kill -0 "$sky_pid" 2>/dev/null; do
  sleep 1
done

if ! kill -0 "$sky_pid" 2>/dev/null; then
  echo "SkyPilot API server stopped unexpectedly" >&2
  kill "$app_pid" 2>/dev/null || true
fi

wait "$app_pid"
