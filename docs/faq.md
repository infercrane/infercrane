# FAQ

## Does v0.1 support any model, engine, or cloud?

No. The public contract is vLLM on RunPod with Hugging Face model artifacts.

## Do I need Kubernetes?

No. Kubernetes support is intentionally excluded from v0.1.

## Does Release Guard use an LLM?

No. It applies a persisted deterministic policy to measured evidence.

## Does serverless mean InferCrane schedules GPUs?

No. RunPod owns worker allocation and scale-to-zero; InferCrane owns logical lifecycle and evidence.

## Are prompts or outputs stored?

Not by default. Telemetry and benchmark history contain measurements and operational metadata.

## Are Durable Sessions in v0.1?

No. The preview is deferred to v0.2 and v0.1 makes no durable-KV claim.

## Is provider pricing estimated?

Only trustworthy observed cost metadata is shown. InferCrane does not fabricate prices.
