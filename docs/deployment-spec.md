# DeploymentSpec

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
  region: EU-RO-1
resources:
  gpu: L40S
scaling:
  min_replicas: 1
  max_replicas: 4
routing:
  strategy: round-robin
```

`compute.mode` defaults to `elastic`; elastic replica bounds default to `1..1`. Serverless requires
`min_replicas: 0` and defaults `max_replicas` to one when omitted. Revisions are immutable; changing
a field creates a candidate. v0.3 accepts vLLM on RunPod elastic/serverless and the configured AWS
EC2 elastic adapter. AWS requires an explicit `provider.region`; AWS Serverless and non-vLLM
runtimes remain unqualified. Cost is omitted unless a trustworthy provider measurement exists.
