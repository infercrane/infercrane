# InferCrane benchmark index

Every result here is bound to an exact model, runtime, accelerator, workload, and date. A measured
win is not generalized to another provider, GPU, context length, concurrency level, or production
network path.

## Qwen3.8-27B — September H200 screen, superseded

| Field | Value |
|---|---|
| Date | 2026-09-23 |
| Model | `Qwen/Qwen3.8-27B-FP8@017b9c7af6b5689d5dd426a76e0bc077eb5ca20a` |
| Runtime | SGLang 0.5.20 |
| Accelerator | 1× NVIDIA H200 |
| Workload | 4,096 input tokens, 512 output tokens, concurrency 12 |
| Evidence level | Screening: one run, 24 requests at concurrency 12 |
| Control | Same model, image, workload, hardware, output checks, FP8 weights, and FP8 KV cache, without speculation |
| Selected | Control plus bounded CUDA graphs and native NEXTN/MTP |

This screen measured 1.72× output throughput at concurrency 12 from turning on native multi-token
prediction (with bounded CUDA graphs), the speculation option the model's publisher documents. Its
prompts were synthetic: one sentence repeated to the target length (`synthetic_prompt_content` in
`tools/modal-openrouter-qwen38/campaign_support.py`). Repeated text is unusually easy for
speculation to predict.

On 2026-11-23 the same pair was run again on real text (OpenAssistant and CNN/DailyMail prompts)
with three fresh servers per setup. At 12 requests at once the same change gave +12% on short
prompts and +33% on 3,900-token prompts; across the six load and prompt-length cells it ranged from
−22% to +68%. The publisher's documented setup on vLLM 0.34.0 was faster than the setup selected
here in every cell, by 5% to 113%. The screen's throughput and latency changes are therefore not
cited. See the [re-run's receipt](docs/testing/qwen38-27b-fp8-h200-rerun-2026-11-23.md).

September's receipts stay as a dated record:

- [Machine-readable receipt](docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json)
- [Raw measurements](docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z-raw.json)

What the screen still records:

- The result is a controlled Modal H200 screen, not an OpenRouter or public-Internet result.
- The launch context boundary stayed at 32K because the near-262K run missed its pre-registered
  p95 TTFT limit even though all requests completed correctly.
- FlashInfer GDN candidates preserved correctness but lost throughput and cold-start time on this
  tuple, so they were rejected.
- DFlash2 won some lower-concurrency screening lanes but did not replace native MTP at the launch
  saturation boundary.
- H100 and B200 screens remain separate hardware evidence; they do not inherit this result.

A change InferCrane reports now is measured on the customer's traffic against their current setup,
confirmed again on fresh servers, and, when it changes answers, checked on a scored task set: see
[optimization](docs/features/optimization.mdx) and [semantic quality](docs/features/semantic-quality.mdx).

## Reading InferCrane evidence

InferCrane distinguishes modeled proposals, screening measurements, qualification evidence, and
public repeated evidence. Only comparable measurements can be ranked. Missing identity, quality,
cost, or workload evidence produces `WAIT` or `REJECT`; it is never converted into a successful
release.

See [compatibility policy](docs/compatibility.md), [benchmarking](docs/features/benchmarking.mdx),
and the [optimization evidence contract](schemas/optimization-evidence-v1.schema.json).
