---
title: Compatibility and qualification
description: How InferCrane separates registered adapters, local conformance, and real-infrastructure support evidence.
---

# Compatibility and qualification policy

InferCrane follows semantic versioning after `1.0`. The complete API, DeploymentSpec, database,
integration-contract, deprecation, mixed-version, and support-window rules are in
[Upgrade and compatibility](/upgrade). PostgreSQL migrations are forward-only and must be backed up
before rollout. Mixed-version operation is unsupported unless a release explicitly says otherwise.

## Compatibility dimensions

Every release candidate must record the tested versions of Go, PostgreSQL, vLLM Router, vLLM,
Python, container runtime, GPU driver, CUDA, model, and infrastructure provider. Absence from that
matrix means unqualified, not incompatible.

`infercrane integrations --output json` is the executable capability inventory. RunPod
elastic/serverless, AWS EC2 elastic, GCP Compute elastic, and Kubernetes elastic are distinct provider profiles;
OpenRouter is a governed external target profile. Local conformance and real-provider qualification
are separate fields. AWS ASG/EKS/SageMaker/Bedrock, GCP MIG/GKE/Vertex, and CoreWeave CKS have
independent registered boundaries but remain non-executable and deferred. Advanced KServe/llm-d/
Dynamo topologies and unregistered external adapters remain unqualified.

The OpenAI-compatible surface is contract-tested for model listing and chat completions. New API
fields should pass through unless InferCrane must interpret them. Removing or changing an accepted
field requires a deprecation period after `1.0`.

Endpoint admission applies uniformly across qualified protocols. Buffered requests to managed
capacity may use a bounded retry budget; streaming and external paid routes are never replayed.
Durable async execution supports Chat Completions, Responses, Embeddings, Completions and bounded
chat batch only when the selected runtime declares the corresponding protocol capability. Async
does not make an unsupported runtime protocol compatible.

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
make test-kubernetes-kind # Kubernetes ownership and recovery without a GPU
```
