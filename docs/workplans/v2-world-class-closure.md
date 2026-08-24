# v2 world-class closure plan

Status: local closure completed; real-infrastructure and clean-release gates remain.

Updated: 2026-08-24.

## Product standard

InferCrane is world class only when a new team can reach one stable endpoint with less operational
work than assembling a gateway, cloud scripts, autoscaling, monitoring, and rollout logic, while an
advanced platform team can still inspect and control every consequential boundary. Breadth of code
is not the measure. The measure is a short first-value path backed by repeatable failure, performance,
quality, cost, rollback, and cleanup evidence.

## Current comparison

This is an engineering comparison of responsibility boundaries, not a benchmark or pricing claim.
Vendor capabilities change; the linked primary documentation is authoritative for their products.

| Product boundary | Managed-platform reference | InferCrane today | Closure requirement |
| --- | --- | --- | --- |
| First request | Baseten Model APIs and Fireworks on-demand start from an account/API key and vendor capacity | InferCrane can deploy or adopt, but BYOC prerequisites remain visible to the operator | One guided `doctor → recipe → plan → deploy → test` journey with exact remediation and no cloud-console archaeology after prerequisites |
| Existing infrastructure | Managed platforms primarily optimize a vendor-owned deployment path | Observe-only and traffic-managed adoption preserve an existing vLLM, SGLang, LiteLLM, or OpenAI-compatible endpoint | Prove a clean-machine adoption in under ten operator minutes against a real design-partner endpoint |
| Stable environments and release | Baseten environments provide stable URLs, canary/shadow options, autoscaling inheritance, and rollback | Stable endpoint identity, immutable serving plans, signed evidence, Release Guard, stream pinning, drain, and automatic post-promotion rollback pass local gates | Repeat active → candidate → reject and active → candidate → promote → degrade → rollback on a real GPU path |
| Overload | Fireworks documents token rate limits plus explicit `429` and overloaded `503`; Baseten exposes autoscaling and request lifecycle | Bounded concurrency/queueing, distributed quotas, explicit `429`, instance-local pressure, no streaming replay, bounded buffered retries, and one end-to-end request deadline | Run the exact overload matrix at concurrency 1/8/32/128 and prove bounded tail latency and correlation |
| Health | Baseten distinguishes deployment lifecycle/health states | InferCrane separates process liveness, control-plane readiness, admission saturation, runtime readiness, and served-model identity | Present the distinction consistently in CLI, console, alerts, and real disruption evidence |
| Cold start | Baseten documents multi-tier weight delivery, node/cluster reuse, file deduplication, and image streaming | Provider-neutral artifact cache, durable prefetch, AWS encrypted snapshot attachment, startup waterfall, and ETA evidence exist | Measure cache miss versus exact-model snapshot hit and prewarmed runtime image; add equivalent GCP disk and Kubernetes PVC/node-cache adapters |
| Runtime performance | Managed providers control images, caches, kernels, fleet topology, and qualified engine configurations | AIConfigurator/catalog proposals, capability registry, AIPerf, Replay, quality/cost evidence, and optimized-artifact provenance exist | Execute and qualify exact FP8/AWQ/speculative/LMCache/Dynamo/TensorRT tuples; rank only measured SLO goodput per sourced currency unit |
| Distributed serving | NVIDIA Dynamo provides KV-aware routing and independent prefill/decode pools; KServe provides Kubernetes-native lifecycle/canary primitives | InferCrane has a single-owner experimental Dynamo DGD adapter and Kind API evidence | Qualify real Dynamo operator, NIXL, cache events, worker loss, topology downgrade, and performance before enabling execution claims |
| Operational evidence | Managed dashboards expose vendor-owned telemetry | Request Inspector, Doctor, Replay, monitoring, capacity, FinOps, quality evidence, Release Guard, and Passports share stable endpoint/revision identity | Prove freshness, partial-state, retention, and scale behavior on a real fleet; add continuous evaluator adapters only with explicit sampling/privacy policy |
| Portability and ownership | Managed products optimize the infrastructure they own | Customer-owned AWS, GCP, Kubernetes, RunPod, external APIs, and replaceable runtimes are core contracts | Publish an exact compatibility/evidence matrix; never imply one provider/runtime result transfers to another |

## What InferCrane should not copy

- Do not claim instant hosted capacity while InferCrane remains BYOC-first.
- Do not build proprietary inference kernels, a cloud scheduler, a KV transfer layer, or another
  Kubernetes distribution.
- Do not silently shadow production prompts. Replay and task-quality sampling require explicit data,
  cost, retention, and side-effect policy.
- Do not expose raw engine flags as the primary developer experience. The common path starts from an
  objective and a qualified recipe; advanced users can inspect the compiled mechanism set.
- Do not market modeled AIConfigurator output as a benchmark or recommendation.

## Dependency-ordered closure

1. Freeze and review the v2 change set; produce a clean commit-addressed release candidate.
2. Complete request-path budgets, health semantics, overload tests, and real AWS disruption/rollback.
3. Prove AWS runtime-image plus artifact-snapshot cache hits and publish the startup waterfall.
4. Dogfood clean-machine deploy and observe-only adoption; remove every unnecessary decision.
5. Execute one optimization loop on at least two model families and two workload shapes.
6. Qualify GCP and GPU Kubernetes when quota/infrastructure is available.
7. Qualify Dynamo/LMCache only on a workload with repeatable shared-prefix or disaggregation benefit.
8. Conduct hosted console, documentation, accessibility, and operator recovery review.
9. Publish only exact claims whose evidence bundle is archived and reproducible.

## Release gates

The v2 candidate is launchable when:

- all local, race, migration, fault-fixture, Docker, Kind, web, documentation, SDK, Terraform, and
  release-package gates pass from a clean commit;
- one real AWS lifecycle proves deploy, request, overload, candidate rejection, promotion, rollback,
  active-stream drain, deletion, and final direct inventory zero;
- one cache-hit comparison and one stock-versus-qualified-profile comparison use identical workloads;
- a clean-machine user completes deploy or adoption without repository knowledge;
- unsupported, stale, modeled, and unavailable evidence is never rendered as measured or qualified;
- remaining GCP, GPU Kubernetes, Dynamo/NIXL, optimized artifacts, and hosted-console boundaries are
  listed as pending rather than hidden.

## Primary references

- Baseten architecture, environments, weight delivery, and request lifecycle:
  https://docs.baseten.co/concepts/howbasetenworks
- Baseten cold starts: https://docs.baseten.co/deployment/autoscaling/cold-starts
- Baseten environments: https://docs.baseten.co/deployment/environments
- Baseten health states: https://docs.baseten.co/observability/health
- Fireworks deployment quickstart: https://docs.fireworks.ai/getting-started/ondemand-quickstart
- Fireworks autoscaling: https://docs.fireworks.ai/deployments/autoscaling
- Fireworks overload and rate limits: https://docs.fireworks.ai/serverless/rate-limits
- NVIDIA Dynamo disaggregated serving:
  https://docs.dynamo.nvidia.com/dynamo/user-guides/disaggregated-serving
- NVIDIA Dynamo KV-aware routing:
  https://docs.dynamo.nvidia.com/dynamo/dev/cli/kv-aware-routing/overview
- KServe architecture and deployment capabilities: https://kserve.github.io/website/
- vLLM metrics design: https://github.com/vllm-project/vllm/blob/main/docs/design/metrics.md

## 2026-08-24 local closure result

Completed without paid-provider mutation:

- one endpoint-wide request deadline now covers admission, every buffered retry attempt, and response
  streaming; partial streams are never replayed;
- cancelled and expired requests persist content-free evidence with a detached recorder context;
- the console exposes the same deadline as a simple endpoint policy instead of an engine flag;
- project scaffolding prints the provider-specific preflight, plan, durable deploy, first request, and
  deterministic Doctor journey using the actual generated endpoint name;
- provider documentation now distinguishes narrow archived AWS evidence from unqualified tuples;
- host, rebuilt-container race, PostgreSQL migration/contention, failure recovery, HA, backup/restore,
  network-partition, simulated-cloud, Kind, KWOK, Kubernetes-version, browser, empty-control-plane,
  docs, SDK, Terraform, fuzz, dead-code, and vulnerability gates pass.

Retained local evidence:

- developer environment: `.infercrane/dev-check/20260824T121412Z-1789`;
- black-box product acceptance: `.infercrane/product-acceptance/20260824T122147Z-world-class-local`.

This does **not** close the release gate. The worktree must be reviewed and committed before a
commit-addressed artifact can pass release packaging. Real AWS disruption/cache-hit, GCP GPU,
real-GPU Kubernetes/KServe, Dynamo/NIXL/LMCache, and optimized-artifact builds remain explicit
external qualification.
