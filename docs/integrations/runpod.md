---
title: RunPod
description: Configure the RunPod elastic and provider-native Serverless adapter.
---

# RunPod

RunPod is one registered infrastructure provider with separate executable adapters. Built-in vLLM
elastic deployments use SkyPilot. Immutable custom OCI workloads use the native `runpod-pods` REST
adapter, which does not require SSH inside the runtime image. Serverless deployments use RunPod's
native endpoint lifecycle. InferCrane's base production stack does not require RunPod; enable it
explicitly with `compose.production.runpod.yaml`.

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

    Large custom OCI workloads may need more than the default 100 GiB container disk. Set, for
    example, `INFERCRANE_RUNPOD_CONTAINER_DISK_GIB=500` in the same private environment file. Disk
    size is validated before the control plane starts and may affect provider cost.

    Native Pods can instead reuse a model-specific RunPod network volume. Create the volume in the
    intended data center and name it `infercrane-artifact-` followed by the first 20 hexadecimal
    characters of SHA-256 over the exact `model@commit` identity. Then configure the exact mapping:

    ```bash
    MODEL_IDENTITY='org/model@0123456789abcdef0123456789abcdef01234567'
    MODEL_DIGEST="$(printf %s "$MODEL_IDENTITY" | shasum -a 256 | awk '{print $1}')"
    # Name the existing volume infercrane-artifact-${MODEL_DIGEST:0:20} in RunPod.
    export INFERCRANE_RUNPOD_ARTIFACT_CACHE_POLICY='required'
    export INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON="{\"$MODEL_IDENTITY\":\"volume_1234\"}"
    ```

    For gated repositories, create a scoped RunPod secret and configure only its name:

    ```bash
    export INFERCRANE_RUNPOD_HF_TOKEN_SECRET='huggingface-read-token'
    ```

    Native Pod requests contain RunPod's secret reference syntax, never the Hugging Face token.
    InferCrane fails closed on an invalid secret name and cannot verify the secret value without
    launching a workload.

    To populate a new identity-named volume with a resumable transfer, first set the exact
    model/commit, data center, size, current provider rates, retention window, and watchdog, then run
    `make plan-runpod-artifact-cache`. Review its JSON worst-case cost before setting
    `INFERCRANE_RUNPOD_MAX_COST_USD` and using `make build-runpod-artifact-cache`. The build command is
    billable; its hermetic safety test is `make test-runpod-artifact-cache`.

    Before creating a Pod, InferCrane performs a read-only volume lookup, verifies its ID, exact
    identity-derived name, positive size, and data center, then mounts it at `/workspace`. Standard
    Hugging Face downloads and filesystem-materialized model profiles use that persistent path.
    The first deployment may still download the model; `required` proves the persistent volume was
    attached, not that its bytes were already warm. Runtime readiness remains the proof that the
    exact model became serveable. Deleting a Pod does not delete this operator-owned volume.
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
  <Tab title="Custom OCI">
    ```bash
    infercrane workload init ./custom-runtime \
      --recipe glm-5.3-flash \
      --profile custom-oci-hopper-tp8 \
      --name glm-production \
      --cloud runpod \
      --provider-adapter runpod-pods

    infercrane workload validate ./custom-runtime
    infercrane workload plan ./custom-runtime
    infercrane workload deploy ./custom-runtime --wait
    ```

    The generated DeploymentSpec preserves the upstream image digest, complete argv, model
    revision, and exact GPU count. The GLM profile's provider-neutral bootstrap materializes the
    complete pinned snapshot to a local container path before replacing itself with vLLM. Configure
    storage for the roughly 306 GiB FP8 checkpoint plus runtime overhead, preferably through an
    identity-bound network volume. Provider stock and real runtime behavior still require paid
    qualification of that exact tuple. A four-B200 Blackwell candidate exists as
    `custom-oci-blackwell-tp4`, but remains unqualified until exact-tuple evidence is recorded.
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
[compatibility and qualification](../compatibility.md) for the current real-provider evidence boundary.
