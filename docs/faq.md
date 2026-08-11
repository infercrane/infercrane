# FAQ

## Does InferCrane support any model, engine, or cloud?

No. The v1 registry contains vLLM, SGLang, and immutable custom OCI profiles across specific
RunPod, AWS EC2 BYOC, and namespaced Kubernetes modes. `infercrane integrations` reports the exact
qualification state; real-GPU evidence remains deferred until final RC qualification. Every exact
provider/runtime/mode combination needs independent evidence.

## Do I need Kubernetes?

No. The primary path does not require Kubernetes. A namespace-scoped Kubernetes adapter is available
for teams that already operate a cluster; InferCrane does not install a distribution or custom
operator.

## Does Release Guard use an LLM?

No. It applies a persisted deterministic policy to measured evidence.

## Does serverless mean InferCrane schedules GPUs?

No. The registered serverless provider owns worker allocation and scale-to-zero; InferCrane owns
logical lifecycle and evidence. RunPod Serverless is the first and currently only registered native
Serverless backend.

## Is InferCrane coupled to RunPod or vLLM?

No at the lifecycle boundary. Providers, runtimes, serverless status, artifacts, and benchmark tools
are external adapters composed around durable InferCrane state machines. `infercrane integrations`
shows the current exact combinations and separates simulated, local, deferred and real evidence.

## Are prompts or outputs stored?

Not by default. Telemetry and benchmark history contain measurements and operational metadata.

## Are Durable Sessions in v1?

No. Session identity remains deferred and InferCrane makes no durable-KV claim.

## Is provider pricing estimated?

Only trustworthy observed cost metadata is shown. InferCrane does not fabricate prices.
