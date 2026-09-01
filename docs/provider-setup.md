---
title: Provider setup
description: Configure credentials and prerequisites for qualified infrastructure adapters.
---

# Provider setup

Provider integrations are registered control-plane adapters. Each provider owns its credentials, infrastructure semantics, and billable resources; InferCrane owns durable intent, reconciliation, and cleanup. A provider is supported only after its adapter combination appears in the release qualification matrix.

## Choose the least disruptive path

You do not need cloud-administrator access to evaluate InferCrane. Start with the smallest ownership
boundary that proves value:

| Goal | Start here | Cloud changes |
| --- | --- | --- |
| Inspect an existing vLLM, SGLang, LiteLLM, or OpenAI-compatible service | Connect it in observe-only mode | None |
| Inspect the NVIDIA GPUs on the current bare-metal host | Run `infercrane discover local` | None; local read-only process execution |
| Put stable routing in front of an existing service | Move the connection to traffic-managed ownership | InferCrane gateway only |
| Let InferCrane create and delete GPU workers in your account | Configure one BYOC adapter below | One-time identity, network, secret, quota, and runtime-image setup |
| Operate an existing Kubernetes cluster | Configure the namespace-scoped Kubernetes adapter | Namespace, service account, RBAC, worker Secret, and GPU node capacity |

For BYOC, a cloud administrator prepares the boundary once. Day-to-day operators then use a
short-lived control-plane identity; deployment specifications never contain provider credentials.

## Common onboarding sequence

The provider-specific pages contain the exact prerequisites and configuration fields. The safe
sequence is the same across providers:

1. **Choose a boundary.** Select one account or project, region, private network, runtime image, and
   GPU profile. Do not start with account-wide administrator credentials.
2. **Create workload identity.** Give the InferCrane control plane only the lifecycle permissions it
   needs. Give workers a separate identity that can read only their worker secret and required
   artifacts.
3. **Confirm quota and cost controls.** Check GPU quota and regional availability. Configure a
   provider budget alert, while remembering that an alert does not stop spend.
4. **Configure InferCrane.** Store provider settings outside deployment YAML and keep private files
   mode `0600`.
5. **Run a read-only preflight.** `infercrane doctor --aws`, `infercrane doctor --gcp`, or
   `infercrane doctor --kubernetes` validates identity and required API reads without provisioning a
   GPU.
6. **Plan before mutation.** Review `infercrane plan` output, the exact model revision, immutable
   runtime digest, GPU, region, and cache policy.
7. **Deploy with a recovery handle.** Use an idempotency key and retain the operation ID. Closing the
   terminal does not cancel the durable operation.
8. **Prove cleanup.** Delete through InferCrane, check `infercrane orphans`, and confirm provider
   inventory returns to the recorded baseline.

<Note>
`doctor` proves configuration and identity reads. It cannot prove GPU stock, IAM propagation,
private routing, driver compatibility, model fit, or deletion semantics. Those boundaries are
qualified by the first guarded deployment and remain specific to the exact provider/runtime/model
combination.
</Note>

## Data residency and production qualification

A region flag is placement intent, not a residency guarantee. InferCrane cannot prove where a
provider stores control metadata, model artifacts, logs, backups, or failed requests unless the
provider and customer configuration expose evidence for each boundary. Credentials must remain in
the provider-specific workload identity or referenced secret path; region selection does not weaken
that rule.

Before approving a residency-constrained production path:

```bash
infercrane integrations --output json
infercrane models inspect MODEL_CATALOG_NAME --output json
infercrane plan MODEL_REPOSITORY \
  --cloud PROVIDER \
  --region REQUIRED_REGION \
  --gpu ACCELERATOR \
  --output json
```

Require the exact provider/runtime/mode/model/accelerator/region evidence, private-network design,
artifact location, telemetry destination, backup location, and external-fallback policy. See the
[exact compatibility procedure](/compatibility#check-one-exact-serving-combination) and
[security boundary](/security).

<Warning>
The private preview has narrow real-AWS evidence for the exact model, runtime, accelerator, region,
image, and workload tuples recorded in [AWS real-infrastructure evidence](/testing/aws-real-evidence).
That evidence does not qualify a different AWS tuple, GCP, RunPod, real-GPU Kubernetes, or any data-
residency guarantee. For an unqualified or residency-constrained path, stop after `plan`, or connect
a customer-owned in-region endpoint in observe-only mode while keeping routing and lifecycle
ownership unchanged. Never infer qualification from a provider name, region flag, or hermetic
adapter test alone.
</Warning>

## SkyPilot provider breadth and launch preflight

SkyPilot is the portable capacity driver for clouds that do not need a native InferCrane lifecycle
adapter. Provider registration is declarative and fail closed: a cloud appears as executable only
when its manifest is present and every named credential environment variable exists in the control-
plane process.

```bash
export INFERCRANE_SKYPILOT_API=enabled
export INFERCRANE_SKYPILOT_PROVIDERS_JSON='[
  {
    "cloud": "lambda",
    "label": "Lambda Cloud",
    "runtimes": ["vllm", "sglang", "custom-oci"],
    "credential_env": ["LAMBDA_API_KEY"]
  },
  {
    "cloud": "vast",
    "label": "Vast.ai",
    "runtimes": ["vllm", "custom-oci"],
    "credential_env": ["VAST_API_KEY"]
  }
]'
```

`credential_env` contains environment-variable names, never secret values. The underlying SkyPilot
installation must support the declared cloud and the process must receive the corresponding
credentials. A registered runtime is a routing capability, not real-infrastructure qualification;
the exact provider, region, accelerator, runtime, image, and model tuple must still pass the release
qualification workflow.

Before a billable launch, an authenticated client can compare the current catalog quote with
provider capacity evidence:

```bash
curl -fsS "$INFERCRANE_URL/api/v1/capacity/probes" \
  -H "Authorization: Bearer $INFERCRANE_CONTROL_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"provider":"runpod","region":"EU-RO-1","gpu":"L40S","gpu_count":1}'
```

The response deliberately keeps three facts separate:

- `catalog_quote` is a timestamped catalog observation; it does not reserve inventory.
- `launch_evidence` reports connection, availability, quota, and deployability independently.
  Missing credentials and unsupported provider probes return `unknown` or `connection-required`,
  never an optimistic success.
- Only an accepted provider launch reserves capacity. The resulting operation and qualification
  evidence remain the durable proof used by Release Guard.

RunPod exposes an advisory stock check through its read-only API. Generic SkyPilot clouds currently
prove only that declared credentials are configured; their stock and quota remain `unknown` until
the actual launch or a provider-specific read-only probe supplies evidence.

## RunPod

Set a scoped `RUNPOD_API_KEY` on the control plane and configure SkyPilot's RunPod credentials for elastic workers. Run `infercrane doctor --cloud` before provisioning.

For Serverless, create a RunPod vLLM template with `MODEL_NAME`, immutable `MODEL_REVISION`, and `RAW_OPENAI_OUTPUT=1`, then set `INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID`. `infercrane doctor --serverless` reads and validates the template without creating an endpoint.

Set `INFERCRANE_URL` to a URL reachable from AIPerf and clients. Keep provider and InferCrane credentials out of specifications, logs, issue reports, and benchmark artifacts. Always inspect existing pods/endpoints before retrying manual acceptance and delete paid resources after the test.

## AWS EC2 BYOC

AWS elastic support uses a separately registered EC2 adapter rather than provider conditionals in the
lifecycle engine. It requires a complete role, private network, AMI, instance profile, worker secret,
instance type/GPU, region, and immutable image configuration. See [AWS EC2 BYOC](/integrations/aws-ec2)
and run `infercrane doctor --aws` before provisioning.

ASG, EKS, SageMaker, and Bedrock have separate registered profiles. Registration documents their
ownership boundary; it is not executable qualification. Inspect `infercrane integrations`.

## GCP Compute BYOC

The `gcp-compute` adapter launches private, digest-pinned workers with an attached service account
and deterministic adoption identity. Configuration is all-or-nothing. See
[GCP Compute BYOC](/integrations/gcp-compute). MIG, GKE, and Vertex remain separate registered,
deferred profiles rather than implicit aliases. Run `infercrane doctor --gcp` before provisioning;
it performs only identity and Compute API reads.

## CoreWeave

The `coreweave-cks` profile is CKS-first: InferCrane reuses its namespaced Kubernetes lifecycle and
does not install or own the provider-managed GPU operator. The profile is registered but not yet
executable or locally qualified; real CKS qualification remains deferred.

## Kubernetes

The Kubernetes adapter uses an explicit kubeconfig context, one namespace, an immutable default
runtime image, and a worker Secret reference. It owns a bounded Deployment/Service set or one optional
KServe InferenceService. Apply the reviewed namespace and RBAC manifests, then run
`infercrane doctor --kubernetes`. See [Kubernetes](/integrations/kubernetes) for exact configuration,
security boundaries, local Kind qualification, and current real-GPU limitations.
