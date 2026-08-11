---
title: RunPod
description: Configure the RunPod elastic and provider-native Serverless adapter.
---

# RunPod

RunPod is one registered infrastructure adapter. Elastic deployments use SkyPilot to provision
RunPod Pods; Serverless deployments use RunPod's native endpoint lifecycle. InferCrane's base
production stack does not require RunPod—enable it explicitly with
`compose.production.runpod.yaml`.

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
