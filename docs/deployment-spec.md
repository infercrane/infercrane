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
model:
  id: Qwen/Qwen3-8B
  revision: main
runtime:
  engine: vllm
  version: 0.10.2
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
scaling:
  min_replicas: 1
  max_replicas: 4
routing:
  strategy: round-robin
```

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
