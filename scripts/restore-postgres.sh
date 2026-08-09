#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/restore-postgres.sh INPUT.dump" >&2
  exit 2
fi
: "${INFERCRANE_DATABASE_URL:?INFERCRANE_DATABASE_URL is required}"
: "${INFERCRANE_ALLOW_RESTORE:?Set INFERCRANE_ALLOW_RESTORE=yes after verifying the target database}"
if [ "$INFERCRANE_ALLOW_RESTORE" != "yes" ]; then
  echo "INFERCRANE_ALLOW_RESTORE must equal yes" >&2
  exit 2
fi

pg_restore --clean --if-exists --no-owner --no-privileges --dbname="$INFERCRANE_DATABASE_URL" "$1"
echo "restore completed; restart InferCrane and verify /readyz"
