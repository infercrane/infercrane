# InferCrane feature invariants

These invariants define observable correctness. A passing test is relevant only when it proves one or more
of these properties at the appropriate boundary.

## Control plane and lifecycle

- Deployment plan is side-effect free; apply/deploy creates one durable intent and retries do not duplicate it.
- Operations survive CLI disconnect and worker/control-plane restart; cancellation is cooperative and durable.
- Provider ensure is adoptable after a lost response and creates at most one external resource per intent.
- Readiness requires the intended immutable model/runtime identity, not merely an open port.
- Delete is idempotent, restart-safe, withdraws routing first, drains active generations, and leaves no owned resource.
- Revisions/specifications are immutable; invalid or stale candidates cannot replace the active revision.
- Rollback and Release Guard decisions are deterministic, persisted, and require fresh sufficient evidence.

## Routing and inference data plane

- Authenticated aliases resolve through an immutable in-memory route snapshot without synchronous PostgreSQL.
- A request pins one route generation until buffered/streaming completion or cancellation.
- Protocol capability claims fail closed; request/response bodies are forwarded without lossy schema translation.
- Retry budgets never retry streaming or governed external requests and preserve request identity/attempt metadata.
- Tenant, endpoint, plan, binding, revision, replica/provider/runtime and operation telemetry identities remain coherent.
- Prompt/output content and raw session identifiers are not persisted by default.

## Endpoints, adoption and sessions

- Logical model, environment, endpoint, binding and serving-plan references are tenant-isolated and immutable where specified.
- Observe-only adoption never routes or mutates; ownership promotion is explicit and monotonic.
- Context Passport is bounded and content-free; affinity is best effort and healthy routing overrides a stale hint.
- Context Passport does not imply durable KV; request survival fails closed unless delegated capability evidence is qualified.

## Scaling, admission, async and burst

- Autoscaling respects min/max, cooldown/stability policy, restart intent and generation-safe scale-down.
- Admission enforces concurrency, queue depth/time, size, token, priority and retry limits without database hot-path dependency.
- Distributed quota leasing cannot exceed tenant bounds and fails safely when leases/storage are unavailable.
- Async jobs are idempotent, encrypted when retained, bounded by TTL/deadline/retry, cancellable and webhook-signed.
- External fallback requires explicit privacy acknowledgement and hard request/cost reservation.
- Burst Guard requires fresh sustained evidence, healthy overflow and hard incremental-cost bounds; it cannot bypass governed fallback.

## Evidence and intelligence

- Request Inspector/Doctor/explain output derives only from persisted evidence and never invents causes.
- Benchmarks use AIPerf, persist exact reproduction configuration and never claim unavailable metrics or costs.
- ModelArtifact resolves mutable references to immutable identity; cache observations expire and prefetch remains provider delegated.
- Recipes are content-addressed and require matching immutable artifact/runtime/benchmark evidence.
- Lab, Replay, Capacity Intelligence, recommendations, FinOps and Autopilot preserve tenant scope, evidence class,
  window/sample/digest, and leave missing facts unavailable.
- Replay stores workload shape, not content; explicit execution requires cost acknowledgement.
- Autopilot approval is auditable but never mutates production.
- Inference Passports are canonical, signed, offline-verifiable and fail tamper checks.

## Providers, runtimes and infrastructure

- Core domain types remain provider/runtime neutral; executable adapters own provider/runtime details.
- Provider ensure/observe/delete and inventory satisfy idempotency, adoption, error normalization and ownership contracts.
- Runtime/protocol compatibility is versioned and independently qualified; unsupported claims fail closed.
- Kubernetes applies strict field ownership and namespace/RBAC boundaries; KServe is CRD-gated and single-owner.
- Provider-native serverless delegates worker scheduling and reconciles endpoints/orphans without duplicate resources.

## Persistence, HA and security

- PostgreSQL is the source of truth; migrations are ordered, checksummed, serialized, forward-only and reject newer schemas.
- Leases/fencing prevent stale workers and mixed-version instances from committing unsafe mutations.
- Backup/restore preserves migration ledger and resource identity; restored startup reconciles instead of recreating resources.
- Authentication, authorization, scopes and tenant isolation fail closed; rotation/revocation take effect.
- Secret values never appear in persisted state, API output, logs, events, diagnostics, reports or generated artifacts.
- TLS/mTLS/private identity configuration rejects partial or insecure production configuration.

## User surfaces and delivery

- Every CLI command has valid/invalid/config/output/exit behavior consistent with API contracts and durable operations.
- OpenAPI, server routes, Python/TypeScript SDKs and Terraform remain generated/behaviorally compatible.
- Dashboard handles authentication, loading, empty and error states without leaking credentials.
- Archives, checksums, SBOMs, Docker startup, native smoke and Homebrew metadata bind to the exact RC version.
- Acceptance workflows require explicit paid approval, fixed run identities, resumability, guarded cleanup and inventory evidence.
