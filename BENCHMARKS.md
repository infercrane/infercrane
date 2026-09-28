# InferCrane benchmark index

Every result here is bound to an exact model, runtime, accelerator, workload, and date. A measured
win is not generalized to another provider, GPU, context length, concurrency level, or production
network path.

## Qwen3.8-27B — paired H200 serving qualification

| Field | Value |
|---|---|
| Date | 2026-09-23 |
| Model | `Qwen/Qwen3.8-27B-FP8@017b9c7af6b5689d5dd426a76e0bc077eb5ca20a` |
| Runtime | SGLang 0.5.20 |
| Accelerator | 1× NVIDIA H200 |
| Workload | 4,096 input tokens, 512 output tokens, concurrency 12 |
| Control | Same model, image, workload, hardware, and output checks without the selected serving recipe |
| Selected | FP8 weights and KV cache, bounded CUDA graphs, native NEXTN/MTP |

| Measurement | Control | Selected | Change |
|---|---:|---:|---:|
| Aggregate output throughput | 810.4 tok/s | 1,393.6 tok/s | 1.72× |
| Per-request p50 output speed | 76.1 tok/s | 141.0 tok/s | 1.85× |
| p50 TTFT | 1,591 ms | 739 ms | 53.6% lower |
| p95 inter-token latency | 14.02 ms | 8.67 ms | 38.1% lower |
| Successful requests | 24/24 | 24/24 | equal |

The deterministic outputs matched. Streaming and buffered usage, finish reasons, structured output,
forced tools, malformed-request handling, and prompt-token accounting passed.

- [Human decision record](docs/testing/qwen38-openrouter-launch-decision-2026-09-23.md)
- [Machine-readable receipt](docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json)
- [Raw measurements](docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z-raw.json)

### Important limits and rejected candidates

- The result is a controlled Modal H200 qualification, not an OpenRouter or public-Internet result.
- The launch context boundary stayed at 32K because the near-262K run missed its pre-registered
  p95 TTFT limit even though all requests completed correctly.
- FlashInfer GDN candidates preserved correctness but lost throughput and cold-start time on this
  tuple, so they were rejected.
- DFlash2 won some lower-concurrency screening lanes but did not replace native MTP at the launch
  saturation boundary.
- H100 and B200 screens remain separate hardware evidence; they do not inherit this paired result.

## Reading InferCrane evidence

InferCrane distinguishes modeled proposals, screening measurements, qualification evidence, and
public repeated evidence. Only comparable measurements can be ranked. Missing identity, quality,
cost, or workload evidence produces `WAIT` or `REJECT`; it is never converted into a successful
release.

See [compatibility policy](docs/compatibility.md), [benchmarking](docs/features/benchmarking.mdx),
and the [optimization evidence contract](schemas/optimization-evidence-v1.schema.json).
