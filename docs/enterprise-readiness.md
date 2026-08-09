# Enterprise readiness plan

Enterprise readiness is ordered by blast radius, not feature visibility. A capability moves to
implemented only when its executable contract is tested; it moves to production-qualified only
after environment evidence exists.

## Priority 0 — trustworthy mutation and identity

1. **Durable execution:** PostgreSQL leases, heartbeats, crash recovery, bounded retry, cancellation,
   and idempotency are implemented for asynchronous existing-target apply. Cloud provisioning and
   deletion still need leased handlers.
2. **Scoped identity:** hashed credentials, rotation/revocation, tenant context, RBAC enforcement,
   and audit attribution are implemented. Distributed request-rate quota enforcement remains.
3. **Control-plane API:** operation read/cancel, asynchronous apply, targets, deployments, orphans,
   audit events, quotas, and principals are implemented. Optimistic concurrency and remaining
   provider/scaling resources remain.
4. **Security defaults:** production secret strength, PostgreSQL TLS, least-privilege containers,
   dependency scanning, provenance, and documented threat boundaries.

## Priority 1 — observable and recoverable operation

- OpenTelemetry traces and operation/reconciliation metrics.
- HA PostgreSQL and multi-replica failure qualification.
- Backup/restore, rolling upgrade, rollback, and schema compatibility drills.
- Provider timeouts, rate limits, circuit breakers, and orphan cleanup.
- Real vLLM streaming, cancellation, overload, and 24–72 hour soak evidence.

## Priority 2 — efficient fleet automation

- Production SkyPilot RunPod fleet scaling with distributed control-plane ownership.
- Drain, warm-up, rollback, capacity exhaustion, and scale-to-zero policies.
- Live capacity inventory, model cache management, and timestamped provider prices.
- Further runtime and cloud adapters are post-v0.1 work.

## Release blockers

Do not call InferCrane enterprise-ready until all Priority 0 paths are integrated end to end, tenant
isolation has adversarial tests, operations resume after process loss, and the production
qualification gates in [Compatibility and qualification](compatibility.md) have evidence.
