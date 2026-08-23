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
independent registered boundaries but remain non-executable and deferred. The optional Dynamo DGD
adapter is locally API-qualified but not real-GPU qualified; llm-d and unregistered external
adapters remain unqualified.

## Check one exact serving combination

There is no universal “supported model” flag. Qualification is the intersection of adapter,
runtime, compute mode, runtime version, immutable model artifact, accelerator, and real evidence:

```bash
infercrane integrations --output json
infercrane models inspect MODEL_CATALOG_NAME --output json
infercrane plan MODEL_REPOSITORY --cloud PROVIDER --gpu ACCELERATOR --output json
```

- `integrations` proves which provider/runtime/mode combinations are registered and locally or
  externally qualified.
- `models inspect` exposes reviewed immutable configuration and license evidence, not GPU fit or
  performance.
- `plan` checks the requested serving intent without allocating capacity, but cannot manufacture
  real provider, model-load, or benchmark evidence.

If no release evidence names the complete combination, treat it as **unqualified** and run the
documented real-infrastructure gate. Do not combine several partial “implemented” rows into a
production-support claim.

## Reconcile a partial declarative apply

`apply` and the control API persist desired state plus a durable operation before provider mutation.
Automation must preserve both the exact desired document and its idempotency key:

```bash
infercrane plan release.yaml --output json
infercrane apply release.yaml \
  --idempotency-key release-2026-08-22 \
  --output json

infercrane operation inspect OPERATION_ID --output json
infercrane operation watch OPERATION_ID --wait-timeout 15m --output json
infercrane inspect DEPLOYMENT --output json
infercrane events DEPLOYMENT --output json
infercrane orphans --output json
```

If the API response is lost or the watcher disconnects, do not submit a new intent. Query the
operation. Retrying is safe only with the byte-equivalent desired state and original idempotency key;
a conflicting intent must fail rather than reuse the key. The worker re-observes deterministic
provider identity and adopts a matching resource before create.

For an update, the active revision remains routed while the immutable candidate converges. A failed
candidate is rejected and cleaned independently; it does not authorize in-place mutation of active
capacity. For a create/update whose provider outcome is ambiguous, compare persisted resource ID,
intent digest, ownership tags, revision, and ordinal with direct provider inventory. Exact match may
resume; proven absence may recreate from the original intent; mismatch or incomplete inventory
stops all create/delete work for manual ownership recovery.

CLI, generated SDK, Terraform, and raw API callers share this same operation contract. An
experimental client surface does not weaken server-side idempotency or qualify a provider. See
[Provisioning recovery](/features/provisioning#reconcile-an-interrupted-create) and the
[provider outage runbook](/runbooks/provider-outage).

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
make test-kubernetes-kwok # large fleet, pending GPU capacity, node loss, and cleanup
make qualify-product-nightly # coverage-guided fuzzing and repeated reliability soak
```
