#!/usr/bin/env bash
# Phase 5. Collects the evidence while the cluster still exists. Reads only;
# creates nothing. Run it after 40-run.sh, whether that passed or failed, and
# before 90-delete.sh.
#
# Output: .infercrane/eks-agent-run/evidence/ (raw files) and
# evidence/summary.json, which becomes docs/testing/evidence/ after review.
# The account number is replaced in every file. No prompt, answer, key or
# engine log is collected.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUT="$STATE_DIR/evidence"

collect_phase() {
  need_tools aws kubectl helm python3 git
  need_cluster
  guard_phase collect 10
  trap stop_port_forwards EXIT
  mkdir -p "$OUT"

  step "Cluster"
  aws eks describe-cluster --name "$CLUSTER" \
    --query 'cluster.{name:name,version:version,platformVersion:platformVersion,status:status,createdAt:createdAt,tags:tags}' --output json >"$OUT/cluster.json"
  local group
  # After a failed create a node group may not exist; the rest is still
  # worth having, and so is the reason CloudFormation recorded.
  for group in "$GPU_NODEGROUP" "$SYSTEM_NODEGROUP"; do
    if ! aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$group" \
      --query 'nodegroup.{name:nodegroupName,status:status,instanceTypes:instanceTypes,amiType:amiType,releaseVersion:releaseVersion,capacityType:capacityType,scaling:scalingConfig,diskSize:diskSize,subnets:subnets,health:health}' --output json >"$OUT/nodegroup-$group.json" 2>/dev/null; then
      rm -f "$OUT/nodegroup-$group.json"
      say "  node group $group does not exist"
    fi
  done
  create_failures >"$OUT/create-failures.txt"
  launch_refusals >"$OUT/launch-refusals.txt"
  if [ -s "$OUT/create-failures.txt" ] || [ -s "$OUT/launch-refusals.txt" ]; then
    say "  what AWS recorded about node groups that did not start ($(grep -c . "$OUT/launch-refusals.txt" || true) refused requests):"
    sed -n '1,4p' "$OUT/create-failures.txt" "$OUT/launch-refusals.txt" | cut -c1-400 | sed 's/^/    /'
  fi
  kc version -o json >"$OUT/kubernetes-version.json"
  kc get nodes -o json | python3 -c 'import json, sys
nodes = []
for item in json.load(sys.stdin)["items"]:
    labels = item["metadata"]["labels"]
    info = item["status"]["nodeInfo"]
    nodes.append({
        "pool": labels.get("infercrane.dev/pool"),
        "instance_type": labels.get("node.kubernetes.io/instance-type"),
        "zone": labels.get("topology.kubernetes.io/zone"),
        "architecture": info.get("architecture"),
        "kernel": info.get("kernelVersion"),
        "os_image": info.get("osImage"),
        "kubelet": info.get("kubeletVersion"),
        "container_runtime": info.get("containerRuntimeVersion"),
        "taints": item["spec"].get("taints") or [],
        "nvidia_labels": {k: v for k, v in sorted(labels.items()) if k.startswith("nvidia.com/")},
        "allocatable": {k: v for k, v in item["status"]["allocatable"].items() if k in ("cpu", "memory", "ephemeral-storage", "nvidia.com/gpu")},
    })
json.dump(nodes, sys.stdout, indent=2)' >"$OUT/nodes.json"
  helm list --all-namespaces -o json >"$OUT/helm-releases.json"
  kc get pods --all-namespaces -o json | python3 -c 'import json, sys
pods = []
for item in json.load(sys.stdin)["items"]:
    for status in item["status"].get("containerStatuses") or []:
        pods.append({"namespace": item["metadata"]["namespace"], "pod": item["metadata"]["name"], "node": item["spec"].get("nodeName"),
                     "container": status["name"], "image": status["image"], "image_id": status["imageID"], "restarts": status["restartCount"]})
json.dump(pods, sys.stdout, indent=2)' >"$OUT/pods.json"
  kc --namespace "$NAMESPACE_MODELS" get role,rolebinding,serviceaccount,deployment,service -o wide >"$OUT/models-namespace.txt"
  kc --namespace "$NAMESPACE_MODELS" get events --sort-by=.lastTimestamp >"$OUT/models-events.txt" || true
  say "  cluster, node groups, nodes, Helm releases, pods and the customer namespace recorded"

  step "GPU"
  kc --namespace "$NAMESPACE_MODELS" exec deployment/qwen3 -- nvidia-smi \
    --query-gpu=name,driver_version,memory.total,memory.used --format=csv >"$OUT/nvidia-smi.csv" 2>/dev/null || say "  nvidia-smi could not be read"
  cat "$OUT/nvidia-smi.csv" 2>/dev/null | sed 's/^/  /' || true

  step "Control plane records"
  local agent_id run_id
  agent_id=$(state_get AGENT_ID)
  run_id=$(state_get CANDIDATE_RUN_ID)
  # In a subshell: after a failed install there may be no control plane to ask,
  # and the rest of the evidence is still worth having.
  (
    open_control
    api GET "/api/v1/agents/$agent_id" "$OUT/agent.json" >/dev/null || true
    api GET "/api/v1/endpoints/$ENDPOINT_NAME" "$OUT/endpoint.json" >/dev/null || true
    if [ -n "$run_id" ]; then
      api GET "/api/v1/agents/$agent_id/candidate-runs/$run_id" "$OUT/candidate-run.json" >/dev/null || true
    fi
    stop_port_forwards
  ) || say "  the control plane could not be asked; its records are missing from the evidence"
  # How many content-free request records the data plane left, by status.
  kc --namespace "$NAMESPACE_HOSTED" exec deployment/postgres -- psql -U infercrane -d infercrane -tAc \
    "SELECT compute_mode || ' ' || status_code || ' ' || count(*) FROM request_records GROUP BY 1, status_code ORDER BY 1" >"$OUT/request-records.txt" 2>/dev/null || true
  cp "$STATE_DIR/run/checks.tsv" "$OUT/checks.tsv" 2>/dev/null || true
  cp "$STATE_DIR/run/candidate-spec.json" "$OUT/candidate-spec.json" 2>/dev/null || true
  cp "$STATE_DIR/run/gpu-both-servers.csv" "$OUT/gpu-both-servers.csv" 2>/dev/null || true
  cp "$STATE_DIR/created-resources.txt" "$OUT/created-resources.txt" 2>/dev/null || true
  say "  agent, endpoint, candidate run and request-record counts recorded"

  step "Diagnostics (kept on this machine only, never part of the evidence)"
  # What explains a failure after the cluster is gone: pod descriptions,
  # events, and the last lines of each component's log. The model servers log
  # no request text unless asked to, and nothing here is committed.
  local diagnostics="$STATE_DIR/diagnostics"
  mkdir -p "$diagnostics"
  kc get pods --all-namespaces -o wide >"$diagnostics/pods.txt" 2>&1 || true
  kc get events --all-namespaces --sort-by=.lastTimestamp >"$diagnostics/events.txt" 2>&1 || true
  kc describe nodes >"$diagnostics/nodes.txt" 2>&1 || true
  kc --namespace "$NAMESPACE_MODELS" describe pods >"$diagnostics/models-pods.txt" 2>&1 || true
  kc --namespace "$NAMESPACE_HOSTED" describe pods >"$diagnostics/hosted-pods.txt" 2>&1 || true
  kc --namespace nvidia-device-plugin get pods -o wide >"$diagnostics/nvidia-pods.txt" 2>&1 || true
  kc --namespace "$NAMESPACE_MODELS" logs -l app.kubernetes.io/name=infercrane-agent --tail=400 >"$diagnostics/agent.log" 2>&1 || true
  kc --namespace "$NAMESPACE_MODELS" logs -l app=qwen3 --tail=300 >"$diagnostics/model-server.log" 2>&1 || true
  kc --namespace "$NAMESPACE_MODELS" logs -l app.kubernetes.io/managed-by=infercrane --tail=300 >"$diagnostics/candidate.log" 2>&1 || true
  kc --namespace "$NAMESPACE_HOSTED" logs deployment/infercrane --tail=400 >"$diagnostics/control-plane.log" 2>&1 || true
  for file in "$diagnostics"/*; do
    ACCOUNT="$ACCOUNT" perl -pi -e 's/\Q$ENV{ACCOUNT}\E/<ACCOUNT>/g' "$file"
  done
  say "  $diagnostics"

  step "Summary"
  local created now
  created=$(state_get CREATED_AT)
  now=$(date +%s)
  CREATED="$created" NOW="$now" RATE_LIST="$(rate_list)" RATE_GUARD="$(rate_guard)" COMMIT="$(git -C "$REPO_DIR" rev-parse HEAD)" \
    IMAGE_COMMIT="$(state_get IMAGE_COMMIT)" IMAGE_DIGEST="$(state_get IMAGE_DIGEST)" GPU_PRODUCT="$(state_get GPU_PRODUCT)" \
    RUN_STARTED="$(state_get RUN_STARTED_AT)" RUN_FINISHED="$(state_get RUN_FINISHED_AT)" \
    MODEL="$MODEL" MODEL_REVISION="$MODEL_REVISION" VLLM_IMAGE="$VLLM_IMAGE" REGION="$REGION" \
    GPU_INSTANCES="$GPU_INSTANCES" GPU_PRICES="$GPU_PRICES" PRICE_GPU_MAX="$PRICE_GPU_INSTANCE" SYSTEM_INSTANCE="$SYSTEM_INSTANCE" NVIDIA_CHART_VERSION="$NVIDIA_CHART_VERSION" \
    python3 - "$OUT" <<'PY'
import datetime, json, os, sys
out = sys.argv[1]
def load(name, default=None):
    try:
        return json.load(open(os.path.join(out, name)))
    except Exception:
        return default
def text(name):
    try:
        return open(os.path.join(out, name)).read().strip().splitlines()
    except Exception:
        return []
def iso(value):
    return datetime.datetime.fromtimestamp(int(value), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ") if value else None
env = os.environ
created, now = int(env["CREATED"] or 0), int(env["NOW"])
hours = (now - created) / 3600 if created else None
run = (load("candidate-run.json") or {}).get("candidate_run") or {}
# The GPU type AWS started, and the hourly list price with that type.
gpu_ran = next((n.get("instance_type") for n in (load("nodes.json") or []) if n.get("pool") == "gpu"), None)
gpu_prices = dict(pair.split("=") for pair in env["GPU_PRICES"].split())
rate_ran = round(float(env["RATE_LIST"]) - float(env["PRICE_GPU_MAX"]) + float(gpu_prices[gpu_ran]), 4) if gpu_ran in gpu_prices else None
summary = {
    "schema": "infercrane.eks-agent-proof/v1",
    "what": "The InferCrane agent on a real Kubernetes cluster in AWS (EKS) with one GPU node. A path proof, not a speed result.",
    "collected_at": iso(now),
    "source_commit": env["COMMIT"],
    "infercrane_image": {"built_from_commit": env["IMAGE_COMMIT"], "digest": env["IMAGE_DIGEST"]},
    "aws": {"region": env["REGION"], "gpu_instances_allowed_in_order": env["GPU_INSTANCES"].split(), "system_instance": env["SYSTEM_INSTANCE"],
            "note": "nodes[].instance_type and nodes[].zone say what AWS actually started"},
    "cluster": load("cluster.json"),
    "node_groups": [load("nodegroup-gpu.json"), load("nodegroup-system.json")],
    "node_groups_not_created": text("create-failures.txt"),
    "node_requests_refused": text("launch-refusals.txt"),
    "nodes": load("nodes.json"),
    "gpu": {"product_label": env["GPU_PRODUCT"], "nvidia_smi": text("nvidia-smi.csv"), "while_both_servers_loaded": text("gpu-both-servers.csv"),
            "device_plugin_chart": env["NVIDIA_CHART_VERSION"]},
    "model": {"id": env["MODEL"], "revision": env["MODEL_REVISION"], "engine_image": env["VLLM_IMAGE"]},
    "checks": [dict(zip(("name", "outcome", "detail"), line.split("\t"))) for line in text("checks.tsv")],
    "candidate_run": {k: run.get(k) for k in ("id", "state", "phase", "attempts", "failure_code", "failure_detail", "created_at", "started_at", "finished_at", "spec", "control", "result")},
    "request_records": text("request-records.txt"),
    "timing": {"cluster_created_at": iso(created), "run_started_at": iso(env["RUN_STARTED"]), "run_finished_at": iso(env["RUN_FINISHED"]),
               "cluster_hours_at_collection": round(hours, 3) if hours is not None else None},
    "cost": {"gpu_instance_that_ran": gpu_ran, "list_price_usd_per_hour": rate_ran,
             "list_price_usd_per_hour_with_dearest_gpu_allowed": float(env["RATE_LIST"]), "counted_usd_per_hour": float(env["RATE_GUARD"]),
             "list_price_usd_at_collection": round(hours * rate_ran, 2) if hours is not None and rate_ran is not None else None,
             "counted_usd_at_collection": round(hours * float(env["RATE_GUARD"]), 2) if hours is not None else None,
             "note": "List prices before tax for the whole cluster age, not a bill. Without a GPU node no list price is given. The billed amount comes from AWS Cost Explorer a day later."},
}
json.dump(summary, open(os.path.join(out, "summary.json"), "w"), indent=2, default=str)
print("  checks: %d passed, %d failed" % (sum(c.get("outcome") == "pass" for c in summary["checks"]), sum(c.get("outcome") == "fail" for c in summary["checks"])))
print("  cluster age: %s hours; GPU node %s; $%s at list price, $%s as the time limit counts" % (
    summary["timing"]["cluster_hours_at_collection"], gpu_ran or "none", summary["cost"]["list_price_usd_at_collection"], summary["cost"]["counted_usd_at_collection"]))
PY

  # No file may hold the account number.
  local file
  for file in "$OUT"/*; do
    ACCOUNT="$ACCOUNT" perl -pi -e 's/\Q$ENV{ACCOUNT}\E/<ACCOUNT>/g' "$file"
  done
  if grep -rl "$ACCOUNT" "$OUT" >/dev/null 2>&1; then
    die "an evidence file still holds the account number"
  fi
  stop_port_forwards
  echo
  say "Evidence is in $OUT (summary.json first)."
  say "Next: tools/eks-agent-run/90-delete.sh --yes ($(minutes_left) minutes left before the cluster must be gone)"
}

run_masked collect_phase
