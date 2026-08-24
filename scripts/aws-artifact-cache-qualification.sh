#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
model=${1:-}
[ -n "$model" ] || { echo "usage: $0 mistral|deepseek|granite --approve-paid-resources" >&2; exit 2; }
[ "${2:-}" = "--approve-paid-resources" ] || {
  echo "AWS artifact-cache qualification creates paid GPU resources; pass --approve-paid-resources" >&2
  exit 1
}

source_env=${INFERCRANE_V1_PROVIDER_ENV_FILE:?set the private AWS qualification environment file}
mapping_file=${INFERCRANE_AWS_ARTIFACT_SNAPSHOT_MAPPING_FILE:?set the builder snapshot-mapping.json path}
[ -r "$source_env" ] || { echo "provider environment file is not readable" >&2; exit 1; }
[ -r "$mapping_file" ] || { echo "artifact snapshot mapping is not readable" >&2; exit 1; }
jq -e 'type == "object" and length == 1 and (to_entries[0].key | test("@[0-9a-f]{40}$")) and (to_entries[0].value | test("^snap-[0-9a-f]+$"))' "$mapping_file" >/dev/null || {
  echo "artifact snapshot mapping is invalid" >&2
  exit 1
}

run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-aws-$model-artifact-cache}
state_root=${INFERCRANE_V1_ACCEPTANCE_STATE_DIR:-"$root/.infercrane/v1-acceptance"}
derived_dir="$state_root/$run_id/cache-qualification"
mkdir -p "$derived_dir"
chmod 700 "$derived_dir"
derived_env="$derived_dir/aws.env"

# Remove only the two cache settings and append a one-line verified mapping.
# Provider credentials remain references in a mode-0600 file and are never
# printed or copied into the evidence archive.
awk '!/^INFERCRANE_AWS_ARTIFACT_CACHE_POLICY=/ && !/^INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON=/' "$source_env" >"$derived_env"
printf 'INFERCRANE_AWS_ARTIFACT_CACHE_POLICY=required\n' >>"$derived_env"
printf 'INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON=%s\n' "$(jq -c . "$mapping_file")" >>"$derived_env"
chmod 0600 "$derived_env"

INFERCRANE_V1_PROVIDER_ENV_FILE="$derived_env" \
INFERCRANE_V1_EXPECT_ARTIFACT_CACHE=hit \
INFERCRANE_V1_PERFORMANCE_MATRIX=false \
INFERCRANE_ACCEPTANCE_RUN_ID="$run_id" \
  "$root/scripts/aws-performance-qualification.sh" "$model" --approve-paid-resources

echo "$state_root/$run_id/aws/qualification.json"
