---
title: Reproducible benchmarking
description: Run AIPerf workloads and persist exact model, runtime, hardware, and workload evidence.
---

# Reproducible benchmarking

InferCrane delegates load generation and measurement to [AIPerf](https://github.com/ai-dynamo/aiperf). It does not contain a second load generator.

```console
infercrane benchmark qwen-prod --requests 1000 --concurrency 32
```

Use a versioned workload profile when you want comparable evidence for a specific operating goal:

```console
infercrane benchmark mistral-prod --profile interactive \
  --ttft-slo-ms 250 --tpot-slo-ms 25

infercrane benchmark mistral-prod --profile throughput
infercrane benchmark mistral-prod --profile long-context
infercrane benchmark mistral-prod --profile buffered
```

Profiles define load shape, not runtime tuning claims. `interactive`, `balanced`, `throughput`,
`buffered`, `long-context`, `long-generation`, and `overload` record their immutable
`benchmark-profile-v1` identity inside the workload evidence. Explicit token or concurrency flags
override profile defaults and remain visible in the result.

For a bounded qualification sweep across concurrency 1, 8, 32, and 128 plus long prompt,
long-generation, streaming, buffered, and overload shapes:

```console
scripts/benchmark-matrix.sh mistral-prod --approve-load
```

The script refuses to send load without explicit approval, archives each control-plane result,
validates failure bounds, and produces `.infercrane/performance/RUN_ID/matrix.json`. Set
`INFERCRANE_BENCHMARK_TTFT_SLO_MS`, `INFERCRANE_BENCHMARK_TPOT_SLO_MS`, and/or
`INFERCRANE_BENCHMARK_LATENCY_SLO_MS` to compute measured goodput: successful requests per second
that satisfy every declared threshold. A zero threshold means no SLO was declared and goodput stays
unavailable rather than being invented.

The profile matrix deliberately changes workload shape by objective, so its rows are not a
scaling curve. Use the separate same-workload campaign when comparing concurrency behavior:

```console
scripts/benchmark-concurrency-sweep.sh mistral-prod --approve-load
```

It holds request count, input tokens, output tokens, seed, streaming mode, revision, and SLOs
constant while running concurrency `1`, `8`, `32`, and `128`. The resulting
`.infercrane/performance/RUN_ID-concurrency-sweep/sweep.json` is the only one of these two artifacts
that supports a concurrency scaling comparison. Override the bounded defaults with
`INFERCRANE_BENCHMARK_SWEEP_*` variables; every override remains in persisted workload evidence.

The CLI submits the benchmark through the authenticated control-plane API. The control plane resolves the active immutable revision, runs AIPerf against the logical InferCrane endpoint, and persists the result. A fixed dataset seed defaults to `17` and can be changed with `--random-seed`.

Release Guard validation can benchmark an isolated candidate explicitly:

```console
infercrane benchmark qwen-prod --revision active --requests 100 --concurrency 4 --random-seed 17
infercrane benchmark qwen-prod --revision candidate --requests 100 --concurrency 4 --random-seed 17
```

The candidate command selects one healthy ready replica deterministically and runs AIPerf directly
from the control plane using its server-side worker credential. It never exposes that credential
to the CLI. This is explicit synthetic validation, not shadow traffic: InferCrane does not copy any
user request or conversation content. It creates additional inference work and may incur provider
cost. Active and candidate results are eligible for Release Guard comparison only when AIPerf tool
version and workload parameters match exactly.

Each result records the immutable ModelArtifact, runtime and configuration, revision, provider,
region, GPU type and grounded GPU count, compute mode, workload parameters, TTFT, TPOT, request
latency, throughput, SLO goodput when declared, errors, timestamp, and exact reproduction command. The persisted command uses
a portable local export prefix rather than the deleted temporary execution directory, and replaces
the credential with its environment-variable reference. GPU utilization is attached only when
measured DCGM evidence for the exact immutable revision overlaps the complete benchmark window;
provider-reported or stale samples remain `null`. Goodput remains `null` without SLO thresholds.
Cost metadata becomes available only when fresh, revision-bound `deployment_hourly_rate` evidence
covers the run; otherwise it contains `available: false` and a reason. None are fabricated.

## What “fast” means

InferCrane does not promise one globally fastest configuration. It measures the exact combination
and lets the operator choose the objective:

| Objective | Primary evidence |
| --- | --- |
| Interactive latency | TTFT and TPOT percentiles under the declared concurrency |
| Throughput | Output tokens/second and requests/second with bounded errors |
| Cost efficiency | Measured tokens per sourced currency unit; unavailable until cost evidence exists |
| Long context | TTFT, total latency, memory safety, and errors at the declared prompt shape |
| Fast startup | Time-to-ready waterfall and verified image/artifact cache state |
| Reliability | Goodput under explicit SLOs, rollout evidence, and active-stream safety |

Runtime arguments are immutable revision data. Tune a candidate, run the same profile against active
and candidate revisions, and let Release Guard compare matching evidence before promotion. This is
how InferCrane can provide a fast qualified default without turning an upstream vLLM or SGLang flag
into an unsupported universal claim.

For reviewed generation models, scaffold two candidate projects and preserve their different runtime
arguments explicitly:

```console
infercrane workload init ./mistral-interactive \
  --recipe mistral-7b-instruct --profile vllm-interactive
infercrane workload init ./mistral-throughput \
  --recipe mistral-7b-instruct --profile vllm-throughput
```

Deploy them as isolated candidates, benchmark both with the *same* workload profile, then use Lab or
Release Guard. Serving-profile names describe runtime configuration; benchmark-profile names describe
load. Neither is evidence until the resulting measurements are persisted.

Compare multiple measured configurations with the same workload:

```console
infercrane lab 'mistralai/Mistral-7B-Instruct-v0.3@IMMUTABLE_COMMIT' \
  --objective interactive \
  --profile interactive \
  --max-ttft-p95-ms 200ms \
  --max-hourly-cost 3

infercrane lab 'mistralai/Mistral-7B-Instruct-v0.3@IMMUTABLE_COMMIT' \
  --objective cost-efficiency \
  --profile throughput
```

Lab emits `RECOMMENDED` only when candidates share one exact workload digest and the required
metric - and sourced hourly cost for cost efficiency - is present. Unlike workload shapes remain
`UNRANKED`.

Maintainers can qualify one pinned AWS model family at a time without rerunning unrelated runtime
paths:

```console
scripts/aws-performance-qualification.sh mistral --approve-paid-resources
scripts/aws-performance-qualification.sh deepseek --approve-paid-resources
scripts/aws-performance-qualification.sh granite --approve-paid-resources
```

Each run uses the same seven-profile matrix, the fixed-workload concurrency sweep, and an independent
zero-resource inventory. These are paid,
commit-bound qualification commands, not normal developer tests and not permission to publish a
cross-model performance claim.

InferCrane normalizes the unit carried by each AIPerf latency record to milliseconds and rejects
unknown units instead of silently mislabeling evidence. Percentiles reconstructed from those records
use the nearest-rank definition. Small samples remain visibly weak evidence: Release Guard separately
requires the persisted minimum request count, and matching workload/tool identity, before a comparison
can pass.

InferCrane asks AIPerf for the `records` export level. This contains per-request measurements but not the raw request/response export, so prompt and generated content are not persisted. Results remain in the InferCrane PostgreSQL database and are never uploaded by default.

The control-plane image pins its AIPerf version. For a standalone control plane, install AIPerf with `pipx install aiperf`; `infercrane doctor` verifies that the configured executable is available.
