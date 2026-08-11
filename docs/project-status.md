# Project status

Status labels are strict: **implemented** is tested in this repository; **experimental** works
but lacks production qualification; **planned** is not a product capability.

| Capability | Status | Notes |
|---|---|---|
| Side-effect-free deployment planning | Implemented | Deterministic human and JSON output; live cost estimation is not yet available. |
| CLI discovery and contexts | Implemented | Cobra grouped help, suggestions, generated completion, named contexts, authenticated identity, and durable timeline following are wired. |
| Terminal operations workspace | Experimental | Responsive evidence views and state-valid guarded actions use authenticated APIs and durable operations; `--read-only` disables mutations, and broader terminal compatibility qualification remains. |
| Embedded operations dashboard | Experimental | Authenticated fleet/evidence views, explicit stale and partial states, tab-scoped credentials, and guarded cooperative cancellation pass local security and Docker tests; broader browser qualification remains. |
| Environment diagnostics | Implemented | Read-only checks for API auth, PostgreSQL, vLLM Router, SkyPilot/RunPod, Serverless, AWS role assumption, and Kubernetes API/RBAC. |
| Declarative existing-target apply | Implemented | Atomic convergence of routing, replica bounds, and target membership. |
| Durable lifecycle operations | Implemented | Idempotency, progress, retry classification, and cooperative cancellation state. |
| Leased operation execution engine | Experimental | Existing-target, elastic, revision, scale, delete, and serverless handlers resume from durable state; real provider restart qualification remains. |
| Versioned control-plane API | Experimental | Tenant-scoped operations, apply, targets, deployments, orphans, audit, quota, and principal endpoints exist. |
| OpenAPI and generated SDKs | Experimental | Full route coverage, deterministic Python/TypeScript low-level generation, typed durable helpers, and hermetic SSE tests are implemented; public package publication is deferred. |
| Terraform provider | Experimental | Logical deployment CRUD, import, guarded update, and interrupted-apply adoption pass real Terraform protocol fixtures; Registry publication is deferred. |
| GitHub delivery action | Experimental | Read-only semantic PR plans, explicit protected apply, exact-revision plus verified-passport checks, bounded summaries, and secret redaction are implemented; Marketplace publication is deferred. |
| Production configuration gates | Implemented | Production mode enforces strong API secrets and PostgreSQL TLS; the base Compose stack is provider-neutral and RunPod, AWS, and Kubernetes use explicit overlays. |
| Safe deletion and orphan discovery | Implemented | Side-effect-free deletion plans, explicit confirmation, and provisioned-resource inventory. |
| PostgreSQL control-plane state | Implemented | Transactional advisory-locked migrations, checksum/gap/newer-binary rejection, every-prefix upgrade coverage, concurrent-start serialization, and bounded pool. |
| Control-plane HA and recovery | Local-qualified | Stateless replicas, fenced work reclaim, protocol-overlap admission, TLS 1.3/mTLS, guarded backup/restore, and restored-database startup pass local Docker drills; customer database topology and RTO/RPO remain external. |
| Release packaging | Implemented | Exact-version darwin/linux amd64/arm64 archives, checksums, SPDX SBOMs, native smoke verification, and generated Homebrew formula run locally without publication. |
| Existing vLLM target registration | Implemented | Persistent, idempotent registration. |
| Logical deployments and aliases | Implemented | One common upstream model per deployment. |
| Stable endpoint domain | Local-qualified | Logical models, environments, immutable serving plans, lifecycle-managed deployment bindings, route generation pinning, legacy alias backfill, and deterministic endpoint Release Guard pass the v1.1 clean-tree gates. External import/adopt begins in v1.2. |
| Incremental adoption and diagnostics | Local-qualified | Observe-only and traffic-managed adoption, content-free request inspection, deterministic Doctor findings, and bounded signed webhook alerts pass the v1.2 clean-tree gates. Real external workloads remain deferred to consolidated manual qualification. |
| Capability-qualified inference protocols | Local-qualified | Chat, Completions, Embeddings, Responses, and online chat-batch paths are independently gated and faithfully proxied. The pinned vLLM profile locally qualifies Chat, Completions, and model-dependent Embeddings; Responses and online batch require a newer explicitly qualified runtime profile. |
| OpenAI-compatible chat proxy | Implemented | Streaming, auth, alias rewrite, request accounting. |
| vLLM health and model reconciliation | Implemented | Bounded concurrent probes. |
| Supervised vLLM Router processes | Implemented | Instance-owned generations and deterministic ports. |
| Prometheus gateway telemetry | Implemented | Core counters/gauges; expand before public SLOs. |
| Prometheus latency histograms and alerts | Implemented | Baseline rules require workload-specific tuning. |
| Reproducible benchmark and recovery tooling | Experimental | AIPerf execution and benchmark history are implemented; local control-plane HA/restore is qualified while real GPU and customer PostgreSQL evidence remain external. |
| Model Recipes and Inference Lab | Local-qualified | Immutable artifact/revision/AIPerf recipes and tenant-scoped measured comparisons are implemented; public registry and real GPU catalog evidence are deferred. |
| Provider pricing contract | Implemented | Timestamp and staleness semantics; no live provider catalog is shipped. |
| Bounded autoscaling controller | Experimental | Durable fleet scaling and router-fenced scale-down are enabled; real RunPod 1→N→1 acceptance remains. |
| Immutable ModelArtifact identity | Experimental | Hugging Face references resolve to immutable commits with grounded metadata; real transfer/cache evidence remains. |
| Release Guard V2 | Experimental | Compatibility, explicit bounded AIPerf, performance, error, sourced-cost policy, persisted post-promotion monitoring, and restart-safe automatic rollback pass hermetic tests; real active/candidate evidence remains. |
| Inference Passports | Experimental | Canonical Ed25519-signed revision, artifact, benchmark, cold-start and policy evidence is byte-preserving, tenant-safe, and offline-verifiable; organizational key custody and public release qualification remain. |
| Provider-native serverless contract | Experimental | Replay-safe endpoint lifecycle, scale-to-zero routing, cancellation, accounting, and registered direct-target reconciliation are implemented. RunPod is the first adapter; real cold/warm acceptance remains. |
| Cold-start intelligence | Experimental | Grounded worker-at-arrival and gateway TTFT evidence persist; provider-hidden substages remain unavailable. |
| Deterministic explanations | Implemented | Deployment, scaling, rollout, and cold-start output is reproduced from persisted state and measurements. |
| Integration registration and qualification | Implemented | Lifecycle backends resolve by cloud/runtime and durable adapter identity; the release qualification matrix remains separate. |
| Versioned provider/runtime contract inventory | Experimental | V1 descriptors, validated executable bindings, capabilities, authenticated inspection, and hermetic conformance evidence are implemented; real-provider/runtime qualification remains deferred. |
| Portable custom OCI workloads | Experimental | Immutable digest, argv, OpenAI protocol, standard probes, cancellation/drain and shutdown declarations persist in revisions and pass hermetic launch conformance; real GPU evidence is deferred. |
| SGLang runtime profile | Experimental | Official v0.5.12 image manifest is digest-pinned and hermetic fixtures exercise Runtime Contract V1 on the AWS EC2 elastic adapter; real GPU and feature-level compatibility evidence is deferred. |
| Tiered developer qualification | Implemented | Fast provider contracts, isolated Docker recovery checks, paid-run locking, and CI evidence separate local correctness from real-cloud qualification. |
| Capacity and runtime adapter contracts | Experimental | GPU/cache-aware deterministic placement exists; adapters are registered independently for RunPod, AWS EC2, Kubernetes, governed external targets, vLLM, SGLang, and custom OCI with explicit qualification states. |
| Provider capacity preflight | Experimental | Optional provider advisors persist available/constrained/unavailable/unknown evidence; RunPod secure-GPU stock is the first read-only implementation. |
| Inference decisions and SLO policy | Experimental | Versioned deterministic recommendations persist canonical benchmark provenance, exact compatibility, missing signals and sourced cost constraints; autonomous apply is excluded. |
| Scoped tenant identity and RBAC | Experimental | Role-bounded service-account scopes, hashed rotation/revocation credentials, audit attribution, and adversarial tenant isolation are wired. |
| Distributed request-rate quotas | Implemented | PostgreSQL reserves aggregate UTC-minute leases; gateways authorize from memory and fail closed without adding database reads to the inference path. |
| Reference-only secrets | Experimental | Environment references persist without values; additional production secret-manager resolvers remain planned. |
| Governed external capacity | Experimental | Explicit health/queue overflow, hysteresis, cooldown, privacy acknowledgement, atomic request/cost reservations, OpenRouter, and no-replay streaming behavior are hermetically qualified; real billing evidence is deferred. |
| AWS EC2 BYOC | Experimental | Private-network, role-assumed, tag-owned, immutable-image elastic lifecycle passes Provider Contract V1 hermetically; real AWS evidence is deferred. |
| GCP Compute BYOC | Experimental | Private-network, attached-identity, label-owned, immutable-image lifecycle passes hermetic adapter tests; real GCP GPU evidence is deferred. |
| Managed provider profiles | Planned | AWS ASG/EKS/SageMaker/Bedrock, GCP MIG/GKE/Vertex, and CoreWeave CKS have explicit ownership/capability boundaries but are not executable or locally qualified. |
| Kubernetes elastic | Experimental | Namespace-scoped Deployment/Service and optional standard KServe ownership pass hermetic and Kind lifecycle gates; real Kubernetes GPU/runtime evidence is deferred. |
| SkyPilot RunPod provisioning | Experimental | Requires credentialed elastic lifecycle acceptance and soak tests. |
| Production performance claims | Planned | Must be backed by reproducible real-vLLM benchmarks. |

Update this table whenever a feature changes maturity. Never describe planned behavior in present
tense elsewhere in the documentation.
