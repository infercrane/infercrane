#!/bin/sh
set -eu

if [ -n "${INFERCRANE_GCLOUD_CONFIG_SOURCE:-}" ]; then
  [ -d "$INFERCRANE_GCLOUD_CONFIG_SOURCE" ] || {
    echo "INFERCRANE_GCLOUD_CONFIG_SOURCE is not a readable directory" >&2
    exit 1
  }
  : "${CLOUDSDK_CONFIG:?set CLOUDSDK_CONFIG with INFERCRANE_GCLOUD_CONFIG_SOURCE}"
  case "$CLOUDSDK_CONFIG" in
    /tmp/*) ;;
    *) echo "CLOUDSDK_CONFIG must use container-local /tmp storage" >&2; exit 1 ;;
  esac
  mkdir -p "$CLOUDSDK_CONFIG"
  cp -R "$INFERCRANE_GCLOUD_CONFIG_SOURCE"/. "$CLOUDSDK_CONFIG"/
  chmod -R u+rwX,go-rwx "$CLOUDSDK_CONFIG"
fi

runpod_key=${RUNPOD_API_KEY:-}
if [ -z "$runpod_key" ] && [ -n "${RUNPOD_API_KEY_FILE:-}" ]; then
  [ -r "$RUNPOD_API_KEY_FILE" ] || {
    echo "RUNPOD_API_KEY_FILE is not readable: $RUNPOD_API_KEY_FILE" >&2
    exit 1
  }
  runpod_key=$(tr -d '\r\n' <"$RUNPOD_API_KEY_FILE")
fi
if [ -n "$runpod_key" ]; then
  # SkyPilot's RunPod backend requires ~/.runpod/config.toml. The runpod 2.x
  # CLI removed `runpod config`, so write the file it used to write directly,
  # as a TOML basic string with backslashes and quotes escaped.
  umask 077
  mkdir -p "${HOME:?}/.runpod"
  rm -f "$HOME/.runpod/config.toml"
  runpod_toml_key=$(printf '%s' "$runpod_key" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '[default]\napi_key = "%s"\n' "$runpod_toml_key" >"$HOME/.runpod/config.toml"
  unset runpod_toml_key
  export RUNPOD_API_KEY="$runpod_key"
fi
unset runpod_key

# SkyPilot's Vast.ai backend reads its key only from the Vast CLI config file.
vast_key=${VAST_API_KEY:-}
if [ -z "$vast_key" ] && [ -n "${VAST_API_KEY_FILE:-}" ]; then
  [ -r "$VAST_API_KEY_FILE" ] || {
    echo "VAST_API_KEY_FILE is not readable: $VAST_API_KEY_FILE" >&2
    exit 1
  }
  vast_key=$(tr -d '\r\n' <"$VAST_API_KEY_FILE")
fi
if [ -n "$vast_key" ]; then
  umask 077
  mkdir -p "${HOME:?}/.config/vastai"
  printf '%s' "$vast_key" >"$HOME/.config/vastai/vast_api_key"
  export VAST_API_KEY="$vast_key"
fi
unset vast_key

# Fly and similar platforms inject secrets as environment variables, while the
# Brezel client deliberately accepts only a protected credential file. Materialize
# the deployment secret on container-local storage and remove it from the child
# process environment before starting InferCrane.
if [ -n "${INFERCRANE_BREZEL_SANDBOX_TOKEN:-}" ]; then
  brezel_secret_dir=/tmp/infercrane-secrets
  brezel_token_file="$brezel_secret_dir/brezel-sandbox-token"
  umask 077
  mkdir -p "$brezel_secret_dir"
  printf '%s' "$INFERCRANE_BREZEL_SANDBOX_TOKEN" >"$brezel_token_file"
  chmod 600 "$brezel_token_file"
  export INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE="$brezel_token_file"
  unset INFERCRANE_BREZEL_SANDBOX_TOKEN brezel_secret_dir brezel_token_file
fi

# The passport signing key follows the same rule: InferCrane reads it only from
# an owner-only file, so a platform-injected secret becomes one here.
if [ -n "${INFERCRANE_PASSPORT_SIGNING_KEY:-}" ]; then
  if [ -n "${INFERCRANE_PASSPORT_SIGNING_KEY_FILE:-}" ]; then
    echo "set INFERCRANE_PASSPORT_SIGNING_KEY or INFERCRANE_PASSPORT_SIGNING_KEY_FILE, not both" >&2
    exit 1
  fi
  passport_secret_dir=/tmp/infercrane-secrets
  passport_key_file="$passport_secret_dir/passport-signing-key"
  umask 077
  mkdir -p "$passport_secret_dir"
  printf '%s\n' "$INFERCRANE_PASSPORT_SIGNING_KEY" >"$passport_key_file"
  chmod 600 "$passport_key_file"
  export INFERCRANE_PASSPORT_SIGNING_KEY_FILE="$passport_key_file"
  unset INFERCRANE_PASSPORT_SIGNING_KEY passport_secret_dir passport_key_file
fi

if [ "${1:-}" != "infercrane" ] || [ "${2:-}" != "serve" ]; then
  exec "$@"
fi

skypilot_mode=${INFERCRANE_SKYPILOT_API:-auto}
skypilot_providers=${INFERCRANE_SKYPILOT_PROVIDERS_JSON:-}
case "$skypilot_mode" in
  auto) [ -n "$skypilot_providers" ] || exec "$@" ;;
  enabled) [ -n "$skypilot_providers" ] || {
    echo "INFERCRANE_SKYPILOT_API=enabled requires INFERCRANE_SKYPILOT_PROVIDERS_JSON" >&2
    exit 1
  } ;;
  disabled) exec "$@" ;;
  *)
    echo "INFERCRANE_SKYPILOT_API must be auto, enabled, or disabled" >&2
    exit 1
    ;;
esac
unset skypilot_providers

# SkyPilot logs failed provider calls with their full URL, and some provider SDKs
# (Vast.ai) put the API key in the query string. Pass the server's output through
# a filter that redacts key-bearing parameters before it reaches the log stream.
sky_log_dir=$(mktemp -d /tmp/infercrane-sky-log.XXXXXX)
mkfifo "$sky_log_dir/out"
sed -u -E 's/((api_key|apikey|api-key|token|secret)=)[^&[:space:]"]+/\1[REDACTED]/Ig' <"$sky_log_dir/out" &
sky_log_pid=$!
sky api start --host 127.0.0.1 --foreground >"$sky_log_dir/out" 2>&1 &
sky_pid=$!

cleanup() {
  kill "${app_pid:-}" "$sky_pid" 2>/dev/null || true
  wait "${app_pid:-}" "$sky_pid" 2>/dev/null || true
  wait "$sky_log_pid" 2>/dev/null || true
  rm -rf "$sky_log_dir"
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
