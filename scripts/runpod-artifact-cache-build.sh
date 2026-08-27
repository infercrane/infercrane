#!/bin/sh
set -eu

command_name=${1:-plan}
approval=${2:-}
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
api_base=${INFERCRANE_RUNPOD_REST_URL:-https://rest.runpod.io/v1}
key_file=${RUNPOD_KEY_FILE:-${HOME}/.config/infercrane/runpod.key}
model=${INFERCRANE_ARTIFACT_MODEL:-}
revision=${INFERCRANE_ARTIFACT_REVISION:-}
data_center=${INFERCRANE_RUNPOD_DATA_CENTER_ID:-}
volume_size=${INFERCRANE_RUNPOD_VOLUME_GIB:-}
gpu_type=${INFERCRANE_RUNPOD_PREFETCH_GPU:-NVIDIA GeForce RTX 4090}
gpu_hourly=${INFERCRANE_RUNPOD_GPU_HOURLY_USD:-}
storage_monthly=${INFERCRANE_RUNPOD_VOLUME_USD_PER_GB_MONTH:-}
max_seconds=${INFERCRANE_RUNPOD_MAX_PAID_SECONDS:-7200}
retention_hours=${INFERCRANE_RUNPOD_VOLUME_RETENTION_HOURS:-24}
max_cost=${INFERCRANE_RUNPOD_MAX_COST_USD:-}
hf_secret=${INFERCRANE_RUNPOD_HF_TOKEN_SECRET:-}
preparer_image=${INFERCRANE_RUNPOD_PREFETCH_IMAGE:-python:3.12-slim-bookworm@sha256:0f5b26b9518d002b6173fd61daad821fa340635ebfec5bba471013f9ca114579}
hub_version=1.24.0

fail() { printf '%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }
need jq
need curl
need shasum

valid_model=$(printf '%s' "$model" | awk '
  /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/ { print "yes" }
')
[ "$valid_model" = yes ] || fail 'INFERCRANE_ARTIFACT_MODEL must be an explicit owner/repository name'
case "$revision" in
  *[!a-f0-9]*|'') fail 'INFERCRANE_ARTIFACT_REVISION must be a lowercase hexadecimal immutable commit' ;;
esac
[ "${#revision}" -ge 40 ] || fail 'INFERCRANE_ARTIFACT_REVISION must contain at least 40 hexadecimal characters'
case "$data_center" in *[!A-Za-z0-9_-]*|'') fail 'INFERCRANE_RUNPOD_DATA_CENTER_ID is required and invalid' ;; esac
case "$volume_size" in *[!0-9]*|'') fail 'INFERCRANE_RUNPOD_VOLUME_GIB must be a positive integer' ;; esac
[ "$volume_size" -ge 50 ] || fail 'INFERCRANE_RUNPOD_VOLUME_GIB must be at least 50'
case "$max_seconds" in *[!0-9]*|'') fail 'INFERCRANE_RUNPOD_MAX_PAID_SECONDS must be a positive integer' ;; esac
[ "$max_seconds" -ge 300 ] && [ "$max_seconds" -le 10800 ] || fail 'INFERCRANE_RUNPOD_MAX_PAID_SECONDS must be between 300 and 10800'
case "$retention_hours" in *[!0-9]*|'') fail 'INFERCRANE_RUNPOD_VOLUME_RETENTION_HOURS must be a positive integer' ;; esac
case "$hf_secret" in *[!A-Za-z0-9_-]*) fail 'INFERCRANE_RUNPOD_HF_TOKEN_SECRET contains unsafe characters' ;; esac

identity=${model}@${revision}
identity_digest=$(printf %s "$identity" | shasum -a 256 | awk '{print $1}')
infercrane_commit=$(git -C "$root" rev-parse HEAD 2>/dev/null || printf unknown)
observed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
volume_name=infercrane-artifact-$(printf %.20s "$identity_digest")
pod_name=infercrane-prefetch-$(printf %.20s "$identity_digest")
report_dir=${INFERCRANE_E2E_REPORT_DIR:-/tmp/infercrane-e2e-report}
mkdir -p "$report_dir"

decimal() {
  awk -v value="$1" 'BEGIN { if (value !~ /^[0-9]+([.][0-9]+)?$/ || value <= 0) exit 1; printf "%.8f", value }' || fail "$2 must be a positive decimal"
}

cost_plan() {
  [ -n "$gpu_hourly" ] || fail 'INFERCRANE_RUNPOD_GPU_HOURLY_USD is required; unknown price cannot authorize spend'
  [ -n "$storage_monthly" ] || fail 'INFERCRANE_RUNPOD_VOLUME_USD_PER_GB_MONTH is required; unknown storage price cannot authorize spend'
  normalized_gpu=$(decimal "$gpu_hourly" INFERCRANE_RUNPOD_GPU_HOURLY_USD)
  normalized_storage=$(decimal "$storage_monthly" INFERCRANE_RUNPOD_VOLUME_USD_PER_GB_MONTH)
  projected=$(awk -v gpu="$normalized_gpu" -v seconds="$max_seconds" -v storage="$normalized_storage" -v gib="$volume_size" -v hours="$retention_hours" 'BEGIN { printf "%.4f", gpu*seconds/3600 + storage*gib*hours/730 }')
  jq -n --arg identity "$identity" --arg digest "$identity_digest" --arg image "$preparer_image" --arg commit "$infercrane_commit" --arg observed_at "$observed_at" --arg dc "$data_center" --arg gpu "$gpu_type" --argjson volume_gib "$volume_size" --argjson max_seconds "$max_seconds" --argjson retention_hours "$retention_hours" --argjson gpu_hourly "$normalized_gpu" --argjson storage_monthly "$normalized_storage" --argjson projected "$projected" '{schema:"infercrane.runpod-artifact-plan/v1",model_identity:$identity,model_identity_digest:$digest,preparer_image:$image,infercrane_commit:$commit,observed_at:$observed_at,data_center_id:$dc,prefetch_gpu:$gpu,volume_gib:$volume_gib,max_paid_seconds:$max_seconds,volume_retention_hours:$retention_hours,price_evidence:{gpu_hourly_usd:$gpu_hourly,volume_usd_per_gib_month:$storage_monthly,source:"operator-supplied current provider price"},worst_case_cost_usd:$projected,mutation:"none"}'
}

case "$command_name" in
  plan)
    plan_json=$(cost_plan)
    printf '%s\n' "$plan_json" | tee "$report_dir/runpod-artifact-plan-${identity_digest}.json"
    exit 0
    ;;
  build|status|cleanup|delete-volume) ;;
  *) fail 'usage: runpod-artifact-cache-build.sh plan|build|status|cleanup|delete-volume [approval]' ;;
esac

if [ "$command_name" = build ]; then
  [ "$approval" = --approve-paid-resources ] || fail 'build requires --approve-paid-resources'
  [ -n "$max_cost" ] || fail 'INFERCRANE_RUNPOD_MAX_COST_USD is required for build'
  plan_json=$(cost_plan)
  normalized_max=$(decimal "$max_cost" INFERCRANE_RUNPOD_MAX_COST_USD)
  projected=$(printf '%s' "$plan_json" | jq -r .worst_case_cost_usd)
  awk -v projected="$projected" -v maximum="$normalized_max" 'BEGIN { exit !(projected <= maximum) }' || fail "worst-case cost USD $projected exceeds hard budget USD $normalized_max"
fi

if [ "$command_name" = delete-volume ]; then
  [ "$approval" = --approve-volume-deletion ] || fail 'delete-volume requires --approve-volume-deletion'
fi

[ -r "$key_file" ] || fail "RunPod key file is not readable: $key_file"
api_key=$(tr -d '\r\n' < "$key_file")
[ -n "$api_key" ] || fail 'RunPod key file is empty'
api() {
  method=$1
  endpoint_path=$2
  body=${3:-}
  response_file=$(mktemp "${TMPDIR:-/tmp}/infercrane-runpod-response.XXXXXX")
  if [ -n "$body" ]; then
    status=$(curl -sS --max-time 30 -o "$response_file" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' --data "$body" "$api_base$endpoint_path") || {
      rm -f "$response_file"
      fail "RunPod API transport failed for $method $endpoint_path"
    }
  else
    status=$(curl -sS --max-time 30 -o "$response_file" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $api_key" "$api_base$endpoint_path") || {
      rm -f "$response_file"
      fail "RunPod API transport failed for $method $endpoint_path"
    }
  fi
  case "$status" in
    2??) cat "$response_file"; rm -f "$response_file" ;;
    *)
      diagnostic=$(tr '\r\n' '  ' < "$response_file" | cut -c 1-500)
      rm -f "$response_file"
      case "$diagnostic" in *"$api_key"*) diagnostic='provider diagnostic contained credential and was redacted' ;; esac
      fail "RunPod API returned HTTP $status for $method $endpoint_path: $diagnostic"
      ;;
  esac
}

volumes=$(api GET /networkvolumes)
volume_id=$(printf '%s' "$volumes" | jq -r --arg name "$volume_name" '[.[] | select(.name==$name)] | if length==1 then .[0].id else empty end')
pods=$(api GET /pods)
pod_id=$(printf '%s' "$pods" | jq -r --arg name "$pod_name" '[.[] | select(.name==$name and ((.desiredStatus // "")|ascii_upcase)!="TERMINATED")] | if length==1 then .[0].id else empty end')

if [ "$command_name" = status ]; then
  jq -n --arg identity "$identity" --arg volume_id "$volume_id" --arg pod_id "$pod_id" --arg volume_name "$volume_name" --arg pod_name "$pod_name" '{model_identity:$identity,volume:{id:$volume_id,name:$volume_name,present:($volume_id!="")},preparer:{id:$pod_id,name:$pod_name,present:($pod_id!="")}}'
  exit 0
fi

if [ "$command_name" = cleanup ]; then
  [ -z "$pod_id" ] || api DELETE "/pods/$pod_id" >/dev/null
  jq -n --arg pod_id "$pod_id" --arg volume_id "$volume_id" '{preparer_deleted:($pod_id!=""),preserved_volume_id:$volume_id,recoverable:true}'
  exit 0
fi

if [ "$command_name" = delete-volume ]; then
  [ -z "$pod_id" ] || fail 'refusing to delete a network volume while its preparer Pod exists'
  [ -z "$volume_id" ] || api DELETE "/networkvolumes/$volume_id" >/dev/null
  jq -n --arg volume_id "$volume_id" '{volume_deleted:($volume_id!=""),recoverable:false}'
  exit 0
fi

printf '%s\n' "$plan_json" > "$report_dir/runpod-artifact-plan-${identity_digest}.json"

if [ -z "$volume_id" ]; then
  create_volume=$(jq -n --arg name "$volume_name" --arg dc "$data_center" --argjson size "$volume_size" '{name:$name,dataCenterId:$dc,size:$size}')
  created=$(api POST /networkvolumes "$create_volume")
  volume_id=$(printf '%s' "$created" | jq -r '.id // empty')
  [ -n "$volume_id" ] || fail 'RunPod network volume create returned no ID'
fi

cleanup_pod() {
  if [ -n "${pod_id:-}" ]; then
    api DELETE "/pods/$pod_id" >/dev/null 2>&1 || true
  fi
}
trap cleanup_pod EXIT INT TERM

if [ -z "$pod_id" ]; then
  token_env='{}'
  if [ -n "$hf_secret" ]; then
    token_env=$(jq -n --arg value "{{ RUNPOD_SECRET_${hf_secret} }}" '{HF_TOKEN:$value}')
  fi
  env_json=$(jq -n --arg model "$model" --arg revision "$revision" --arg digest "$identity_digest" --argjson token "$token_env" '$token + {INFERCRANE_MODEL:$model,INFERCRANE_MODEL_REVISION:$revision,INFERCRANE_MODEL_IDENTITY_DIGEST:$digest,HF_HOME:"/workspace/huggingface",HUGGINGFACE_HUB_CACHE:"/workspace/huggingface/hub",HF_XET_HIGH_PERFORMANCE:"1"}')
  prepare='python -m pip install --no-cache-dir "huggingface_hub[hf_xet]=='"$hub_version"'"; mkdir -p /workspace/infercrane/model; python -c '\''import json,os,time; from huggingface_hub import snapshot_download; started=time.time(); path=snapshot_download(repo_id=os.environ["INFERCRANE_MODEL"],revision=os.environ["INFERCRANE_MODEL_REVISION"],local_dir="/workspace/infercrane/model"); manifest={"schema":"infercrane.artifact-cache/v1","model":os.environ["INFERCRANE_MODEL"],"revision":os.environ["INFERCRANE_MODEL_REVISION"],"model_identity_digest":os.environ["INFERCRANE_MODEL_IDENTITY_DIGEST"],"path":path,"download_seconds":round(time.time()-started,3),"completed_at":time.strftime("%Y-%m-%dT%H:%M:%SZ",time.gmtime())}; open("/workspace/infercrane/manifest.json","w").write(json.dumps(manifest,sort_keys=True))'\''; cd /workspace/infercrane; exec python -m http.server 8000'
  pod_body=$(jq -n --arg name "$pod_name" --arg image "$preparer_image" --arg gpu "$gpu_type" --arg dc "$data_center" --arg volume "$volume_id" --arg command "$prepare" --argjson env "$env_json" '{name:$name,imageName:$image,gpuTypeIds:[$gpu],gpuTypePriority:"custom",gpuCount:1,cloudType:"SECURE",computeType:"GPU",interruptible:false,containerDiskInGb:20,networkVolumeId:$volume,volumeMountPath:"/workspace",dataCenterIds:[$dc],dataCenterPriority:"custom",dockerEntrypoint:["/bin/sh"],dockerStartCmd:["-ec",$command],ports:["8000/http"],env:$env}')
  created=$(api POST /pods "$pod_body")
  pod_id=$(printf '%s' "$created" | jq -r '.id // empty')
  [ -n "$pod_id" ] || fail 'RunPod preparer Pod create returned no ID'
fi

started_epoch=$(date +%s)
deadline=$((started_epoch + max_seconds))
manifest=''
while [ "$(date +%s)" -lt "$deadline" ]; do
  if manifest=$(curl -fsS --max-time 15 "https://${pod_id}-8000.proxy.runpod.net/manifest.json" 2>/dev/null); then
    if printf '%s' "$manifest" | jq -e --arg model "$model" --arg revision "$revision" --arg digest "$identity_digest" '.model==$model and .revision==$revision and .model_identity_digest==$digest' >/dev/null; then
      break
    fi
    manifest=''
  fi
  sleep 15
done
[ -n "$manifest" ] || fail "RunPod artifact preparation exceeded ${max_seconds}s watchdog"
finished_epoch=$(date +%s)
api DELETE "/pods/$pod_id" >/dev/null
pod_id=''
trap - EXIT INT TERM
evidence=$(printf '%s' "$manifest" | jq --arg volume_id "$volume_id" --arg volume_name "$volume_name" --arg data_center "$data_center" --arg image "$preparer_image" --arg commit "$infercrane_commit" --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson wall_seconds "$((finished_epoch-started_epoch))" '. + {volume_id:$volume_id,volume_name:$volume_name,data_center_id:$data_center,preparer_image:$image,infercrane_commit:$commit,qualification_completed_at:$completed_at,watch_wall_seconds:$wall_seconds,state:"present"}')
printf '%s\n' "$evidence" | tee "$report_dir/runpod-artifact-evidence-${identity_digest}.json"
printf '%s\n' "INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON=$(jq -cn --arg identity "$identity" --arg volume "$volume_id" '{($identity):$volume}')"
