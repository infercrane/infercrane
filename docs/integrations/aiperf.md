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
gamma arrival timing. Independent runs produce 95% confidence intervals in persisted workload
evidence; they are not flattened into a single unexplained number.

<Card title="Benchmarking" icon="gauge" href="/features/benchmarking">
  Understand measurements, persisted reproduction metadata, and evidence limits.
</Card>
