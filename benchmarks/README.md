# Benchmarks

InferCrane qualifies serving plans with measured workload evidence. The benchmark path uses
[AIPerf](https://github.com/ai-dynamo/aiperf) and records the exact model artifact, runtime,
runtime configuration, accelerator, provider, region, workload shape, and evidence timestamp.

Run a benchmark through the CLI:

```bash
infercrane benchmark DEPLOYMENT --profile interactive
```

Run the maintained qualification matrix only when the required infrastructure and spend are
explicitly approved:

```bash
scripts/benchmark-matrix.sh DEPLOYMENT --approve-load
```

Local benchmark output belongs under the ignored `.infercrane/performance/` directory. Reports
must not contain raw prompts, model outputs, credentials, or provider secrets.

Results are comparable only when the full workload tuple matches. InferCrane reports completion
rate, queue latency, time to first token, inter-token latency, output throughput, errors, and
sourced cost when those signals are available. Modeled results are never presented as measured
or qualified evidence.

See [product qualification](../docs/testing/product-qualification.mdx) and
[Release Guard](../docs/features/release-guard.md) for the evidence and promotion contracts.
