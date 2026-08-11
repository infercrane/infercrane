# Product strategy

InferCrane is the portable release and operations control plane for production inference.

Its promise is **production inference without the platform engineering**. The primary experience
remains `infercrane deploy <model>` while the control plane preserves durable state, safe revisions,
provider/runtime portability, evidence, and deterministic explanations.

## Product wedge

The first users are small AI platform teams that need production vLLM without first operating a
Kubernetes platform. They value a stable OpenAI-compatible endpoint, safe updates, autoscaling,
clear failure states, predictable cleanup, and infrastructure they control.

Enterprise expansion comes through bring-your-own-cloud, private networking, scoped identity,
auditability, automation interfaces, and qualified runtime/provider integrations.

## What InferCrane owns

- DeploymentSpec and logical deployments
- desired and observed state, durable operations, revisions, rollout and rollback
- Release Guard and persisted operational explanations
- provider/runtime capability contracts and qualification evidence
- normalized telemetry, benchmarks, cold-start evidence, and recommendations
- CLI, API, SDK, Terraform, dashboard, and agent-facing developer experience

## What InferCrane reuses

Inference engines, provider provisioning APIs, provider-native serverless, container runtimes,
Kubernetes serving projects, AIPerf, Hugging Face transfer, and OpenTelemetry remain independently
owned systems. InferCrane integrates them through explicit adapters.

InferCrane does not become an inference engine, distributed KV system, general scheduler, second
general-purpose router, container engine, Kubernetes distribution, workflow engine, agent
framework, or model API billing marketplace.

## Compounding advantage

Every qualified deployment can add evidence about the relationship between immutable artifacts,
runtimes, configuration, hardware, providers, workload, cost, cold start, reliability, and release
outcomes. Recommendations must expose provenance and confidence; missing evidence remains unknown.

The long-term moat is this portable decision and evidence layer, not ownership of every runtime or
GPU.

## Product principles

1. One concern has one owner.
2. Registration is not qualification.
3. Durable intent survives clients and process restarts.
4. External traffic and cost changes require explicit policy.
5. Measurements identify their source; estimates are never presented as facts.
6. The simple path hides infrastructure jargon while inspection preserves provider-native detail.
7. Documentation and release evidence ship with behavior.

