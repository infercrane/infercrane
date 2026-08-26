---
title: Integration model
description: How providers, runtimes, artifacts, routers, metrics, and benchmark tools extend InferCrane without forking its lifecycle.
---

# Integration model

InferCrane is a portable production inference platform, not a wrapper around one cloud. Its durable
control plane owns deployment, revision, operation, Release Guard, explanation, and telemetry
semantics. Those semantics do not belong to RunPod, SkyPilot, or vLLM. External systems enter
through capability-specific contracts.

```bash
infercrane integrations
```

Before selecting an adapter, run the [exact-combination compatibility check](/compatibility#check-one-exact-serving-combination):

```bash
infercrane integrations --output json
infercrane models inspect MODEL_CATALOG_NAME --output json
infercrane plan MODEL_REPOSITORY --cloud PROVIDER --gpu ACCELERATOR --output json
```

All three commands are read-only. If the release evidence does not name the complete provider,
runtime, compute mode, immutable model, accelerator, and environment combination, treat it as
unqualified before committing money or a migration plan.

## Keep sensitive inputs on self-hosted infrastructure

Use a serving plan that contains only a self-hosted deployment or a directly adopted self-hosted
target. Do not add an external provider connection or an external fallback binding.

For a new InferCrane-managed deployment, qualify the exact combination before creating billable
capacity. `plan` is read-only; `deploy` is not:

```bash
infercrane integrations --output json
infercrane models inspect MODEL_CATALOG_NAME --output json
infercrane plan MODEL_REPOSITORY \
  --cloud PROVIDER \
  --gpu ACCELERATOR \
  --output json
```

Stop if those results do not identify the intended adapter, runtime, compute mode, immutable model,
accelerator, environment, and required real-system evidence. Otherwise deploy and wait for the
durable operation to reach readiness before binding it:

```bash
infercrane deploy MODEL_REPOSITORY \
  --name private-coder-v1 \
  --cloud PROVIDER \
  --gpu ACCELERATOR

infercrane status private-coder-v1 --watch

infercrane logical-model create private-coder
infercrane endpoint create private-coder-production \
  --model private-coder \
  --environment production

infercrane endpoint bind private-coder-production \
  --name self-hosted \
  --deployment private-coder-v1 \
  --ownership lifecycle-managed

infercrane endpoint plan private-coder-production \
  --policy manual \
  --bindings self-hosted

infercrane endpoint inspect private-coder-production
```

For a vLLM or SGLang endpoint you already operate, begin read-only and transfer traffic ownership
only after inspection:

```bash
infercrane connect https://vllm.internal.example/v1 \
  --as private-coder-production \
  --type vllm \
  --model company/private-coder

infercrane observe private-coder-production
infercrane doctor private-coder-production
infercrane adopt promote private-coder-production --ownership traffic-managed
```

The first plan is a single manual binding, so there is no fallback destination that can receive
request content. Prompt and output bodies are not recorded by default; verify network controls,
runtime logging, and the adopted service itself according to your security policy.

<Warning>
Do not use a LiteLLM route with external upstreams for a strict self-hosted-only requirement.
InferCrane can govern the connection to a user-managed LiteLLM gateway, but LiteLLM owns routing
inside that gateway. InferCrane cannot prove that its internal routes keep data local. An endpoint
binding that uses `--acknowledge-external-data --enable-external` explicitly permits request data to
leave controlled infrastructure; request and cost limits bound usage, not data residency.
</Warning>

See [Stable endpoints and serving plans](/features/endpoints) for the binding model and
[LiteLLM gateway](/integrations/litellm) for the responsibility boundary.

This authenticated read returns the compiled Provider Contract, Runtime Contract, and Composition
Contract versions, registered adapters, capabilities, ownership boundaries, and separate local
versus real-system qualification states. It never upgrades registration or hermetic simulation into
a public support claim.

| Concern | InferCrane owns | Adapter owns |
|---|---|---|
| Elastic infrastructure | Replica intent, retries, drain and deletion ordering | Ensure, observe, delete and inventory |
| Serverless infrastructure | Logical endpoint, durable operation and evidence | Endpoint lifecycle, workers and scale-to-zero |
| Inference runtime | Expected model identity and normalized health | Model execution and runtime-specific inspection |
| Replica routing | Desired membership and generation cutover | Request distribution among standalone replicas |
| Model artifacts | Immutable `ModelArtifact` identity and attachment | Repository resolution and transfer |
| Benchmarking | Reproduction record and history | Workload generation and raw measurements |
| Metrics | Normalized dimensions and persisted evidence | Runtime/provider signal extraction |
| Gateway composition | Stable endpoint, adoption state, policy, and evidence | Protocol translation, upstream credentials, and gateway lifecycle |
| External sandbox | External reference and endpoint-scoped access | Isolation, commands, files, network policy, and sandbox lifecycle |
| Training handoff | Signature verification, immutable artifact lineage, and revision binding | Training data, execution, checkpoints, and scheduler |
| Agent applications | Stable OpenAI-compatible endpoint, request evidence, and optional logical session identity | Agent graph, tools, memory, and business logic |
| Vector database / RAG | Inference request behavior after retrieval | Documents, embeddings, indexes, retrieval policy, and data lifecycle |
| Workflow orchestration | Idempotent async inference, cancellation, encrypted result retention, and signed completion webhook | DAG, retries between business steps, schedules, and non-inference work |
| Kubernetes GPU scheduling | Desired inference workload and observed serving evidence | Queue admission, placement, preemption, and node scheduling |

## Registration is not qualification

A control-plane process registers concrete adapters during startup. Durable replicas persist the
adapter identity used to create them, so restart, rollback, and deletion return to the same
implementation without guessing from a cloud name.

The release support matrix is a separate policy. An adapter can exist in development without being
advertised publicly. Qualification requires configuration, documentation, compatibility records,
failure testing, real infrastructure acceptance, and zero leaked billable resources.

<Note>
The current adapter registry includes RunPod elastic/serverless, narrow AWS EC2, GCP Compute, and
Kubernetes elastic and optional Dynamo serving-graph adapters; vLLM, SGLang, custom OCI, and governed external targets; plus LiteLLM,
external-sandbox access, and signed-artifact-handoff composition profiles. The compatibility matrix
qualifies only exact combinations; registration never implies real-system evidence.
</Note>

## Adding an integration

1. Implement only the narrow capability contract the external system owns.
2. Give the adapter a stable durable identity and register it at process composition.
3. Map trustworthy observations into InferCrane's normalized state and telemetry.
4. Add configuration, diagnostics, documentation, and deterministic failure behavior.
5. Qualify the exact cloud/runtime/compute-mode combination with real lifecycle evidence.

The versioned contract details are documented in [Provider Contract V1](/architecture/provider-contract),
[Runtime Contract V1](/architecture/runtime-contract), and
[Integration ownership](/integrations/ownership).

See [Ecosystem compatibility](/integrations/ecosystem) for the exact user path and maturity of
model APIs, gateways, agents, vector databases, workflow engines, training systems, sandboxes,
runtimes, Kubernetes, and GPU schedulers.

Do not add a provider conditional to a generic workflow, expose registration as support, build a
second scheduler, or silently fabricate unavailable provider data. Registration and qualification
remain separate states throughout the API, CLI, console, and documentation.
