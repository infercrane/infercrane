---
title: DeploymentSpec
description: Declaratively define model identity, runtime, provider, compute, scaling, and routing.
---

# DeploymentSpec

New files declare the stable v1 file contract:

```yaml
apiVersion: infercrane.dev/v1
kind: Deployment
```

Pre-v1 files without the header are interpreted as v1. Unknown versions, kinds, and fields fail
closed. See [Upgrade and compatibility](/upgrade) for the change policy.

The primary path uses defaults:

```console
infercrane deploy Qwen/Qwen3-8B
```

The YAML form groups model, runtime, compute, provider, resources, scaling, and routing concerns.
Mutable model revisions such as `main` are resolved and persisted as immutable Hugging Face commits
before provisioning.

```yaml
name: qwen-prod
endpoint: support-production
model:
  id: Qwen/Qwen3-8B
  revision: main
runtime:
  engine: vllm
  version: 0.22.1
  args:
    - --enable-prefix-caching
compute:
  mode: elastic
provider:
  cloud: runpod
  # adapter: skypilot # optional exact profile selection
  region: EU-RO-1
resources:
  gpu: L40S
  gpu_count: 1
scaling:
  min_replicas: 1
  max_replicas: 4
routing:
  strategy: round-robin
```

`name` identifies the deployment lifecycle. `endpoint` identifies the stable application-facing
route and defaults to `name` when omitted. A new deployment revision can therefore replace the
serving plan underneath `support-production` without changing application configuration.

SGLang uses a built-in immutable workload profile. Custom OCI images declare the full contract in
`runtime.workload`; see [Custom OCI workloads](/features/custom-oci). Mutable image tags and
underspecified probes are rejected before provisioning.

An optional `serving` object describes an advanced backend topology without exposing provider CRD
shape to the core schema. Today `backend: dynamo` requires one outer graph replica, Kubernetes, and
the `kubernetes-dynamo` adapter. See [NVIDIA Dynamo](/integrations/dynamo) for the simple command and
the complete reviewable YAML form.

`compute.mode` defaults to `elastic`; elastic replica bounds default to `1..1`. Serverless requires
`min_replicas: 0` and defaults `max_replicas` to one when omitted. Revisions are immutable; changing
a field creates a candidate. `provider.adapter` persists the exact implementation when more than one
profile can serve a cloud/runtime pair; it is optional for an unambiguous default. Current executable
elastic defaults include configured AWS EC2 and GCP Compute, while other provider-product profiles
remain registered but deferred until their exact combination is qualified. AWS and GCP require an
explicit `provider.region`. Cost is omitted unless a trustworthy provider measurement exists.

`resources.gpu_count` is the number of accelerators allocated to each runtime replica and defaults
to one. It is part of immutable intent, pricing, capacity history, benchmarks, recommendations, and
inference passports. SkyPilot, GCP Compute, and Kubernetes compile the exact count into provider
resources. AWS EC2 binds the count to `INFERCRANE_AWS_GPU_COUNT` for the configured instance type and
rejects a mismatch before creating paid capacity. Serverless currently supports one accelerator per
worker and rejects any other count explicitly.

## Engine environment switches

`runtime.env` sets a few reviewed engine switches through the runtime container's environment, for
example AMD's AITER kernels for vLLM:

```yaml
runtime:
  engine: vllm
  version: 0.22.1
  env:
    VLLM_ROCM_USE_AITER: "1"
resources:
  gpu: MI300X
```

It is not a general environment. Each name is reviewed for one engine version, its pinned image,
and the accelerator platforms listed, with a fixed set of values read from the engine's source:

| Name | Runtime | Platforms | Values | Default |
|---|---|---|---|---|
| `VLLM_ROCM_USE_AITER` | vLLM 0.22.1, pinned image | `rocm-gfx942`, `rocm-gfx950` | `0`, `1` | `0` |

Anything else is refused when the spec is loaded, when the control API receives the deployment, and
again in the provider adapter: another name or value, a custom image, another engine version, a GPU
on another platform, or more than 8 entries. A refused value is not repeated in the error, so a
credential pasted here does not reach logs. Credentials never go in `runtime.env`; the worker key
and model tokens reach the runtime through their own references.

RunPod Pods, Kubernetes, AWS EC2, and GCP Compute pass the switches to the runtime container.
RunPod Pods, Kubernetes, and GCP Compute also compare them when adopting an existing replica, so a
running replica with different switches is not adopted. SkyPilot, RunPod Serverless, and Dynamo on Kubernetes cannot pass them, and a revision
that sets any is refused before anything is provisioned. The API field is `runtime_env` on
`POST /api/v1/deployments`. The switches are part of the immutable revision: changing one creates a
candidate, and benchmark rows record them next to the engine arguments.
