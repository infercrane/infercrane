# DeploymentSpec

The primary path uses defaults:

```console
infercrane deploy Qwen/Qwen3-8B
```

Equivalent advanced fields include `model`, `model_revision`, `runtime: vllm`, `runtime_version`, `runtime_args`, `compute_mode: elastic|serverless`, `cloud: runpod`, `gpu`, `region`, `min_replicas`, `max_replicas`, and `routing_strategy`.

```yaml
name: qwen-prod
model: Qwen/Qwen3-8B
model_revision: main
runtime: vllm
compute_mode: elastic
cloud: runpod
gpu: L40S
min_replicas: 1
max_replicas: 4
routing_strategy: round-robin
```

InferCrane resolves `main` to a Hugging Face commit before provisioning. Revisions are immutable; changing a field creates a candidate. v0.1 rejects non-vLLM runtimes, clouds other than RunPod, and serverless minimums other than zero. Cost is omitted unless a trustworthy provider measurement exists.
