# Compatibility and qualification policy

InferCrane follows semantic versioning after `1.0`. Before `1.0`, release notes identify breaking
CLI, configuration, schema, and API changes. PostgreSQL migrations are forward-only and must be
backed up before rollout. Mixed-version operation is unsupported unless a release explicitly says
otherwise.

## Compatibility dimensions

Every release candidate must record the tested versions of Go, PostgreSQL, vLLM Router, vLLM,
Python, container runtime, GPU driver, CUDA, model, and infrastructure provider. Absence from that
matrix means unqualified, not incompatible.

The OpenAI-compatible surface is contract-tested for model listing and chat completions. New API
fields should pass through unless InferCrane must interpret them. Removing or changing an accepted
field requires a deprecation period after `1.0`.

## Release qualification gates

1. Unit and PostgreSQL integration tests pass with the race detector and `go vet`.
2. The Docker stack smoke test passes, including planning, diagnostics, routing, metrics, and load.
3. Upgrade and backup/restore drills pass against a copy of production-like data.
4. Worker loss, router failure, PostgreSQL failover, cancellation, and shutdown are exercised.
5. A sustained real-vLLM GPU benchmark records throughput, p50/p95 latency, errors, and versions.

Local fake workers validate control flow only. They cannot satisfy gates 3–5 or support performance
claims.

Repository commands:

```bash
make test-container  # race tests, vet, and real PostgreSQL integration
make test-stack      # full Compose request and CLI smoke path
make test-failure    # worker loss and control-plane restart recovery
```
