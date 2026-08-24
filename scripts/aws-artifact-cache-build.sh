#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command=${1:-}
if [ "$command" = build ]; then
  shift
  [ "${1:-}" = "--approve-paid-resources" ] || {
    echo "artifact cache build creates paid AWS resources; pass --approve-paid-resources" >&2
    exit 1
  }
  shift
elif [ "$command" = cleanup ]; then
  shift
elif [ "$command" = delete-snapshot ]; then
  shift
  [ "${1:-}" = "--approve-snapshot-deletion" ] || {
    echo "snapshot deletion is destructive; pass --approve-snapshot-deletion" >&2
    exit 1
  }
  shift
else
  echo "usage: $0 build --approve-paid-resources | cleanup | delete-snapshot --approve-snapshot-deletion" >&2
  exit 2
fi

region=${INFERCRANE_AWS_REGION:-}
run_id=${INFERCRANE_AWS_ARTIFACT_BUILD_RUN_ID:-}
state_root=${INFERCRANE_AWS_ARTIFACT_BUILD_STATE_DIR:-"$root/.infercrane/aws-artifact-cache-build"}
case "$run_id" in
  ""|*[!A-Za-z0-9._-]*) echo "set a safe INFERCRANE_AWS_ARTIFACT_BUILD_RUN_ID" >&2; exit 2 ;;
esac
[ -n "$region" ] || { echo "set INFERCRANE_AWS_REGION" >&2; exit 2; }
state="$state_root/$run_id"
mkdir -p "$state"
chmod 700 "$state"

record() {
  key=$1
  value=$2
  case "$key" in *[!A-Za-z0-9._-]*) return 2 ;; esac
  (umask 077; printf '%s\n' "$value" >"$state/$key")
}

read_record() {
  key=$1
  [ -r "$state/$key" ] && sed -n '1p' "$state/$key" || true
}

aws_cli() {
  aws --region "$region" --no-cli-pager "$@"
}

closed_console_output() {
  raw=$(aws_cli ec2 get-console-output --instance-id "$1" --latest --query Output --output text 2>/dev/null || true)
  case "$raw" in None) raw='' ;; esac
  if printf '%s' "$raw" | grep -Fq 'infercrane_artifact_builder stage='; then
    printf '%s\n' "$raw"
    return
  fi
  decoded=$(printf '%s' "$raw" | tr -d '\r\n' | openssl base64 -d -A 2>/dev/null || true)
  if printf '%s' "$decoded" | grep -Fq 'infercrane_artifact_builder stage='; then
    printf '%s\n' "$decoded"
  fi
}

valid_id() {
  value=$1
  prefix=$2
  case "$value" in "$prefix"[A-Za-z0-9-]*) return 0 ;; *) return 1 ;; esac
}

cleanup_recorded_resources() {
  instance_id=$(read_record instance-id)
  volume_id=$(read_record volume-id)
  if [ -n "$instance_id" ] && valid_id "$instance_id" i-; then
    aws_cli ec2 terminate-instances --instance-ids "$instance_id" --output json >/dev/null 2>&1 || true
    aws_cli ec2 wait instance-terminated --instance-ids "$instance_id" >/dev/null 2>&1 || true
  fi
  if [ -n "$volume_id" ] && valid_id "$volume_id" vol-; then
    aws_cli ec2 delete-volume --volume-id "$volume_id" >/dev/null 2>&1 || true
  fi
}

if [ "$command" = cleanup ]; then
  cleanup_recorded_resources
  echo "InferCrane artifact-cache builder resources cleaned for $run_id"
  exit 0
fi

if [ "$command" = delete-snapshot ]; then
  snapshot_id=$(read_record snapshot-id)
  valid_id "$snapshot_id" snap- || { echo "no valid snapshot is recorded for $run_id" >&2; exit 1; }
  owned=$(aws_cli ec2 describe-snapshots --snapshot-ids "$snapshot_id" --query \
    "Snapshots[0].Tags[?Key=='infercrane:artifact-cache' && Value=='true'] | length(@)" --output text)
  matching_run=$(aws_cli ec2 describe-snapshots --snapshot-ids "$snapshot_id" --query \
    "Snapshots[0].Tags[?Key=='infercrane:build-run-id' && Value=='$run_id'] | length(@)" --output text)
  [ "$owned" = 1 ] && [ "$matching_run" = 1 ] || {
    echo "refusing to delete a snapshot outside the recorded InferCrane build ownership boundary" >&2
    exit 1
  }
  aws_cli ec2 delete-snapshot --snapshot-id "$snapshot_id"
  record snapshot-deleted-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "Deleted InferCrane artifact snapshot $snapshot_id for $run_id"
  exit 0
fi

for binary in aws jq openssl; do
  command -v "$binary" >/dev/null 2>&1 || { echo "$binary is required" >&2; exit 2; }
done

model=${INFERCRANE_AWS_ARTIFACT_MODEL:-}
revision=${INFERCRANE_AWS_ARTIFACT_REVISION:-}
ami_id=${INFERCRANE_AWS_ARTIFACT_BUILDER_AMI_ID:-${INFERCRANE_AWS_AMI_ID:-}}
subnet_id=${INFERCRANE_AWS_ARTIFACT_BUILDER_SUBNET_ID:-${INFERCRANE_AWS_SUBNET_ID:-}}
security_groups=${INFERCRANE_AWS_ARTIFACT_BUILDER_SECURITY_GROUP_IDS:-${INFERCRANE_AWS_SECURITY_GROUP_IDS:-}}
instance_type=${INFERCRANE_AWS_ARTIFACT_BUILDER_INSTANCE_TYPE:-t3.small}
volume_gib=${INFERCRANE_AWS_ARTIFACT_VOLUME_GIB:-40}
ignore_patterns=${INFERCRANE_AWS_ARTIFACT_IGNORE_PATTERNS_JSON:-[]}
max_wait_seconds=${INFERCRANE_AWS_ARTIFACT_BUILD_TIMEOUT_SECONDS:-7200}

case "$model" in */*) ;; *) echo "set INFERCRANE_AWS_ARTIFACT_MODEL to a Hub organization/model ID" >&2; exit 2 ;; esac
case "$model" in *[!A-Za-z0-9._/-]*) echo "artifact model contains unsupported characters" >&2; exit 2 ;; esac
case "$revision" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) echo "artifact revision must be an exact lowercase 40-character commit" >&2; exit 2 ;;
esac
valid_id "$ami_id" ami- || { echo "set a valid builder AMI ID" >&2; exit 2; }
valid_id "$subnet_id" subnet- || { echo "set a valid builder subnet ID" >&2; exit 2; }
[ -n "$security_groups" ] || { echo "set builder security-group IDs" >&2; exit 2; }
case "$volume_gib" in ''|*[!0-9]*) echo "artifact volume size must be an integer" >&2; exit 2 ;; esac
[ "$volume_gib" -ge 20 ] && [ "$volume_gib" -le 2048 ] || { echo "artifact volume size must be 20-2048 GiB" >&2; exit 2; }
case "$max_wait_seconds" in ''|*[!0-9]*) echo "artifact build timeout must be an integer" >&2; exit 2 ;; esac
jq -e 'type == "array" and all(.[]; type == "string")' >/dev/null <<EOF
$ignore_patterns
EOF

identity="$model@$revision"
identity_digest="sha256:$(printf '%s' "$identity" | openssl dgst -sha256 -r | awk '{print $1}')"
record model-identity "$identity"
record model-identity-digest "$identity_digest"

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
user_data="$temporary/user-data.sh"
cat >"$user_data" <<'EOF'
#!/bin/sh
set -eu
current_stage=booting
stage() { current_stage=$1; printf 'infercrane_artifact_builder stage=%s at=%s\n' "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/console; }
failed() {
  status=$?
  trap - EXIT HUP INT TERM
  stage "${current_stage}_failed"
  sync
  sleep 5
  poweroff || true
  exit "$status"
}
trap failed EXIT HUP INT TERM
stage booted
root_source=$(findmnt -n -o SOURCE /)
root_parent=$(lsblk -n -o PKNAME "$root_source" 2>/dev/null | head -1)
[ -n "$root_parent" ] || root_parent=$(basename "$root_source")
attempt=0
artifact_device=''
while [ "$attempt" -lt 120 ]; do
  for candidate in $(lsblk -dpno NAME,TYPE | awk '$2 == "disk" {print $1}'); do
    [ "$(basename "$candidate")" = "$root_parent" ] && continue
    artifact_device=$candidate
    break
  done
  [ -n "$artifact_device" ] && break
  attempt=$((attempt + 1))
  sleep 5
done
[ -n "$artifact_device" ] || { stage volume_missing; exit 1; }
stage volume_attached
mkfs.ext4 -F -L INFERCRANE_ART "$artifact_device" >/dev/null
mkdir -p /var/lib/infercrane/artifact
mount -o noatime,nosuid,nodev "$artifact_device" /var/lib/infercrane/artifact
stage filesystem_ready
stage dependencies_start
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq python3-pip ca-certificates
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q python3-pip ca-certificates
elif command -v yum >/dev/null 2>&1; then
  yum install -y -q python3-pip ca-certificates
fi
stage dependencies_ready
mkdir -p /opt/infercrane-cache-builder/lib
stage environment_ready
python3 -m pip install --disable-pip-version-check --quiet --target /opt/infercrane-cache-builder/lib 'huggingface_hub==0.36.0'
stage client_ready
stage download_start
HF_HOME=/var/lib/infercrane/artifact \
  HF_HUB_DISABLE_XET=1 \
  HF_HUB_DISABLE_PROGRESS_BARS=1 \
  PYTHONPATH=/opt/infercrane-cache-builder/lib \
  INFERCRANE_MODEL="$INFERCRANE_MODEL" \
  INFERCRANE_REVISION="$INFERCRANE_REVISION" \
  INFERCRANE_IGNORE_PATTERNS_JSON="$INFERCRANE_IGNORE_PATTERNS_JSON" \
  python3 - <<'PY'
import json
import os
from huggingface_hub import snapshot_download

snapshot_download(
    repo_id=os.environ["INFERCRANE_MODEL"],
    revision=os.environ["INFERCRANE_REVISION"],
    cache_dir="/var/lib/infercrane/artifact/hub",
    ignore_patterns=json.loads(os.environ["INFERCRANE_IGNORE_PATTERNS_JSON"]),
    max_workers=4,
)
PY
stage download_complete
sync
umount /var/lib/infercrane/artifact
stage complete
trap - EXIT HUP INT TERM
sleep 5
poweroff
EOF
chmod 0600 "$user_data"

# Pass model metadata through cloud-init variables, not shell interpolation.
python3 - "$user_data" "$model" "$revision" "$ignore_patterns" <<'PY'
import pathlib
import shlex
import sys

path = pathlib.Path(sys.argv[1])
body = path.read_text()
exports = "export INFERCRANE_MODEL={} INFERCRANE_REVISION={} INFERCRANE_IGNORE_PATTERNS_JSON={}\n".format(
    shlex.quote(sys.argv[2]), shlex.quote(sys.argv[3]), shlex.quote(sys.argv[4])
)
path.write_text(body.replace("set -eu\n", "set -eu\n" + exports, 1))
PY

instance_id=$(read_record instance-id)
if [ -z "$instance_id" ]; then
  block_devices=$(jq -cn --argjson size "$volume_gib" '[{DeviceName:"/dev/sdf",Ebs:{DeleteOnTermination:false,Encrypted:true,VolumeSize:$size,VolumeType:"gp3"}}]')
  tags=$(jq -cn --arg run "$run_id" '[
    {ResourceType:"instance",Tags:[{Key:"infercrane:artifact-builder",Value:"true"},{Key:"infercrane:build-run-id",Value:$run},{Key:"Name",Value:("infercrane-artifact-builder-"+$run)}]},
    {ResourceType:"volume",Tags:[{Key:"infercrane:artifact-builder",Value:"true"},{Key:"infercrane:build-run-id",Value:$run},{Key:"Name",Value:("infercrane-artifact-builder-"+$run)}]}
  ]')
  set -- $security_groups
  client_token="infercrane-artifact-$(printf '%s' "$region/$run_id/$identity" | openssl dgst -sha256 -r | awk '{print substr($1,1,32)}')"
  instance_id=$(aws_cli ec2 run-instances \
    --image-id "$ami_id" \
    --instance-type "$instance_type" \
    --subnet-id "$subnet_id" \
    --security-group-ids "$@" \
    --block-device-mappings "$block_devices" \
    --tag-specifications "$tags" \
    --user-data "file://$user_data" \
    --client-token "$client_token" \
    --query 'Instances[0].InstanceId' --output text)
  valid_id "$instance_id" i- || { echo "AWS did not return a builder instance ID" >&2; exit 1; }
  record instance-id "$instance_id"
fi
# A normal interrupt initiates best-effort cleanup. The recorded run ID also
# lets an operator repeat the cleanup after a laptop or network failure.
trap 'rm -rf "$temporary"; cleanup_recorded_resources; exit 130' HUP INT TERM
echo "Artifact builder $instance_id is downloading the exact model revision on $instance_type"

volume_id=$(read_record volume-id)
started=$(date +%s)
while [ -z "$volume_id" ]; do
  volume_id=$(aws_cli ec2 describe-instances --instance-ids "$instance_id" \
    --query 'Reservations[0].Instances[0].BlockDeviceMappings[?DeviceName==`/dev/sdf`].Ebs.VolumeId | [0]' --output text)
  [ "$volume_id" = None ] && volume_id=''
  [ -n "$volume_id" ] && break
  [ $(( $(date +%s) - started )) -lt 300 ] || { echo "builder data volume did not appear" >&2; cleanup_recorded_resources; exit 1; }
  sleep 5
done
valid_id "$volume_id" vol- || { echo "AWS returned an invalid artifact volume ID" >&2; cleanup_recorded_resources; exit 1; }
record volume-id "$volume_id"

last_status=''
last_stage=''
while :; do
  status=$(aws_cli ec2 describe-instances --instance-ids "$instance_id" --query 'Reservations[0].Instances[0].State.Name' --output text)
  if [ "$status" != "$last_status" ]; then
    echo "Artifact builder state: $status"
    last_status=$status
  fi
  current_stage=$(closed_console_output "$instance_id" |
    sed -n 's/^.*infercrane_artifact_builder stage=\([a-z_]*\) at=.*$/\1/p' | tail -1 || true)
  if [ -n "$current_stage" ] && [ "$current_stage" != "$last_stage" ]; then
    echo "Artifact builder stage: $current_stage"
    last_stage=$current_stage
  fi
  case "$status" in stopped) break ;; terminated|shutting-down) echo "builder terminated before cache completion" >&2; exit 1 ;; esac
  [ $(( $(date +%s) - started )) -lt "$max_wait_seconds" ] || {
    echo "artifact build timed out; cleaning recorded resources" >&2
    cleanup_recorded_resources
    exit 1
  }
  sleep 20
done

console=''
attempt=0
# GetConsoleOutput is eventually consistent and a stopped instance can report
# an empty result briefly. Keep the instance and volume intact until the
# closed completion/failure marker becomes observable.
while [ "$attempt" -lt 30 ]; do
  console=$(closed_console_output "$instance_id")
  printf '%s\n' "$console" | grep -Eq 'infercrane_artifact_builder stage=(complete|[a-z_]+_failed)' && break
  attempt=$((attempt + 1))
  sleep 10
done
printf '%s\n' "$console" | grep -Fq 'infercrane_artifact_builder stage=complete' || {
  echo "artifact builder stopped without a completion marker" >&2
  printf '%s\n' "$console" | grep 'infercrane_artifact_builder stage=' | tail -12 >&2 || true
  cleanup_recorded_resources
  exit 1
}

snapshot_id=$(read_record snapshot-id)
if [ -z "$snapshot_id" ]; then
  snapshot_tags=$(jq -cn --arg run "$run_id" --arg digest "$identity_digest" '[{ResourceType:"snapshot",Tags:[
    {Key:"infercrane:artifact-cache",Value:"true"},
    {Key:"infercrane:model-identity-digest",Value:$digest},
    {Key:"infercrane:build-run-id",Value:$run},
    {Key:"Name",Value:("infercrane-artifact-cache-"+$run)}
  ]}]')
  snapshot_id=$(aws_cli ec2 create-snapshot --volume-id "$volume_id" \
    --description "InferCrane immutable model artifact cache" \
    --tag-specifications "$snapshot_tags" --query SnapshotId --output text)
  valid_id "$snapshot_id" snap- || { echo "AWS did not return a snapshot ID" >&2; cleanup_recorded_resources; exit 1; }
  record snapshot-id "$snapshot_id"
fi
echo "Waiting for encrypted artifact snapshot $snapshot_id"
while :; do
  snapshot_state=$(aws_cli ec2 describe-snapshots --snapshot-ids "$snapshot_id" --query 'Snapshots[0].State' --output text)
  case "$snapshot_state" in completed) break ;; error) echo "artifact snapshot failed" >&2; cleanup_recorded_resources; exit 1 ;; esac
  [ $(( $(date +%s) - started )) -lt "$max_wait_seconds" ] || { echo "artifact snapshot timed out" >&2; cleanup_recorded_resources; exit 1; }
  sleep 20
done

cleanup_recorded_resources
aws_cli ec2 describe-snapshots --snapshot-ids "$snapshot_id" \
  --query 'Snapshots[0].{snapshot_id:SnapshotId,state:State,encrypted:Encrypted,volume_size_gib:VolumeSize,full_snapshot_size_bytes:FullSnapshotSizeInBytes}' \
  --output json >"$state/result.json"
chmod 0600 "$state/result.json"
jq -n --arg identity "$identity" --arg snapshot "$snapshot_id" '{($identity):$snapshot}' >"$state/snapshot-mapping.json"
chmod 0600 "$state/snapshot-mapping.json"
echo "Artifact cache ready: $snapshot_id"
echo "Mapping: $state/snapshot-mapping.json"
echo "The snapshot remains billable until explicitly deleted. Deploy with cache policy required, qualify, then delete it when evidence is archived."
