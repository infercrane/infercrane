---
title: vLLM
description: How InferCrane manages and routes requests to its only v0.1 inference runtime.
---

# vLLM

vLLM is InferCrane v0.1's inference runtime. InferCrane owns deployment state and lifecycle; vLLM owns model execution. A supervised [vLLM Router](https://github.com/vllm-project/semantic-router) process distributes requests across healthy standalone replicas.

## Connect existing workers

Each worker must expose the OpenAI-compatible vLLM API and be reachable from the InferCrane gateway.

```bash
infercrane target add gpu-a \
  --url http://gpu-a:8000 \
  --runtime vllm \
  --upstream-model Qwen/Qwen3-8B

infercrane apply Qwen/Qwen3-8B \
  --name qwen-prod \
  --targets gpu-a \
  --idempotency-key qwen-prod-initial \
  --wait
```

The reconciler verifies health and served-model identity before publishing a route. An unhealthy or mismatched worker does not enter the request path.

## Responsibility boundary

| InferCrane | vLLM |
|---|---|
| Desired and observed deployment state | Model loading and execution |
| Durable operations and revisions | OpenAI-compatible worker API |
| Health/model reconciliation | Token generation and KV cache |
| Safe routing generations | Per-replica runtime metrics |

InferCrane does not implement an inference engine or distributed KV cache. SGLang is not supported in v0.1.

<Card title="Gateway and routing" icon="route" href="/features/gateway">
  Follow an OpenAI request from alias resolution to a healthy vLLM replica.
</Card>
