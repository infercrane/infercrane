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
runtime profile's workload for the revision's engine release and the GPU's accelerator platform
(`RuntimeProfile.WorkloadFor`), and every reader of the contract, such as the metrics scrape during
optimization measurement, resolves the same workload (`support.ServedWorkloadFor`) rather than the
empty stored value. A test keeps the two in step for every pinned release and platform. Every pinned
release of a runtime serves the same paths and port.

### Engine releases and accelerator platforms

A runtime profile records one digest-pinned image per engine release and accelerator platform. One
release is current: a revision that names no version gets it, and new setups are planned on it.
An older release stays pinned as legacy only while revisions record it, so a deployment keeps the
engine it was measured on when it scales, heals, or rolls back. A revision that names a release with
no pinned image is refused before provisioning; before vLLM 0.34.0 it ran the current image while
recording another version. Serverless workers run the operator's template image and Dynamo serving
graphs the operator's Dynamo image, so their recorded release is the operator's and is not checked
against this table.

The platform comes from the reviewed accelerator identity in `provideridentity`: a reviewed AMD
Instinct name resolves to `rocm-gfx942` (MI300X, MI325X) or `rocm-gfx950` (MI350X, MI355X), and
every other name keeps the default `cuda` platform. Google Cloud TPUs and Intel Gaudi and Arc Pro
names resolve to their own platforms (`tpu-v5e`, `tpu-v5p`, `tpu-v6e`, `tpu-v7x`, `hpu-gaudi2`,
`hpu-gaudi3`, `xpu-xe2`), which are not qualified: they have no selectable image or placement, and
every spec naming one is refused. Their candidate images are recorded apart from this table; see
[Google Cloud TPU](/architecture/google-tpu) and
[Intel Gaudi and Intel GPUs](/architecture/intel-gaudi-and-gpus). Lifecycle code looks the image up by release and
platform and never branches on a vendor name. A GPU whose platform a release has no image for fails
before provisioning (`runtime_platform_unsupported`); it never starts another platform's or
release's image.

| Runtime | Status | Platform | Image | Stack |
|---|---|---|---|---|
| vLLM 0.34.0 | current | `cuda` | `vllm/vllm-openai:v0.34.0@sha256:20fdb089…` | CUDA 13.0.2 |
| vLLM 0.34.0 | current | `rocm-gfx942`, `rocm-gfx950` | `vllm/vllm-openai-rocm:v0.34.0@sha256:3cb773d2…` | ROCm 10.0.0, AITER v0.1.28.post1 |
| vLLM 0.22.1 | legacy | `cuda` | `vllm/vllm-openai:v0.22.1@sha256:953d3a06…` | CUDA 13.0.2 |
| vLLM 0.22.1 | legacy | `rocm-gfx942`, `rocm-gfx950` | `vllm/vllm-openai-rocm:v0.22.1@sha256:368b2992…` | ROCm 7.2.2, AITER v0.1.13 |
| SGLang 0.5.23 | current | `cuda` | `lmsysorg/sglang:v0.5.23@sha256:f832b5c3…` | CUDA 13.0.3 |
| SGLang 0.5.23 | current | `rocm-gfx942` | `lmsysorg/sglang:v0.5.23-rocm720-mi30x@sha256:a3a6eb29…` | ROCm 7.2, AITER 285579b6 |
| SGLang 0.5.23 | current | `rocm-gfx950` | `lmsysorg/sglang:v0.5.23-rocm720-mi35x@sha256:1f45d56f…` | ROCm 7.2, AITER 285579b6 |
| SGLang 0.5.12 | legacy | `cuda` | `lmsysorg/sglang:v0.5.12@sha256:42194170…` | CUDA 13 |
| SGLang 0.5.12 | legacy | `rocm-gfx942` | `lmsysorg/sglang:v0.5.12-rocm720-mi30x@sha256:1f5c080b…` | ROCm 7.2 |
| SGLang 0.5.12 | legacy | `rocm-gfx950` | `lmsysorg/sglang:v0.5.12-rocm720-mi35x@sha256:3be15561…` | ROCm 7.2 |

An SGLang revision stores its workload, so it keeps the image of the release it recorded on every
later operation; a new one stores the image of the release it names. The SGLang workload starts the
server with `--enable-metrics`, without which SGLang serves no `/metrics`.

Every engine requires the worker key. vLLM reads it from `VLLM_API_KEY`, which every adapter sets.
SGLang reads a key only from `--api-key` or a `--config` file, and a workload may not carry the key
in its arguments, so an SGLang runtime's command starts through a short launcher
(`/bin/sh -c … infercrane-engine-key`): it writes `INFERCRANE_WORKER_API_KEY`, which the adapter sets
from its secret, to a file only the container user can read, and starts SGLang with
`--config <file>`. Without a well-formed key the server does not start. This applies to every
`sglang` runtime command that starts the server (`sglang serve` or `-m sglang.launch_server`), also
behind a prefix such as `numactl`, `taskset` or `env`: the built-in command, curated recipes, the
setups `--search setups` adds, your own command, and agent candidates. The launcher adds `--config`
at the end of the command, so it reaches SGLang. A revision stored before it gets the launcher the
next time it is provisioned. `/health` and `/metrics` stay open in both engines, so probes and
scrapes need no key; the router and the runtime inspector send it. SGLang 0.5.23 also
publishes ROCm 7.2.4 and ROCm 10 builds; the pin keeps ROCm 7.2, the stack of the 0.5.12 pin.

The vLLM 0.34.0 ROCm stack is the one the tag's `docker/Dockerfile.rocm_base` names; the image's
labels say `rocm100` and list `gfx90a;gfx942;gfx950`, and its layers were not read. Pinning an image
is not qualification: no pinned ROCm image has served a model through InferCrane yet.

The full digests are in `internal/support/matrix.go` and `GET /api/v1/catalog/accelerators`. The
support matrix separately records which cloud and compute mode may run each platform
(`support.Matrix.ValidateAccelerator`); see [AMD Instinct GPUs](/integrations/amd-instinct).

## Speculation switch

`infercrane-specswitch` (`internal/specswitch`) is a server's own front, not a runtime adapter: it
runs one vLLM engine per speculation setup of one model on the server's GPU, with sleep mode, and
keeps one awake by the load ([speculation that changes with load](/features/adaptive-speculation)).
It serves the contract's paths (`/health`, `/v1/models`, `/metrics`) itself, passes the awake
engine's metrics on after its own, and never forwards an engine's sleep or development routes. The
engines listen on the loopback interface only. Lifecycle code sees one server; it reads no database
and holds no credential. Its serving image is not published, so no revision uses it yet.

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
