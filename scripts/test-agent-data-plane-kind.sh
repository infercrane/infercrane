#!/usr/bin/env bash
# Proves the InferCrane agent as the data plane inside a customer's cluster.
#
# One kind cluster, two namespaces:
#   hosted  the InferCrane control plane, its PostgreSQL, and capture-proxy,
#           which terminates the agent's TLS and keeps a copy of every byte
#           the agent sends to the control plane.
#   models  the customer's model server and the agent, installed with the
#           Helm chart in deploy/helm/infercrane-agent.
#
# The Go test then makes the endpoint agent-managed, sends requests to the
# agent inside the cluster, runs one candidate through the agent's command
# channel, and asserts that no prompt or response bytes reached the hosted
# namespace (on the wire, in PostgreSQL, or in the control plane's logs).
#
# The model servers are the development fake engine (no GPU, no weights): the
# run proves the data path and the command channel, not engine performance.
# Nothing here spends money. Set INFERCRANE_KIND_KEEP=1 to keep the cluster.
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixtures="$repo_dir/internal/testtools/agentintegration/kind"
kind_version=v0.32.0
helm_version=v3.16.4
node_image=${INFERCRANE_KIND_NODE_IMAGE:-'kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5'}
registry_image='registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373'
postgres_image='postgres:17-alpine@sha256:e5d2edd28b1f8d40a0a6c0c9cdc545b19b56a0d5d88f94bc11a97d970d4700d0'
tools="$repo_dir/.infercrane/tools"

run_id=${INFERCRANE_KIND_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}
cluster="infercrane-dataplane-$run_id"
context="kind-$cluster"
registry="infercrane-dataplane-registry-$run_id"
registry_port=${INFERCRANE_KIND_REGISTRY_PORT:-5055}
registry_alias="registry.dataplane.local"
image=${INFERCRANE_DEV_IMAGE:-}
work=$(mktemp -d)
forwards=()

kind_bin=${KIND_BIN:-}
if [[ -z "$kind_bin" ]]; then
  if command -v kind >/dev/null 2>&1; then
    kind_bin=$(command -v kind)
  else
    kind_bin="$tools/kind-$kind_version/kind"
    if [[ ! -x "$kind_bin" ]]; then
      mkdir -p "$tools/kind-$kind_version"
      GOBIN="$tools/kind-$kind_version" go install "sigs.k8s.io/kind@$kind_version"
    fi
  fi
fi
helm_bin=${HELM_BIN:-}
if [[ -z "$helm_bin" ]]; then
  if command -v helm >/dev/null 2>&1; then
    helm_bin=$(command -v helm)
  else
    helm_bin="$tools/helm-$helm_version/helm"
    if [[ ! -x "$helm_bin" ]]; then
      mkdir -p "$tools/helm-$helm_version"
      GOBIN="$tools/helm-$helm_version" go install "helm.sh/helm/v3/cmd/helm@$helm_version"
    fi
  fi
fi

cleanup() {
  for pid in "${forwards[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" >/dev/null 2>&1 || true
  done
  if [[ "${INFERCRANE_KIND_KEEP:-}" != "1" ]]; then
    "$kind_bin" delete cluster --name "$cluster" >/dev/null 2>&1 || true
    docker rm -f "$registry" >/dev/null 2>&1 || true
  else
    echo "kept cluster $cluster and registry $registry"
  fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
kc() { kubectl --context "$context" "$@"; }
render() { sed -e "s|@@IMAGE@@|$image|g" -e "s|@@ENGINE_IMAGE@@|$engine_image|g" -e "s|@@POSTGRES_IMAGE@@|$postgres_image|g" -e "s|@@API_KEY@@|$api_key|g" "$1"; }
forward() {
  local namespace=$1 target=$2 port=$3 log=$4
  kc --namespace "$namespace" port-forward "$target" "$port" >"$work/$log.log" 2>&1 &
  forwards+=($!)
}
wait_http() {
  local url=$1 deadline=$((SECONDS + 120))
  until curl -sk -o /dev/null "$url"; do
    if ((SECONDS > deadline)); then
      echo "$url did not answer" >&2
      return 1
    fi
    sleep 1
  done
}

if [[ -z "$image" ]]; then
  image="infercrane-dataplane/dev:$run_id"
  step "building the development image (control plane, agent, capture-proxy, fake engine)"
  docker build --target development -t "$image" "$repo_dir"
fi

step "starting a local registry for the digest-pinned candidate image"
docker run -d --name "$registry" -p "127.0.0.1:$registry_port:5000" "$registry_image" >/dev/null
docker build -q -t "localhost:$registry_port/infercrane-dataplane/fake-engine:$run_id" - >/dev/null <<EOF
FROM $image AS source
FROM scratch
COPY --from=source /usr/local/bin/fake-vllm /fake-vllm
USER 65532:65532
ENTRYPOINT ["/fake-vllm"]
EOF
docker push -q "localhost:$registry_port/infercrane-dataplane/fake-engine:$run_id" >/dev/null
digest=$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "localhost:$registry_port/infercrane-dataplane/fake-engine:$run_id" | grep "localhost:$registry_port/" | head -1 | cut -d@ -f2)
engine_image="$registry_alias/infercrane-dataplane/fake-engine@$digest"

step "creating kind cluster $cluster"
# kind makes its new cluster the current kubectl context; other work on this
# machine may rely on the current one, so put it back. This script always
# names its own context.
previous_context=$(kubectl config current-context 2>/dev/null || true)
"$kind_bin" create cluster --name "$cluster" --image "$node_image" --config "$fixtures/kind-config.yaml" --wait 180s
if [[ -n "$previous_context" ]]; then
  kubectl config use-context "$previous_context" >/dev/null
fi
node="$cluster-control-plane"
docker network connect kind "$registry" >/dev/null 2>&1 || true
docker exec "$node" mkdir -p "/etc/containerd/certs.d/$registry_alias"
docker exec -i "$node" sh -c "cat > /etc/containerd/certs.d/$registry_alias/hosts.toml" <<EOF
server = "http://$registry:5000"
[host."http://$registry:5000"]
  capabilities = ["pull", "resolve"]
EOF
"$kind_bin" load docker-image "$image" --name "$cluster"
"$repo_dir/internal/testtools/poc/simulated-accelerator.sh" "$context" "$node" infercrane.dev/simulated-accelerator nvidia.com/gpu.product SIMULATED-CPU 4

step "starting the hosted control plane behind capture-proxy"
api_key="ic_kind_$(openssl rand -hex 24)"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout "$work/tls.key" -out "$work/tls.crt" \
  -subj "/CN=capture.hosted.svc" -addext "subjectAltName=DNS:capture.hosted.svc,DNS:capture.hosted.svc.cluster.local" >/dev/null 2>&1
render "$fixtures/hosted.yaml" | kc apply -f - >/dev/null
kc --namespace hosted create secret tls capture-tls --cert "$work/tls.crt" --key "$work/tls.key" >/dev/null
for deployment in postgres infercrane capture; do
  kc --namespace hosted rollout status "deployment/$deployment" --timeout=300s
done
forward hosted service/infercrane 18088:8080 control
forward hosted service/capture 19090:9090 capture
wait_http http://127.0.0.1:18088/readyz
wait_http http://127.0.0.1:19090/stats

step "creating the agent key and the customer's model server"
created=$(curl -fsS -X POST http://127.0.0.1:18088/api/v1/agents -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d '{"name":"kind-cluster"}')
agent_key=$(printf '%s' "$created" | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])')
agent_id=$(printf '%s' "$created" | python3 -c 'import json,sys; print(json.load(sys.stdin)["agent"]["id"])')
render "$fixtures/models.yaml" | kc apply -f - >/dev/null
kc --namespace models create secret generic infercrane-agent-key --from-literal=key="$agent_key" >/dev/null
kc --namespace models rollout status deployment/qwen3 --timeout=180s

step "installing the agent with the Helm chart"
"$helm_bin" --kube-context "$context" install agent "$repo_dir/deploy/helm/infercrane-agent" --namespace models \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set controlPlane.url=https://capture.hosted.svc:8443 --set-file controlPlane.caBundle="$work/tls.crt" \
  --set 'agent.servers[0]=http://qwen3.models.svc:8000' --set agent.endpointName=support-bot --set agent.model=Qwen/Qwen3-0.6B \
  --set executor.enabled=true --set executor.gpuResource=infercrane.dev/simulated-accelerator \
  --set "executor.allowedImages[0]=$registry_alias/infercrane-dataplane/" --set 'executor.allowedAccelerators[0]=SIMULATED-CPU' \
  --wait --timeout 5m
forward models service/agent-infercrane-agent 18089:8080 dataplane
wait_http http://127.0.0.1:18089/livez

step "proving it"
cd "$repo_dir"
INFERCRANE_AGENT_KIND_CONTEXT="$context" \
INFERCRANE_AGENT_KIND_CONTROL_URL=http://127.0.0.1:18088 \
INFERCRANE_AGENT_KIND_API_KEY="$api_key" \
INFERCRANE_AGENT_KIND_AGENT_ID="$agent_id" \
INFERCRANE_AGENT_KIND_DATA_PLANE_URL=http://127.0.0.1:18089 \
INFERCRANE_AGENT_KIND_CAPTURE_URL=http://127.0.0.1:19090 \
INFERCRANE_AGENT_KIND_ENGINE_IMAGE="$engine_image" \
  go test -count=1 -timeout 30m -v -run '^TestAgentDataPlaneOnKind$' ./internal/testtools/agentintegration
