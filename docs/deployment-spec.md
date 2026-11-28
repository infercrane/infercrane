---
title: DeploymentSpec
description: Declaratively define model identity, runtime, provider, compute, scaling, and routing.
---

# DeploymentSpec

New files declare the stable v1 file contract:

```yaml
apiVersion: infercrane.dev/v1
kind: Deployment
```

Pre-v1 files without the header are interpreted as v1. Unknown versions, kinds, and fields fail
closed. See [Upgrade and compatibility](/upgrade) for the change policy.

The primary path uses defaults:

```console
infercrane deploy Qwen/Qwen3-8B
```

The YAML form groups model, runtime, compute, provider, resources, scaling, and routing concerns.
Mutable model revisions such as `main` are resolved and persisted as immutable Hugging Face commits
before provisioning.

```yaml
name: qwen-prod
endpoint: support-production
model:
  id: Qwen/Qwen3-8B
  revision: main
runtime:
  engine: vllm
  version: 0.34.0
  args:
    - --enable-prefix-caching
compute:
  mode: elastic
provider:
  cloud: runpod
  # adapter: skypilot # optional exact profile selection
  region: EU-RO-1
resources:
  gpu: L40S
  gpu_count: 1
scaling:
  min_replicas: 1
  max_replicas: 4
routing:
  strategy: round-robin
```

`runtime.version` is the engine release. Built-in runtimes default to their current pinned release
(vLLM 0.34.0, SGLang 0.5.23) and boot its digest-pinned image. vLLM 0.22.1 and SGLang 0.5.12 stay
available only for revisions that already record them; any other release is refused when the spec is loaded, unless the
image is not InferCrane's (a custom workload, serverless, or Dynamo). See
[Engine releases](/architecture/runtime-contract#engine-releases-and-accelerator-platforms).

`name` identifies the deployment lifecycle. `endpoint` identifies the stable application-facing
route and defaults to `name` when omitted. A new deployment revision can therefore replace the
serving plan underneath `support-production` without changing application configuration.

SGLang uses a built-in immutable workload profile. Custom OCI images declare the full contract in
`runtime.workload`; see [Custom OCI workloads](/features/custom-oci). Mutable image tags and
underspecified probes are rejected before provisioning.

An optional `serving` object describes an advanced backend topology without exposing provider CRD
shape to the core schema. Today `backend: dynamo` requires one outer graph replica, Kubernetes, and
the `kubernetes-dynamo` adapter. See [NVIDIA Dynamo](/integrations/dynamo) for the simple command and
the complete reviewable YAML form.

`compute.mode` defaults to `elastic`; elastic replica bounds default to `1..1`. Serverless requires
`min_replicas: 0` and defaults `max_replicas` to one when omitted. Revisions are immutable; changing
a field creates a candidate. `provider.adapter` persists the exact implementation when more than one
profile can serve a cloud/runtime pair; it is optional for an unambiguous default. Current executable
elastic defaults include configured AWS EC2 and GCP Compute, while other provider-product profiles
remain registered but deferred until their exact combination is qualified. AWS and GCP require an
explicit `provider.region`. Cost is omitted unless a trustworthy provider measurement exists.

`resources.gpu_count` is the number of accelerators allocated to each runtime replica and defaults
to one. It is part of immutable intent, pricing, capacity history, benchmarks, recommendations, and
inference passports. SkyPilot, GCP Compute, and Kubernetes compile the exact count into provider
resources. AWS EC2 binds the count to `INFERCRANE_AWS_GPU_COUNT` for the configured instance type and
rejects a mismatch before creating paid capacity. Serverless currently supports one accelerator per
worker and rejects any other count explicitly.

## Engine arguments

`runtime.args` adds arguments to a built-in runtime's own command. On vLLM and SGLang they are a
reviewed set: an argument outside it is refused when the spec is loaded, when the control API
receives the deployment or an optimization proposal, and when a deployment is provisioned, scaled,
or rolled out. An InferCrane agent refuses the same arguments in a candidate it runs. Both forms
`--flag value` and `--flag=value` are read; a value such as `-1` is a value, not a flag.

When `runtime.args` is empty on vLLM or SGLang, InferCrane may add one argument itself: a context
length, when one request at the model's own maximum would not fit the GPU next to the weights. The
deploy response says which length and why; see
[Context length at deploy](/features/model-catalog#context-length-at-deploy). Any argument you set
turns this off.

Not in the set: arguments InferCrane sets itself (host, port, model, revision, API key, LoRA
adapters, configuration files) and arguments that load code or files named in their value (plugins,
middleware, logits processors, load formats, KV connector modules). Two arguments are refused on
every runtime and image: `--trust-remote-code`, because InferCrane serves models only with the
engine's own implementation, and `--enable-custom-logit-processor`, because it lets a request carry
code for the server to run. A custom OCI workload's own command is otherwise not checked.

| Runtime | Reviewed arguments |
|---|---|
| vLLM | `--max-model-len`, `--tensor-parallel-size` (`-tp`), `--pipeline-parallel-size` (`-pp`), `--data-parallel-size` (`-dp`), `--enable-expert-parallel`, `--dtype`, `--seed`, `--served-model-name`, `--language-model-only`, `--limit-mm-per-prompt`, `--mm-encoder-tp-mode`, `--max-num-seqs`, `--max-num-batched-tokens`, `--enable-chunked-prefill`, `--no-enable-chunked-prefill`, `--long-prefill-token-threshold`, `--max-num-partial-prefills`, `--async-scheduling`, `--no-async-scheduling`, `--stream-interval`, `--api-server-count` (`-asc`), `--performance-mode`, `--enable-prefix-caching`, `--no-enable-prefix-caching`, `--prefix-caching-hash-algo`, `--cache-aware-admission-window`, `--cache-aware-admission-threshold`, `--kv-cache-dtype`, `--block-size`, `--gpu-memory-utilization`, `--kv-cache-memory-bytes`, `--enable-extensible-kv-cache`, `--no-enable-extensible-kv-cache`, `--kv-offloading-size`, `--kv-offloading-backend`, `--swap-space`, `--cpu-offload-gb`, `--quantization` (`-q`), `--attention-backend`, `--moe-backend`, `--all2all-backend`, `--enable-eplb`, `--enable-dbo`, `--enforce-eager`, `--max-cudagraph-capture-size`, `--cudagraph-capture-sizes`, `--compilation-config` (`-cc`, and `-cc.<key>=<value>`), `-O`, `-O0` to `-O3`, `--speculative-config` (`-sc`), `--max-logprobs`, `--tool-call-parser`, `--enable-auto-tool-choice`, `--reasoning-parser`, `--disable-log-stats` |
| SGLang | `--context-length`, `--tp-size` (`--tp`), `--dp-size`, `--ep-size`, `--attn-dp-size`, `--enable-dp-attention`, `--dtype`, `--seed`, `--served-model-name`, `--language-model-only`, `--max-running-requests`, `--max-prefill-tokens`, `--chunked-prefill-size`, `--schedule-policy`, `--disable-overlap-schedule`, `--enable-mixed-chunk`, `--stream-interval`, `--mem-fraction-static`, `--kv-cache-dtype`, `--page-size`, `--disable-radix-cache`, `--enable-hierarchical-cache` (`--enable-hicache`), `--hicache-ratio`, `--hicache-size`, `--hicache-write-policy`, `--quantization`, `--attention-backend`, `--decode-attention-backend`, `--prefill-attention-backend`, `--fp8-gemm-backend`, `--moe-runner-backend`, `--linear-attn-prefill-backend`, `--linear-attn-decode-backend`, `--enable-flashinfer-autotune`, `--no-enable-flashinfer-autotune`, `--disable-cuda-graph`, `--disable-decode-cuda-graph`, `--disable-prefill-cuda-graph`, `--disable-piecewise-cuda-graph`, `--cuda-graph-max-bs`, `--cuda-graph-max-bs-decode`, `--cuda-graph-max-bs-prefill`, `--cuda-graph-bs`, `--cuda-graph-bs-decode`, `--cuda-graph-bs-prefill`, `--cuda-graph-backend-decode`, `--cuda-graph-backend-prefill`, `--speculative-algorithm`, `--speculative-num-steps`, `--speculative-eagle-topk`, `--speculative-num-draft-tokens`, `--speculative-draft-model-path`, `--speculative-draft-model-revision`, `--speculative-draft-model-quantization`, `--speculative-dflash-block-size`, `--speculative-adaptive`, `--enable-linear-replayssm-spec`, `--tool-call-parser`, `--reasoning-parser`, `--enable-metrics` |

A revision recorded before this check that uses another argument is refused at its next scale or
rollout, with the argument named. To keep serving it, create a revision without that argument, or
run the server as a custom OCI workload with its own image and command.

## Engine environment switches

`runtime.env` sets a few reviewed engine switches through the runtime container's environment, for
example AMD's AITER kernels for vLLM:

```yaml
runtime:
  engine: vllm
  version: 0.34.0
  env:
    VLLM_ROCM_USE_AITER: "1"
resources:
  gpu: MI300X
```

It is not a general environment. Each name is reviewed for one engine version, its pinned image,
and the accelerator platforms listed, with a fixed set of values read from the engine's source:

| Name | Runtime | Platforms | Values | Default |
|---|---|---|---|---|
| `VLLM_ROCM_USE_AITER` | vLLM 0.34.0 and 0.22.1, pinned images | `rocm-gfx942`, `rocm-gfx950` | `0`, `1` | `0` |
| `VLLM_USE_RUST_FRONTEND` | vLLM 0.34.0, pinned image | `cuda` | `0`, `1` | `0` |

`VLLM_USE_RUST_FRONTEND=1` serves the HTTP API from vLLM's Rust server: the same paths, metrics, and
API key as the Python one, one process whatever `--api-server-count` says, no Anthropic Messages or
batch routes, and a different access log line (`[access_log.rs] HTTP response client=… method=…
uri="…" status=…` instead of uvicorn's `"POST /v1/chat/completions HTTP/1.1" 200`).

Anything else is refused when the spec is loaded, when the control API receives the deployment, and
again in the provider adapter: another name or value, a custom image, another engine version, a GPU
on another platform, or more than 8 entries. A refused value is not repeated in the error, so a
credential pasted here does not reach logs. Credentials never go in `runtime.env`; the worker key
and model tokens reach the runtime through their own references.

RunPod Pods, Kubernetes, AWS EC2, and GCP Compute pass the switches to the runtime container.
RunPod Pods, Kubernetes, and GCP Compute also compare them when adopting an existing replica, so a
running replica with different switches is not adopted. SkyPilot, RunPod Serverless, and Dynamo on Kubernetes cannot pass them, and a revision
that sets any is refused before anything is provisioned. The API field is `runtime_env` on
`POST /api/v1/deployments`. The switches are part of the immutable revision: changing one creates a
candidate, and benchmark rows record them next to the engine arguments.

## Tuned kernel configs

`runtime.kernel_configs` serves the engine's own MoE kernel with tile settings tuned for one GPU
and the step sizes of one workload: data files vLLM reads at start, never code. See
[kernels tuned for your shapes](/features/kernel-tuning) for how a set is made and proven.

```yaml
runtime:
  engine: vllm
  kernel_configs:            # a tuned set, usually added by an optimization campaign
    operator: fused-moe
    runtime: vllm
    runtime_version: 0.34.0
    device: NVIDIA_H100_80GB_HBM3
    tuner: infercrane-fused-moe-tuner-v1
    digest: sha256:...
    files:
      - name: E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json
        content: '{"1":{"BLOCK_SIZE_M":16,...}}'
        sha256: ...
resources:
  gpu: H100
```

A set is accepted only for the engine release and GPU it was tuned for, on that release's pinned
image (a revision that records vLLM 0.22.1 is checked against the 0.22.1 image), with file names in the engine's own form, canonical files of tile settings and matching
digests; it is checked when the spec is loaded, when the control API receives it, when a revision
is stored, and again in the provider adapter. Kubernetes writes the files into a folder of the Pod
before the engine starts, checks each sha256, and sets `VLLM_TUNED_CONFIG_FOLDER`; RunPod, AWS
EC2, GCP Compute, SkyPilot and Dynamo refuse a revision with a set before provisioning. The set is
part of the immutable revision, and benchmark rows and capacity evidence record its digest.
