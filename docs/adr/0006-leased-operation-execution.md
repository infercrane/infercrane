# ADR 0006: Lease durable operations through PostgreSQL

- Status: Accepted
- Date: 2026-08-09

## Context

Provisioning and fleet mutations may outlive clients and control-plane replicas. In-memory jobs or
unleased database rows allow duplicate cloud resources, lost work, and unsafe retries.

## Decision

Queued operations are claimed with row locks and `SKIP LOCKED`. A claim has an owner and expiration;
the worker heartbeats while executing. An expired lease is claimable by another replica. Retryable
failures return to pending with bounded exponential delay and a maximum attempt count. Cancellation
is terminal before execution and cooperative at safe boundaries while running.

The first `/api/v1` contract exposes authenticated operation read and cancellation. API errors have
stable machine-readable codes. Production configuration requires a 32-character secret and TLS for
PostgreSQL.

## Consequences

Handlers must be idempotent and treat lease loss as cancellation. Provider resources require stable
identities for recovery. Deploy/apply remains synchronous until its orchestration is extracted into
registered handlers, so the execution engine is not yet production-qualified end to end.

## Verification

Unit tests cover completion, retry delay, and attempt exhaustion. PostgreSQL tests prove exclusive
claims, expired-lease recovery, and queued cancellation. API tests cover authentication and stable
errors; Docker runs the full race and PostgreSQL suites.
