# Settings for the EKS qualification of the customer install (first run: the
# agent proof of 23-24 Nov; second run: the real-cluster qualification of
# 26 Nov, rules-2026-11-26.md). Sourced by lib.sh; no secrets, no account
# number. Every price is an on-demand Linux list price of the region, read
# from the AWS Price List API, in USD before tax.
# shellcheck shell=bash

PROJECT_TAG=infercrane-eks-proof
CLUSTER=infercrane-eks-proof
# The region: eu-central-1 by default; EKS_PROOF_REGION=us-east-1 once that
# region's G quota is granted (requested 26 Nov 13:43). Each region below
# names its zones and its list prices; nothing else changes.
REGION=${EKS_PROOF_REGION:-eu-central-1}
ECR_REPOSITORY=infercrane-eks-proof

GPU_NODEGROUP=gpu
# The GPU: EKS_PROOF_GPU_INSTANCE picks it. The qualified tuple names the
# NVIDIA L4 (g6.xlarge, 24 GB, 4 vCPUs, Ada). g5.xlarge (NVIDIA A10G, 24 GB,
# 4 vCPUs, Ampere) is the fallback added on 26 Nov after both regions refused
# the L4 all day; a run on it qualifies the A10G, not the L4. The account's
# quota for G instances is 4 vCPUs in eu-central-1 and us-east-1, so one node.
# 20-create.sh reserves the instance (an on-demand capacity reservation)
# before it creates anything else, so AWS answers "no capacity" at once and
# for free, instead of after a node group's 33 minutes (23 and 24 Nov).
# GPU_PRODUCT_NAME is what NVIDIA's GPU feature discovery puts in the node's
# nvidia.com/gpu.product label; time-slicing adds "-SHARED".
GPU_INSTANCES=${EKS_PROOF_GPU_INSTANCE:-g6.xlarge}
case "$GPU_INSTANCES" in
  g6.xlarge) GPU_PRODUCT_NAME=NVIDIA-L4 ;;
  g5.xlarge) GPU_PRODUCT_NAME=NVIDIA-A10G ;;
  *)
    printf 'error: EKS_PROOF_GPU_INSTANCE is %s; env.sh knows g6.xlarge (L4) and g5.xlarge (A10G)\n' "$GPU_INSTANCES" >&2
    exit 1
    ;;
esac
# 100 GB: the vLLM 0.34.0 image (about 30 GB on disk once unpacked), Qwen3-8B
# (16.4 GB) and the small model's servers, each downloading its own weights,
# and the kubelet evicts pods below 15% free.
GPU_DISK_GB=100
SYSTEM_NODEGROUP=system
# The stand-in control plane runs the campaign's AIPerf load and the agent
# runs its own, so the system node has 4 vCPUs and 16 GiB.
SYSTEM_INSTANCE=m7g.xlarge
SYSTEM_DISK_GB=30

# Per region: the zones the cluster and its nodes may use, and the on-demand
# Linux list prices (AWS Price List API, read 26 Nov 2026). us-east-1e
# (use1-az3) is left out: EKS does not place a control plane there.
case "$REGION" in
  eu-central-1)
    NODE_ZONES="eu-central-1a eu-central-1b eu-central-1c"
    GPU_PRICE_LIST="g6.xlarge=1.0064 g5.xlarge=1.258"
    PRICE_SYSTEM_INSTANCE=0.1955
    PRICE_GP3_GB_MONTH=0.0952
    ;;
  us-east-1)
    NODE_ZONES="us-east-1a us-east-1b us-east-1c us-east-1d us-east-1f"
    GPU_PRICE_LIST="g6.xlarge=0.8048 g5.xlarge=1.006"
    PRICE_SYSTEM_INSTANCE=0.1632
    PRICE_GP3_GB_MONTH=0.08
    ;;
  *)
    printf 'error: env.sh has no zones or prices for region %s\n' "$REGION" >&2
    exit 1
    ;;
esac
PRICE_GPU_INSTANCE=""
for pair in $GPU_PRICE_LIST; do
  [ "${pair%%=*}" = "$GPU_INSTANCES" ] && PRICE_GPU_INSTANCE=${pair#*=}
done
unset pair
[ -n "$PRICE_GPU_INSTANCE" ] || {
  printf 'error: env.sh has no %s price for %s\n' "$REGION" "$GPU_INSTANCES" >&2
  exit 1
}
GPU_PRICES="$GPU_INSTANCES=$PRICE_GPU_INSTANCE"

# PRICE_GPU_INSTANCE above is the dearest GPU type allowed: the time limit
# and the budget count every hour with it. 00-preflight.sh checks it against
# GPU_PRICES. The capacity reservation is billed at the same rate, from the
# second it is made, and is counted from the start of 20-create.sh.
PRICE_EKS_CONTROL_PLANE=0.10
PRICE_PUBLIC_IPV4=0.005          # one per node
PRICE_ECR_GB_MONTH=0.10
ECR_IMAGE_GB=3                   # generous: the InferCrane image is smaller
HOURS_PER_MONTH=730

# The whole test must fit this at list prices, the control plane included.
# 26 Nov: the coordinator's hard cap for this qualification. The first run's
# $6.00 (23-24 Nov) was counted in its own worktree and its own $15.
BUDGET_USD=10
# The guard counts every hour at the list rate plus this allowance for what the
# list above does not name (data transfer out, API requests).
GUARD_ALLOWANCE=1.03
# One cluster may live this long from the moment 20-create.sh starts.
# MAX_LIFETIME_LIMIT is the most the rules allow. EKS_PROOF_MAX_LIFETIME_HOURS
# can only shorten it, for example so the deadline falls before the AWS
# sign-in ends: without a valid sign-in nothing can be deleted from here.
MAX_LIFETIME_LIMIT=5
MAX_LIFETIME_HOURS=${EKS_PROOF_MAX_LIFETIME_HOURS:-$MAX_LIFETIME_LIMIT}
# Time kept free at the end of that lifetime to delete everything. 90-delete.sh
# took 10 to 20 minutes on 23-24 Nov; its `eksctl delete cluster` may wait 30
# minutes, and the reservation, ECR and the clean check need a few more.
DELETE_MINUTES=35
# What a run that goes to plan takes, the cluster's creation included and
# the deletion not (00-preflight.sh prints it).
FULL_RUN_HOURS=4
# EKS_PROOF_AUTH_UNTIL ("YYYY-MM-DD HH:MM", local time) is when the AWS
# sign-in ends: `aws login` lasts 12 hours from the browser's console session.
# After it nothing can be deleted from here, so 20-create.sh ends the
# cluster's lifetime there when that comes first, and refuses to start when
# the time left is shorter than FULL_RUN_HOURS plus DELETE_MINUTES.
# A new cluster is refused when less than this is affordable.
MIN_USEFUL_HOURS=1.5

# While the cluster is created, 20-create.sh looks at both node groups this
# often. It stops waiting (exit code 7) when a group has no instance, has asked
# for one at least this many times over this many seconds, and AWS refused
# every request. On 2026-11-23 zone a refused 16 and 19 requests in 29 minutes.
LAUNCH_WATCH_SECONDS=30
LAUNCH_REFUSED_ATTEMPTS=6
LAUNCH_REFUSED_SECONDS=300

# What runs.
# The control plane the customer runs in their cluster, in the namespace the
# docs bootstrap (deploy/kubernetes/base). InferCrane's own deployments and
# campaign servers are created there too.
NAMESPACE_HOSTED=infercrane-system
NAMESPACE_MODELS=models              # the customer's namespace
ENDPOINT_NAME=qwen3-eks
AGENT_NAME=eks-proof
HELM_RELEASE=agent

# Two models, because the account can run one L4 and the journey needs up to
# four servers at once on it (rules-2026-11-26.md, "Why two models"):
#
# FULL_MODEL serves alone on the whole L4: deploy and gateway traffic.
# Qwen/Qwen3-8B, Apache-2.0, 16.38 GB of weights, the product's reviewed small
# model (Hugging Face, read 2026-11-26).
FULL_MODEL=Qwen/Qwen3-8B
FULL_MODEL_REVISION=b968826d9c46dd6066d109eabc6255188de91218
FULL_MAX_MODEL_LEN=8192
FULL_GPU_MEMORY_FRACTION=0.90
#
# MODEL is every server that shares the L4: the customer's own server, the
# agent's candidate, and the campaign's servers. Qwen/Qwen3-0.6B, Apache-2.0,
# 1.50 GB of weights, 14 million downloads (read 2026-11-26). Each server
# takes 22% of the GPU's memory: 4.9 GiB, about 2.5 GiB of it for the cache.
MODEL=${EKS_PROOF_MODEL:-Qwen/Qwen3-0.6B}
MODEL_REVISION=${EKS_PROOF_MODEL_REVISION:-c1899de289a04d12100db370d81485cdf75e47ca}
MAX_MODEL_LEN=4096
GPU_MEMORY_FRACTION=0.22
# NVIDIA time-slicing advertises the one L4 as this many nvidia.com/gpu, so the
# scheduler itself refuses a fifth server instead of vLLM running out of memory.
GPU_SHARING_REPLICAS=4

# The operator's rate card for the control plane (INFERCRANE_OPTIMIZATION_PRICES_JSON):
# AWS's list price of the whole GPU instance in the region, for the GPU names the node can carry.
# A test value that lets a campaign be approved, never a cost claim.
PRICES_JSON=$(printf '[{"cloud":"kubernetes","gpu":"%s","replicas":1,"hourly_usd":%s,"currency":"USD","source":"aws-list-price/%s/%s/2026-11-26","authority":"account_contract","cost_scope":"instance_total","observed_at":"2026-11-26T00:00:00Z","valid_until":"2026-11-29T00:00:00Z"},{"cloud":"kubernetes","gpu":"%s-SHARED","replicas":1,"hourly_usd":%s,"currency":"USD","source":"aws-list-price/%s/%s/2026-11-26","authority":"account_contract","cost_scope":"instance_total","observed_at":"2026-11-26T00:00:00Z","valid_until":"2026-11-29T00:00:00Z"}]' "$GPU_PRODUCT_NAME" "$PRICE_GPU_INSTANCE" "$GPU_INSTANCES" "$REGION" "$GPU_PRODUCT_NAME" "$PRICE_GPU_INSTANCE" "$GPU_INSTANCES" "$REGION")

# vLLM 0.34.0, the product's current pin (internal/support/matrix.go:68), CUDA
# 13.0.2: the node's driver must be 580 or newer; 30-install.sh records it.
VLLM_IMAGE=vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1
# The PostgreSQL image of the agent's kind proof (scripts/test-agent-data-plane-kind.sh).
POSTGRES_IMAGE=postgres:17-alpine@sha256:e5d2edd28b1f8d40a0a6c0c9cdc545b19b56a0d5d88f94bc11a97d970d4700d0
# NVIDIA's device plugin chart, with GPU feature discovery.
NVIDIA_CHART_REPO=https://nvidia.github.io/k8s-device-plugin
NVIDIA_CHART_VERSION=0.20.2
NVIDIA_CHART_SHA256=55d4644eee1e3bacc9ec75ce7fa7032b6c23cc38a6babfdc9f09d175efc98526

# The candidate run: the load AIPerf sends to the current setup, the candidate,
# and the current setup again.
LOAD_REQUESTS=96
LOAD_CONCURRENCY=4
LOAD_INPUT_TOKENS=512
LOAD_OUTPUT_TOKENS=128
CANDIDATE_TIMEOUT_SECONDS=3600
