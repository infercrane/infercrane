#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/backup-postgres.sh OUTPUT.dump" >&2
  exit 2
fi
: "${INFERCRANE_DATABASE_URL:?INFERCRANE_DATABASE_URL is required}"

output=$1
output_dir=$(dirname "$output")
mkdir -p "$output_dir"
temporary=$(mktemp "$output_dir/.infercrane-backup.XXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
started=$(date -u +%s)
pg_dump --format=custom --no-owner --no-privileges --file="$temporary" "$INFERCRANE_DATABASE_URL"
pg_restore --list "$temporary" >/dev/null
mv "$temporary" "$output"
trap - EXIT HUP INT TERM
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$output" >"$output.sha256"
else
  shasum -a 256 "$output" >"$output.sha256"
fi
finished=$(date -u +%s)
echo "verified backup written to $output in $((finished-started))s; checksum $output.sha256"
