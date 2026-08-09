---
title: RunPod
description: Configure InferCrane's v0.1 elastic and provider-native serverless infrastructure.
---

# RunPod

RunPod is the only infrastructure provider supported by InferCrane v0.1. Elastic deployments use SkyPilot to provision RunPod pods. Serverless deployments use RunPod's native endpoint lifecycle.

<Tabs>
  <Tab title="Elastic">
    ```bash
    infercrane doctor --cloud
    infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S
    infercrane deploy Qwen/Qwen3-8B \
      --cloud runpod \
      --gpu L40S \
      --min 1 \
      --max 4
    ```
  </Tab>
  <Tab title="Serverless">
    ```bash
    infercrane doctor --serverless
    infercrane deploy Qwen/Qwen3-8B \
      --compute serverless \
      --cloud runpod \
      --gpu L40S \
      --max 4
    ```
  </Tab>
</Tabs>

<Warning>
Both paths can create billable resources. Inspect existing pods and endpoints before retrying a provider operation, retain the original idempotency key, and verify provider inventory after deletion.
</Warning>

See [provider setup](../provider-setup.md) for credentials and the [serverless lifecycle](../features/serverless.md) for worker-zero behavior.
