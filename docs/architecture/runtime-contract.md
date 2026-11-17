---
title: Runtime contract
description: The capability, readiness, protocol, cancellation, and evidence contract for inference runtimes.
---

# Runtime Contract V1

Status: implemented. vLLM is locally qualified; SGLang and declarative custom OCI
workloads exercise the contract through hermetic simulation. Real-GPU evidence remains deferred
until consolidated real-infrastructure qualification. The ROCm images for AMD Instinct GPUs are
pinned but have not served a model through InferCrane yet.

## Ownership

Inference engines own execution, batching, model loading, cache behavior and engine-native metrics.
InferCrane owns immutable runtime identity, capability validation, lifecycle coordination,
normalized evidence, routing membership and safe revision policy.

## Required contract

A runtime profile declares:

- stable adapter and semantic contract version
- engine/version and immutable workload identity
- protocol and supported operations
- readiness and model-identity inspection
- buffered and streaming behavior
- cancellation and graceful drain/shutdown behavior
- telemetry endpoints and normalized metric mappings
- tool, structured-output and embedding capabilities where tested
- compatibility constraints for artifact, accelerator and runtime arguments

Declared, probed, simulated, locally qualified and real-qualified capabilities remain distinct.
Unsupported behavior fails before paid provisioning whenever it is knowable.

Production composition binds each runtime inspector to its validated `RuntimeProfile`. Provisioning
and reconciliation resolve that immutable binding by runtime identity; an adapter cannot execute
under a different runtime's capability claims, and an unregistered runtime remains unroutable.

A built-in runtime (vLLM, SGLang) revision may store no workload. Provisioning then serves the
runtime profile's workload for the GPU's accelerator platform (`RuntimeProfile.WorkloadFor`), and
every reader of the contract's paths, such as the metrics scrape during optimization measurement,
resolves the same workload (`support.ServedWorkload`) rather than the empty stored value. A test
keeps the two in step for every pinned platform.

### Accelerator platforms

A runtime profile records one digest-pinned image per accelerator platform, all at the same engine
version, so the engine's flags and defaults are the same on every GPU it serves. The platform comes
from the reviewed accelerator identity in `provideridentity`: a reviewed AMD Instinct name resolves
to `rocm-gfx942` (MI300X, MI325X) or `rocm-gfx950` (MI350X, MI355X), and every other name keeps the
default `cuda` platform. Lifecycle code looks the image up by platform and never branches on a vendor
name. A GPU whose platform a runtime has no image for fails before provisioning
(`runtime_platform_unsupported`); it never starts another platform's image.

| Runtime | Platform | Image | Stack |
|---|---|---|---|
| vLLM 0.22.1 | `cuda` | `vllm/vllm-openai:v0.22.1@sha256:953d3a06…` | CUDA 13.0.2 |
| vLLM 0.22.1 | `rocm-gfx942`, `rocm-gfx950` | `vllm/vllm-openai-rocm:v0.22.1@sha256:368b2992…` | ROCm 7.2.2, AITER v0.1.13 |
| SGLang 0.5.12 | `cuda` | `lmsysorg/sglang:v0.5.12@sha256:42194170…` | CUDA 13 |
| SGLang 0.5.12 | `rocm-gfx942` | `lmsysorg/sglang:v0.5.12-rocm720-mi30x@sha256:1f5c080b…` | ROCm 7.2 |
| SGLang 0.5.12 | `rocm-gfx950` | `lmsysorg/sglang:v0.5.12-rocm720-mi35x@sha256:3be15561…` | ROCm 7.2 |

The full digests are in `internal/support/matrix.go` and `GET /api/v1/catalog/accelerators`. The
support matrix separately records which cloud and compute mode may run each platform
(`support.Matrix.ValidateAccelerator`); see [AMD Instinct GPUs](/integrations/amd-instinct).

## Custom OCI workloads

InferCrane may accept an immutable OCI image plus explicit protocol, port, health, telemetry and
shutdown declarations. It does not build or execute an image builder, sandbox arbitrary code, or
infer compatibility from an `OpenAI-compatible` label alone.

The portable contract persists argv rather than a shell fragment and standardizes `/health`,
`/v1/models`, and `/metrics`. It supports OpenAI-compatible HTTP, HTTP-disconnect cancellation,
connection-generation draining, and a bounded shutdown-grace declaration. See [Custom OCI
workloads](/features/custom-oci) and inspect the exact runtime/provider matrix with
`infercrane integrations`.

## Agent sandbox boundary

InferCrane supports two explicit sandbox ownership modes. External composition
records a provider-owned sandbox identity and issues a short-lived credential
restricted to one inference endpoint. An optional private-tenant Brezel adapter
lets InferCrane own approved-template lifecycle while Brezel remains the
untrusted-code execution service. InferCrane does not implement a shared
multi-tenant untrusted-code runtime.

External sandbox commands, files, prompts, outputs, runtime credentials, and
snapshot contents remain with the external owner. The initial native Brezel
contract exposes lifecycle metadata only; command and file content remains in
Brezel and is not yet proxied through the InferCrane API.

The runtime choice stays below this contract:

- A managed microVM or hardened-sandbox service is the smallest credible early production path.
- On customer Kubernetes, a `RuntimeClass` backed by gVisor or Kata Containers is a later
  qualification target. The Kubernetes Agent Sandbox CRDs are a useful orchestration adapter, but
  do not themselves provide the isolation boundary.
- Direct microVM ownership remains in Brezel. InferCrane owns customer policy,
  templates, audit, and the provider adapter rather than duplicating guest
  lifecycle and isolation code.
- bVisor is an early implementation reference, not a production security dependency. InferCrane
  will not treat acquisition, popularity or repository availability as security evidence.
- A plain shared container is not an acceptable boundary for hostile customer code.

Every real adapter still requires provider-specific qualification for egress policy, credential
injection, resource exhaustion, cleanup, snapshot confidentiality and tenant escape resistance.
