#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
project=${INFERCRANE_RESTORE_PROJECT:-"infercrane-restore-$(date -u +%Y%m%d%H%M%S)-$$"}
evidence=$(mktemp -d)
compose="docker compose --project-name $project --file $root/compose.yaml --file $root/compose.ha.yaml"
cleanup() {
  docker rm -f "$project-restored-control" >/dev/null 2>&1 || true
  $compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$evidence"
}
trap cleanup EXIT HUP INT TERM

$compose up --build -d postgres worker-a worker-b infercrane >/dev/null
attempt=0
until $compose exec -T infercrane python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/readyz', timeout=2)" >/dev/null 2>&1; do
  attempt=$((attempt+1)); [ "$attempt" -lt 60 ] || { $compose logs infercrane >&2; exit 1; }; sleep 1
done
$compose stop infercrane >/dev/null

network="${project}_default"
database_url='postgres://infercrane:infercrane@postgres:5432/infercrane?sslmode=disable'
docker run --rm --network "$network" -v "$root:/workspace:ro" -v "$evidence:/evidence" -e INFERCRANE_DATABASE_URL="$database_url" postgres:17-alpine /workspace/scripts/backup-postgres.sh /evidence/backup.dump >/dev/null
test -s "$evidence/backup.dump"
test -s "$evidence/backup.dump.sha256"

$compose exec -T postgres createdb -U infercrane infercrane_restore
restore_url='postgres://infercrane:infercrane@postgres:5432/infercrane_restore?sslmode=disable'
docker run --rm --network "$network" -v "$root:/workspace:ro" -v "$evidence:/evidence:ro" -e INFERCRANE_DATABASE_URL="$restore_url" -e INFERCRANE_ALLOW_RESTORE=yes -e INFERCRANE_RESTORE_TARGET_DATABASE=infercrane_restore postgres:17-alpine /workspace/scripts/restore-postgres.sh /evidence/backup.dump >/dev/null

restored_count=$(docker run --rm --network "$network" postgres:17-alpine psql "$restore_url" -X -A -t -v ON_ERROR_STOP=1 -c 'SELECT count(*) FROM schema_migrations')
source_count=$($compose exec -T postgres psql -U infercrane -d infercrane -X -A -t -v ON_ERROR_STOP=1 -c 'SELECT count(*) FROM schema_migrations' | tr -d '\r')
if [ "$restored_count" != "$source_count" ]; then
  echo "restored migration ledger $restored_count differs from source $source_count" >&2
  exit 1
fi

image_id=$($compose images -q infercrane)
test -n "$image_id" || { echo "could not resolve the built InferCrane image" >&2; exit 1; }
container=$(docker run -d --name "$project-restored-control" --network "$network" --entrypoint infercrane -e INFERCRANE_API_KEY=infercrane -e INFERCRANE_DATABASE_URL="$restore_url" -e INFERCRANE_INSTANCE_ID=restored-control "$image_id" serve)
attempt=0
until docker exec "$container" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/readyz', timeout=2)" >/dev/null 2>&1; do
  attempt=$((attempt+1)); [ "$attempt" -lt 30 ] || { docker logs "$container" >&2; exit 1; }; sleep 1
done
docker stop "$container" >/dev/null

echo "Real PostgreSQL backup, guarded restore, migration ledger preservation, and restored control-plane startup passed."
