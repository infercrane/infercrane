# Project status

Status labels are strict: **implemented** is tested in this repository; **experimental** works
but lacks production qualification; **planned** is not a product capability.

| Capability | Status | Notes |
|---|---|---|
| Side-effect-free deployment planning | Implemented | Deterministic human and JSON output; live cost estimation is not yet available. |
| Environment diagnostics | Implemented | Read-only checks for API auth, PostgreSQL, vLLM Router, and optional SkyPilot. |
| Declarative existing-target apply | Implemented | Atomic convergence of routing, replica bounds, and target membership. |
| Durable lifecycle operations | Implemented | Idempotency, progress, retry classification, and cooperative cancellation state. |
| Leased operation execution engine | Experimental | Asynchronous existing-target apply is crash-recoverable; cloud provisioning and deletion handlers remain. |
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
| Reproducible load and recovery tooling | Implemented | Local stack/load and backup/restore commands; real GPU and HA evidence remains external. |
| Provider pricing contract | Implemented | Timestamp and staleness semantics; no live provider catalog is shipped. |
| Bounded autoscaling controller | Experimental | Stability windows, cooldowns, bounds, decisions, and persistence exist; no production fleet scaler is enabled. |
| Capacity and runtime adapter contracts | Experimental | GPU/cache-aware deterministic placement exists; only current SkyPilot/vLLM path is operational. |
| Scoped tenant identity and RBAC | Experimental | Hashed rotation/revocation credentials and resource isolation are wired; distributed request-rate enforcement and adversarial qualification remain. |
| SkyPilot provisioning | Experimental | Requires credentialed cloud acceptance and soak tests. |
| Kubernetes deployment baseline | Experimental | Requires environment-specific secrets, policies and sizing. |
| AIBrix or KServe backends | Planned | No adapter implementation exists. |
| Production performance claims | Planned | Must be backed by reproducible real-vLLM benchmarks. |

Update this table whenever a feature changes maturity. Never describe planned behavior in present
tense elsewhere in the documentation.
