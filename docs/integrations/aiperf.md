---
title: AIPerf
description: Run reproducible load generation without sending benchmark data elsewhere.
---

# AIPerf

InferCrane delegates load generation to AIPerf and persists the workload, runtime, model artifact, provider, GPU, revision, results, and exact reproduction command.

```bash
infercrane benchmark qwen-prod \
  --requests 1000 \
  --concurrency 32 \
  --runs 3 \
  --warmup-requests 8 \
  --random-seed 17
```

Use `--revision candidate` only when you intend to benchmark isolated candidate capacity. Benchmark data remains in the InferCrane control plane; it is not uploaded by default.

InferCrane pins AIPerf `0.12.0`, requests record-only exports, and can drive constant, Poisson, or
gamma arrival timing. From each export it reads every request's latencies, error, and server-counted
output tokens. Failed requests are counted and left out of the latency percentiles, and the mean output
tokens of a successful request is recorded only when every successful request reported a count. Independent runs produce 95% confidence intervals in persisted workload
evidence; they are not flattened into a single unexplained number.

AIPerf's server-metrics scrape and GPU telemetry are off for ordinary benchmarks. Optimization
campaigns turn the server-metrics scrape on for a candidate whose runtime contract declares a
`/metrics` path and keep a bounded summary of its cache, preemption, queue, and speculative-acceptance
metrics. A built-in vLLM or SGLang candidate with no workload of its own is read with its runtime's
default contract, the one it is provisioned with, so its metrics are scraped on every provider. GPU telemetry stays off because benchmarks have no DCGM exporter; it is recorded as
`not_collected`.

<Card title="Benchmarking" icon="gauge" href="/features/benchmarking">
  Understand measurements, persisted reproduction metadata, and evidence limits.
</Card>
