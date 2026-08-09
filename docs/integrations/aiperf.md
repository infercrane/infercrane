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
  --random-seed 17
```

Use `--revision candidate` only when you intend to benchmark isolated candidate capacity. Benchmark data remains in the InferCrane control plane; it is not uploaded by default.

<Card title="Benchmarking" icon="gauge" href="/features/benchmarking">
  Understand measurements, persisted reproduction metadata, and evidence limits.
</Card>
