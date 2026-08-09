# ADR 0005: Durable operations and explicit fleet policy boundaries

- Status: Accepted
- Date: 2026-08-09

## Context

Cloud provisioning and fleet changes outlive CLI processes, may be retried, and can create expensive
resources. Autoscaling, placement, authorization, quotas, and prices also change independently from
provider implementations. Encoding these decisions directly in CLI or provider code would make
recovery unsafe and future integrations inconsistent.

## Decision

Every deploy/apply mutation receives a PostgreSQL operation record with an optional tenant-scoped
idempotency key, progress, result, retry classification, and cooperative cancellation state.
Deletion is preceded by a side-effect-free plan and requires explicit confirmation. Provisioned
targets without active ownership are queryable as orphans.

Autoscaling evaluation, placement, provider/runtime lifecycle, pricing, and authorization are
separate contracts. Policies produce bounded, explainable decisions; adapters execute decisions.
Unknown price is not zero. Tenant quota is enforced transactionally with deployment convergence.

## Consequences

Provider adapters must expose cancellation-safe boundaries and stable resource identities. A
future operation worker can resume retryable work without redesigning persistence. Policy packages
can be tested without clouds or GPUs. Until scoped gateway credentials and a production fleet scaler
are wired, tenancy and autoscaling remain experimental even though their contracts are implemented.

## Verification

Race-tested policy unit tests cover boundaries, cooldowns, placement, roles, and pricing staleness.
PostgreSQL integration tests cover convergence, idempotency, retry state, orphan discovery, and quota
enforcement. The Docker stack smoke test exercises plan, diagnostics, gateway traffic, metrics, and
the benchmark runner.
