# Model-agnostic optimization engine

This design is the engineering boundary for the InferCrane optimization PoC.
It is intentionally operator- and workload-driven. A model name can select a
reviewed recipe, but it can never establish that an optimization is compatible,
correct, or faster.

## Scope and claim

The generic flow works for any immutable open-weight model that a supported
runtime can load and expose through the serving contract. Specialized
optimizations work only when InferCrane can discover the model's operators,
match an exact runtime/hardware capability, and collect the required evidence.
Unsupported architecture features produce a conservative baseline and an
explicit missing-capability result—not a guessed recipe.

The initial custom-kernel compiler set is NVIDIA-only. The planner boundary is
extensible to ROCm, TPU, and other accelerators, but those backends must acquire
their own exact capability and qualification evidence before they can emit
experiments.

```text
pinned model + workload/SLO + data boundary + target environment
                              │
                              ▼
                    immutable baseline
                              │
                              ▼
       architecture/runtime/hardware capability discovery
                              │
                              ▼
      generate independent candidate families with constraints
                              │
            ┌─────────────────┼─────────────────┐
            ▼                 ▼                 ▼
      free modeling      cheap screening   target profiling
            │                 │                 │
            └─────────────────┴────────┬────────┘
                                      ▼
                         Pareto candidate set
                                      │
                                      ▼
                   repeated AIPerf + quality + cost
                                      │
                        ┌─────────────┴─────────────┐
                        ▼                           ▼
                      reject                 guarded promote
                                                    │
                                                    ▼
                                      observe drift and re-run
```

## Workload contract

Optimization begins with a distribution, not a benchmark slogan:

- input/output token distributions, request rate, concurrency, arrival pattern,
  streaming, tool use, structured output, multimodal inputs, and prefix reuse;
- TTFT, inter-token latency, end-to-end latency, goodput, error rate, quality,
  cost, energy, and memory constraints;
- prefill/decode mix, batch size, context growth, KV-cache residency, and
  request cancellation behavior;
- exact model revision, tokenizer, runtime/image, accelerator, driver/CUDA,
  topology, and region; and
- metadata-only, redacted replay, or in-customer-boundary data policy.

[NVIDIA AIPerf](https://developer.nvidia.com/blog/benchmarking-llm-inference-at-scale-with-aiperf/)
is the default load generator because it supports controlled constant, Poisson,
and gamma arrivals, synthetic distributions, trace replay, streaming latency,
and multiprocess load generation. AIPerf is evidence collection; it is not the
optimizer.

## Search dimensions

| Layer | Candidate families | Eligibility signal | Required proof |
|---|---|---|---|
| Runtime | vLLM/SGLang/TensorRT-LLM, eager/compiled execution, CUDA graphs | exact model and runtime support | API parity and paired workload |
| Scheduling | continuous batching, batch-token budget, chunked prefill, admission, queue policy | arrival/token distributions and SLO | tail latency and goodput |
| Cache | paged KV, prefix/Radix cache, KV offload/reuse, cache-aware routing | context length, reuse and memory pressure | hit rate, quality and p95 latency |
| Precision | BF16/FP16, FP8, NVFP4/MXFP4, INT8/INT4, AWQ/GPTQ/SmoothQuant | accelerator support and calibration policy | task quality, memory and throughput |
| Decoding | n-gram, draft model, MTP, EAGLE/DFlash, structured decoding | acceptance rate and exact verifier compatibility | identical/approved output policy and latency |
| Attention | FlashAttention, FlashInfer, paged/ragged attention, MLA/GQA, sparse attention | phase, head layout, context and batch | numerical parity, memory and phase metrics |
| Parallelism | tensor, pipeline, data, expert, context parallelism | model size, interconnect and workload | scaling efficiency and communication profile |
| Topology | aggregated or disaggregated prefill/decode, KV transport, cache-aware routing | phase imbalance and fleet size | end-to-end capacity under SLO |
| Artifact | pruning, distillation, sparsity, adapter merge, model replacement | customer-approved quality boundary | full task evaluation and provenance |
| Hardware | GPU family, count, MIG/profile, CPU/accelerator placement | memory fit, availability, price and data policy | sourced cost and repeated measurements |
| Kernel | vendor kernel choice, fusion, specialized GEMM/GEMV, attention, MoE, sampling | profiler hotspot and Amdahl headroom | local parity, exact-GPU benchmark and AIPerf |

No candidate family is universally beneficial. For example, prefix caching has
no value without reusable prefixes; speculative decoding loses when acceptance
is low; quantization can violate quality; tensor parallelism can lose to
communication; and a faster microkernel may not move end-to-end latency.

## Candidate-generation algorithm

1. **Freeze identity.** Hash model revision, tokenizer, runtime image, hardware,
   workload contract, evaluation set, and policy.
2. **Create controls.** Produce at least one conservative vendor/runtime
   baseline. Never compare only optimized candidates with one another.
3. **Discover capability.** Read model configuration and runtime support, then
   observe the actual operator graph and serving phases. Feature detection wins
   over family-name matching.
4. **Generate one-change candidates.** Start with scheduler, cache, precision,
   decoding, topology, and hardware candidates whose preconditions are true.
   Preserve interactions as later composite candidates rather than changing
   everything at once.
5. **Reject cheaply.** Apply compatibility, memory, quality-risk, cost, and
   historical-rejection filters. Use analytical roofline and Amdahl bounds.
6. **Screen.** Run small deterministic correctness and performance lanes on the
   exact target accelerator. Failed candidates become durable search evidence.
7. **Profile the best control.** Attribute prefill, decode, communication, CPU,
   queue, memory, and kernel time. Kernel work begins only here.
8. **Search kernels.** Translate a measured operator hotspot into a typed
   problem, generate candidates through reviewed compiler backends, and verify
   them against the runtime reference on randomized/adversarial shapes.
9. **Qualify the Pareto set.** Repeat AIPerf replay and customer quality tests;
   compare confidence intervals, SLO-qualified goodput, and landed cost.
10. **Promote separately.** A human-approved Release Guard action creates a
    canary. Monitoring invalidates evidence when workload or identity drifts.

Candidate ranking is constrained optimization, not one scalar benchmark:

```text
eligible(c) = correctness ∧ quality ∧ reliability ∧ SLO ∧ budget

Pareto(c) over {TTFT, TPOT, goodput, cost, memory, energy}

winner = policy_rank(Pareto(eligible candidates))
```

The policy can prefer latency, throughput, or cost, but ineligible candidates
are never scored into becoming eligible.

## Kernel opportunity gate

The new `kernelplanner` accepts a profile manifest with typed operator hotspots.
It is independent of the model repository name. Supported initial families are:

- residual-add + RMSNorm;
- quantized linear/dequant + GEMM or GEMV;
- SwiGLU activation;
- prefill and decode attention;
- KV-cache append/quantization;
- MoE routing/grouped GEMM; and
- token sampling.

For hotspot fraction `f`, the maximum possible end-to-end speedup is:

```text
S_max = 1 / (1 - f)
```

For desired end-to-end speedup `S`, the kernel itself must reach:

```text
K_required = f / (1/S - (1-f))
```

If the ceiling is immaterial, InferCrane records a rejection without generating
code. If it passes, the proof ladder is:

```text
reference parity
  → interpreter/simulator + memory safety
  → compile for exact SM
  → target-GPU randomized correctness
  → CUDA-event microbenchmark + Nsight roofline
  → runtime integration
  → paired AIPerf + quality + cost
```

Laptop simulation proves semantics only. [Triton's interpreter](https://triton-lang.org/main/programming-guide/chapter-3/debugging.html),
[Triton-Viz](https://github.com/Deep-Learning-Profiling-Tools/triton-viz),
and [Numba CUDA Simulator](https://nvidia.github.io/numba-cuda/user/simulator.html)
are useful correctness tools; none produces valid GPU timing. Accel-Sim can be a
later research filter, but its most representative trace mode still depends on
real hardware and is too expensive for the inner MVP loop.

The initial executable candidate is
`tools/kernel-lab/fused_residual_rmsnorm.py`. Its dependency-free laptop path
emulates the fused dataflow. Its optional Triton path performs exact-GPU parity
and a microbenchmark. `tools/kernel-lab/fused_residual_rmsnorm_cuda.py` applies
the same contract to visible handwritten CUDA C++ through NVRTC. On the measured
H100 shapes that CUDA candidate was correct but slower than both Triton and the
vLLM vendor operator, so it was rejected. The result remains operator-level
evidence until a runtime-level AIPerf campaign passes.

## Compiler/backend strategy

InferCrane owns the typed problem, evidence, and selection policy—not a single
kernel language:

- [Triton](https://github.com/triton-lang/triton) for productive fusion and
  shape specialization;
- [CUTLASS/CuTe DSL](https://github.com/NVIDIA/cutlass) for Tensor Core GEMM,
  grouped GEMM, low precision, and architecture-specific control;
- [FlashInfer](https://github.com/flashinfer-ai/flashinfer) as the vendor-grade
  attention/GEMM/MoE/sampling baseline and reusable generator;
- CUDA C++ or [CUDA Rust](https://developer.nvidia.com/blog/introducing-cuda-rust-two-tracks-for-writing-gpu-kernels/)
  when low-level SIMT control, a Rust systems stack, or Tile portability justifies it;
- [KernelBench](https://github.com/ScalingIntelligence/KernelBench) task and
  correctness methodology for generated-kernel evaluation; and
- [Mirage](https://github.com/mirage-project/mirage) as a later whole-graph or
  persistent-megakernel search backend, never as automatic evidence authority.

Vendor kernels remain candidates. The custom path is successful when it finds
the fastest qualified solution, including the result “FlashInfer/CUTLASS won.”

## Technique evidence map

The search space is grounded in primary work and maintained implementations:

- [FlashAttention](https://arxiv.org/abs/2205.14135): IO-aware exact attention.
- [PagedAttention/vLLM](https://arxiv.org/abs/2309.06180): paged KV-cache memory
  management and serving throughput.
- [SGLang/RadixAttention](https://arxiv.org/abs/2312.07104): structured-program
  execution and prefix reuse.
- [Sarathi-Serve](https://arxiv.org/abs/2403.02310): chunked prefill and
  stall-free scheduling.
- [Speculative decoding](https://arxiv.org/abs/2211.17192) and
  [EAGLE](https://arxiv.org/abs/2401.15077): verified parallel token generation.
- [GPTQ](https://arxiv.org/abs/2210.17323),
  [AWQ](https://arxiv.org/abs/2306.00978), and
  [SmoothQuant](https://arxiv.org/abs/2211.10438): weight-only and W8A8
  post-training quantization families.
- [KIVI](https://arxiv.org/abs/2402.02750): asymmetric low-bit KV-cache
  quantization.
- [vLLM](https://github.com/vllm-project/vllm),
  [SGLang](https://github.com/sgl-project/sglang), and
  [TensorRT-LLM](https://github.com/NVIDIA/TensorRT-LLM): production references
  for runtime, scheduling, precision, graph, speculative, and distributed paths.

Published speedups are hypotheses for candidate generation, never InferCrane
product claims. Every product claim is bound to InferCrane's exact immutable
tuple and retained raw evidence.

## Kernel registry and search order

`internal/kernelplanner` owns a reviewed registry of implementation sources.
The registry is not a live GitHub crawler and it does not import code into the
API process. Each entry records the project, HTTPS source, license, backend,
operator families, hardware and dtype filters, revision policy, trust status,
and execution boundary.

Search is deterministic and existing-first:

1. measure the implementation bundled in the pinned vLLM or SGLang image;
2. inspect compatible specialized/vendor libraries such as FlashInfer and
   CUTLASS/CuTe;
3. adapt or specialize a reviewed Triton/CuTe implementation;
4. generate handwritten CUDA only after the prior candidates lose and the
   profiler-backed Amdahl ceiling justifies the experiment.

A registry match is only a source candidate. Before execution, a campaign must
pin an immutable source revision, inspect the license, build in an isolated
sandbox, run randomized and adversarial correctness checks, and qualify the
artifact on the exact target GPU and workload.

The authenticated `POST /api/v1/optimization/kernel-opportunities` endpoint
returns this bounded plan with `code_execution: false`, `provider_mutation:
false`, and `performance_claims: false`.

## Deploy, observe, improve

Deployment and optimization are one lifecycle:

```text
reviewed compatible starting point
              │
              ▼
       stable deployment
              │
              ▼
 content-free traffic monitoring
              │
              ▼
 serving-stage readiness assessment
              │
              ▼
 exact runtime/GPU profile + kernel registry
              │
              ▼
 bounded campaign → qualify → guarded release
```

The rank-one proposal candidate is exposed as `starting_point`. It is selected
from compatibility and declared objective fit, not an unmeasured speed claim.
After deployment, `GET /api/v1/endpoints/{name}/optimization-readiness` turns
fresh request aggregates into a conservative stage diagnosis such as queueing,
prefill/first-token, or decode/generation pressure. Request telemetry never
opens the kernel gate: exact target-GPU profiling and the Amdahl policy remain
mandatory.

## Process boundary

Do not move compiler, profiler, or arbitrary candidate-source execution into
the InferCrane API server.

- **InferCrane control plane:** workload identity, registry metadata, policy,
  plans, approvals, budget leases, evidence, Release Guard, and audit.
- **Brezel worker:** isolated source checkout, license/provenance inspection,
  agent planning, CPU-side compilation and semantic tests, artifact signing,
  and lifecycle receipts. Network access is deny-by-default and source commits
  are explicit allowlisted inputs.
- **GPU worker:** exact-SM build when required, correctness, sanitizers, Nsight,
  microbenchmarks, AIPerf replay, quality tests, and teardown under the campaign
  lease.

The deterministic planner and registry belong in this repository because they
are product policy. Brezel remains a separately deployed execution service;
InferCrane submits immutable jobs through an adapter. This keeps arbitrary code
and heavy toolchains away from the multi-tenant control-plane process while
preserving one customer-facing workflow and evidence graph.

`internal/brezelexecutor` is the implemented adapter boundary. It creates or
adopts a sandbox from a pinned Brezel environment revision, writes a typed
content-addressed job manifest, invokes only the baked-in
`/opt/infercrane/bin/run-optimization-job` entrypoint, validates a typed result,
deletes the sandbox, and retains the lifecycle receipt. Manifests cannot carry
shell commands, environment variables, credentials, or an alternate runner.
Brezel egress remains disabled; approved source access belongs in Brezel
connector policy.

```text
customer deployment                InferCrane campaign
        │                                  │
        │ aggregated profile               │ typed build job
        ▼                                  ▼
 exact GPU worker ◀── artifact ── Brezel private sandbox
        │             + receipt             │
        │ AIPerf + quality + cost           │ source / CPU checks
        └──────────── evidence ──────────────┘
                         │
                         ▼
               Release Guard approval
```

The customer-facing **Sandboxes** product uses the same Brezel API for coding,
evaluation, and agent workspaces, but it is a separate resource and billing
surface. Creating a model deployment does not create a customer-visible
sandbox. Optimization jobs use short-lived internal sandboxes and surface only
their evidence and receipt on the deployment.

## PoC commands

Generate a safe plan from the Qwen documentation fixture:

```bash
infercrane optimize kernel-plan \
  --file internal/kernelplanner/testdata/qwen3-0.6b-fixture.json
```

Reject fixture data for a paid campaign:

```bash
infercrane optimize kernel-plan \
  --file captured-profile.json --require-measured --output json
```

Run the local kernel semantic check:

```bash
python3 tools/kernel-lab/fused_residual_rmsnorm.py \
  --self-test --rows 7 --hidden-size 1024
```

The documentation fixture contains illustrative hotspot fractions and cannot be
used as a Qwen/H100 performance claim. A real H100 run must replace it with an
exact profiler capture.
