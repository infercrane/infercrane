---
title: v0.9.0 RC.1
description: Namespaced Kubernetes lifecycle with bounded KServe and Gateway API integration.
---

# v0.9.0 release candidate 1

v0.9 adds a Kubernetes Provider Contract adapter without introducing a custom operator, scheduler, or
data-plane router.

## Included

- deterministic Deployment/Service ownership with strict server-side apply
- optional standard KServe InferenceService ownership
- optional Gateway API route to the logical InferCrane endpoint
- namespaced least-privilege RBAC examples
- read-only `infercrane doctor --kubernetes`
- vLLM, SGLang, and custom OCI backend registration
- hermetic fault injection and disposable Kind lifecycle qualification
- native resource conditions in provider inspection evidence

## Qualification boundary

Kind validates Kubernetes API semantics without scheduling a GPU. Real Kubernetes GPU and runtime
compatibility is intentionally deferred to the consolidated v1 manual qualification. Dynamo, llm-d,
and KServe LLMInferenceService remain unsupported until routing and scheduling ownership can be made
explicit.
