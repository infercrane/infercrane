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

The planner and evidence protocol cover NVIDIA, AMD ROCm, Google TPU, and AWS
Neuron. That is not a blanket hardware-support claim. At startup a configured
Accelerator Lab worker must declare its installed profiler, runtime, topology,
modality, and generated-kernel capabilities. InferCrane accepts a run only when
that live catalog covers the exact request, and a result becomes qualified only
after the same worker returns evidence for the exact immutable tuple.

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

### Autonomous kernel research incorporated

The implementation follows the common result from current kernel-agent work:
generation is a search primitive, not an evidence authority.

- [KernelBench](https://arxiv.org/abs/2502.10517) supplies the functional
  correctness plus target-device speed evaluation shape. InferCrane also uses
  the project's stricter [evaluation guidance](https://github.com/ScalingIntelligence/KernelBench/blob/main/EVAL.md):
  hidden/adversarial shapes, suspicious-speedup review, and independent
  verification.
- [KernelBench-Verified](https://arxiv.org/abs/2607.16241) and
  [RealisticTritonBench](https://arxiv.org/abs/2608.12004) reinforce why
  realistic operators, stronger tests, and contamination-resistant evaluation
  matter more than a single public-shape score.
- [CUDA Agent](https://arxiv.org/abs/2602.24286),
  [KernelPro](https://arxiv.org/abs/2606.26453), and
  [CudaForge](https://arxiv.org/abs/2511.01884) motivate iterative
  profile/generate/compile/measure loops. InferCrane adopts the loop but places
  generation behind Amdahl materiality, a hard spend/expiry lease, an isolated
  Brezel build, exact-target sanitizers, and full serving replay.
- [AIConfigurator](https://arxiv.org/abs/2601.06288) informs the broader
  configuration search. Its proposals remain candidates until InferCrane's
  model/runtime/hardware/workload evidence gates pass.
- [NVIDIA SOL-ExecBench](https://github.com/NVIDIA/SOL-ExecBench) is a useful
  independent execution benchmark for generated GPU programs; it complements,
  rather than replaces, workload-specific end-to-end qualification.

`internal/acceleratorlab` now exposes an opt-in generator contract. A generated
source bundle must echo the input digest, candidate identity, and exact profile
artifact digest; include immutable revision, license, content hashes, and
sizes; build through the fixed Brezel runner; and then pass the same target
qualification gates as reviewed code. The system never promotes it
automatically.

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

## Implementation coverage

The [Wafer GPU performance engineering resource map](https://github.com/wafer-ai/gpu-perf-engineering-resources)
is a useful field checklist, not a feature checklist InferCrane can honestly mark
complete. The current implementation boundary is:

| Area | Current status | Product boundary |
|---|---|---|
| Workload identity, replay, SLO/goodput, cost caps, immutable evidence and guarded promotion | Implemented | Control-plane workflow and qualification policy |
| vLLM and SGLang recipe candidates, cache/precision/scheduler/topology fields | Implemented for bounded candidates | Every exact tuple still needs compatibility and measured qualification |
| AIPerf load generation and persisted benchmark evidence | Implemented | Requires an installed runner and a reachable exact serving tuple |
| Profiler-hotspot and Amdahl kernel opportunity planning | Implemented and locally qualified | Accelerator worker `POST /v1/profiles` captures vendor-native evidence; a real worker/toolchain is required for measured claims |
| Existing-first kernel search across runtime, FlashInfer, CUTLASS/CuTe, Triton and CUDA sources | Implemented as reviewed registry policy | Matches are unmeasured source candidates, never automatic performance claims |
| Triton and handwritten CUDA correctness/microbenchmark PoCs | Implemented as operator experiments | Manual GPU qualification tools; not yet an automatic campaign executor |
| Profiler → generated/reviewed source → Brezel build → target qualification | Implemented and locally qualified | Durable operation, progressive cost, immutable artifacts, worker-declared capabilities, and no implicit promotion; deployed runner/worker still require real-infrastructure qualification |
| Brezel-isolated optimization build jobs and receipts | Implemented and locally qualified | Fixed baked runner, deny-by-default egress, verified input/output artifacts, cleanup and signed receipt; the optimization environment revision must actually contain that runner |
| TensorRT-LLM, multi-node collectives, expert placement and disaggregated MoE qualification | Executable worker contract | NVIDIA workers may declare these independently; undeclared topology fails before spend, and no real tuple is claimed without evidence |
| Nsight Systems/Compute, rocprof, XProf and Neuron Explorer capture | Executable worker contract | Capabilities are loaded from the configured worker at startup; built-in schemas are never presented as deployed capacity |
| AI-generated kernels and KernelBench-style evaluation | Implemented orchestration and gates | Profile-bound generation, isolated build, hidden shapes, sanitizers, exact-target and serving replay; generator and hardware-worker implementations need deployment qualification |
| AMD ROCm, TPU and Trainium compiler/runtime paths | Planner, registry and worker contracts implemented | AITER/CK/Triton-ROCm/HIP, Pallas/XLA, and NKI/Neuron candidates exist; no real-hardware product claim until the corresponding worker passes |
| Image, video and voice optimization lanes | Typed workload and qualification contracts implemented | Each run requires media shapes, immutable replay, modality quality suite and a worker-declared lane; measured suites remain tuple-specific real-infrastructure work |

For the MVP, the defensible product is the locally qualified control plane plus
the real tuples whose worker evidence has passed. An installed adapter or a
compiled contract is never rendered as a speed, capacity, hardware, or modality
claim.

## Accelerator Lab product wiring

The authenticated surface is:

- `GET /api/v1/optimization/accelerator-lab/capabilities` — shows the live
  worker catalog and whether execution is configured;
- `POST /api/v1/optimization/accelerator-lab/runs` — validates the tenant,
  immutable request, 24-hour authority ceiling, cost cap, and live worker
  capability before enqueueing a durable operation;
- `POST /v1/profiles`, `POST /v1/kernel-generations`, and
  `POST /v1/qualifications` — server-to-worker calls, never browser calls; and
- `/v1/artifacts/...` — same-origin, content-addressed exchange between the
  trusted worker and a deny-by-default Brezel builder.

Enablement requires all three server-only values in addition to the normal
Brezel sandbox configuration:

```text
INFERCRANE_ACCELERATOR_WORKER_URL=https://...
INFERCRANE_ACCELERATOR_WORKER_TOKEN_FILE=/run/secrets/accelerator-worker-token
INFERCRANE_BREZEL_OPTIMIZATION_ENVIRONMENT=envr_<immutable revision>
```

Startup fails if the token path is not absolute, the URL is not HTTPS (except
loopback), the environment is not immutable, or the worker capability catalog
cannot be loaded. The token must be an owner-only regular file.

## FastPath PoC migration

`infercrane-fastpath-poc` remains a research repository; it is not copied into
the control-plane binary or imported at runtime. Its production-worthy
concepts have been absorbed as typed Go policy:

| FastPath concept | InferCrane production location |
|---|---|
| Release/evidence manifest import without trust upgrade | `internal/optimizationevidence/fastpath.go` |
| Hotspot normalization, Amdahl stop gate and existing-first kernel plan | `internal/kernelplanner` |
| Budget, durable campaign stages, ranking and explicit promotion | `internal/optimizationcampaign` and `internal/acceleratorlab` |
| Nsight/profile request and exact target evidence boundary | `internal/acceleratorlab` worker client |
| Isolated source build and lifecycle receipt | `internal/brezelexecutor` |
| vLLM/AIPerf/quality evidence and Release Guard | existing benchmark, quality-evidence and release-guard packages |

The old Python `CandidateGenerator` was only an interface and the PoC registry
kept generator adapters blocked until targeted NCU evidence existed. InferCrane
now has the full typed generation-to-evaluation orchestration, but the actual
generator model/service and vendor workers must still declare and prove their
installed capabilities. This is deliberate separation, not duplicate product
logic.

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
