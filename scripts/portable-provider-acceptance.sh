#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cloud=${1:-}
case "$cloud" in aws|gcp|kubernetes) ;; *) echo "usage: $0 aws|gcp|kubernetes --approve-paid-resources" >&2; exit 2;; esac
[ "${2:-}" = "--approve-paid-resources" ] || {
  echo "portable provider acceptance requires --approve-paid-resources" >&2
  exit 1
}

run_id=${INFERCRANE_ACCEPTANCE_RUN_ID:?set INFERCRANE_ACCEPTANCE_RUN_ID}
env_file=${INFERCRANE_V1_PROVIDER_ENV_FILE:?set INFERCRANE_V1_PROVIDER_ENV_FILE}
[ -r "$env_file" ] || { echo "provider environment file is not readable: $env_file" >&2; exit 1; }
input_spec_dir=${INFERCRANE_V1_SPEC_DIR:?set INFERCRANE_V1_SPEC_DIR}
[ -d "$input_spec_dir" ] || { echo "qualification spec directory is not readable: $input_spec_dir" >&2; exit 1; }

case "$run_id" in *[!A-Za-z0-9._-]*) echo "acceptance run ID contains unsupported characters" >&2; exit 1;; esac
state_root=${INFERCRANE_V1_ACCEPTANCE_STATE_DIR:-"$root/.infercrane/v1-acceptance"}
state="$state_root/$run_id/$cloud"
mkdir -p "$state/logs"
chmod 700 "$state"
spec_dir="$state/specs"
mkdir -p "$spec_dir"
password_file="$state/postgres-password"
key_file=${INFERCRANE_V1_API_KEY_FILE:?set INFERCRANE_V1_API_KEY_FILE to the credential also installed in the worker secret}
[ -r "$key_file" ] || { echo "INFERCRANE_V1_API_KEY_FILE is not readable" >&2; exit 1; }
if [ ! -s "$password_file" ]; then
  (umask 077; openssl rand -hex 24 >"$password_file")
fi
api_key=$(tr -d '\r\n' <"$key_file")
[ "${#api_key}" -ge 32 ] || { echo "qualification API key must contain at least 32 characters" >&2; exit 1; }
postgres_password=$(tr -d '\r\n' <"$password_file")
port=${INFERCRANE_V1_PORT:-}
if [ -z "$port" ]; then
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
fi
base_url="http://127.0.0.1:$port"
project=$(printf '%s' "infercrane-v1-$run_id-$cloud" | tr '[:upper:]._' '[:lower:]---' | cut -c1-63)
candidate_image=${INFERCRANE_V1_IMAGE:-infercrane:v1.0.0-rc.1}
name_suffix=$(printf '%s' "$run_id-$cloud" | cksum | awk '{print $1}')
vllm_name="ic-v1-vllm-$name_suffix"
sglang_name="ic-v1-sglang-$name_suffix"
custom_name="ic-v1-custom-$name_suffix"
baseline_inventory="$state/provider-inventory.before"

case "$cloud" in
  aws) provider_compose="$root/compose.production.aws.yaml"; doctor_flag=--aws ;;
  gcp) provider_compose="$root/compose.production.gcp.yaml"; doctor_flag=--gcp ;;
  kubernetes) provider_compose="$root/compose.production.kubernetes.yaml"; doctor_flag=--kubernetes ;;
esac

compose() {
  INFERCRANE_API_KEY="$api_key" \
  INFERCRANE_POSTGRES_PASSWORD="$postgres_password" \
  INFERCRANE_IMAGE="$candidate_image" \
  INFERCRANE_URL=http://127.0.0.1:8080 \
  INFERCRANE_PORT="$port" \
  INFERCRANE_QUALIFICATION_SPEC_DIR="$spec_dir" \
    docker compose -p "$project" --env-file "$env_file" \
      -f "$root/compose.production.yaml" -f "$provider_compose" \
      -f "$root/compose.portable-acceptance.yaml" "$@"
}

cli() { compose exec -T infercrane infercrane "$@"; }

stage() {
  name=$1
  shift
  marker="$state/$name.passed"
  if [ -f "$marker" ] && [ "$(cat "$marker")" = "$(git -C "$root" rev-parse HEAD)" ]; then
    echo "==> $cloud/$name (already passed)"
    return 0
  fi
  echo "==> $cloud/$name"
  if "$@" >"$state/logs/$name.log" 2>&1; then
    git -C "$root" rev-parse HEAD >"$marker"
    echo "<== $cloud/$name (passed)"
  else
    status=$?
    echo "<== $cloud/$name (failed; $state/logs/$name.log)" >&2
    tail -n 80 "$state/logs/$name.log" >&2
    return "$status"
  fi
}

wait_ready() {
  attempt=0
  until curl -fsS "$base_url/readyz" >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 90 ] || { compose logs --tail 120 infercrane >&2; return 1; }
    sleep 2
  done
}

inventory_clean() {
  cli orphans --output json | jq -e '.data | length == 0' >/dev/null
  current="$state/provider-inventory.after"
  provider_inventory >"$current"
  if ! cmp -s "$baseline_inventory" "$current"; then
    echo "provider inventory changed during $cloud qualification" >&2
    diff -u "$baseline_inventory" "$current" >&2 || true
    return 1
  fi
}

provider_inventory() {
  case "$cloud" in
    aws)
      compose exec -T infercrane sh -eu -c '
        set -- $(aws sts assume-role --role-arn "$INFERCRANE_AWS_ROLE_ARN" --role-session-name infercrane-v1-inventory --external-id "$INFERCRANE_AWS_EXTERNAL_ID" --query "Credentials.[AccessKeyId,SecretAccessKey,SessionToken]" --output text --no-cli-pager)
        AWS_ACCESS_KEY_ID=$1 AWS_SECRET_ACCESS_KEY=$2 AWS_SESSION_TOKEN=$3 \
          aws ec2 describe-instances --region "$INFERCRANE_AWS_REGION" \
            --filters Name=tag:infercrane:managed,Values=true Name=instance-state-name,Values=pending,running,stopping \
            --query "Reservations[].Instances[].InstanceId" --output text --no-cli-pager
      '
      ;;
    kubernetes)
      compose exec -T infercrane sh -eu -c '
        kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" --namespace "$INFERCRANE_KUBERNETES_NAMESPACE" get deployment,service -l app.kubernetes.io/managed-by=infercrane -o name
        if kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" api-resources --api-group serving.kserve.io -o name | grep -qx inferenceservices; then
          kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" --namespace "$INFERCRANE_KUBERNETES_NAMESPACE" get inferenceservices.serving.kserve.io -l app.kubernetes.io/managed-by=infercrane -o name
        fi
      '
      ;;
    gcp)
      compose exec -T infercrane sh -eu -c '
        gcloud compute instances list --project "$INFERCRANE_GCP_PROJECT" \
          --filter="labels.infercrane-managed=true AND (status=PROVISIONING OR status=STAGING OR status=RUNNING OR status=STOPPING)" \
          --format="value(name)" --quiet
      '
      ;;
  esac | tr '\t ' '\n\n' | sed '/^$/d' | sort -u
}

render_spec() {
  source_name=$1
  output_name=$2
  deployment_name=$3
  source_path="$input_spec_dir/$source_name"
  [ -r "$source_path" ] || { echo "required spec is missing: $source_path" >&2; return 1; }
  awk -v deployment_name="$deployment_name" '
    BEGIN { replaced=0 }
    /^name:[[:space:]]/ {
      if (replaced) exit 3
      print "name: " deployment_name
      replaced=1
      next
    }
    { print }
    END { if (!replaced) exit 2 }
  ' "$source_path" >"$spec_dir/$output_name"
  # DeploymentSpecs contain references, not raw credentials, and the runtime
  # container uses a different UID from the qualification host. Keep the
  # rendered mount readable without weakening the password/key files above.
  chmod 0644 "$spec_dir/$output_name"
}

smoke_openai() {
  deployment=$1
  runtime=$2
  cli request "$deployment" --message 'InferCrane v1 acceptance' --output json | jq -e '.choices | length > 0' >/dev/null
  streamed=$(cli request "$deployment" --message 'InferCrane streaming acceptance' --stream)
  [ -n "$streamed" ]
  if [ "$runtime" = vllm ]; then
    curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"weather\"}],\"tool_choice\":{\"type\":\"function\",\"function\":{\"name\":\"weather\"}},\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"weather\",\"parameters\":{\"type\":\"object\"}}}]}" \
      "$base_url/v1/chat/completions" | jq -e '.choices[0].message.tool_calls[0].function.name == "weather"' >/dev/null
    curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"answer with JSON\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"answer\",\"schema\":{\"type\":\"object\",\"properties\":{\"answer\":{\"type\":\"string\"}},\"required\":[\"answer\"]}}}}" \
      "$base_url/v1/chat/completions" | jq -e '.choices[0].message.content | fromjson | has("answer")' >/dev/null
  fi
}

qualify_spec() {
  spec=$1
  deployment=$2
  runtime=$3
  [ -r "$spec_dir/$spec" ] || { echo "required spec is missing: $spec_dir/$spec" >&2; return 1; }
  cli deploy "/qualification/$spec" --wait --wait-timeout "${INFERCRANE_V1_READY_TIMEOUT:-45m}" \
    --idempotency-key "$run_id-$cloud-$runtime-deploy" --output json | jq -e '.operation.status == "succeeded"' >/dev/null
  smoke_openai "$deployment" "$runtime"
  cli benchmark "$deployment" --requests 5 --concurrency 1 --random-seed 17 --output json | jq -e '.id != null' >/dev/null
  cli delete "$deployment" --yes --wait --wait-timeout "${INFERCRANE_V1_DELETE_TIMEOUT:-15m}" \
    --idempotency-key "$run_id-$cloud-$runtime-delete" --output json | jq -e '.operation.status == "succeeded"' >/dev/null
}

cleanup() {
  for name in "$custom_name" "$sglang_name" "$vllm_name"; do
    cli delete "$name" --yes --wait --wait-timeout "${INFERCRANE_V1_DELETE_TIMEOUT:-15m}" \
      --idempotency-key "$run_id-$cloud-$name-cleanup" >/dev/null 2>&1 || true
  done
}
trap 'cleanup' HUP INT TERM EXIT

if ! docker image inspect "$candidate_image" >/dev/null 2>&1; then
  docker build --target runtime -t "$candidate_image" "$root"
fi
render_spec "${INFERCRANE_V1_VLLM_SPEC:-vllm.yaml}" vllm.yaml "$vllm_name"
render_spec "${INFERCRANE_V1_SGLANG_SPEC:-sglang.yaml}" sglang.yaml "$sglang_name"
render_spec "${INFERCRANE_V1_CUSTOM_SPEC:-custom-oci.yaml}" custom-oci.yaml "$custom_name"
echo "==> $cloud/control-plane"
compose up -d
wait_ready
echo "<== $cloud/control-plane (ready)"
stage doctor cli doctor "$doctor_flag" --output json
[ -f "$baseline_inventory" ] || provider_inventory >"$baseline_inventory"
stage vllm qualify_spec vllm.yaml "$vllm_name" vllm
stage sglang qualify_spec sglang.yaml "$sglang_name" sglang
stage custom-oci qualify_spec custom-oci.yaml "$custom_name" custom-oci
stage zero-provider-inventory inventory_clean
trap - HUP INT TERM EXIT
compose down --volumes --remove-orphans
jq -n --arg cloud "$cloud" --arg run_id "$run_id" --arg commit "$(git -C "$root" rev-parse HEAD)" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{schema_version:1,cloud:$cloud,run_id:$run_id,commit:$commit,status:"passed",generated_at:$generated_at}' >"$state/qualification.json"
echo "$state/qualification.json"
