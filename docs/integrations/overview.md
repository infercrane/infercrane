---
title: Integration model
description: How providers, runtimes, artifacts, routers, metrics, and benchmark tools extend InferCrane without forking its lifecycle.
---

# Integration model

InferCrane is a durable inference control plane, not a wrapper around one cloud. Its deployment,
revision, operation, Release Guard, explanation, and telemetry models do not belong to RunPod,
SkyPilot, or vLLM. External systems enter through capability-specific contracts.

```bash
infercrane integrations
```

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

## Registration is not qualification

A control-plane process registers concrete adapters during startup. Durable replicas persist the
adapter identity used to create them, so restart, rollback, and deletion return to the same
implementation without guessing from a cloud name.

The release support matrix is a separate policy. An adapter can exist in development without being
advertised publicly. Qualification requires configuration, documentation, compatibility records,
failure testing, real infrastructure acceptance, and zero leaked billable resources.

<Note>
The current adapter registry includes RunPod elastic/serverless, narrow AWS EC2, GCP Compute, and
Kubernetes elastic adapters; vLLM, SGLang, custom OCI, and governed external targets; plus LiteLLM,
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
[ADR 0033](/adr/0033-replaceable-external-composition-contracts).

Do not add a provider conditional to a generic workflow, expose registration as support, build a
second scheduler, or silently fabricate unavailable provider data. See
[ADR 0009](/adr/0009-qualified-support-and-backend-registration) for the accepted boundary.
