# Project status

Status labels are strict: **implemented** is tested in this repository; **experimental** works
but lacks production qualification; **planned** is not a product capability.

| Capability | Status | Notes |
|---|---|---|
| Side-effect-free deployment planning | Implemented | Deterministic human and JSON output; live cost estimation is not yet available. |
| CLI discovery and contexts | Implemented | Cobra grouped help, suggestions, generated completion, named contexts, authenticated identity, and durable timeline following are wired. |
| Terminal operations workspace | Experimental | Responsive evidence views and state-valid guarded actions use authenticated APIs and durable operations; `--read-only` disables mutations, and broader terminal compatibility qualification remains. |
| Embedded operations dashboard | Experimental | Authenticated fleet/evidence views, explicit stale and partial states, tab-scoped credentials, and guarded cooperative cancellation pass local security and Docker tests; broader browser qualification remains. |
| Environment diagnostics | Implemented | Read-only checks for API auth, PostgreSQL, vLLM Router, SkyPilot/RunPod, Serverless, and AWS role assumption. |
| Declarative existing-target apply | Implemented | Atomic convergence of routing, replica bounds, and target membership. |
| Durable lifecycle operations | Implemented | Idempotency, progress, retry classification, and cooperative cancellation state. |
| Leased operation execution engine | Experimental | Existing-target, elastic, revision, scale, delete, and serverless handlers resume from durable state; real provider restart qualification remains. |
| Versioned control-plane API | Experimental | Tenant-scoped operations, apply, targets, deployments, orphans, audit, quota, and principal endpoints exist. |
| OpenAPI and generated SDKs | Experimental | Full route coverage, deterministic Python/TypeScript low-level generation, typed durable helpers, and hermetic SSE tests are implemented; public package publication is deferred. |
| Terraform provider | Experimental | Logical deployment CRUD, import, guarded update, and interrupted-apply adoption pass real Terraform protocol fixtures; Registry publication is deferred. |
| GitHub delivery action | Experimental | Read-only semantic PR plans, explicit protected apply, exact-revision checks, bounded summaries, and secret redaction are implemented; Marketplace publication is deferred. |
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
| Provider-native serverless contract | Experimental | Replay-safe endpoint lifecycle, scale-to-zero routing, cancellation, accounting, and registered direct-target reconciliation are implemented. RunPod is the first adapter; real cold/warm acceptance remains. |
| Cold-start intelligence | Experimental | Grounded worker-at-arrival and gateway TTFT evidence persist; provider-hidden substages remain unavailable. |
| Deterministic explanations | Implemented | Deployment, scaling, rollout, and cold-start output is reproduced from persisted state and measurements. |
| Integration registration and qualification | Implemented | Lifecycle backends resolve by cloud/runtime and durable adapter identity; the release qualification matrix remains separate. |
| Versioned provider/runtime contract inventory | Experimental | V1 descriptors, validated executable bindings, capabilities, authenticated inspection, and hermetic conformance evidence are implemented; real-provider/runtime qualification remains deferred. |
| Portable custom OCI workloads | Experimental | Immutable digest, argv, OpenAI protocol, standard probes, cancellation/drain and shutdown declarations persist in revisions and pass hermetic launch conformance; real GPU evidence is deferred. |
| SGLang runtime profile | Experimental | Official v0.5.12 image manifest is digest-pinned and hermetic fixtures exercise Runtime Contract V1 on the AWS EC2 elastic adapter; real GPU and feature-level compatibility evidence is deferred. |
| Tiered developer qualification | Implemented | Fast provider contracts, isolated Docker recovery checks, paid-run locking, and CI evidence separate local correctness from real-cloud qualification. |
| Capacity and runtime adapter contracts | Experimental | GPU/cache-aware deterministic placement exists; the first v0.1 operational composition is SkyPilot/RunPod elastic, RunPod Serverless, and vLLM. |
| Provider capacity preflight | Experimental | Optional provider advisors persist available/constrained/unavailable/unknown evidence; RunPod secure-GPU stock is the first read-only implementation. |
| Scoped tenant identity and RBAC | Experimental | Role-bounded service-account scopes, hashed rotation/revocation credentials, audit attribution, and adversarial tenant isolation are wired. |
| Reference-only secrets | Experimental | Environment references persist without values; additional production secret-manager resolvers remain planned. |
| Governed external capacity | Experimental | Explicit health fallback, privacy acknowledgement, atomic request/cost reservations, OpenRouter, and no-replay streaming behavior are hermetically qualified; real billing evidence is deferred. |
| AWS EC2 BYOC | Experimental | Private-network, role-assumed, tag-owned, immutable-image elastic lifecycle passes Provider Contract V1 hermetically; real AWS evidence is deferred. |
| SkyPilot RunPod provisioning | Experimental | Requires credentialed elastic lifecycle acceptance and soak tests. |
| Production performance claims | Planned | Must be backed by reproducible real-vLLM benchmarks. |

Update this table whenever a feature changes maturity. Never describe planned behavior in present
tense elsewhere in the documentation.
