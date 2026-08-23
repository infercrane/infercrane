#!/bin/sh
set -eu

# Execute one InferCrane CLI command inside the isolated portable-provider
# qualification stack. Secrets are read from restricted files at execution
# time and are never written into the wrapper or benchmark evidence.
: "${INFERCRANE_PORTABLE_ROOT:?}"
: "${INFERCRANE_PORTABLE_PROJECT:?}"
: "${INFERCRANE_PORTABLE_ENV_FILE:?}"
: "${INFERCRANE_PORTABLE_PROVIDER_COMPOSE:?}"
: "${INFERCRANE_PORTABLE_SPEC_DIR:?}"
: "${INFERCRANE_PORTABLE_API_KEY_FILE:?}"
: "${INFERCRANE_PORTABLE_PASSWORD_FILE:?}"
: "${INFERCRANE_PORTABLE_IMAGE:?}"
: "${INFERCRANE_PORTABLE_PORT:?}"

api_key=$(tr -d '\r\n' <"$INFERCRANE_PORTABLE_API_KEY_FILE")
postgres_password=$(tr -d '\r\n' <"$INFERCRANE_PORTABLE_PASSWORD_FILE")

INFERCRANE_API_KEY="$api_key" \
INFERCRANE_POSTGRES_PASSWORD="$postgres_password" \
INFERCRANE_IMAGE="$INFERCRANE_PORTABLE_IMAGE" \
INFERCRANE_URL=http://127.0.0.1:8080 \
INFERCRANE_PORT="$INFERCRANE_PORTABLE_PORT" \
INFERCRANE_QUALIFICATION_SPEC_DIR="$INFERCRANE_PORTABLE_SPEC_DIR" \
  exec docker compose -p "$INFERCRANE_PORTABLE_PROJECT" \
    --env-file "$INFERCRANE_PORTABLE_ENV_FILE" \
    -f "$INFERCRANE_PORTABLE_ROOT/compose.production.yaml" \
    -f "$INFERCRANE_PORTABLE_PROVIDER_COMPOSE" \
    -f "$INFERCRANE_PORTABLE_ROOT/compose.portable-acceptance.yaml" \
    exec -T infercrane infercrane "$@"
