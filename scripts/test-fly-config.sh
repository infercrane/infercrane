#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
config="$project_root/deploy/fly/control-plane.toml"

python3 - "$config" <<'PY'
import pathlib
import sys
import tomllib

path = pathlib.Path(sys.argv[1])
data = tomllib.loads(path.read_text())

assert "app" not in data, "the reusable profile must not hard-code a Fly application name"
assert data["primary_region"] == "fra"
assert data["kill_timeout"] >= 30
project_root = path.parents[2]
dockerfile = (path.parent / data["build"]["dockerfile"]).resolve()
assert dockerfile == (project_root / "Dockerfile").resolve()
assert dockerfile.is_file()
assert data["build"]["build-target"] == "runtime"

env = data["env"]
assert env["INFERCRANE_ENV"] == "production"
assert env["INFERCRANE_HOST"] == "0.0.0.0"
assert env["INFERCRANE_PORT"] == "8080"
assert env["INFERCRANE_SKYPILOT_API"] == "auto"
assert env["SKYPILOT_DISABLE_USAGE_COLLECTION"] == "1"
assert env["INFERCRANE_GPU_PRICE_SYNC_SECONDS"] == "75"
assert "INFERCRANE_INSTANCE_ID" not in env, "replica identity must default to the unique machine hostname"

service = data["http_service"]
assert service["internal_port"] == 8080
assert service["force_https"] is True
assert service["auto_stop_machines"] == "off"
assert service["auto_start_machines"] is False
assert service["min_machines_running"] == 1
assert service["checks"][0]["path"] == "/readyz"
assert service["checks"][0]["method"] == "GET"

machine = data["vm"][0]
assert machine["size"] == "shared-cpu-1x"
assert machine["memory"] == "2gb"

serialized = path.read_text().lower()
for forbidden in ("database_url", "api_key", "secret_key", "webhook_secret", "private key"):
    assert forbidden not in serialized, f"Fly profile contains secret-like field: {forbidden}"
for forbidden in ("[mounts]", "[[mounts]]", "source =", "volume"):
    assert forbidden not in serialized, f"hosted control plane must not depend on local durable storage: {forbidden}"
PY

echo 'Fly control-plane profile passed'
