# EKS agent proof

## Second run: the customer install, end to end (26 Nov 2026)

Rules: [rules-2026-11-26.md](rules-2026-11-26.md). One NVIDIA L4 (`g6.xlarge`) in `eu-central-1`
(default) or `us-east-1`, the two regions where this account has a G-instance quota (`01-regions.sh`
reads all regions). The GPU is the L4 (`g6.xlarge`) unless `EKS_PROOF_GPU_INSTANCE=g5.xlarge` picks
the A10G fallback; `EKS_PROOF_GPU_ORDER="g6.xlarge g5.xlarge"` makes `launch.sh` ask for the L4 and
then the A10G in every attempt, and the first grant decides. Pick the region with `EKS_PROOF_REGION=us-east-1` for 00, 10 and `launch.sh`;
once 20-create.sh has recorded a cluster's region and GPU type, every later phase uses them on its
own, and naming others is refused until 90-delete.sh has removed that cluster. An image is pushed to
one region's repository; 10-image.sh pushes an image it built earlier for this code, without a new
build, when this laptop still has it. On 26 Nov Frankfurt refused the L4 in every zone from
13:28 to 18:59 (192 answers, all "insufficient capacity"). The phases, from the repository root;
only 10, 20 and 90 change anything in AWS:

```bash
tools/eks-agent-run/01-regions.sh eu-central-1  # reads quota, zones, price
tools/eks-agent-run/00-preflight.sh             # reads only
tools/eks-agent-run/launch.sh --yes             # 20-create.sh: reserves the L4 first (a refusal costs nothing), then
                                                # the cluster; once the L4 is granted, 10-image.sh pushes the image
                                                # to a private ECR repository meanwhile (building it only when this
                                                # laptop has no image of this code), and 95-watchdog.sh starts
tools/eks-agent-run/run-phases.sh               # 30 to 50 below in order, each under a wall limit, then 90-delete;
                                                # a phase that fails or hangs ends the chain: collect, then delete
tools/eks-agent-run/30-install.sh               # device plugin, control plane in infercrane-system (docs bootstrap)
tools/eks-agent-run/35-deploy.sh                # the docs' infercrane deploy of Qwen3-8B, policy check, gateway, delete
tools/eks-agent-run/40-run.sh                   # shares the L4, customer's server, Helm install of agent + executor, agent path
tools/eks-agent-run/45-migrate.sh               # connect, capture, migration campaign, 5% canary, rollback
tools/eks-agent-run/60-uninstall.sh             # deletes, helm uninstall, bootstrap removed, leftovers listed
tools/eks-agent-run/50-collect.sh               # evidence
tools/eks-agent-run/90-delete.sh --yes          # always: reservations, cluster, stacks, ECR, clean check
```

`aws login` lasts 12 hours from the browser's console session, and nothing can be deleted from
here after it. Give its end to `launch.sh` as `EKS_PROOF_AUTH_UNTIL="YYYY-MM-DD HH:MM"`: the
deadline then falls there when it comes first, and no reservation is asked for once a whole run
(`FULL_RUN_HOURS` plus the time to delete) no longer fits (exit code 8). `95-watchdog.sh`, started
by `launch.sh` after a grant and detached from it, runs `90-delete.sh --yes` when deleting must
begin unless that has happened; a laptop that sleeps stops it, and AWS's own scale-to-zero at the
deadline then leaves only the control plane ($0.10 an hour) until someone deletes it.

`run-phases.sh` gives each phase a wall limit (the sum of its own waits at their limits, and never
past the moment deleting must begin, `DELETE_MINUTES` = 35 before the deadline: `90-delete.sh`
took 10 to 20 minutes on 23-24 Nov and its `eksctl delete cluster` may wait 30). A phase past its
limit is stopped with its whole process group. `--keep-on-failure` leaves the cluster to someone
who fixes the phase and continues with `--from PHASE`; the watchdog still deletes it in time.

Budget $10 for all attempts, one cluster at most 5 hours (AWS scales the nodes to zero and ends the
reservation at the deadline). The first run's text below still describes the agent proof of 23-24
Nov; where it differs, the rules of 26 Nov and `env.sh` win.


One run that proves the InferCrane agent on a real Kubernetes cluster in a customer's cloud:
Amazon EKS in Frankfurt with one NVIDIA A10G node. The agent is installed with its Helm chart,
finds the customer's model server, serves it inside the cluster, runs one candidate setup beside
it, measures both, and removes the candidate. Then everything is deleted.

It is a path proof on a small GPU with a small model. It measures no speed. The rules, written
before the run, are in [rules-2026-11-23.md](rules-2026-11-23.md).

Nothing here touches production, and nothing is created in AWS without `--yes`.

## The design in five answers

**1. Which control plane does the agent dial?** A stand-in inside the same cluster:
`infercrane serve` with PostgreSQL in the namespace `infercrane-hosted`, built from this checkout.
The agent and the model servers live in the customer's namespace `models` and reach the control
plane only by HTTPS, as they would across the internet. This is the layout of the agent's kind
proof (`scripts/test-agent-data-plane-kind.sh:137`, `internal/testtools/agentintegration/kind/hosted.yaml`).
The agent refuses plain HTTP to another host (`internal/agent/client.go:54`), so the control plane
serves TLS itself with a self-signed certificate (`INFERCRANE_TLS_CERT_FILE`,
`internal/config/config.go:555`) and the chart gets it as `controlPlane.caBundle`. It costs one
small system node (`m7g.large`, $0.0978 an hour), which also keeps the load test off the GPU
node's four vCPUs.

**2. What does the agent need from the cluster?**

| Need | Who provides it |
|---|---|
| A namespaced Role: Deployments and Services (get, list, create, patch, delete), read-only PVCs | the chart (`deploy/helm/infercrane-agent/templates/rbac.yaml:13`) |
| Service accounts for the agent and for candidate pods, the candidates' API key Secret, the hashing key | the chart |
| The agent key Secret | the operator, from `POST /api/v1/agents` (`30-install.sh`) |
| An image the nodes can pull | **not covered today**: the release image is not pullable without credentials, so `10-image.sh` builds it and pushes it to a private ECR repository |
| NVIDIA driver and container toolkit | the EKS AL2023 NVIDIA AMI |
| The `nvidia.com/gpu` resource | NVIDIA's device plugin; not in the AMI, not installed by InferCrane (`docs/integrations/kubernetes.mdx:26`) |
| The node label `nvidia.com/gpu.product` | NVIDIA's GPU feature discovery. Candidate pods select their node by it (`internal/provision/kubernetes.go:378`) |
| GPU nodes without a taint | the cluster. Candidate pods get no tolerations (`internal/provision/kubernetes.go:378`) |
| Storage | none. Candidates download weights into their own container (`ArtifactCachePolicy: "disabled"`, `cmd/infercrane/agent.go:212`), so the node's disk must hold the image and the weights |
| Egress | the control plane (HTTPS), Hugging Face (weights, and the tokenizer AIPerf loads in the agent's pod), Docker Hub. Nothing inbound |

**3. Does one run need two GPU servers at once?** Yes. A candidate run starts the candidate
while the current server keeps running, then measures the current setup, the candidate and the
current setup again, and runs the answer check on both (`internal/agent/kubernetes_executor.go:129`,
`:147` to `:169`). The candidate always asks for at least one `nvidia.com/gpu`
(`internal/agentprotocol/commands.go:229`, `internal/provision/kubernetes.go:365`). The account's
quota allows one 4-vCPU GPU node, and no 4-vCPU instance has two GPUs.

So the one A10G is shared: NVIDIA's device plugin advertises it as two `nvidia.com/gpu`
(time-slicing), and each server limits itself to 45% of the GPU's memory. That is enough to prove
the whole path: install, discovery, serving, one candidate started, measured and removed, the
answer check, teardown. It is not enough to compare the two setups, because they share a GPU.
GPU feature discovery then labels the node `NVIDIA-A10G-SHARED`, so the sharing shows in every
record.

Through the agent a "campaign" is one candidate run today. Candidate runs are not yet ranked or
promoted by optimization campaigns (`docs/architecture/agent.mdx:329`).

**4. Which model?** `Qwen/Qwen3-4B` (Apache-2.0, 8.04 GB, 3.1 million downloads on 2026-11-23).
The product's reviewed small model is its sibling `Qwen/Qwen3-8B` (`internal/curatedrecipe/catalog.go:190`),
on the pinned vLLM 0.22.1 image (`internal/support/matrix.go:38`). Its 16.4 GB of weights fit one
A10G once, not twice, so the shared-GPU run uses the 4B model of the same family, with the 1.7B
model as the fallback the rules allow. No registry flag is needed: an agent registers whatever its
server serves, and a candidate run names its image and command itself
(`internal/agentrelay/commands.go:40`). The commands are the product's own "vLLM balanced" and
"vLLM interactive" profiles (`internal/curatedrecipe/catalog.go:86`, `:94`).

**5. Which GPU?** `g5.xlarge`, one A10G with 24 GB, $1.258 an hour. It is the only 24 GB type
with an earlier InferCrane run to compare notes with (August, `docs/testing/aws-real-evidence.md`),
the AMI's 580 driver supports the engine image's CUDA 13, and 24 GB holds two 4B servers. The T4
(16 GB) is too small for that, the L4 (`g6.xlarge`, $1.0064) is the fallback if AWS has no A10G,
and the L40S costs twice as much and had no capacity in Frankfurt in August.

## What a quota of 16 vCPUs would add

Two `g5.xlarge` nodes (8 vCPUs, $1.258 an hour more): each server on its own A10G, no GPU
sharing, the reviewed `Qwen/Qwen3-8B` itself at full memory. That is the layout a customer would
run, and the only one in which the current setup and the candidate could be compared at all. It
would still be a small-GPU path proof, not a speed result. Three changes: `desiredCapacity` and
`maxSize` 2 in `cluster.yaml`, no `sharing` block in `values/nvidia-device-plugin.yaml`, and the
model, its revision and `GPU_MEMORY_FRACTION=0.90` in `env.sh`.

## Files

| File | Purpose |
|---|---|
| `rules-2026-11-23.md` | what counts as passed, what is reported, what is not claimed |
| `env.sh` | every name, pin, price and limit |
| `cluster.yaml` | the eksctl cluster config |
| `manifests/hosted.yaml` | the stand-in control plane and PostgreSQL |
| `manifests/models.yaml` | the customer's model server |
| `values/nvidia-device-plugin.yaml` | NVIDIA device plugin with feature discovery and GPU sharing |
| `values/agent.yaml` | Helm values for `deploy/helm/infercrane-agent` |
| `candidate-run.template.json` | the candidate run that is queued |
| `00-preflight.sh` … `91-verify-clean.sh` | one script per phase, below |
| `lib.sh` | shared helpers: the time limit, the account mask, the API calls |

Local state (the cluster's own kubeconfig, the test's keys, the deadline, outputs) is kept in
`.infercrane/eks-agent-run/`, which git ignores. The user's `~/.kube/config` is never touched.
The AWS account number is replaced by `<ACCOUNT>` in everything the scripts print or save.

## What is created in AWS

`20-create.sh` prints this list before it creates anything, and the real resource list after.

- CloudFormation stack `eksctl-infercrane-eks-proof-cluster`: the EKS cluster (Kubernetes 1.36,
  public endpoint, no logging); a VPC with an internet gateway, two public and two private
  subnets and route tables; two security groups; the IAM role
  `eksctl-infercrane-eks-proof-cluster-ServiceRole-<suffix>`. No NAT gateway and no elastic IP.
- CloudFormation stacks `eksctl-infercrane-eks-proof-nodegroup-gpu` and `-system`: the IAM roles
  `infercrane-eks-proof-gpu-node` and `infercrane-eks-proof-system-node`, a launch template each,
  and the two managed node groups (one `g5.xlarge` with 80 GB, one `m7g.large` with 30 GB).
- Made by EKS for them: two Auto Scaling groups, two instances with their disks, network
  interfaces and public IPv4 addresses, one more security group, the default add-ons.
- One scheduled action per Auto Scaling group that scales it to zero at the deadline.
- By `10-image.sh`: the private ECR repository `infercrane-eks-proof`.
- Created once per account by AWS and kept afterwards, free: the service-linked roles
  `AWSServiceRoleForAmazonEKS`, `AWSServiceRoleForAmazonEKSNodegroup` and
  `AWSServiceRoleForAutoScaling`.

Every resource carries the tag `project=infercrane-eks-proof`. No IAM user, access key, OIDC
provider, KMS key, log group, load balancer or persistent volume is created.

## Cost and the time limit

List prices for `eu-central-1` from the AWS Price List API on 2026-11-23, in USD before tax.

| Item | An hour |
|---|---:|
| EKS control plane, standard support | $0.1000 |
| 1 x `g5.xlarge`, on demand | $1.2580 |
| 1 x `m7g.large`, on demand | $0.0978 |
| 2 public IPv4 addresses | $0.0100 |
| 110 GB of gp3 disk | $0.0143 |
| ECR storage, about 3 GB | $0.0004 |
| **Total** | **$1.4806** |
| Counted by the time limit (3% added for data transfer and requests) | $1.5250 |

Since the third cluster the GPU node may be a `g5.xlarge`, a `g6.xlarge` ($1.0064) or a
`g6e.xlarge` ($2.327), whichever AWS has first in that order. The table above is the
`g5.xlarge` case. The time limit counts every hour with the dearest of the three: $2.5496 an
hour at list price, $2.6261 counted, so one cluster can count up to $10.50. A run that goes to
plan (1.5 hours) is about $2.22 on an A10G, $1.84 on an L4 and $3.82 on an L40S.

| | Hours | Cost |
|---|---:|---:|
| A run that goes to plan | about 1.5 | about $2.20 |
| The most one cluster can cost | 4 | $6.10 |
| The whole test, all clusters together | | $15 at most |

How the limit is enforced:

- `20-create.sh` writes the deadline (start plus 4 hours, or less if earlier clusters used part
  of the budget) to local state and as a tag on the cluster. It refuses to create a cluster when
  the budget left pays for less than 1.5 hours.
- Every later phase refuses to start if it could run into the last 25 minutes, which are kept
  for deleting, and says how many minutes are left.
- At the deadline AWS itself scales both node groups to zero. If nobody runs `90-delete.sh`,
  what is left costs $0.10 an hour (the control plane) until someone does.
- `90-delete.sh` counts each cluster's whole lifetime against the $15.

## Run it

From the repository root, in this
order. The phases that create something in AWS are 1 and 2. Without `--yes` they only print.

| # | Command | Creates | Time |
|---|---|---|---|
| 0 | `tools/eks-agent-run/00-preflight.sh` | nothing (reads only) | 1 min |
| 1 | `tools/eks-agent-run/10-image.sh --yes` | the ECR repository and one image; a `docker build` on this laptop | 5 to 15 min |
| 2 | `tools/eks-agent-run/20-create.sh --yes` | the cluster and both nodes; **the bill starts** | 15 to 20 min |
| 3 | `tools/eks-agent-run/30-install.sh` | Kubernetes objects only | 15 to 25 min |
| 4 | `tools/eks-agent-run/40-run.sh` | Kubernetes objects only | 15 to 30 min |
| 5 | `tools/eks-agent-run/50-collect.sh` | nothing (reads only) | 2 min |
| 9 | `tools/eks-agent-run/90-delete.sh --yes` | deletes everything, then checks | 10 to 20 min |

The billed part as one command, so no paid minute is spent waiting between phases. Whatever
happens, it ends by collecting what there is and deleting everything:

```bash
mkdir -p .infercrane/eks-agent-run/logs && L=.infercrane/eks-agent-run/logs && { tools/eks-agent-run/20-create.sh --yes >$L/20-create.log 2>&1 && tools/eks-agent-run/30-install.sh >$L/30-install.log 2>&1 && tools/eks-agent-run/40-run.sh >$L/40-run.log 2>&1; tools/eks-agent-run/50-collect.sh >$L/50-collect.log 2>&1; tools/eks-agent-run/90-delete.sh --yes >$L/90-delete.log 2>&1; }
```

Its exit code is the delete phase's: 0 only when nothing billable is left. A failed phase is
then studied from the logs, `evidence/` and `diagnostics/` (local only) in
`.infercrane/eks-agent-run/`, not on a billed cluster.

`tools/eks-agent-run/91-verify-clean.sh` reads only and can be run at any time: it lists
anything of the test that is left, by name, by tag, and by comparing the whole region with the
counts taken before the test.

`90-delete.sh --yes` works alone at any moment, also while the cluster is being created, and may
be repeated until it ends with "nothing billable of this test is left".

## When a phase fails

- **"AWS credentials are missing or expired" (exit code 3).** Stop. The user runs `aws login`.
  An `aws login` sign-in ends after some hours; AWS documents up to 12, and this tool must not
  count on a figure (see below). Every phase checks it
  before it does anything and exits with code 3 if it is gone, the delete phase too: **without a
  valid sign-in nothing can be deleted from this machine.** A phase that is already running stops
  at its next refused call. The cluster keeps billing meanwhile. The scale-to-zero actions live
  in AWS and still fire at the deadline, so from then on only the control plane ($0.10 an hour)
  is billed until someone signs in and runs `90-delete.sh --yes`.
  **Nothing on this machine says when a sign-in ends, and running `aws login` again does not
  reliably move that moment.** On 23 Nov 2026 the user signed in at 06:28 and again at 13:26;
  the sign-in ended at 18:28, 12 hours after the first, in the middle of the second cluster's
  deletion. The node groups were already gone and CloudFormation finished the cluster by
  itself, but the image store stayed and nothing could be checked until the next sign-in. The
  second `aws login` seems to have ridden on the browser's console session from the morning.
  So start `20-create.sh` right after a sign-in for which the user has first signed out of the
  AWS console in the browser, and plan for the whole run, deletion included, to end within a few
  hours of it. `EKS_PROOF_MAX_LIFETIME_HOURS=2.5 tools/eks-agent-run/20-create.sh --yes` shortens
  the cluster's lifetime for one launch; only the create phase reads that setting.
  After a cut-off deletion, sign in and run `90-delete.sh --yes` again. It deletes what is left,
  checks, and counts the cluster's lifetime until AWS finished deleting it.
- **`20-create.sh`, or the laptop, dies while the cluster is being created.** The deadline is
  written to local state before `eksctl` starts, but the two scale-to-zero actions are set only
  after `eksctl create cluster` returns, 15 to 20 minutes later. CloudFormation keeps working
  without the laptop, so a death in the last minutes of that window can leave both nodes running
  with nothing in AWS to stop them. Run `90-delete.sh --yes`: it works on a cluster that is still
  being created and ends with the clean check. Do not start `20-create.sh` a second time while
  the first one is still running; once the first has ended and the cluster is `ACTIVE`,
  `20-create.sh --yes` adds what is missing (a node group, the deadline tag, the actions) and
  keeps the first deadline. Before the node groups exist only the control plane is billed
  ($0.10 an hour).
- **`20-create.sh` fails because AWS has no capacity (`InsufficientInstanceCapacity`).** This
  happened twice on 23 Nov 2026. The first cluster, pinned to zone `a`, got neither a
  `g5.xlarge` nor an `m7g.large`; the Auto Scaling groups retried for 29 minutes and EKS reported
  the failure only after 33. The second, in zones `b` and `c`, got its system node at once and
  no `g5.xlarge` in either zone. So both node groups now use all three zones, and the GPU node
  group lists three types in order of preference, `g5.xlarge` (A10G), `g6.xlarge` (L4, the
  fallback the rules name) and `g6e.xlarge` (L40S): AWS starts the first it has.
  The create phase also looks at both node groups' Auto Scaling groups every 30 seconds while
  `eksctl` waits. It finds them by the tags EKS puts on them, because EKS names a node group's
  Auto Scaling group only once the node group is active (the second cluster's watch looked
  there and saw nothing). When a group has no instance, has asked at least 6 times over 5
  minutes, and AWS refused every request, the phase prints AWS's answer, stops waiting and
  exits with **code 7**. Anything it cannot read counts as no answer and never ends a healthy
  create (limits: `LAUNCH_*` in `env.sh`). `50-collect.sh` keeps the refusals and
  CloudFormation's reason in the evidence.
  The answer is a new cluster, not a repair: let the chain delete, then change what AWS may
  choose from (`cluster.yaml`, with `NODE_ZONES` and `GPU_INSTANCES` in `env.sh`; the preflight
  checks that they agree). A quota or a zone listing does not show capacity: AWS says so only
  when the node is requested.
  EKS does not delete a node group it is still creating: it keeps asking until its own attempt
  ends, about 33 minutes after the node group started. Seen on the third cluster (24 Nov 2026):
  the early stop came at 14:54, the GPU node group stack was gone only at 15:23, the cluster at
  15:26. The system node, which had started, was terminated at 15:01. So the early stop saves
  the create phase's wait, not the cluster's lifetime; while EKS keeps asking, only the control
  plane is billed, plus a GPU node for those minutes if AWS finds one after all.
  On 24 Nov AWS refused all three GPU types in all three zones: CloudTrail's `CreateFleet`
  records show each request trying every type in one zone and getting
  `InsufficientInstanceCapacity` for each. The Auto Scaling messages name only the first type,
  so read CloudTrail to know what was tried.
- **The model server does not become ready.** `30-install.sh` prints the pod's events and the
  engine's last lines. Out of GPU memory: set `EKS_PROOF_MODEL` and `EKS_PROOF_MODEL_REVISION`
  to the fallback in `env.sh` for this and every later phase.
- **The candidate run fails.** `40-run.sh` prints the failure code and the namespace's events.
  Run `50-collect.sh` anyway. A second run needs `CANDIDATE_RUN_ID` removed from
  `.infercrane/eks-agent-run/state.env`.
- **Anything unclear, or the time limit is near.** Run `90-delete.sh --yes`. A new cluster is
  cheaper than a debugging session on a billed one.

## Found while preparing

- The agent chart's default image tag was the bare version (`2.0.2`), but releases publish
  `v2.0.2`. Fixed on this branch with a test (`internal/testtools/agentintegration/chart_test.go`).
- `ghcr.io/infercrane/infercrane` cannot be pulled without credentials (checked 2026-11-23), so
  no cluster can install the chart from the published image yet.
- Candidate pods get no tolerations, CPU or memory requests, or `/dev/shm` volume. A cluster that
  taints its GPU nodes, which is common, cannot run a candidate.
- One candidate run needs a second GPU slot beside the running server. A cluster with exactly as
  many GPUs as servers cannot run one without GPU sharing.
- Inside Kubernetes the agent's pod has no GPU, so the hardware inventory it reports is expected
  to be "unavailable". The run records what it reports.
- `kubectl --dry-run=client` needs an API server for discovery. Preflight therefore parses the
  manifests offline, and `30-install.sh` runs a server-side dry run on the new cluster first.
