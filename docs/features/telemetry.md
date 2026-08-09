# Inference telemetry

InferCrane records operational measurements for each request without storing prompts or generated content. Request bodies are used only to forward the OpenAI-compatible request, and response chunks are observed transiently to extract timing and usage metadata when the runtime supplies it.

Persisted request dimensions are:

- deployment and active revision
- provider, runtime, and compute mode
- GenAI operation name (`chat`)
- HTTP status, error type, and whether the response streamed
- response model, when returned by the runtime

Persisted measurements are request latency, time to first response byte (TTFT at the InferCrane gateway boundary), and input/output token counts when vLLM returns OpenAI usage fields. Missing token usage remains unknown; InferCrane does not estimate it. Replica identity remains unset when the standalone router does not provide a trustworthy selected-worker identity.

The corresponding OpenTelemetry GenAI concepts are `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.server.request.duration`, and `gen_ai.server.time_to_first_token`. InferCrane-specific deployment, revision, provider/runtime, compute-mode, and operation dimensions are retained with the durable request record so decisions can be reproduced later.

Aggregated deployment statistics expose request rate, error rate, latency p50/p95, TTFT p50/p95, and observed input/output tokens per second over the selected window. Token throughput is emitted only from runtime-reported usage.
