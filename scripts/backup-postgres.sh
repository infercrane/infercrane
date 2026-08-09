#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/backup-postgres.sh OUTPUT.dump" >&2
  exit 2
fi
: "${INFERCRANE_DATABASE_URL:?INFERCRANE_DATABASE_URL is required}"

pg_dump --format=custom --no-owner --no-privileges --file="$1" "$INFERCRANE_DATABASE_URL"
pg_restore --list "$1" >/dev/null
echo "verified backup written to $1"
