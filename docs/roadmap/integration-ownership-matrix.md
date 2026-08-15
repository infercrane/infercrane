---
title: Integration ownership and license matrix
description: Build, integrate, or reject adjacent systems without blurring InferCrane's ownership boundary.
---

# Integration ownership and license matrix

This is an engineering decision matrix, not a promise that every candidate is supported. A
permissive license is necessary but not sufficient: each integration still needs a narrow contract,
security review, pinned version, conformance suite, and real-system qualification. License labels
below were reviewed on 2026-08-16 from the linked upstream repositories; they must be reverified at
the exact version and path before bundling or redistribution.

| Adjacent system | Recommended integration | Candidate upstreams | License reviewed | InferCrane must own | InferCrane must not own |
|---|---|---|---|---|---|
| LLM gateway | Direct API adapter plus replaceable external/managed gateway backend | [LiteLLM](https://github.com/BerriAI/litellm); [Envoy AI Gateway](https://github.com/envoyproxy/ai-gateway) | LiteLLM non-enterprise tree MIT; Envoy AI Gateway Apache-2.0 | Endpoint identity, policy, budgets, evidence, releases | Provider translation catalog or a LiteLLM fork |
| Training | Signed run/checkpoint handoff first; job adapter only after demand | [Kubeflow Trainer](https://github.com/kubeflow/trainer); [MLflow](https://github.com/mlflow/mlflow); SkyPilot jobs | Apache-2.0 | Artifact lineage, qualification, deployment handoff | Training framework, dataset plane, scheduler |
| Vector database | Observed external dependency metadata and health only when a user workflow requires it | [Qdrant](https://github.com/qdrant/qdrant); [Milvus](https://github.com/milvus-io/milvus) | Apache-2.0 | Endpoint linkage and evidence references | Vector storage, indexing, retrieval engine |
| Agent framework | SDK examples and OpenTelemetry/request identity integration | [LangGraph](https://github.com/langchain-ai/langgraph); [Google ADK](https://github.com/google/adk-python) | MIT / Apache-2.0 | Stable inference endpoint and execution evidence | Agent framework or planning loop |
| Sandbox | External `SandboxBackend` capability contract | [E2B](https://github.com/e2b-dev/E2B); [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | Apache-2.0 | Expiring endpoint access, policy, evidence references | Isolation runtime, commands/files, microVM lifecycle |
| Inference engine | Runtime Contract adapter and conformance | [Ollama](https://github.com/ollama/ollama); [llama.cpp](https://github.com/ggml-org/llama.cpp); [TensorRT-LLM](https://github.com/NVIDIA/TensorRT-LLM) after path-level audit | MIT / MIT / mixed-tree audit required | Protocol/runtime capability truth and lifecycle evidence | Engine internals, CUDA kernels, distributed KV |
| Kubernetes distribution | Qualify upstream Kubernetes APIs; do not distribute a cluster | [Kind](https://github.com/kubernetes-sigs/kind) for tests; [K3s](https://github.com/k3s-io/k3s) for user-operated edge installs | Apache-2.0 | Workload manifests and controller contract | Kubernetes distribution or cluster lifecycle |
| GPU scheduler | CRD/API integration selected by cluster owner | [Kueue](https://github.com/kubernetes-sigs/kueue); [Volcano](https://github.com/volcano-sh/volcano) | Apache-2.0 | Serving intent and observed scheduling evidence | Queue/scheduler algorithm or device allocation |
| Workflow engine | Events, webhooks, SDK steps, and bounded async inference | [Argo Workflows](https://github.com/argoproj/argo-workflows); [Temporal](https://github.com/temporalio/temporal) | Apache-2.0 / MIT | Durable inference operations and inference idempotency | Generic DAG/workflow runtime |

## Build versus integrate rule

Build only when the concern is part of InferCrane's durable inference contract and no upstream
component owns it. Integrate when the concern already has a specialist owner. Fork only when all of
the following are true:

1. the license permits the exact distribution model;
2. an adapter cannot satisfy the user outcome;
3. the divergence is small and intentionally bounded;
4. a named maintainer owns upstream security merges;
5. a removal or upstreaming plan exists.

None of the systems in the table currently meets that fork threshold.

## Qualification ladder

Every accepted adapter progresses independently through:

```text
REGISTERED → CONTRACT_TESTED → LOCAL_QUALIFIED → REAL_SYSTEM_QUALIFIED
```

Documentation and console labels must show the achieved tier, not merely that code exists.
