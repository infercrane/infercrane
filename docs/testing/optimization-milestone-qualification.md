# Optimization and portability milestone qualification

Date: 2026-08-24  
Scope: local, fixture, PostgreSQL, container, Kind, documentation, and browser evidence only  
Verdict: **LOCAL MILESTONES QUALIFIED — EXTERNAL EXECUTION AND GPU EVIDENCE REMAIN**

## What is proven locally

- Optimization requests produce immutable, compatibility-filtered candidates from the reviewed
  catalog or the pinned optional AIConfigurator process boundary.
- Paid experiment authority is durable, explicitly approved, spend-bounded, expiring, tenant-safe,
  and fenced against stale workers.
- Viable candidates reach a measurement barrier before ranking, so proposal order cannot select a
  winner before peers have comparable evidence.
- Benchmark, revision-bound semantic-quality, exact-workload Lab ranking, and Release Guard evidence
  are distinct durable records. A modeled estimate cannot satisfy a measured gate.
- The selected Lab identity is durably attached in a separate `guarding` state before Release Guard
  executes; restart cannot collapse ranking and release evidence into one write.
- Ranking rejects explicit SLO or cost violations and becomes inconclusive when workload identity,
  sourced cost, or comparable measurements are missing.
- Promotion remains a separate human-authorized action after Release Guard; optimization never
  silently mutates production traffic.
- Optimized artifacts bind the exact base artifact, tool and builder digest, candidate deployment,
  immutable revision, hardware constraints, output digest, and passing quality evidence.
- Capability compilation fails closed for unknown runtime versions, model families, artifact
  precision, accelerators, conflicting mechanisms, and generic Kubernetes GPU resource names.
- Dynamo candidate topology preserves one scaling/routing mutation owner.
- Artifact-cache and capacity selection distinguish definite preflight failures from unknown state,
  reject stale cache observations, honor warm-capacity requirements, and rank deterministic
  readiness/reliability/cost evidence.
- `infercrane discover local` performs bounded read-only NVIDIA inventory discovery without adding a
  scheduler, daemon, or lifecycle owner. Missing `nvidia-smi` is an explicit non-fatal unavailable
  state.
- Community, hosted-control-plane, Enterprise, and future supplied-compute boundaries are explicit.
  The repository exposes no pretend InferCrane Compute option before a supplier is qualified.

## Automated evidence

- `make verify`
- `make test-container`
- rebuilt PostgreSQL migration and store regression suite, including every historical migration
  prefix, concurrent migration startup, deterministic Lab evidence replay, campaign durability, and
  exact optimized-artifact binding
- `go test -race -count=20 ./internal/optimizationcampaign ./internal/nodediscovery ./internal/capacity ./internal/artifactcache`
- `cd docs && npm run check && npm run check:a11y`
- `cd ../infercrane-web && npm run check`
- direct human/JSON smoke of `infercrane discover local` on a host without NVIDIA tooling

## Evidence not created by this milestone

The following remain real-execution boundaries and must not be described as qualified merely because
their adapters, provenance, or failure fixtures pass locally:

- a real LLM Compressor or ModelOpt quantized checkpoint build and quality comparison;
- a real EAGLE, MTP, DFlash, draft-model, or n-gram speculative-decoding run;
- a real TensorRT-LLM engine build and hardware-compatible runtime;
- LMCache eviction/isolation and Dynamo/NIXL/KVBM multi-GPU performance;
- llm-d or AIBrix controller ownership, finalizer, upgrade, and real GPU-cluster behavior;
- real AWS/GCP artifact-cache hits, readiness improvement, pricing, identity, networking, quota, and
  cleanup;
- a real GPU Kubernetes/KServe cluster;
- current NVIDIA driver/runtime inventory across supported bare-metal hosts;
- hosted billing, supplier isolation, prepaid balances, abuse controls, warm pools, and
  InferCrane-supplied capacity.

## Publication boundary

Publish only exact tuple evidence: model and immutable artifact, runtime and version, mechanism,
accelerator, provider/region, workload fingerprint, concurrency, quality gate, cost source, and
InferCrane revision. Do not publish a universal performance improvement or claim Baseten/Fireworks
parity from local fixtures.
