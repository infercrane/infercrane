# FAQ

## Does v0.1 support any model, engine, or cloud?

No. v0.1 qualified only vLLM on RunPod. The v0.6 development matrix adds simulated SGLang and
custom OCI profiles on AWS EC2 elastic, but real-GPU evidence remains deferred. Every exact
provider/runtime/mode combination needs independent evidence.

## Do I need Kubernetes?

No. Kubernetes support is intentionally excluded from v0.1.

## Does Release Guard use an LLM?

No. It applies a persisted deterministic policy to measured evidence.

## Does serverless mean InferCrane schedules GPUs?

No. The registered serverless provider owns worker allocation and scale-to-zero; InferCrane owns logical lifecycle and evidence. RunPod Serverless is the first v0.1 backend.

## Is InferCrane coupled to RunPod or vLLM?

No at the lifecycle boundary. Providers, runtimes, serverless status, artifacts, and benchmark tools
are external adapters composed around durable InferCrane state machines. `infercrane integrations`
shows the current exact combinations and separates simulated, local, deferred and real evidence.

## Are prompts or outputs stored?

Not by default. Telemetry and benchmark history contain measurements and operational metadata.

## Are Durable Sessions in v0.1?

No. The preview is deferred to v0.2 and v0.1 makes no durable-KV claim.

## Is provider pricing estimated?

Only trustworthy observed cost metadata is shown. InferCrane does not fabricate prices.
