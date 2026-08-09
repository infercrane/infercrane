# ADR 0002: PostgreSQL is the production source of truth

Status: Accepted  
Date: 2026-08-09

## Context

The control plane must support concurrent gateway replicas, operational backups, failover,
rolling deployments, and sustained request accounting. SQLite serializes writers and binds state
to one filesystem.

## Decision

Use PostgreSQL through `database/sql` and pgx. Apply embedded, ordered migrations transactionally
under a PostgreSQL advisory lock. Bound every process connection pool and retain request records
for a configurable period.

## Consequences

Production requires PostgreSQL and database operations expertise. Development and CI require an
isolated database. Horizontal replicas can safely share durable desired and observed state.

## Rejected alternatives

- SQLite: appropriate for a local POC, not the production concurrency/failover model.
- An ORM: unnecessary abstraction and less visible query behavior for the current schema.
