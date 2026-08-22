#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/restore-postgres.sh INPUT.dump" >&2
  exit 2
fi
: "${INFERCRANE_DATABASE_URL:?INFERCRANE_DATABASE_URL is required}"
: "${INFERCRANE_ALLOW_RESTORE:?Set INFERCRANE_ALLOW_RESTORE=yes after verifying the target database}"
: "${INFERCRANE_RESTORE_TARGET_DATABASE:?Set INFERCRANE_RESTORE_TARGET_DATABASE to the exact current_database() target}"
if [ "$INFERCRANE_ALLOW_RESTORE" != "yes" ]; then
  echo "INFERCRANE_ALLOW_RESTORE must equal yes" >&2
  exit 2
fi

pg_restore --list "$1" >/dev/null
actual_database=$(psql "$INFERCRANE_DATABASE_URL" -X -A -t -v ON_ERROR_STOP=1 -c 'SELECT current_database()')
if [ "$actual_database" != "$INFERCRANE_RESTORE_TARGET_DATABASE" ]; then
  echo "restore target mismatch: connected to $actual_database, expected $INFERCRANE_RESTORE_TARGET_DATABASE" >&2
  exit 2
fi
live_instances=0
membership_table=$(psql "$INFERCRANE_DATABASE_URL" -X -A -t -v ON_ERROR_STOP=1 -c "SELECT COALESCE(to_regclass('public.control_plane_instances')::text,'')")
if [ -n "$membership_table" ]; then
  live_instances=$(psql "$INFERCRANE_DATABASE_URL" -X -A -t -v ON_ERROR_STOP=1 -c "SELECT count(*) FROM control_plane_instances WHERE heartbeat_at > NOW()-INTERVAL '45 seconds'")
fi
if [ "$live_instances" != "0" ]; then
  echo "restore refused: $live_instances control-plane instance(s) are still live; stop them before restore" >&2
  exit 2
fi

started=$(date -u +%s)
pg_restore --clean --if-exists --no-owner --no-privileges --dbname="$INFERCRANE_DATABASE_URL" "$1"
psql "$INFERCRANE_DATABASE_URL" -X -A -t -v ON_ERROR_STOP=1 -c 'SELECT count(*) FROM schema_migrations' >/dev/null
finished=$(date -u +%s)
echo "restore completed into $actual_database in $((finished-started))s; keep InferCrane stopped until offline state and provider ownership reconcile"
