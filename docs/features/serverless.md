# RunPod Serverless

InferCrane v0.1 uses RunPod Serverless as its provider-native serverless backend. RunPod owns
worker allocation, queueing, scale-up, idle scale-down, and GPU scheduling. InferCrane owns the
logical deployment, durable endpoint operation, immutable model identity, routing metadata,
telemetry, deletion, and explanations.

## Provider setup

Create or select a RunPod Serverless template using RunPod's maintained vLLM worker. The template
must set:

- `MODEL_NAME` to the exact Hugging Face repository, such as `Qwen/Qwen3-8B`.
- `MODEL_REVISION` to the immutable Hugging Face commit resolved for the deployment. `main` and
  `master` are rejected.
- `RAW_OPENAI_OUTPUT=1` so streaming remains OpenAI-compatible SSE.

Configure the control plane, keeping the RunPod credential separate from InferCrane client
credentials:

```bash
export RUNPOD_API_KEY='...'
export INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID='...'
infercrane doctor --serverless
```

The template ID is intentionally explicit. InferCrane does not build or own a custom inference
image, and it refuses to create an endpoint when the template does not match the requested
immutable model artifact.

## Deploy

```bash
infercrane plan Qwen/Qwen3-8B --compute serverless --cloud runpod --gpu L40S --max 4
infercrane deploy Qwen/Qwen3-8B --compute serverless --cloud runpod --gpu L40S --max 4 --wait
```

Serverless deployments always use zero minimum workers in v0.1. The logical InferCrane endpoint
remains stable while RunPod scales workers from zero on the first request, back to zero after its
idle timeout, and from zero again on later requests. InferCrane does not poll the inference URL for
health because doing so would create or retain warm workers.

Inference requests continue to use the InferCrane credential and logical model name. The gateway
replaces that credential with the RunPod credential only for the upstream request. It forwards
streaming incrementally and propagates client cancellation through the request context.

## Delete and recovery

Deletion first withdraws the logical route, then deletes the RunPod endpoint, confirms that the
endpoint is absent from provider inventory, removes persisted target capacity, and finally removes
the logical deployment. Every step is a durable operation and can resume after a control-plane
restart.

Endpoint creation is replay-safe. Before creating capacity, InferCrane lists endpoints for the
deterministic deployment/revision key. A retry adopts the one exact matching endpoint; multiple
matches or immutable-spec mismatches fail visibly instead of creating another billable resource.

## Current limitations

- One configured immutable vLLM template is supported per control-plane process.
- Real RunPod cold/warm, streaming, cancellation, scale-to-zero, and billing acceptance is still
  required before the v0.1 release candidate is declared ready.
- InferCrane records request timing and token metadata but does not record prompts or generated
  content by default.

See [cold-start intelligence](/features/cold-starts) for the exact classification evidence and unavailable
provider timing boundaries.
