#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
project=${INFERCRANE_HA_PROJECT:-"infercrane-ha-$(date -u +%Y%m%d%H%M%S)-$$"}
compose="docker compose --project-name $project --file $root/compose.yaml --file $root/compose.ha.yaml"
cleanup() { $compose down --volumes --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM

$compose up --build -d postgres worker-a worker-b infercrane infercrane-2 >/dev/null

attempt=0
until $compose exec -T infercrane-2 python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/readyz', timeout=2)" >/dev/null 2>&1; do
  attempt=$((attempt+1))
  if [ "$attempt" -ge 60 ]; then
    $compose logs infercrane infercrane-2 >&2
    exit 1
  fi
  sleep 1
done

members=$($compose exec -T infercrane-2 python -c "import json,urllib.request; r=urllib.request.Request('http://127.0.0.1:8080/api/v1/system/instances',headers={'Authorization':'Bearer infercrane'}); print(json.load(urllib.request.urlopen(r))['count'])")
if [ "$members" != "2" ]; then
  echo "expected two live control-plane instances, got $members" >&2
  exit 1
fi

$compose stop infercrane >/dev/null
attempt=0
until [ "$($compose exec -T infercrane-2 python -c "import json,urllib.request; r=urllib.request.Request('http://127.0.0.1:8080/api/v1/system/instances',headers={'Authorization':'Bearer infercrane'}); print(json.load(urllib.request.urlopen(r))['count'])")" = "1" ]; do
  attempt=$((attempt+1))
  [ "$attempt" -lt 15 ] || {
    echo "stopped instance remained live" >&2
    $compose logs --tail 100 infercrane infercrane-2 >&2 || true
    $compose exec -T postgres psql -U infercrane -d infercrane -c 'TABLE control_plane_instances' >&2 || true
    exit 1
  }
  sleep 1
done
$compose exec -T infercrane-2 python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/readyz', timeout=2)" >/dev/null

echo "Two-instance membership, graceful withdrawal, and surviving API readiness passed."
