#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
mkdir -p "$temporary/bin" "$temporary/state"

cat >"$temporary/bin/aws" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$INFERCRANE_FAKE_AWS_LOG"
args=$*
case "$args" in
  *' ec2 run-instances '*)
    previous=''
    for argument in "$@"; do
      case "$previous" in
        --user-data)
          path=${argument#file://}
          cp "$path" "$INFERCRANE_FAKE_USER_DATA"
          ;;
      esac
      previous=$argument
    done
    printf '%s\n' i-1234567890abcdef0
    ;;
  *' ec2 describe-instances '*'BlockDeviceMappings'*) printf '%s\n' vol-1234567890abcdef0 ;;
  *' ec2 describe-instances '*) printf '%s\n' stopped ;;
  *' ec2 get-console-output '*) printf '%s\n' 'infercrane_artifact_builder stage=complete at=2026-08-24T10:00:00Z' ;;
  *' ec2 create-snapshot '*) printf '%s\n' snap-1234567890abcdef0 ;;
  *' ec2 describe-snapshots '*'full_snapshot_size_bytes'*) printf '%s\n' '{"snapshot_id":"snap-1234567890abcdef0","state":"completed","encrypted":true,"volume_size_gib":40,"full_snapshot_size_bytes":1234}' ;;
  *' ec2 describe-snapshots '*'State'*) printf '%s\n' completed ;;
  *' ec2 describe-snapshots '*'artifact-cache'*) printf '%s\n' 1 ;;
  *' ec2 describe-snapshots '*'build-run-id'*) printf '%s\n' 1 ;;
  *' ec2 describe-snapshots '*) echo "unexpected describe-snapshots query: $args" >&2; exit 23 ;;
  *' ec2 terminate-instances '*|*' ec2 wait instance-terminated '*|*' ec2 delete-volume '*|*' ec2 delete-snapshot '*) : ;;
  *) echo "unexpected fake AWS command: $args" >&2; exit 23 ;;
esac
EOF
cat >"$temporary/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$temporary/bin/aws" "$temporary/bin/sleep"

export PATH="$temporary/bin:$PATH"
export INFERCRANE_FAKE_AWS_LOG="$temporary/aws.log"
export INFERCRANE_FAKE_USER_DATA="$temporary/user-data.sh"
export INFERCRANE_AWS_REGION=eu-central-1
export INFERCRANE_AWS_ARTIFACT_BUILD_RUN_ID=fixture-cache
export INFERCRANE_AWS_ARTIFACT_BUILD_STATE_DIR="$temporary/state"
export INFERCRANE_AWS_ARTIFACT_MODEL=mistralai/Mistral-7B-Instruct-v0.3
export INFERCRANE_AWS_ARTIFACT_REVISION=c170c708c41dac9275d15a8fff4eca08d52bab71
export INFERCRANE_AWS_ARTIFACT_BUILDER_AMI_ID=ami-12345678
export INFERCRANE_AWS_ARTIFACT_BUILDER_SUBNET_ID=subnet-12345678
export INFERCRANE_AWS_ARTIFACT_BUILDER_SECURITY_GROUP_IDS=sg-12345678
export INFERCRANE_AWS_ARTIFACT_IGNORE_PATTERNS_JSON='["consolidated.safetensors"]'

"$root/scripts/aws-artifact-cache-build.sh" build --approve-paid-resources >/dev/null
jq -e '.["mistralai/Mistral-7B-Instruct-v0.3@c170c708c41dac9275d15a8fff4eca08d52bab71"] == "snap-1234567890abcdef0"' \
  "$temporary/state/fixture-cache/snapshot-mapping.json" >/dev/null
jq -e '.encrypted == true and .state == "completed"' "$temporary/state/fixture-cache/result.json" >/dev/null
grep -Fq 'export INFERCRANE_MODEL=' "$temporary/user-data.sh"
grep -Fq 'snapshot_download(' "$temporary/user-data.sh"
grep -Fq 'mkfs.ext4 -F -L INFERCRANE_ART' "$temporary/user-data.sh"
grep -Fq 'stage dependencies_ready' "$temporary/user-data.sh"
grep -Fq 'stage client_ready' "$temporary/user-data.sh"
grep -Fq 'HF_HUB_DISABLE_XET=1' "$temporary/user-data.sh"
grep -Fq 'GetConsoleOutput is eventually consistent' "$root/scripts/aws-artifact-cache-build.sh"
grep -Fq 'ec2 terminate-instances' "$temporary/aws.log"
grep -Fq 'ec2 delete-volume' "$temporary/aws.log"

"$root/scripts/aws-artifact-cache-build.sh" cleanup >/dev/null
"$root/scripts/aws-artifact-cache-build.sh" delete-snapshot --approve-snapshot-deletion >/dev/null
grep -Fq 'ec2 delete-snapshot' "$temporary/aws.log"
test -s "$temporary/state/fixture-cache/snapshot-deleted-at"

echo "AWS artifact-cache build and guarded cleanup fixture passed"
