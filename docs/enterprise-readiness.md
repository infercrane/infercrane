# Enterprise readiness plan

Enterprise readiness is ordered by blast radius, not feature visibility. A capability moves to
implemented only when its executable contract is tested; it moves to production-qualified only
after environment evidence exists.

## Priority 0 — trustworthy mutation and identity

1. **Durable execution:** PostgreSQL leases, heartbeats, crash recovery, bounded retry, cancellation,
   and idempotency cover existing targets, provisioned replicas, revisions, scaling, Serverless, and
   deletion. Real multi-provider disruption evidence remains required.
2. **Scoped identity:** hashed credentials, rotation/revocation, tenant context, RBAC enforcement,
   audit attribution, and distributed hard request-rate leases are implemented.
3. **Control-plane API:** operation read/cancel, asynchronous apply, targets, deployments, orphans,
   audit events, quotas, and principals are implemented. Optimistic concurrency and remaining
   provider/scaling resources remain.
4. **Security defaults:** production secret strength, PostgreSQL TLS, least-privilege containers,
   dependency scanning, provenance, and documented threat boundaries.

## Priority 1 — observable and recoverable operation

- OpenTelemetry traces and operation/reconciliation metrics.
- Multi-replica control-plane failover, fenced work recovery, mixed-version protocol admission, and
  local PostgreSQL backup/restore drills are implemented. Customer HA PostgreSQL topology evidence remains external.
- Rolling upgrade, rollback, and schema compatibility policy is executable; customer RTO/RPO remains operator-measured.
- Provider timeouts, rate limits, circuit breakers, and orphan cleanup.
- Real vLLM streaming, cancellation, overload, and 24–72 hour soak evidence.

## Priority 2 — efficient fleet automation

- Production SkyPilot RunPod fleet scaling with distributed control-plane ownership.
- Drain, warm-up, rollback, capacity exhaustion, and scale-to-zero policies.
- Live capacity inventory, model cache management, and timestamped provider prices.
- Every additional provider/runtime combination requires contract plus real-environment evidence.

## Release blockers

Do not call InferCrane enterprise-ready until all Priority 0 paths are integrated end to end, tenant
isolation has adversarial tests, operations resume after process loss, and the production
qualification gates in [Compatibility and qualification](compatibility.md) have evidence.
