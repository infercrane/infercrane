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

## Qwen3.8-27B — a speculation setting confirmed on fresh H100s by the product's judge

| Field | Value |
|---|---|
| Date | 2026-11-23 |
| Model | `Qwen/Qwen3.8-27B-FP8@c3a462cc3d38cf351f3b26cbd763469eeb74ef81` |
| Runtime | vLLM 0.34.0 |
| Accelerator | 1× NVIDIA H100 SXM5 80GB per setup |
| Workload | The first 100 rows of the Mooncake trace, scaled to short chat, real text, 32 requests at once, reasoning on |
| Evidence level | Product campaign: a first window, a second window on two fresh servers, and a copy of the current setup |
| Control | The publisher's vLLM command, plus `--max-num-seqs 512` so it starts on one H100 |
| Confirmed | The publisher's documented speculation option: multi-token prediction, 3 draft tokens |

InferCrane's campaign judge measured the speculation option at +32.1% output tokens per second in
its first window and +33.1% on two fresh H100s; a copy of the current setup came out −0.1% and was
rejected as no measurable difference. Each setup had its own H100, taking turns with the current
setup's. On the fresh servers p95 time to first token regressed by 31.4%. The answer check was a
word-overlap guard on 24 short prompts, not a quality score. See the
[campaign's receipt](docs/testing/qwen38-27b-fp8-h100-campaign-2026-11-23.md).

## Qwen3-8B — FP8 on one L4, refused on answers

| Field | Value |
|---|---|
| Date | 2026-11-26 |
| Model | `Qwen/Qwen3-8B@b968826d9c46dd6066d109eabc6255188de91218` |
| Runtime | vLLM 0.34.0 |
| Accelerator | 1× NVIDIA L4 per server, three fresh servers per setup |
| Workload | Short chat at 16 and 64, long context at 4 and 16, long output at 16; a 1,000-item task set (GSM8K and MMLU) |
| Evidence level | Confirmation run in the test harness, rules committed before any server |
| Control | vLLM's defaults |
| Candidates | FP8 weights, and FP8 weights with an FP8 KV cache, both quantized at load |

FP8 weights with an FP8 KV cache served 62.1% to 158.7% more output tokens per second than the
defaults in every cell, and scored 1.10 points lower on the task set (interval −2.20 to −0.002);
FP8 weights alone were 48.2% to 110.8% faster and 1.37 points lower. By the rules a setup that is
worse on the task set is confirmed nowhere, so neither was confirmed. See the
[run's receipt](docs/testing/qwen3-8b-fp8-l4-confirmation-2026-11-26.md).

A second run checked two published FP8 checkpoints of the same model on answers only, three fresh
L4s per setup: an FP8 checkpoint that also quantizes the KV cache scored 1.33 points lower than the
original (interval −2.57 to −0.10), and the model publisher's FP8 checkpoint scored the same, 85.77%
against 85.77% (interval −1.03 to +1.03), which InferCrane's 1-point rule reads as not shown to hold
within 1 point. Its speed is not part of that run. See the
[checkpoints' receipt](docs/testing/qwen3-8b-fp8-checkpoints-l4-2026-11-26.md).

## Reading InferCrane evidence

InferCrane distinguishes modeled proposals, screening measurements, qualification evidence, and
public repeated evidence. Only comparable measurements can be ranked. Missing identity, quality,
cost, or workload evidence produces `WAIT` or `REJECT`; it is never converted into a successful
release.

See [compatibility policy](docs/compatibility.md), [benchmarking](docs/features/benchmarking.mdx),
and the [optimization evidence contract](schemas/optimization-evidence-v1.schema.json).
