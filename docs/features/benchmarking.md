# Reproducible benchmarking

InferCrane delegates load generation and measurement to [AIPerf](https://github.com/ai-dynamo/aiperf). It does not contain a second load generator.

```console
infercrane benchmark qwen-prod --requests 1000 --concurrency 32
```

The CLI submits the benchmark through the authenticated control-plane API. The control plane resolves the active immutable revision, runs AIPerf against the logical InferCrane endpoint, and persists the result. A fixed dataset seed defaults to `17` and can be changed with `--random-seed`.

Each result records the immutable ModelArtifact, runtime and configuration, revision, provider, region, GPU, compute mode, workload parameters, TTFT, TPOT, request latency, throughput, errors, timestamp, and exact reproduction command. Goodput, GPU telemetry, and cost remain unavailable rather than being fabricated when AIPerf or the provider did not measure them.

InferCrane asks AIPerf for the `records` export level. This contains per-request measurements but not the raw request/response export, so prompt and generated content are not persisted. Results remain in the InferCrane PostgreSQL database and are never uploaded by default.

The control-plane image pins its AIPerf version. For a standalone control plane, install AIPerf with `pipx install aiperf`; `infercrane doctor` verifies that the configured executable is available.
