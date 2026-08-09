# Project status

Status labels are strict: **implemented** is tested in this repository; **experimental** works
but lacks production qualification; **planned** is not a product capability.

| Capability | Status | Notes |
|---|---|---|
| Side-effect-free deployment planning | Implemented | Deterministic human and JSON output; live cost estimation is not yet available. |
| Environment diagnostics | Implemented | Read-only checks for API auth, PostgreSQL, vLLM Router, and optional SkyPilot. |
| Declarative existing-target apply | Implemented | Atomic convergence of routing, replica bounds, and target membership. |
| Durable lifecycle operations | Implemented | Idempotency, progress, retry classification, and cooperative cancellation state. |
| Leased operation execution engine | Experimental | Existing-target, elastic, revision, scale, delete, and serverless handlers resume from durable state; real provider restart qualification remains. |
| Versioned control-plane API | Experimental | Tenant-scoped operations, apply, targets, deployments, orphans, audit, quota, and principal endpoints exist. |
| Production configuration gates | Implemented | Production mode enforces strong API secrets and PostgreSQL TLS. |
| Safe deletion and orphan discovery | Implemented | Side-effect-free deletion plans, explicit confirmation, and provisioned-resource inventory. |
| PostgreSQL control-plane state | Implemented | Transactional embedded migrations and bounded pool. |
| Existing vLLM target registration | Implemented | Persistent, idempotent registration. |
| Logical deployments and aliases | Implemented | One common upstream model per deployment. |
| OpenAI-compatible chat proxy | Implemented | Streaming, auth, alias rewrite, request accounting. |
| vLLM health and model reconciliation | Implemented | Bounded concurrent probes. |
| Supervised vLLM Router processes | Implemented | Instance-owned generations and deterministic ports. |
| Prometheus gateway telemetry | Implemented | Core counters/gauges; expand before public SLOs. |
| Prometheus latency histograms and alerts | Implemented | Baseline rules require workload-specific tuning. |
| Reproducible benchmark and recovery tooling | Experimental | AIPerf execution and benchmark history are implemented; real GPU evidence and HA qualification remain external. |
| Provider pricing contract | Implemented | Timestamp and staleness semantics; no live provider catalog is shipped. |
| Bounded autoscaling controller | Experimental | Durable fleet scaling and router-fenced scale-down are enabled; real RunPod 1→N→1 acceptance remains. |
| Immutable ModelArtifact identity | Experimental | Hugging Face references resolve to immutable commits with grounded metadata; real transfer/cache evidence remains. |
| Release Guard | Experimental | Policies, measurements, evaluations, and deterministic promote/reject reasons persist; real active/candidate evidence remains. |
| RunPod Serverless | Experimental | Replay-safe endpoint lifecycle, scale-to-zero routing, cancellation, and accounting are implemented; real cold/warm acceptance remains. |
| Cold-start intelligence | Experimental | Grounded worker-at-arrival and gateway TTFT evidence persist; provider-hidden substages remain unavailable. |
| Deterministic explanations | Implemented | Deployment, scaling, rollout, and cold-start output is reproduced from persisted state and measurements. |
| Capacity and runtime adapter contracts | Experimental | GPU/cache-aware deterministic placement exists; v0.1 operational paths are SkyPilot/RunPod elastic, RunPod Serverless, and vLLM. |
| Scoped tenant identity and RBAC | Experimental | Hashed rotation/revocation credentials and resource isolation are wired; distributed request-rate enforcement and adversarial qualification remain. |
| SkyPilot RunPod provisioning | Experimental | Requires credentialed elastic lifecycle acceptance and soak tests. |
| Production performance claims | Planned | Must be backed by reproducible real-vLLM benchmarks. |

Update this table whenever a feature changes maturity. Never describe planned behavior in present
tense elsewhere in the documentation.
