# InferCrane v1 consolidated release acceptance

The v1 workflow freezes one RC and performs all paid/manual evidence once. It is resumable by run ID,
rejects concurrent paid runs, stops on the first missing observation, cleans each deployment before
moving on, and records logs under `.infercrane/v1-acceptance/RUN_ID/`.

```bash
export INFERCRANE_ACCEPTANCE_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-v1-rc1"

./scripts/v1-acceptance.sh preflight
./scripts/v1-acceptance.sh qualify --approve-paid-resources
./scripts/v1-acceptance.sh cleanup
./scripts/v1-acceptance.sh report
```

`preflight` performs local qualification plus read-only provider/configuration checks. `qualify`
requires a clean checkout at local tag `v1.0.0-rc.1`; it never creates a stable tag or publishes an
artifact. Reuse the same run ID after disconnect or interruption. Do not start a replacement run
while an external create response is unresolved.

## Required infrastructure

### RunPod

- readable API-key file in `RUNPOD_KEY_FILE`
- immutable Serverless vLLM template ID in `INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID`
- secure elastic GPU capacity for a maximum of two replicas
- public, digest-pinned qualified worker image

The existing RunPod harness runs elastic/serverless traffic, benchmark, autoscaling, Guard,
disconnect/restart/delete disruption, lost-create-response adoption, cancellation, and cleanup.

### AWS BYOC

Create the narrow resources documented in [AWS EC2 BYOC](/integrations/aws-ec2): role with external
ID, private subnet/security group, qualified GPU AMI and instance type, instance profile, worker
secret, and immutable runtime images. Copy `.env.production.example` plus `.env.aws.example` into one
private environment file and set:

```bash
export INFERCRANE_V1_AWS_ENV_FILE=/private/infercrane-aws.env
export INFERCRANE_V1_AWS_SPEC_DIR=/private/infercrane-v1-specs/aws
```

The spec directory must contain `vllm.yaml`, `sglang.yaml`, and `custom-oci.yaml`. The harness copies
each spec into its restricted run state and replaces the top-level deployment name with a unique,
run-scoped name; it never mutates the source files or reuses a fixed public identity. Every image and
model revision must be immutable. The control plane is started through the provider-neutral
production Compose file plus the AWS overlay.

Create one restricted file containing a random credential of at least 32 characters and install the
same value in the AWS Secrets Manager and Kubernetes worker Secrets referenced by the provider
configuration:

```bash
export INFERCRANE_V1_API_KEY_FILE=/private/infercrane-v1-worker-and-control-key
```

The harness reads this file without copying its value into evidence. Do not reuse a personal or
long-lived production credential.

### Kubernetes

Provide a restricted kubeconfig, namespace/RBAC from `deploy/kubernetes`, runtime service account
and worker secret, GPU nodes/device plugin, and immutable runtime images. Copy
`.env.production.example` plus `.env.kubernetes.example` into one private file and set:

```bash
export INFERCRANE_V1_KUBERNETES_ENV_FILE=/private/infercrane-kubernetes.env
export INFERCRANE_V1_KUBERNETES_SPEC_DIR=/private/infercrane-v1-specs/kubernetes
```

Use the same three filenames. Select `deployment` or standard `kserve` explicitly; advanced
Dynamo/llm-d topologies are not implied by this gate.

## Runtime spec example

```yaml
apiVersion: infercrane.dev/v1
kind: Deployment
name: v1-vllm
model:
  id: Qwen/Qwen3-8B
  revision: REPLACE_WITH_IMMUTABLE_COMMIT
runtime:
  engine: vllm
  version: 0.8.5.post1
provider:
  cloud: aws
  region: eu-central-1
compute:
  mode: elastic
resources:
  gpu: L40S
scaling:
  minReplicas: 1
  maxReplicas: 1
routing:
  strategy: round-robin
```

Use the provider/runtime documentation to construct SGLang and OCI specs; do not reuse placeholder
digests from repository examples.

## Evidence and cleanup

The portable stages prove doctor, readiness, buffered/streaming traffic, vLLM tool/structured output,
AIPerf, deletion, InferCrane orphan inventory, and direct provider inventory. The portable harness
captures the existing managed AWS/Kubernetes inventory before mutation and requires an identical
inventory afterward. This detects run-owned leaks without requiring an otherwise-used account or
namespace to be empty. RunPod directly lists run-owned Pods and endpoints.

After the report says `real_infrastructure: passed`, independently confirm:

```text
RunPod Pods/endpoints owned by the run                         0
AWS managed inventory added by the run                         0
Kubernetes managed inventory added by the run                  0
```

Provider deletion is not proven by absent local rows. If cleanup was interrupted, resume `qualify`
with the same run ID so it can use persisted identities; do not erase its PostgreSQL volume first.

## What automated local evidence covers

The frozen RC already carries hermetic evidence for explicit governed fallback, budget exhaustion,
privacy acknowledgement, oscillation control, no-replay streaming, provider create-response loss,
runtime translation, migration history, and tenant isolation. A real OpenRouter request is not
silently added to final qualification: it would transmit content and incur separate cost. If an
operator elects to add one, it must remain an explicit, redacted, hard-budgeted supplemental record.

No fake worker, Kind cluster, or provider simulator may be cited as real GPU qualification. No
provider timing, cost, or performance result may be inferred from an operator timeout.
