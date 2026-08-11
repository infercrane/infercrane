#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
bin="$temporary/bin"
mkdir -p "$bin"

cat >"$bin/pg_dump" <<'EOF'
#!/usr/bin/env sh
set -eu
for argument in "$@"; do
  case "$argument" in --file=*) output=${argument#--file=} ;; esac
done
printf 'fixture dump\n' >"$output"
EOF
cat >"$bin/pg_restore" <<'EOF'
#!/usr/bin/env sh
set -eu
case " $* " in *" --list "*) exit 0 ;; esac
printf 'restore\n' >>"${RESTORE_CALLS:?}"
EOF
cat >"$bin/psql" <<'EOF'
#!/usr/bin/env sh
set -eu
query=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-c" ]; then query=$2; break; fi
  shift
done
case "$query" in
  *current_database*) printf '%s\n' "${FAKE_DATABASE:-restore_target}" ;;
  *to_regclass*) printf '%s\n' "${FAKE_MEMBERSHIP_TABLE:-control_plane_instances}" ;;
  *heartbeat_at*) printf '%s\n' "${FAKE_LIVE_INSTANCES:-0}" ;;
  *schema_migrations*) printf '30\n' ;;
  *) exit 1 ;;
esac
EOF
chmod 755 "$bin/pg_dump" "$bin/pg_restore" "$bin/psql"

PATH="$bin:$PATH" INFERCRANE_DATABASE_URL=postgres://fixture "$root/scripts/backup-postgres.sh" "$temporary/backup.dump" >/dev/null
test -s "$temporary/backup.dump"
test -s "$temporary/backup.dump.sha256"

: >"$temporary/restore.calls"
if PATH="$bin:$PATH" RESTORE_CALLS="$temporary/restore.calls" INFERCRANE_DATABASE_URL=postgres://fixture INFERCRANE_ALLOW_RESTORE=yes INFERCRANE_RESTORE_TARGET_DATABASE=wrong "$root/scripts/restore-postgres.sh" "$temporary/backup.dump" >/dev/null 2>&1; then
  echo "restore accepted the wrong target database" >&2
  exit 1
fi
test ! -s "$temporary/restore.calls"

if PATH="$bin:$PATH" RESTORE_CALLS="$temporary/restore.calls" FAKE_LIVE_INSTANCES=1 INFERCRANE_DATABASE_URL=postgres://fixture INFERCRANE_ALLOW_RESTORE=yes INFERCRANE_RESTORE_TARGET_DATABASE=restore_target "$root/scripts/restore-postgres.sh" "$temporary/backup.dump" >/dev/null 2>&1; then
  echo "restore accepted a live control plane" >&2
  exit 1
fi
test ! -s "$temporary/restore.calls"

PATH="$bin:$PATH" RESTORE_CALLS="$temporary/restore.calls" INFERCRANE_DATABASE_URL=postgres://fixture INFERCRANE_ALLOW_RESTORE=yes INFERCRANE_RESTORE_TARGET_DATABASE=restore_target "$root/scripts/restore-postgres.sh" "$temporary/backup.dump" >/dev/null
test "$(wc -l <"$temporary/restore.calls" | tr -d ' ')" = 1
echo "Backup/restore approval, target identity, live-member, archive, and checksum safety passed."
