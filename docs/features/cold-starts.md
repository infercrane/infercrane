---
title: Cold-start intelligence
description: Measure exposed cold-start boundaries without inventing provider-hidden timing.
---

# Cold-start intelligence

InferCrane classifies a RunPod Serverless request as cold only when a fresh provider `/health`
observation proves that the endpoint had zero idle and zero running workers immediately before the
request arrived. A nonzero observation classifies the request as warm. Missing or stale evidence
leaves the request unclassified.

The background observation uses RunPod's control endpoint and does not send inference traffic, so
it does not create or retain a warm worker. Once zero-worker evidence classifies a request, that
evidence is invalidated immediately; it cannot label later requests unless RunPod is observed at
zero again.

Persisted request evidence includes:

- cold, warm, or unclassified state;
- provider workers observed at arrival;
- the provider observation timestamp;
- end-to-end request latency;
- time to first response byte/token for streaming and non-streaming requests;
- deployment, revision, provider, runtime, compute mode, and operation dimensions.

The JSON explanation always includes `available_boundaries` and `unavailable_boundaries`.
`time_to_ready_p50_ms` and `time_to_ready_p95_ms` are explicit `null` values until a provider
exposes a trustworthy readiness boundary. The same rule applies to statistically insufficient
TTFT percentiles; absence is never presented as zero.

Prompts and generated output are not stored by default.

Inspect the deterministic aggregate:

```bash
infercrane explain cold-start qwen-prod
infercrane explain cold-start qwen-prod --output json
```

The output includes classified request counts, cold and warm TTFT p50, and p95 only after at least
20 samples in the corresponding class. This threshold avoids presenting a tail percentile from a
statistically meaningless handful of requests.

RunPod does not expose trustworthy per-request boundaries for capacity allocation, container
startup, artifact download, model load, runtime initialization, readiness, time-to-ready, or the
true model first-token timestamp through the OpenAI-compatible vLLM request. InferCrane measures
gateway time to first response byte and labels it accordingly. It reports the machine-readable bottleneck code
`provider_capacity_or_worker_initialization` and does not fabricate a more detailed waterfall.
When a provider later exposes grounded boundaries, they can be added without changing the meaning
of existing evidence.
