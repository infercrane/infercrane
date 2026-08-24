# Inference optimization execution plan

Status: active workplan.

Updated: 2026-08-24.

## Outcome

InferCrane should turn an operator objective into a qualified serving decision without claiming that
an estimate is proof:

```text
model + hardware + workload + SLO + cost boundary
                         ↓
discover compatible upstream mechanisms
                         ↓
propose a small candidate set
                         ↓
explicitly approve bounded infrastructure spend
                         ↓
benchmark + replay + semantic evaluation
                         ↓
rank by SLO, goodput, cost, and quality
                         ↓
persist a qualified Serving Plan
                         ↓
Release Guard + human-approved promotion or rollback
                         ↓
record predicted versus actual outcome
```

The durable rule is: **the optimizer proposes; InferCrane proves**.

## Current baseline

Already implemented and verified:

- reviewed, immutable model recipes and vLLM balanced/interactive/throughput starting profiles;
- replaceable `optimizer.Source` and deterministic offline `optimize propose`;
- optional pinned AIConfigurator `0.11.0` modeled-candidate source behind that boundary, including a
  real Linux package contract and explicit catalog fallback;
- exact provider/runtime compatibility evidence with simulated candidates excluded by default;
- immutable revisions and DeploymentSpecs;
- AIPerf benchmark execution and persisted TTFT, TPOT, throughput, goodput, GPU-utilization, and
  sourced-cost evidence;
- exact-workload Inference Lab comparisons;
- evaluator-neutral signed semantic-quality evidence;
- advisory Serving Plans, Release Guard, promotion, rejection, rollback, and Inference Passports;
- experimental Dynamo aggregated/disaggregated topology, KV-aware routing, and bounded KVBM intent.

The optimizer now also persists a bounded durable campaign, advances candidates through a fenced
restart-safe proof coordinator, compiles qualified mechanisms through an exact capability registry,
and records immutable external-builder provenance for quantized, speculator, and TensorRT engine
artifacts. The coordinator has a local fixture driver and operation contract; no production driver is
registered until its deployment, benchmark, quality, guard, and cleanup composition is qualified.
InferCrane does not yet execute quantization, speculative decoding, LMCache, NIXL, or TensorRT-LLM.

## AISimulate decision

InferCrane will use AISimulate as an **optional modeled-candidate source**, not as its domain model,
source of truth, deployment engine, or qualification authority.

As of 2026-08-24:

- the published `aisimulate` Python artifact is `0.1.0.dev1`;
- the implementation currently lives with Dynamo and the proposed standalone repository is not
  publicly accessible;
- upstream's accepted transition plan targets AISimulate `0.12.0`, AIConfigurator `0.12.0`, and
  Dynamo `1.5.0` for 2026-09-16;
- upstream explicitly requires public packages, documentation, result-schema mapping, parity tests,
  performance comparison, critical integration validation, and rollback instructions before
  deprecating AIConfigurator.

Therefore:

1. Keep the pinned AIConfigurator adapter optional and out of the control-plane runtime dependency
   graph; never import its objects into InferCrane domain or persistence types.
2. Do not ship `aisimulate==0.1.0.dev1` in an InferCrane release.
3. Keep the existing `optimizer.Source` as the stable InferCrane boundary.
4. Build the campaign and evidence pipeline independently of either upstream package.
5. Replace the candidate source with AISimulate only after the upstream readiness gates below pass.

### AISimulate admission gates

All are required:

- stable public release, repository, SDK/CLI documentation, and result schema;
- Apache-2.0 artifact and path-level transitive-license review;
- immutable package/image pins, hashes, SBOM, vulnerability scan, and rollback pin;
- clean installation and smoke tests on InferCrane's supported execution platform;
- explicit model, GPU, runtime, precision, and topology support inventory;
- deterministic bounded execution with CPU, memory, time, and output-size limits;
- no provider credentials, prompt bodies, response bodies, or customer secrets in inputs or output;
- malformed, partial, unknown-enum, timeout, crash, and version-skew fixture coverage;
- AIC/AISimulate parity results within upstream's published tolerance;
- InferCrane calibration comparing modeled versus measured results on at least three model families,
  two workload shapes, and two hardware classes;
- modeled rows remain labeled `MODELED` and can never satisfy a measured Release Guard requirement.

### Adapter shape

Run AISimulate out of process behind a pinned helper image or executable:

```text
InferCrane OptimizationRequest v1
                ↓ JSON stdin / bounded process contract
AISimulateSource adapter
                ↓
versioned raw result retained as private provenance
                ↓
InferCrane CandidateProposal v1 (MODELED)
```

The adapter owns upstream field translation. Core InferCrane types must not import AISimulate Python
or Rust types. Network access is disabled by default. Failure returns unavailable modeled evidence;
it never falls back to an unlabeled estimate.

## Dependency-ordered milestones

### M0 — Conservative proposal source

Status: complete.

Delivered:

- reviewed-catalog source;
- immutable input digest and candidate identity;
- objective-aware ordering;
- strict missing-evidence responses;
- offline CLI and loadable candidate files;
- tests, ADR, capability matrix, and user documentation.

### M0.5 — Pinned AIConfigurator candidate source

Status: complete.

Delivered:

- out-of-process, versioned and bounded estimator protocol;
- exact `aiconfigurator==0.11.0` and `plotext==5.3.2` compatibility tuple;
- credential-scrubbed, offline-by-default execution;
- explicit `modeled` evidence and upstream result digest;
- executable-topology filtering and single mutation-owner preservation;
- catalog fallback, doctor UX, real-package contract, multi-model catalog regression, ADR and docs.

### M1 — Durable optimization campaigns

Priority: P0. Impact: highest. Effort: medium.

Status: local coordinator complete; production driver and real-GPU qualification pending.

Delivered locally: immutable idempotent campaigns, ordered candidates, bounded expiring approval,
fenced transitions, separate predicted/actual evidence, deterministic aggregate state, cancellation,
cleanup, tenant isolation, REST/CLI inspection, PostgreSQL restart-safe persistence, and a bounded
durable operation handler. The coordinator persists each boundary before side effects, measures all
viable candidates before ranking, ignores proposal order once measurements exist, and links separate
benchmark, revision-bound quality, Lab-ranking, and Release Guard evidence. Exact-workload Lab
ranking fails closed on missing cost or incompatible evidence, while explicit SLO violations reject.
The coordinator preserves retryable lost-response adoption, fails expired authority into cleanup,
preserves permanent driver failures, treats unknown rank or guard results as inconclusive, and stops
at human promotion. Approval still starts no provider mutation because a production composite driver
is not registered.

Add `OptimizationCampaign` and `CandidateRun` durable state without introducing a workflow engine.
Reuse InferCrane operations, leases, revisions, benchmark evidence, quality evidence, Lab, and Release
Guard.

States:

```text
proposed → awaiting_approval → provisioning → ready → measuring
         → validating → ranked → guarding → guard_passed | rejected | inconclusive
         → approved → promoted → observed
         → cleaned
```

CLI:

```bash
infercrane optimize create MODEL --objective interactive --profile interactive \
  --max-ttft-p95-ms 200 --max-hourly-cost 3 --max-candidates 3
infercrane optimize inspect CAMPAIGN
infercrane optimize approve CAMPAIGN --max-cost-usd 20
infercrane optimize watch CAMPAIGN
infercrane optimize results CAMPAIGN
infercrane optimize promote CAMPAIGN CANDIDATE --yes
infercrane optimize cleanup CAMPAIGN
```

Required behavior:

- proposal and inspection are free of provider mutation;
- paid execution requires explicit candidate count, maximum spend, expiry, and approval identity;
- every candidate gets a durable idempotency key and immutable revision;
- client and control-plane restarts resume safely;
- partial candidate failure does not erase successful evidence;
- cancellation stops future work and deterministically cleans run-owned resources;
- promotion remains a separate human-authorized Release Guard action;
- predicted and actual outcomes are recorded separately.
- the selected Lab ranking identity is persisted before Release Guard runs, so a crash cannot merge
  ranking and release evidence into one ambiguous boundary;

Exit gate:

- crash/restart, duplicate request, cancellation, stale lease, partial benchmark, failed quality gate,
  promotion race, and cleanup-to-zero tests pass;
- one local fixture campaign completes from proposal through rejected bad candidate and cleanup;
- no unapproved provider mutation is reachable.

Local exit gate: passed with the in-memory fault driver. Real provider cleanup, actual builder and
benchmark execution, and operation registration remain qualification boundaries rather than implied
capabilities.

### M2 — Versioned optimization capability registry

Priority: P0. Impact: high. Effort: medium.

Status: local foundation complete; mechanism-by-mechanism GPU qualification ongoing.

Delivered locally: exact version/model/precision/accelerator descriptors, fail-closed conflict and
downgrade behavior, concrete NVIDIA accelerator alias normalization, generic Kubernetes GPU-resource
rejection, compilers for runtime-owned continuous batching, qualified vLLM prefix caching, bounded
scheduler token budgets, and one-owner Dynamo proposal compilation, plus a PostgreSQL-enforced
binding between candidate deployment, immutable revision, base model artifact, and optimized artifact provenance. Attention backend,
quantization, speculative decoding, LMCache, and distributed topology descriptors remain deferred
until their exact tuples qualify.

Represent mechanisms as compatibility facts, not arbitrary flags:

```text
runtime + exact version + model architecture + artifact precision + accelerator
    → supported mechanism + constraints + compiler + qualification state
```

Initial mechanisms:

- vLLM and SGLang continuous batching;
- prefix caching;
- chunked prefill and scheduler token budgets;
- attention backend selection;
- weight and KV-cache precision;
- speculative decoding;
- aggregated versus prefill/decode topology;
- KV reuse/offload.

Runtime-specific compilers translate a structured mechanism into exact arguments. Unsupported or
unknown combinations fail closed. Generated flags are output details, not the public domain model.

Exit gate:

- exact-version compatibility, enum expansion, conflicting mechanisms, model mismatch, accelerator
  mismatch, and downgrade tests pass;
- every produced argument links to a capability descriptor and evidence source.

### M3 — AISimulate modeled-candidate source

Priority: P1 after upstream gates. Impact: high for multi-GPU search. Effort: medium.

Status: intentionally blocked on the upstream admission gates above. AIConfigurator remains the
optional replaceable source; no unstable AISimulate development artifact is shipped.

Use AISimulate for GPU-free prediction, recommendation, topology search, and scheduler-aware replay.
Request only the top bounded candidates, then pass them through InferCrane's capability registry.

Persist:

- exact AISimulate/Dynamo versions;
- model/system database identity;
- input digest and output digest;
- modeled TTFT, TPOT, throughput, and topology;
- unsupported fields and warnings;
- model age and calibration error where available.

Exit gate:

- isolated adapter conformance passes;
- the top-N result is deterministic for pinned inputs;
- unsupported tuples fail closed;
- modeled-to-measured error and top-k recall are recorded, not hidden;
- no modeled candidate can bypass AIPerf and quality gates.

### M4 — Immutable quantized-artifact pipeline

Priority: P1. Impact: very high. Effort: medium-high.

Status: provenance and control-plane lifecycle complete; external builds and GPU qualification
pending.

Delivered locally: LLM Compressor and ModelOpt builder boundaries, FP8/AWQ/GPTQ/NVFP4 plan presets,
digest-pinned external builders, base/calibration/config/hardware/license provenance, immutable output
attestation, exact candidate/revision/quality binding, and failure/duplicate/cross-tenant tests.

Integrate LLM Compressor first. Add ModelOpt only for NVIDIA/TensorRT-specific paths where it adds a
qualified capability.

Start with:

1. FP8 weight/activation candidates on compatible Hopper/Ada paths;
2. AWQ/GPTQ/Marlin weight-only candidates for memory-constrained serving;
3. KV-cache quantization only after long-context quality and stability qualification.

Every artifact records base model commit, tool/version, algorithm, calibration-dataset digest,
configuration, output digest, license, and builder image. Prompt or calibration content is not stored
in public evidence.

Exit gate:

- corrupted output, incompatible GPU/runtime, calibration mismatch, quality regression, builder crash,
  duplicate build, and artifact cleanup tests pass;
- quantized candidates cannot promote without signed semantic-quality evidence;
- exact baseline and candidate AIPerf workloads are comparable.

### M5 — Speculative-decoding candidate adapter

Priority: P1. Impact: high for decode-bound workloads. Effort: medium.

Status: artifact/provenance adapter complete; runtime activation and GPU qualification pending.

EAGLE-3, MTP, and DFlash plans use the vLLM Speculators external-builder boundary and require an exact
verifier plus quality review. No runtime speculative flag becomes executable until acceptance rate,
tail latency, streaming, cancellation, and output-equivalence evidence exists for that tuple.

Reuse vLLM Speculators and upstream runtime support for EAGLE, MTP, DFlash, draft-model, and n-gram
candidates. Enable only mechanisms listed for the exact model/runtime pair.

Measure:

- acceptance rate and accepted tokens per step;
- TTFT, TPOT, output throughput, goodput, memory, and failure rate;
- semantic-quality evidence and deterministic structured/tool-call behavior;
- performance across short, long-generation, and overload workloads.

Exit gate:

- incompatible draft/target pairs are rejected before GPU creation;
- low acceptance or increased tail latency causes candidate rejection;
- speculative streaming, cancellation, and rollback pass real-runtime qualification.

### M6 — LMCache and Dynamo/NIXL qualification

Priority: P1. Impact: high for repeated-prefix and distributed workloads. Effort: high.

Status: Dynamo topology locally simulated; LMCache registered but deliberately non-executable; real
GPU/NIXL/cache qualification pending.

Add executable LMCache configuration behind the serving topology contract. Compare:

1. engine-native prefix cache baseline;
2. LMCache local CPU/disk reuse;
3. Dynamo KV-aware routing;
4. Dynamo prefill/decode with NIXL transfer;
5. KVBM where the exact backend combination is qualified.

Capture cache hit rate, reusable-token ratio, bytes moved, transfer latency, TTFT, TPOT, goodput,
storage cost, and eviction behavior. Enforce tenant and model/revision cache isolation.

Exit gate:

- stale, corrupted, cross-model, cross-tenant, storage-full, worker-loss, transfer-timeout, and
  topology-downgrade tests pass;
- real multi-GPU results show which workload shapes benefit and which regress.

### M7 — TensorRT-LLM runtime adapter

Priority: P2. Impact: high for a narrow set of large-volume NVIDIA tuples. Effort: high.

Status: immutable engine-build provenance complete; runtime adapter and real qualification pending.

Do not make TensorRT-LLM the universal default. Support only exact upstream-qualified
model/GPU/precision tuples. Treat the engine build as an immutable artifact with its own provenance,
hardware compatibility, build duration, storage, and rollback behavior.

Exit gate:

- path-level licensing and transitive redistribution review passes;
- engine build and runtime images are digest-pinned;
- build failure, architecture mismatch, CUDA mismatch, plugin mismatch, and rollback tests pass;
- measured benefit survives quality and total-cost comparison including engine-build amortization.

### M8 — llm-d and AIBrix composition

Priority: P2, customer-demand driven. Impact: medium for Kubernetes fleets. Effort: medium-high.

Status: replaceable ownership boundaries registered; discovery/lifecycle adapters and cluster
qualification pending.

Use their upstream APIs and controllers for scheduling, routing, model download, and heterogeneous
fleet behavior. InferCrane owns stable endpoints, lifecycle intent, evidence, Release Guard, and
qualified Serving Plans. It does not fork or reproduce their schedulers.

Begin with discovery, manifest compilation, observation, and conformance. Add lifecycle ownership only
after exact field ownership, finalizers, upgrade compatibility, and cleanup pass Kind plus real GPU
cluster qualification.

## Qualification corpus

Avoid optimizing around one model. Initial evidence must cover:

- dense instruction: Mistral 7B and Llama 3.1 8B;
- reasoning/distilled: DeepSeek R1 Distill 7B;
- coding: Qwen2.5 Coder 7B;
- embeddings: BGE-M3;
- multimodal: Gemma 3 4B or Qwen2.5-VL 7B;
- later multi-GPU MoE: one current supported DeepSeek, Kimi, or GLM family only after exact runtime,
  license, and hardware qualification.

Workloads:

- interactive short prompt;
- repeated shared prefix;
- long context;
- long generation;
- structured output and tool calling;
- concurrency 1, 8, 32, and bounded overload;
- burst and steady-state arrival patterns.

Hardware evidence should begin with qualified AWS L40S/L4-class paths, then add H100/H200 and a real
Kubernetes topology. Provider availability is not performance evidence.

## Product metrics and stopping rules

Track per exact serving tuple:

- TTFT p50/p95/p99;
- TPOT p50/p95/p99;
- request and output-token throughput;
- SLO goodput and goodput per sourced currency unit;
- GPU utilization and memory headroom;
- cold and warm readiness;
- semantic-quality score and pass/fail;
- modeled versus measured error;
- provision, runtime, and request failure rates;
- realized versus predicted cost after promotion.

An optimization mechanism advances from experimental only when:

1. it beats or matches a stock qualified baseline on its declared objective;
2. configured SLO and quality gates pass;
3. results repeat within a declared tolerance;
4. failure, rollback, and cleanup behavior pass;
5. the exact software, model, artifact, hardware, workload, and cost source are reproducible;
6. at least one workload where it should not be used is documented.

Do not publish a universal percentage improvement. Publish evidence tables for exact tuples.

## Recommended execution order

1. M1 durable optimization campaigns.
2. M2 capability registry.
3. Maintain AIConfigurator calibration and prepare AISimulate adapter fixtures, but wait for stable
   upstream admission gates.
4. M4 quantized artifacts with LLM Compressor.
5. Activate M3 AISimulate when stable and calibrate it against real measurements.
6. M5 speculative decoding.
7. M6 LMCache plus real Dynamo/NIXL qualification.
8. M7 narrow TensorRT-LLM support.
9. M8 llm-d/AIBrix only from demonstrated Kubernetes demand.

This sequence delivers user-visible optimization and evidence before taking on the largest runtime and
distributed-system compatibility surfaces.

## Primary upstream references

- AIConfigurator transition decision: https://github.com/ai-dynamo/aiconfigurator/issues/1517
- AIConfigurator: https://github.com/ai-dynamo/aiconfigurator
- Dynamo: https://github.com/ai-dynamo/dynamo
- vLLM: https://github.com/vllm-project/vllm
- vLLM Speculators: https://github.com/vllm-project/speculators
- LLM Compressor: https://github.com/vllm-project/llm-compressor
- ModelOpt: https://github.com/NVIDIA/Model-Optimizer
- LMCache: https://github.com/LMCache/LMCache
- TensorRT-LLM: https://github.com/NVIDIA/TensorRT-LLM
- llm-d: https://github.com/llm-d/llm-d
- AIBrix: https://github.com/vllm-project/aibrix
- Wafer GPU performance engineering resources:
  https://github.com/wafer-ai/gpu-perf-engineering-resources
