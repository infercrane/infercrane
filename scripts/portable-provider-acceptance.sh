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
candidate_revision=$(git -C "$root" rev-parse --short=12 HEAD)
# A commit-bound qualification must execute a control-plane image built from
# that commit. Reusing a mutable release-candidate tag can silently exercise an
# older binary while the evidence names the current checkout.
candidate_image=${INFERCRANE_V1_IMAGE:-infercrane:acceptance-$candidate_revision}
name_suffix=$(printf '%s' "$run_id-$cloud" | cksum | awk '{print $1}')
vllm_name="ic-v1-vllm-$name_suffix"
sglang_name="ic-v1-sglang-$name_suffix"
custom_name="ic-v1-custom-$name_suffix"
baseline_inventory="$state/provider-inventory.before"
runtimes=${INFERCRANE_V1_RUNTIMES:-"vllm sglang custom-oci"}
for runtime in $runtimes; do
  case "$runtime" in vllm|sglang|custom-oci) ;; *) echo "unsupported qualification runtime: $runtime" >&2; exit 2;; esac
done

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
        AWS_ACCESS_KEY_ID=$1 AWS_SECRET_ACCESS_KEY=$2 AWS_SESSION_TOKEN=$3
        export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
        aws ec2 describe-instances --region "$INFERCRANE_AWS_REGION" \
          --filters Name=tag:infercrane:managed,Values=true Name=instance-state-name,Values=pending,running,stopping,shutting-down \
          --query "Reservations[].Instances[].InstanceId" --output text --no-cli-pager |
          tr "\t " "\n\n" | sed "/^$/d;s/^/instance:/"
        aws ec2 describe-volumes --region "$INFERCRANE_AWS_REGION" \
          --filters Name=tag:infercrane:managed,Values=true Name=status,Values=creating,available,in-use,deleting,error \
          --query "Volumes[].VolumeId" --output text --no-cli-pager |
          tr "\t " "\n\n" | sed "/^$/d;s/^/volume:/"
        unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
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
    features=${INFERCRANE_V1_VLLM_FEATURES:-"tools structured"}
    for feature in $features; do
      case "$feature" in
        tools)
          curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
            -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"weather\"}],\"tool_choice\":{\"type\":\"function\",\"function\":{\"name\":\"weather\"}},\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"weather\",\"parameters\":{\"type\":\"object\"}}}]}" \
            "$base_url/v1/chat/completions" | jq -e '.choices[0].message.tool_calls[0].function.name == "weather"' >/dev/null
          ;;
        structured)
          curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
            -d "{\"model\":\"$deployment\",\"messages\":[{\"role\":\"user\",\"content\":\"answer with JSON\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"answer\",\"schema\":{\"type\":\"object\",\"properties\":{\"answer\":{\"type\":\"string\"}},\"required\":[\"answer\"]}}}}" \
            "$base_url/v1/chat/completions" | jq -e '.choices[0].message.content | fromjson | has("answer")' >/dev/null
          ;;
        none) ;;
        *) echo "unsupported vLLM feature qualification: $feature" >&2; return 2 ;;
      esac
    done
  fi
}

qualify_spec() {
  spec=$1
  deployment=$2
  runtime=$3
  benchmark_file="$state/$runtime-benchmark.json"
  [ -r "$spec_dir/$spec" ] || { echo "required spec is missing: $spec_dir/$spec" >&2; return 1; }
  cli deploy "/qualification/$spec" --wait --wait-timeout "${INFERCRANE_V1_READY_TIMEOUT:-45m}" \
    --idempotency-key "$run_id-$cloud-$runtime-deploy" --output json | jq -e '.operation.status == "succeeded"' >/dev/null || return
  smoke_openai "$deployment" "$runtime" || return
  cli benchmark "$deployment" --requests 5 --concurrency 1 --random-seed 17 --output json >"$benchmark_file" || return
  chmod 0600 "$benchmark_file"
  jq -e --arg runtime "$runtime" '
    .id != null and
    .runtime == $runtime and
    .model_identity != "" and
    .provider != "" and
    .region != "" and
    .request_count == 5 and
    .failed == 0 and
    (.reproduction_command | contains("${INFERCRANE_API_KEY}"))
  ' "$benchmark_file" >/dev/null || return
  if [ "$cloud" = aws ] && [ "$runtime" = vllm ]; then
    matrix_dir="$state/performance-matrix"
    INFERCRANE_BENCHMARK_CLI="$root/scripts/portable-provider-cli.sh" \
    INFERCRANE_BENCHMARK_RUN_ID="$run_id-aws-vllm" \
    INFERCRANE_PORTABLE_ROOT="$root" \
    INFERCRANE_PORTABLE_PROJECT="$project" \
    INFERCRANE_PORTABLE_ENV_FILE="$env_file" \
    INFERCRANE_PORTABLE_PROVIDER_COMPOSE="$provider_compose" \
    INFERCRANE_PORTABLE_SPEC_DIR="$spec_dir" \
    INFERCRANE_PORTABLE_API_KEY_FILE="$key_file" \
    INFERCRANE_PORTABLE_PASSWORD_FILE="$password_file" \
    INFERCRANE_PORTABLE_IMAGE="$candidate_image" \
    INFERCRANE_PORTABLE_PORT="$port" \
      "$root/scripts/benchmark-matrix.sh" "$deployment" --approve-load --output "$matrix_dir" || return
    jq -e '
      .evidence_class == "measured" and
      .profile_version == "benchmark-profile-v1" and
      ([.results[].workload.concurrency] | sort) == [1,4,4,8,8,32,128] and
      ([.results[].failed] | add) == 0
    ' "$matrix_dir/matrix.json" >/dev/null || return
    if [ "${INFERCRANE_V1_CONCURRENCY_SWEEP:-false}" = true ]; then
      sweep_dir="$state/concurrency-sweep"
      INFERCRANE_BENCHMARK_CLI="$root/scripts/portable-provider-cli.sh" \
      INFERCRANE_BENCHMARK_RUN_ID="$run_id-aws-vllm-sweep" \
      INFERCRANE_PORTABLE_ROOT="$root" \
      INFERCRANE_PORTABLE_PROJECT="$project" \
      INFERCRANE_PORTABLE_ENV_FILE="$env_file" \
      INFERCRANE_PORTABLE_PROVIDER_COMPOSE="$provider_compose" \
      INFERCRANE_PORTABLE_SPEC_DIR="$spec_dir" \
      INFERCRANE_PORTABLE_API_KEY_FILE="$key_file" \
      INFERCRANE_PORTABLE_PASSWORD_FILE="$password_file" \
      INFERCRANE_PORTABLE_IMAGE="$candidate_image" \
      INFERCRANE_PORTABLE_PORT="$port" \
        "$root/scripts/benchmark-concurrency-sweep.sh" "$deployment" --approve-load --output "$sweep_dir" || return
      jq -e '
        .campaign == "same-workload-concurrency-sweep" and
        .evidence_class == "measured" and
        [.results[].workload.concurrency] == [1,8,32,128]
      ' "$sweep_dir/sweep.json" >/dev/null || return
    fi
  fi
  cli delete "$deployment" --yes --wait --wait-timeout "${INFERCRANE_V1_DELETE_TIMEOUT:-15m}" \
    --idempotency-key "$run_id-$cloud-$runtime-delete" --output json | jq -e '.operation.status == "succeeded"' >/dev/null || return
}

cleanup() {
  for name in "$custom_name" "$sglang_name" "$vllm_name"; do
    cli delete "$name" --yes --wait --wait-timeout "${INFERCRANE_V1_DELETE_TIMEOUT:-15m}" \
      --idempotency-key "$run_id-$cloud-$name-cleanup" >/dev/null 2>&1 || true
  done
	# A failed or interrupted paid suite must not leave its isolated control
	# plane, PostgreSQL volume, or network running on the durable runner. Cloud
	# deletion happens first because it needs the persisted operation state.
	compose down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap 'cleanup' HUP INT TERM EXIT

if ! docker image inspect "$candidate_image" >/dev/null 2>&1; then
  docker build --target runtime -t "$candidate_image" "$root"
fi
for runtime in $runtimes; do
  case "$runtime" in
    vllm) render_spec "${INFERCRANE_V1_VLLM_SPEC:-vllm.yaml}" vllm.yaml "$vllm_name" ;;
    sglang) render_spec "${INFERCRANE_V1_SGLANG_SPEC:-sglang.yaml}" sglang.yaml "$sglang_name" ;;
    custom-oci) render_spec "${INFERCRANE_V1_CUSTOM_SPEC:-custom-oci.yaml}" custom-oci.yaml "$custom_name" ;;
  esac
done
echo "==> $cloud/control-plane"
compose up -d
wait_ready
echo "<== $cloud/control-plane (ready)"
stage doctor cli doctor "$doctor_flag" --output json
[ -f "$baseline_inventory" ] || provider_inventory >"$baseline_inventory"
for runtime in $runtimes; do
  case "$runtime" in
    vllm) stage vllm qualify_spec vllm.yaml "$vllm_name" vllm ;;
    sglang) stage sglang qualify_spec sglang.yaml "$sglang_name" sglang ;;
    custom-oci) stage custom-oci qualify_spec custom-oci.yaml "$custom_name" custom-oci ;;
  esac
done
stage zero-provider-inventory inventory_clean
trap - HUP INT TERM EXIT
compose down --volumes --remove-orphans
jq -n --arg cloud "$cloud" --arg run_id "$run_id" --arg commit "$(git -C "$root" rev-parse HEAD)" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{schema_version:1,cloud:$cloud,run_id:$run_id,commit:$commit,status:"passed",generated_at:$generated_at}' >"$state/qualification.json"
echo "$state/qualification.json"
