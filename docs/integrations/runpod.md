---
title: RunPod
description: Configure the RunPod elastic and provider-native Serverless adapter.
---

# RunPod

RunPod is one registered infrastructure adapter. Elastic deployments use SkyPilot to provision
RunPod Pods; Serverless deployments use RunPod's native endpoint lifecycle. InferCrane's base
production stack does not require RunPod; enable it explicitly with
`compose.production.runpod.yaml`.

<Warning>
The commands in this guide can create billable GPU resources. The RunPod adapters have hermetic
contract coverage, but the current elastic path remains **experimental** until the exact release,
image, model, GPU, and RunPod environment pass the credentialed qualification suite. A successful
plan or local simulation is not real-provider qualification.
</Warning>

## Before the first deployment

<Steps>
  <Step title="Store a scoped RunPod key">
    Give the key only the Pod and Serverless permissions you intend this control plane to use. Keep
    it outside the repository and restrict the file:

    ```bash
    mkdir -p "$HOME/.config/infercrane"
    chmod 700 "$HOME/.config/infercrane"
    # Save the key without printing it to the terminal.
    chmod 600 "$HOME/.config/infercrane/runpod-key"
    ```
  </Step>

  <Step title="Start the explicit RunPod production profile">
    Copy `.env.production.example` to a private path, replace its placeholder values, and add the
    key-file path from `.env.runpod.example`. The Serverless template ID is optional unless you use
    provider-native Serverless.

    ```bash
    docker compose --env-file /private/path/infercrane.env \
      -f compose.production.yaml \
      -f compose.production.runpod.yaml config --quiet

    docker compose --env-file /private/path/infercrane.env \
      -f compose.production.yaml \
      -f compose.production.runpod.yaml up -d
    ```

    The base production profile is provider-neutral. The additional overlay mounts the key
    read-only and enables the RunPod/SkyPilot adapter explicitly.
  </Step>

  <Step title="Connect and run read-only checks">
    Use the same URL and API key configured in the private environment file:

    ```bash
    infercrane init \
      --url https://infercrane.example \
      --api-key "$INFERCRANE_API_KEY"

    infercrane doctor --cloud
    infercrane integrations
    ```

    `doctor` checks local dependencies and credentials without creating a Pod. Stop here if a
    required check fails.
  </Step>

  <Step title="Plan before paying">
    ```bash
    infercrane plan Qwen/Qwen3-8B \
      --name qwen-production \
      --cloud runpod \
      --gpu L40S \
      --min 1 \
      --max 4
    ```

    Confirm the model, immutable artifact identity, GPU, compute mode, and replica bounds. RunPod
    stock can change after a read-only availability check, and InferCrane never fabricates a price
    when the provider has not supplied trustworthy cost data.
  </Step>
</Steps>

## Deploy and follow durable state

<Tabs>
  <Tab title="Elastic">
    ```bash
    infercrane deploy Qwen/Qwen3-8B \
      --name qwen-production \
      --cloud runpod \
      --gpu L40S \
      --min 1 \
      --max 4 \
      --idempotency-key qwen-production-v1
    ```
  </Tab>
  <Tab title="Serverless">
    ```bash
    infercrane doctor --serverless
    infercrane deploy Qwen/Qwen3-8B \
      --name qwen-production \
      --compute serverless \
      --cloud runpod \
      --gpu L40S \
      --max 4 \
      --idempotency-key qwen-production-serverless-v1
    ```
  </Tab>
</Tabs>

The command returns a durable operation ID. Closing the terminal does not cancel the operation. Use
the ID from the deploy response to reattach, or inspect the deployment timeline:

```bash
infercrane operation watch <operation-id> --wait-timeout 15m
infercrane status qwen-production
infercrane events qwen-production
infercrane inspect qwen-production
```

Do not submit another deployment because allocation is slow. Reuse the same idempotency key and
inspect RunPod inventory before retrying an unresolved create response.

## Delete and verify cleanup

Deletion is also a durable operation. Preview it, confirm explicitly, then follow provider cleanup:

```bash
infercrane delete qwen-production --plan
infercrane delete qwen-production \
  --yes \
  --wait \
  --idempotency-key delete-qwen-production-v1

infercrane orphans
```

The final `orphans` check proves InferCrane has no known unmanaged resource. For a credentialed
qualification, also verify the RunPod inventory itself: zero run-owned Pods and zero run-owned
Serverless endpoints. Never erase the PostgreSQL state while provider cleanup is unresolved.

See [provider setup](../provider-setup.md) for the credential boundary, [production operations](../production.md)
for self-hosting, [serverless lifecycle](../features/serverless.md) for worker-zero behavior, and
[product qualification](../testing/product-qualification.mdx) for the resumable real-provider gate.
