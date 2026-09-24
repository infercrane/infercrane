# Qwen3.8 OpenRouter launch decision

Date: 2026-09-23

## Decision

The exact-target canary passed. Proceed to supplier qualification and the
production soak with this bounded offer:

- model: `Qwen/Qwen3.8-27B-FP8@017b9c7af6b5689d5dd426a76e0bc077eb5ca20a`;
- runtime: SGLang 0.5.20;
- target: one H200 for the first public canary; B200 is a separately measured
  latency/capacity tier and H100 is retained only as API-canary evidence;
- recipe: FP8 weights and KV cache, bounded CUDA graphs, NEXTN/MTP with three
  speculative steps and four draft tokens, and the runtime-selected DeepGEMM
  FP8 path;
- admission: adaptive four-to-twelve decode requests, starting at eight, with
  a three-second p95 TTFT objective and a separately bounded prefill-token
  budget; reject immediately with HTTP 429 above either boundary;
- public contract: text input, 32,768 input tokens, 2,048 output tokens, tools,
  structured output, streaming, and usage accounting; and
- base price: $0.10/M input tokens and $2.20/M output tokens, with a 19%
  launch discount that displays $0.081/M input and $1.782/M output until
  production utilization and contribution margin justify reducing it.

Keep `is_ready: false` until the exact deployment passes the external provider
qualifier, a 24-hour soak, usage reconciliation, overload/recovery tests, and
an OpenRouter test canary. Modal results are screening evidence, not a hosted
provider claim.

## Exact H100 provider canary

An externally reachable RunPod Secure H100 SXM canary passed the provider
qualifier on 2026-09-23 using the immutable image
`ghcr.io/infercrane/qwen38-openrouter@sha256:ff70583b16ce9d2bddbf7d258be3146a0fd2dcd3a6c1a700d3ba9cc238e32f7c`.
The model artifact was the pinned revision above, copied from the verified
persistent artifact volume to node-local storage before SGLang startup.

| External measurement | Result |
|---|---:|
| Stream TTFT | 166 ms |
| Bounded-load latency p50 | 644 ms |
| Bounded-load latency p95 | 829 ms |
| Load requests | 64/64 HTTP 200 |
| Server or unexpected errors | 0 |

All twelve gates passed: model discovery, buffered usage, streaming usage and
finish semantics, structured output, forced tools, reasoning output, unknown
model rejection, malformed request rejection, bounded load, and post-load
recovery. The receipt is
`docs/testing/evidence/qwen38-openrouter-runpod-h100-2026-09-23.json`.

This qualifies API compatibility and a bounded H100 load shape. It is not a
production or OpenRouter leaderboard result. A 24-hour soak, the long-context
boundary, durable usage reconciliation, provider onboarding, and OpenRouter's
own network measurements remain open.

Cold startup exposed two release issues. SGLang spent approximately 200
seconds compiling and capturing the first prefill graph, so the exact compiled
kernel and graph cache must be prepared before production traffic. The RunPod
network volume also did not preserve the owner-only mode required by the
content-free receipt recorder; the canary used an owner-only local receipt
path. Production needs a durable encrypted receipt collector or a filesystem
that preserves the fail-closed POSIX permission contract.

## Hardware and cache decision

Exact-hardware screens on the same pinned model, SGLang image, recipe, workload,
and harness changed the preferred launch accelerator. The H200 campaign was a
paired control/selected qualification. H100 and B200 were single-candidate
hardware screens: their API, semantic, long-state, token-identity, and load
gates passed, but they deliberately remain unpromoted because runtime-parity
was not measured in those runs.

| GPU | c12 output tok/s | c12 p95 TTFT | c12 GPU COGS / M output | c16 output tok/s | c16 projected margin |
|---|---:|---:|---:|---:|---:|
| H100 80 GB SXM | 852.7 | 6,092 ms | $1.287 | 1,054.4 | 30.2% |
| H200 SXM | 1,304.7 | 2,494 ms | $0.967 | 1,575.2 | 44.0% |
| B200 | 1,803.5 | 1,463 ms | $0.963 | 2,033.0 | 40.9% |

H200 produced 53.0% more output throughput than H100 at concurrency twelve
and cost 24.9% less per measured million output-equivalent tokens. B200 was
38.2% faster than H200 and had the better TTFT, but H200 retained 6.3% lower
cost per output-equivalent token at concurrency sixteen. H200 is therefore the
default economic tier; B200 is the performance tier. These Modal costs use the
public $3.9492, $4.5396, and $6.2496 hourly rates and are not a substitute for
the final hosting invoice.

The compile-volume warm run reduced H200 startup from 336.2 to 237.1 seconds,
a 29.5% improvement. The immutable release cache covers Triton, DeepGEMM,
FlashInfer, Inductor, CUDA, TileLang, TVM FFI, and CuTe artifacts. CUDA graph
capture remains process-local and accounts for much of the remaining startup.

Evidence:

- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T111756Z.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T114709Z.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T115711Z.json`

## GDN, prefix cache, and context boundary

SGLang 0.5.20 ships FlashInfer 0.6.18. The newer FlashInfer 0.7.0 was released
on 2026-09-22 but is not the dependency pinned by this SGLang release, so it is
not silently substituted into the production tuple.

On the exact H200 saturation screen, the current Triton GDN route remained the
winner. Full FlashInfer GDN reached 1,483.4 tok/s at concurrency sixteen, 5.8%
below the selected 1,575.2 tok/s. FlashInfer prefill with Triton decode reached
1,386.2 tok/s, 12.0% below selected. Both preserved all correctness gates, so
they are clean performance rejections. They also extended cold readiness from
325–336 seconds to 410–446 seconds.

The source image contained three known recurrent-state precision narrowings.
The provider build now applies the exact reviewed upstream expressions only
when all original SGLang 0.5.20 source hashes match, writes a patch receipt, and
fails closed otherwise. The long-state semantic sentinel and prompt-token
identity checks passed on H100, H200, and B200 screens.

For the 95K-token agent-session workload, cold/warm deterministic outputs were
byte-identical and the warm request reused 95,808 tokens at a 99.95% cache-hit
rate. LPM scheduling improved concurrency-one throughput by 2.7% and was tied
at concurrency four, which is insufficient to replace the simpler selected
recipe.

At the near-262K boundary, 12/12 requests completed with zero prompt-token
mismatches and 5.7 ms p95 ITL, but p95 TTFT was 30.15 seconds against the
pre-registered 20-second boundary. The launch contract therefore stays at 32K;
262K is supported by the runtime but is not launch-qualified.

Evidence:

- `docs/testing/evidence/qwen38-agent-prefix-reuse-modal-2026-09-23T113633Z.json`
- `docs/testing/evidence/qwen38-public-context-boundary-262k-modal-2026-09-23T113709Z.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T111756Z.json`

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

At concurrency twelve, the measured Modal H200 GPU cost was $0.967 per million
successful output tokens. For the measured 2.847:1 input/output token ratio,
the proposed price produces $2.485 of revenue per million output-equivalent
tokens.

Using the target RunPod H200 price of $4.59/hour and reserving 10% of revenue
for gateway, storage, monitoring, recovery, and other non-GPU costs:

- contribution break-even requires approximately 43.2% utilization; and
- a 35% contribution margin requires approximately 70.7% utilization.

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

The recurrent GDN family accounted for 14.66% of device time in the saturated
concurrency-sixteen trace, so it cleared the gate for a bounded custom search.
The exact H200 lab tested eight tile and warp configurations at Qwen3.8's real
16 key-head, 48 value-head, 128-dimension geometry across packed concurrency
one through sixteen and four-token MTP verification. The pinned upstream
BV32/one-warp kernel remained the winner. The closest parity-preserving
candidate was 0.43% slower; larger tiles were 26% to 89% slower, and multi-warp
variants changed reduction results without improving speed. No custom GDN
kernel is promoted.

Backend evidence:

- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045002Z-failures.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045916Z.json`
- `docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T045916Z-failures.json`
- `docs/testing/evidence/qwen38-gdn-kernel-h200-2026-09-23T181527Z.json`

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

The first rejected exact-target canary image,
`ghcr.io/infercrane/qwen38-openrouter@sha256:4fccddbde2ae93e66e83813e1b916448a82de6e530e9d2028b5d3061a7960c42`,
was rejected before traffic. Its startup script selected the base system Python
instead of the Python environment shipped by the pinned SGLang image, so model
artifact preparation could not import `huggingface_hub`. The replacement image
passed the dependency smoke check and exact hosted canary recorded above.

Multimodal input, 262K/1M context, 32K output, prompt-cache pricing, and
multi-replica availability are follow-on qualification lanes. They are not
part of the first offer.
