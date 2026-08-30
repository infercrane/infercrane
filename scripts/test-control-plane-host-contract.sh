#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fly_config="$project_root/deploy/fly/control-plane.toml"
hosting_doc="$project_root/docs/architecture/control-plane-hosting.mdx"
production_doc="$project_root/docs/production.md"

python3 - "$fly_config" <<'PY'
import pathlib
import sys
import tomllib

path = pathlib.Path(sys.argv[1])
data = tomllib.loads(path.read_text())

assert data["kill_signal"] == "SIGTERM"
assert data["kill_timeout"] >= 30
assert data["build"]["build-target"] == "runtime"
assert data["http_service"]["checks"][0]["path"] == "/readyz"
assert data["http_service"]["auto_stop_machines"] == "off"
assert data["http_service"]["min_machines_running"] == 1
assert "mounts" not in data
assert "INFERCRANE_INSTANCE_ID" not in data["env"]
PY

grep -Fq 'external PostgreSQL' "$hosting_doc"
grep -Fq 'immutable OCI image' "$hosting_doc"
grep -Fq '`/livez`' "$hosting_doc"
grep -Fq '`/readyz`' "$hosting_doc"
grep -Fq 'No host-local durable state' "$hosting_doc"
grep -Fq 'Render' "$hosting_doc"
grep -Fq 'AWS ECS' "$hosting_doc"
grep -Fq 'Kubernetes' "$hosting_doc"
grep -Fq 'one shared value' "$production_doc"

if grep -Eq "INFERCRANE_INSTANCE_ID='fly-|INFERCRANE_INSTANCE_ID=fly-" "$production_doc"; then
  echo 'Fly runbook hard-codes a replica identity that would collide after scale-out' >&2
  exit 1
fi

echo 'provider-neutral hosted control-plane contract passed'
