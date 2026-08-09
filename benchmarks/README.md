# Benchmarks

The original benchmark and ThunderAgent evidence remains intact in the sibling `relay-poc`
repository. It is not imported into the InferCrane product package.

The reusable assets identified for migration are the deterministic workload generator, fake
OpenAI backend behaviors, reliability/completion gates, vLLM metric fixtures, result models,
statistics, and terminal/Markdown reporting. ThunderAgent launch code and the custom routing
implementations remain experiments rather than product dependencies.

Migration is intentionally deferred until the product request path is stable so benchmark
history and measured results are not silently rewritten. Future InferCrane benchmarks will
separate router/control-plane overhead from inference-engine performance and will report
completion rate alongside throughput and latency.

