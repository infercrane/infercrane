# Qwen3.8 OpenRouter launch decision

Date: 2026-09-23

## Decision

Proceed to an exact-target canary with this bounded offer:

- model: `Qwen/Qwen3.8-27B-FP8@017b9c7af6b5689d5dd426a76e0bc077eb5ca20a`;
- runtime: SGLang 0.5.20;
- target: one H200 for canary qualification;
- recipe: FP8 weights and KV cache, bounded CUDA graphs, NEXTN/MTP with three
  speculative steps and four draft tokens, and the runtime-selected DeepGEMM
  FP8 path;
- admission: twelve in-flight requests, with immediate HTTP 429 above the
  boundary;
- public contract: text input, 32,768 input tokens, 2,048 output tokens, tools,
  structured output, streaming, and usage accounting; and
- starting price: $0.10/M input tokens and $2.20/M output tokens.

Keep `is_ready: false` until the exact deployment passes the external provider
qualifier, a 24-hour soak, usage reconciliation, overload/recovery tests, and
an OpenRouter test canary. Modal results are screening evidence, not a hosted
provider claim.

## Why this recipe

The paired H200 run used the same model, image, workload, hardware, and output
checks for the control and selected recipe. At concurrency twelve:

| Measurement | Control | Selected | Change |
|---|---:|---:|---:|
| Aggregate output throughput | 810.4 tok/s | 1,393.6 tok/s | 1.72x |
| Per-request p50 output speed | 76.1 tok/s | 141.0 tok/s | 1.85x |
| p50 TTFT | 1,591 ms | 739 ms | 53.6% lower |
| p95 inter-token latency | 14.02 ms | 8.67 ms | 38.1% lower |
| Successful requests | 24/24 | 24/24 | equal |

The deterministic control and selected outputs matched. Streaming and buffered
usage, finish reasons, structured output, forced tools, malformed-request
handling, and prompt token accounting passed.

The result is in
`docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json`
with raw measurements in the adjacent `-raw.json` file.

### Runtime and decoder challenge

A later single-run screen challenged the selected recipe against SGLang
DFlash2 and vLLM 0.30.0 MTP on the same H200 workload shape. It does not replace
the repeated result above, but it changes what InferCrane should search next.

| Runtime recipe | c4 output tok/s | c8 | c12 | c16 | c12 GPU COGS / M output |
|---|---:|---:|---:|---:|---:|
| SGLang native MTP | 680.7 | 1,080.5 | 1,335.7 | 1,582.1 | $0.944 |
| SGLang DFlash2 | 767.5 | 1,147.0 | 1,323.5 | 1,560.3 | $0.953 |
| vLLM native MTP | 517.4 | 957.1 | 1,203.2 | 1,570.3 | $1.048 |

DFlash2 won aggregate throughput at concurrency four and eight, while native
MTP retained the better TTFT and narrowly won saturation economics. vLLM
nearly converged at concurrency sixteen but was slower at the launch boundary.
The launch recipe therefore remains SGLang native MTP. DFlash2 becomes a
workload-specific candidate for interactive/code-heavy traffic rather than a
universal replacement.

The campaign now normalizes speculative-health evidence across SGLang gauges
and vLLM counters. vLLM MTP accepted approximately 2.11 tokens per verification
round and 70.3% of drafted tokens; the earlier zero-acceptance result was a
measurement-adapter bug, not a runtime failure. Thinking output is checked for
valid final-answer semantics while byte parity remains mandatory for stable
non-thinking probes. Unknown public model rejection belongs to the provider
edge, not the internal runtime.

Screening receipts:

- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T063227Z.json`
- `docs/testing/evidence/qwen38-vllm-public-decode-saturation-modal-2026-09-23T063555Z.json`

## Competitive position

OpenRouter's endpoint API on 2026-09-23 showed sixteen providers and a market
floor of $0.10/M input and $1.80/M output. In the 06:25 UTC snapshot, the best
stable p50 output speed was 64 tok/s and the lowest stable p50 latency was 308
ms. Wafer displayed $0.11/$2.50, 61 tok/s, 808 ms latency, and approximately
99.82% one-day uptime. These fields can be null when an endpoint has
insufficient recent traffic, so every comparison preserves its capture time.

The selected lane's 141 tok/s per-request p50 has meaningful throughput
headroom over the current public maximum. Its 739 ms p50 TTFT at saturated
concurrency twelve does not establish latency leadership. At lower concurrency
the same recipe measured 202 tok/s and 265 ms at four requests, and 164 tok/s
and 351 ms at eight requests. Only OpenRouter's own production measurements
can establish rank because network, arrival distribution, prompt mix, and
regional routing differ from the Modal loopback screen.

For the measured 2.847:1 input/output ratio, the $0.10/$2.20 price is about
$2.485 per million output-equivalent tokens. It is cheaper than thirteen of
the sixteen current endpoints on that workload mix, behind only Darkbloom and
DeepInfra. The price is intended to enter OpenRouter's low-price routing set
without matching the absolute output-price floor. OpenRouter's default routing
first considers recent stability, then weights stable low-cost providers by
inverse-square price; `:nitro` explicitly sorts by throughput. Reliability and
early 429 responses are therefore as important as raw token speed.

Sources:

- [Qwen3.8-27B providers](https://openrouter.ai/qwen/qwen3.8-27b)
- [Provider routing](https://openrouter.ai/docs/guides/routing/provider-selection)
- [OpenRouter provider requirements](https://openrouter.ai/docs/guides/community/for-providers)

### Demand boundary

OpenRouter's authenticated rankings dataset reported 59.52 billion total
Qwen3.8-27B tokens on 2026-09-22. Applying the measured 2.847:1 input/output
mix yields an estimated 15.47 billion output-equivalent tokens for that day.
The selected lane can produce approximately 120.4 million output tokens/day at
its measured ceiling, so one H200 needs only about 0.32% of that estimated
market volume to cover the GPU and a 10% non-GPU reserve. Approximately 0.52%
would fill the capacity needed for a 35% contribution margin.

This removes market volume as the primary one-GPU risk, but it does not predict
InferCrane's routing share. A new provider begins without production
performance history, OpenRouter has a provider-application backlog, and actual
traffic depends on feature eligibility, geography, uptime, price, routing
policy, and explicit user selection. The captured public row and derivation
are stored in
`docs/testing/evidence/qwen38-openrouter-demand-2026-09-23.json`.

## Economics

At concurrency twelve, the measured Modal H200 GPU cost was $0.905 per million
successful output tokens. For the measured 2.847:1 input/output token ratio,
the proposed price produces $2.485 of revenue per million output-equivalent
tokens.

Using the target RunPod H200 price of $4.59/hour and reserving 10% of revenue
for gateway, storage, monitoring, recovery, and other non-GPU costs:

- contribution break-even requires approximately 40.9% utilization; and
- a 35% contribution margin requires approximately 66.9% utilization.

This is capacity economics, not a revenue forecast. OpenRouter demand may not
fill the GPU, and one replica is not enough for high availability. Do not add a
second always-on replica until canary demand, failure-domain requirements, or
the onboarding agreement justifies it. Requalify any lower-cost GPU offer on
the exact hardware before substituting its price.

## Kernel decision

The captured profile shows GEMM is the material family: 57.4% of decode GPU
time and 73.0% of prefill GPU time. The selected path uses existing
architecture-specialized kernels instead of promoting custom code for its own
sake.

The endpoint-level backend search produced three useful rejections:

- explicit FlashInfer TensorRT-LLM and FlashInfer CUTLASS FP8 backends require
  Blackwell for this recipe and were rejected on H200;
- explicit CUTLASS FP8 is deprecated on H200 by SGLang, which directs Hopper
  to DeepGEMM; and
- the compatible Triton FP8 backend reached 935.4 aggregate tok/s at
  concurrency twelve, 32.9% below the selected path. It also had 1,098 ms p50
  TTFT and 13.78 ms p95 inter-token latency versus 739 ms and 8.67 ms.

The selected automatic backend was 1.49x faster than Triton on the full
workload. A handwritten GEMM is not justified unless a future exact profile
finds a shape that the vendor path does not cover. Earlier custom residual plus
RMSNorm Triton and CUDA candidates were also correctly rejected after their
endpoint ceiling or measured performance lost to the runtime operator.

Backend evidence:

- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045002Z-failures.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045916Z.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045916Z-failures.json`

## Required launch gates

1. Build and publish the immutable candidate image.
2. Prepare the exact model revision on a persistent target volume and verify
   every file hash before startup.
3. Deploy one regional H200 endpoint behind HTTPS using the provider edge.
4. Run the external qualifier for `/models`, streaming, usage, tools,
   structured output, invalid requests, bounded overload, recovery, and
   cleanup.
5. Qualify 32,768-token inputs and 2,048-token outputs at the public boundary.
6. Run a 24-hour soak with controlled load and reconcile provider token counts
   against gateway and runtime records.
7. Publish privacy, retention, security-contact, status, and incident terms;
   complete monthly-invoice onboarding.
8. Give OpenRouter the hidden `is_ready: false` endpoint for test traffic.
9. Set `is_ready: true` only after OpenRouter validates the endpoint.
10. Compare the resulting public TTFT, throughput, uptime, 429 rate, and actual
    paid utilization with this decision; change price or capacity from measured
    production data, not the screening projection.

The first exact-target canary image,
`ghcr.io/infercrane/qwen38-openrouter@sha256:4fccddbde2ae93e66e83813e1b916448a82de6e530e9d2028b5d3061a7960c42`,
was rejected before traffic. Its startup script selected the base system Python
instead of the Python environment shipped by the pinned SGLang image, so model
artifact preparation could not import `huggingface_hub`. The replacement image
must pass the dependency smoke check and hosted canary before becoming a launch
digest.

Multimodal input, 262K/1M context, 32K output, prompt-cache pricing, and
multi-replica availability are follow-on qualification lanes. They are not
part of the first offer.
